package repl

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"terox/internal/db"
	"terox/internal/render"
	"terox/internal/ui"
)

// Feature 9: дашборд нагрузки по pg_stat_statements. Read-only веер по шардам.
// Текст запроса сервер сам скрывает от ролей без прав (pg_read_all_stats), и terox
// показывает лишь нормализованный обрезанный текст. queryid НЕ глобально стабилен
// между серверами/мажорами — это локальный идентификатор (предупреждаем в подвале).

// statementsOpts — параметры \statements.
type statementsOpts struct {
	orderBy string // total|mean|calls|rows|max
	limit   int
	user    string // фильтр по rolname
	db      string // фильтр по datname
	queryid string // фильтр по queryid
}

// statementsQuery строит запрос top-N к pg_stat_statements и соответствующие
// заголовки. Набор колонок версионный: max/stddev/plan/WAL доступны с PostgreSQL 13.
// Идентичность (datname/rolname) добавляется через join, чтобы было видно, чьи это
// запросы (и для снапшот-ключа). Фильтры user/db/queryid уходят в WHERE.
func statementsQuery(serverVer int, o statementsOpts) (string, []string) {
	pg13 := serverVer == 0 || serverVer >= 130000
	totalCol, meanCol := "total_exec_time", "mean_exec_time"
	if !pg13 {
		totalCol, meanCol = "total_time", "mean_time"
	}
	order := totalCol
	switch o.orderBy {
	case "mean":
		order = meanCol
	case "calls":
		order = "s.calls"
	case "rows":
		order = "s.rows"
	case "max":
		if pg13 {
			order = "s.max_exec_time"
		}
	}

	cols := []string{
		"s.queryid::text AS queryid",
		"d.datname AS db",
		"r.rolname AS role",
		"s.calls::text AS calls",
		fmt.Sprintf("round(s.%s::numeric, 1)::text AS total_ms", totalCol),
		fmt.Sprintf("round(s.%s::numeric, 2)::text AS mean_ms", meanCol),
	}
	headers := []string{"queryid", "db", "role", "calls", "total_ms", "mean_ms"}
	if pg13 {
		cols = append(cols,
			"round(s.max_exec_time::numeric, 2)::text AS max_ms",
			"round(s.stddev_exec_time::numeric, 2)::text AS stddev_ms",
			"s.wal_bytes::text AS wal_bytes")
		headers = append(headers, "max_ms", "stddev_ms", "wal_bytes")
	}
	cols = append(cols,
		"s.rows::text AS rows",
		"(s.shared_blks_hit + s.shared_blks_read)::text AS shared_blks",
		`left(regexp_replace(s.query, '\s+', ' ', 'g'), 60) AS query`)
	headers = append(headers, "rows", "shared_blks", "query")

	var where []string
	if o.user != "" {
		where = append(where, "r.rolname = "+sqlLiteral(o.user))
	}
	if o.db != "" {
		where = append(where, "d.datname = "+sqlLiteral(o.db))
	}
	if o.queryid != "" {
		where = append(where, "s.queryid::text = "+sqlLiteral(o.queryid))
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "\nWHERE " + strings.Join(where, " AND ")
	}
	limit := o.limit
	if limit <= 0 {
		limit = 20
	}
	sql := "SELECT " + strings.Join(cols, ",\n       ") + `
FROM pg_stat_statements s
LEFT JOIN pg_database d ON d.oid = s.dbid
LEFT JOIN pg_roles r ON r.oid = s.userid` + whereSQL + fmt.Sprintf(`
ORDER BY %s DESC NULLS LAST
LIMIT %d`, order, limit)
	return sql, headers
}

// statementsSQL — обратносовместимая обёртка (используется тестами).
func statementsSQL(serverVer int, orderBy string, limit int) string {
	sql, _ := statementsQuery(serverVer, statementsOpts{orderBy: orderBy, limit: limit})
	return sql
}

// doStatements (\statements [N] [--mean|--calls|--rows|--max] [--user U] [--db D]
// [--queryid Q] [--skew]) — top-N запросов по нагрузке из pg_stat_statements.
func (r *REPL) doStatements(args []string) {
	// Подкоманды трендов (F9+): захват снимка нагрузки и сравнение с ним.
	if len(args) > 0 {
		switch args[0] {
		case "snapshot":
			r.doWorkloadSnapshot()
			return
		case "diff":
			r.doWorkloadDiff()
			return
		}
	}
	o := statementsOpts{orderBy: "total", limit: 20}
	skew := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--mean":
			o.orderBy = "mean"
		case "--calls":
			o.orderBy = "calls"
		case "--rows":
			o.orderBy = "rows"
		case "--max":
			o.orderBy = "max"
		case "--skew":
			skew = true
		case "--user", "--db", "--queryid":
			// Флаг последним токеном (без значения) раньше молча игнорировался, и
			// пользователь думал, что фильтр применён. По образцу diagnose.go
			// (--sample/--shard) сообщаем об ошибке вместо тихого пропуска.
			if i+1 >= len(args) {
				fmt.Fprintf(r.out, "flag %s needs a value\n", a)
				return
			}
			i++
			switch a {
			case "--user":
				o.user = args[i]
			case "--db":
				o.db = args[i]
			case "--queryid":
				o.queryid = args[i]
			}
		default:
			if n, err := strconv.Atoi(a); err == nil && n > 0 {
				o.limit = n
			}
		}
	}
	if len(r.targets) == 0 {
		fmt.Fprintln(r.out, "no shard selected")
		return
	}
	if skew {
		r.statementsSkew(o)
		return
	}
	sql, headers := statementsQuery(r.serverVersion(), o)
	results := r.fanoutRead(sql)
	// Graceful degradation: расширение может быть не установлено (42P01) или текст
	// скрыт без прав (42501). Показываем понятную подсказку и не выводим сырой ERR.
	if r.statementsDegradationNotice(results) {
		return
	}
	r.renderDiagResults(headers, results)
	r.statementsCaveats()
}

// statementsDegradationNotice печатает подсказку, если pg_stat_statements недоступен
// на всех шардах, и возвращает true (выводить таблицу незачем). Если расширение есть
// хотя бы на одном шарде — false (рендерим обычно, ошибочные шарды попадут в ERR).
func (r *REPL) statementsDegradationNotice(results []db.ShardResult) bool {
	missing, denied, ok := 0, 0, 0
	for _, sr := range results {
		if sr.Err == nil {
			ok++
			continue
		}
		switch db.ClassifyError(sr.Err).SQLState {
		case "42P01", "42704": // undefined_table / undefined_object — расширение не установлено
			missing++
		case "55000": // object_not_in_prerequisite_state — не в shared_preload_libraries
			missing++
		case "42501": // insufficient_privilege
			denied++
		}
	}
	if ok > 0 {
		if denied > 0 {
			fmt.Fprintln(r.out, ui.Dim.Render("note: some shards hid query text — the role lacks pg_read_all_stats (only its own statements are visible)"))
		}
		return false
	}
	switch {
	case missing > 0:
		fmt.Fprintln(r.out, "pg_stat_statements is not available on the selected shard(s).")
		fmt.Fprintln(r.out, ui.Dim.Render("enable it: add 'pg_stat_statements' to shared_preload_libraries (needs restart), then CREATE EXTENSION pg_stat_statements;"))
		return true
	case denied > 0:
		fmt.Fprintln(r.out, "pg_stat_statements: permission denied — the connection role lacks pg_read_all_stats.")
		return true
	}
	return false
}

// statementsCaveats печатает заметки о queryid и track_planning.
func (r *REPL) statementsCaveats() {
	fmt.Fprintln(r.out, ui.Dim.Render("queryid is server-local — NOT comparable across shards/servers or major versions; planning columns need track_planning (off by default, has overhead)"))
}

// statementsSkew агрегирует один и тот же queryid ПО ШАРДАМ и показывает дисбаланс
// (min/max total_ms и отношение) — узкое место/перекос на отдельных шардах.
func (r *REPL) statementsSkew(o statementsOpts) {
	o.limit = 0 // берём широкий срез и агрегируем сами
	sql, _ := statementsQuery(r.serverVersion(), o)
	results := r.fanoutRead(sql)
	if r.statementsDegradationNotice(results) {
		return
	}
	type agg struct {
		shards       int
		minMs, maxMs float64
		totalCalls   int64
		sample       string
	}
	byQid := map[string]*agg{}
	var order []string
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil {
			continue
		}
		for _, row := range sr.Result.Rows {
			// Колонки queryid(0)/db(1)/role(2)/calls(3)/total_ms(4) и query(последняя)
			// присутствуют при ЛЮБОЙ версии; версионные max/stddev/wal лишь вставляются
			// между ними, не сдвигая эти индексы. Поэтому требуем минимум 5 колонок
			// (раньше жёсткий <11 пропускал все строки на PostgreSQL < 13).
			if len(row) < 5 {
				continue
			}
			qid := str(row[0])
			totalMs, _ := strconv.ParseFloat(str(row[4]), 64)
			calls := asInt64viaString(str(row[3]))
			a := byQid[qid]
			if a == nil {
				a = &agg{minMs: totalMs, maxMs: totalMs, sample: str(row[len(row)-1])}
				byQid[qid] = a
				order = append(order, qid)
			}
			a.shards++
			a.totalCalls += calls
			if totalMs < a.minMs {
				a.minMs = totalMs
			}
			if totalMs > a.maxMs {
				a.maxMs = totalMs
			}
		}
	}
	// Сортируем по перекосу (max/min), затем по max_ms.
	sort.SliceStable(order, func(i, j int) bool {
		return skewRatio(byQid[order[i]].minMs, byQid[order[i]].maxMs) > skewRatio(byQid[order[j]].minMs, byQid[order[j]].maxMs)
	})
	headers := []string{"queryid", "shards", "min_total_ms", "max_total_ms", "skew_ratio", "calls", "query"}
	var rows [][]string
	limit := 20
	for i, qid := range order {
		if i >= limit {
			break
		}
		a := byQid[qid]
		rows = append(rows, []string{
			qid,
			strconv.Itoa(a.shards),
			strconv.FormatFloat(a.minMs, 'f', 1, 64),
			strconv.FormatFloat(a.maxMs, 'f', 1, 64),
			fmt.Sprintf("%.1fx", skewRatio(a.minMs, a.maxMs)),
			strconv.FormatInt(a.totalCalls, 10),
			a.sample,
		})
	}
	if len(rows) == 0 {
		fmt.Fprintln(r.out, "no statements to compare across shards")
		return
	}
	render.Table(r.out, headers, rows, fmt.Sprintf("%d queryid(s) by cross-shard skew (max/min total_ms)", len(rows)))
	r.statementsCaveats()
}

// skewRatio — отношение max/min (1.0, если min<=0).
func skewRatio(min, max float64) float64 {
	if min <= 0 {
		return 1
	}
	return max / min
}

// asInt64viaString парсит целое из строковой ячейки (calls приходит как text).
func asInt64viaString(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
