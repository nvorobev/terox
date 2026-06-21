package repl

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"terox/internal/cluster"
	"terox/internal/db"
	"terox/internal/explain"
)

// finding — один результат диагностики.
type finding struct {
	sev    string // explain.Critical / Warning / Info
	title  string
	detail string
}

// docQuery выполняет read-only диагностический запрос. Возвращает результат и
// флаг "errored": TRUE только при настоящей ошибке, FALSE при безобидном
// пропуске (нет вью/расширения или нет прав). Доступность шарда проверяется
// отдельно (см. reachable).
func (r *REPL) docQuery(shard cluster.Shard, sql string) (*db.Result, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res, err := r.mgr.Exec(ctx, shard, sql, true)
	if err == nil {
		return res, false
	}
	return nil, !benignCheckError(err.Error())
}

// benignCheckError сообщает, является ли ошибка запроса безобидным "этого нет на
// сервере" (нет отношения/столбца/функции или нет прав), а не настоящей ошибкой.
func benignCheckError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"does not exist", "undefined", "permission denied", "must be superuser", "is not supported"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// reachable проверяет, что шард отвечает на тривиальный запрос, чтобы недоступный
// шард не выглядел здоровым.
func (r *REPL) reachable(shard cluster.Shard) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := r.mgr.Exec(ctx, shard, "SELECT 1", true)
	return err
}

func cellStr(row []any, i int) string {
	if i < len(row) && row[i] != nil {
		if b, ok := row[i].([]byte); ok {
			return string(b)
		}
		return fmt.Sprintf("%v", row[i])
	}
	return ""
}

func cellInt(row []any, i int) int64 {
	return asInt64Cell(row, i)
}

func asInt64Cell(row []any, i int) int64 {
	if i >= len(row) || row[i] == nil {
		return 0
	}
	switch n := row[i].(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int16:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		var v int64
		fmt.Sscanf(fmt.Sprintf("%v", row[i]), "%d", &v)
		return v
	}
}

// doDoctor запускает проверки на текущем шарде (первая цель), либо на всех
// выбранных с --all (с агрегацией находок по шардам).
func (r *REPL) doDoctor(args []string) error {
	if len(r.targets) == 0 {
		return fmt.Errorf("no shard selected")
	}
	for _, a := range args {
		if strings.EqualFold(a, "--all") && len(r.targets) > 1 {
			return r.doctorAll()
		}
	}
	shard := r.targets[0]
	if len(r.targets) > 1 {
		fmt.Fprintf(r.out, "diagnosing %s (first of %d targets; use --all for every shard)\n", shard.Label, len(r.targets))
	}
	fs, err := r.doctorChecks(shard)
	if err != nil {
		return err
	}
	r.renderDoctor(shard.Label, fs)
	return nil
}

// doctorChecks запускает все проверки против одного шарда и возвращает находки
// (или ошибку доступности).
func (r *REPL) doctorChecks(shard cluster.Shard) ([]finding, error) {
	if err := r.reachable(shard); err != nil {
		return nil, fmt.Errorf("cannot diagnose %s: %v", shard.Label, oneLine(err.Error()))
	}

	var fs []finding
	// q оборачивает docQuery и считает проверки с настоящей ошибкой (не безобидным
	// пропуском), чтобы итог мог сообщить "N проверок не выполнено".
	errored := 0
	q := func(sql string) *db.Result {
		res, isErr := r.docQuery(shard, sql)
		if isErr {
			errored++
		}
		return res
	}

	// Проверка прав: без pg_monitor/superuser pg_stat_activity скрывает состояние
	// чужих бэкендов, и проверки активности ниже занижают счёт.
	if res := q(`SELECT bool_or(state IS NULL) FROM pg_stat_activity
		WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'`); res != nil && len(res.Rows) > 0 {
		if cellStr(res.Rows[0], 0) == "true" {
			fs = append(fs, finding{explain.Info, "Limited activity visibility",
				"this role cannot see other backends' state — active/idle-in-tx/long-query checks may undercount; grant pg_monitor or connect as superuser"})
		}
	}

	// Соединения относительно max_connections.
	if res := q(`SELECT count(*) FILTER (WHERE state='active'),
		count(*) FILTER (WHERE state='idle in transaction'),
		count(*), current_setting('max_connections')::int
		FROM pg_stat_activity`); res != nil && len(res.Rows) > 0 {
		row := res.Rows[0]
		active, idleTx, total, max := cellInt(row, 0), cellInt(row, 1), cellInt(row, 2), cellInt(row, 3)
		if max > 0 && float64(total) > 0.8*float64(max) {
			fs = append(fs, finding{explain.Warning, "Connection pool near capacity",
				fmt.Sprintf("%d/%d connections used (%.0f%%), %d active", total, max, 100*float64(total)/float64(max), active)})
		}
		if idleTx > 0 {
			fs = append(fs, finding{explain.Info, "Idle-in-transaction connections present",
				fmt.Sprintf("%d connection(s) idle in transaction — they can block autovacuum and hold locks", idleTx)})
		}
	}

	// Долго выполняющиеся активные запросы.
	if res := q(`SELECT count(*), coalesce(round(max(extract(epoch from now()-query_start))),0)::bigint
		FROM pg_stat_activity WHERE state='active' AND now()-query_start > interval '30 seconds'`); res != nil && len(res.Rows) > 0 {
		if n := cellInt(res.Rows[0], 0); n > 0 {
			fs = append(fs, finding{explain.Warning, "Long-running queries",
				fmt.Sprintf("%d active query(ies) running over 30s (longest ~%ds)", n, cellInt(res.Rows[0], 1))})
		}
	}

	// Idle-in-transaction дольше минуты.
	if res := q(`SELECT count(*), coalesce(round(max(extract(epoch from now()-xact_start))),0)::bigint
		FROM pg_stat_activity WHERE state='idle in transaction' AND now()-xact_start > interval '1 minute'`); res != nil && len(res.Rows) > 0 {
		if n := cellInt(res.Rows[0], 0); n > 0 {
			fs = append(fs, finding{explain.Warning, "Stuck idle-in-transaction",
				fmt.Sprintf("%d transaction(s) idle over a minute (oldest ~%ds) — blocks autovacuum & holds snapshots", n, cellInt(res.Rows[0], 1))})
		}
	}

	// Блокирующие локи. Серьёзность зависит от длительности ожидания: Warning по
	// умолчанию, Critical при ожидании дольше 5с.
	if res := q(`SELECT count(*), coalesce(round(max(extract(epoch from now()-state_change))),0)::bigint
		FROM pg_stat_activity WHERE cardinality(pg_blocking_pids(pid)) > 0`); res != nil && len(res.Rows) > 0 {
		if waiters := cellInt(res.Rows[0], 0); waiters > 0 {
			age := cellInt(res.Rows[0], 1)
			sev := explain.Warning
			if age > 5 {
				sev = explain.Critical
			}
			fs = append(fs, finding{sev, "Blocking locks detected",
				fmt.Sprintf("%d session(s) waiting on locks held by others (longest wait ~%ds)", waiters, age)})
		}
	}

	// Мёртвые кортежи / кандидаты на раздувание.
	if res := q(`SELECT relname, n_dead_tup, n_live_tup
		FROM pg_stat_user_tables
		WHERE n_dead_tup > 10000 AND n_dead_tup > n_live_tup * 0.2
		ORDER BY n_dead_tup DESC LIMIT 5`); res != nil {
		for _, row := range res.Rows {
			dead, live := cellInt(row, 1), cellInt(row, 2)
			pct := 0.0
			if dead+live > 0 {
				pct = 100 * float64(dead) / float64(dead+live)
			}
			fs = append(fs, finding{explain.Warning, "Table has many dead tuples",
				fmt.Sprintf("%s: ~%.0f%% dead (%d dead / %d live) — check autovacuum and long transactions", cellStr(row, 0), pct, dead, live)})
		}
	}

	// Невалидные индексы.
	if res := q(`SELECT c.relname FROM pg_index i
		JOIN pg_class c ON c.oid=i.indexrelid WHERE NOT i.indisvalid`); res != nil {
		for _, row := range res.Rows {
			fs = append(fs, finding{explain.Warning, "Invalid index",
				fmt.Sprintf("%s is invalid (a failed CREATE INDEX CONCURRENTLY) — drop and recreate it", cellStr(row, 0))})
		}
	}

	// Неиспользуемые крупные индексы.
	if res := q(`SELECT relname, indexrelname, pg_size_pretty(pg_relation_size(indexrelid))
		FROM pg_stat_user_indexes WHERE idx_scan = 0 AND pg_relation_size(indexrelid) > 1048576
		ORDER BY pg_relation_size(indexrelid) DESC LIMIT 5`); res != nil {
		for _, row := range res.Rows {
			fs = append(fs, finding{explain.Info, "Possibly unused index",
				fmt.Sprintf("%s.%s (%s) has 0 scans — verify before dropping (stats may be reset; it may back a constraint)", cellStr(row, 0), cellStr(row, 1), cellStr(row, 2))})
		}
	}

	// Неактивные слоты репликации, удерживающие WAL. Серьёзность по объёму
	// удержанного: Warning свыше 100 МБ, Critical свыше 1 ГБ.
	if res := q(`SELECT slot_name, pg_wal_lsn_diff(
		CASE WHEN pg_is_in_recovery() THEN pg_last_wal_receive_lsn() ELSE pg_current_wal_lsn() END, restart_lsn)::bigint
		FROM pg_replication_slots WHERE NOT active AND restart_lsn IS NOT NULL`); res != nil {
		for _, row := range res.Rows {
			retained := cellInt(row, 1)
			if retained <= 100*1024*1024 {
				continue // <100 МБ удержано — не стоит оповещения
			}
			sev := explain.Warning
			if retained > 1024*1024*1024 {
				sev = explain.Critical
			}
			fs = append(fs, finding{sev, "Inactive replication slot holding WAL",
				fmt.Sprintf("slot %s retains %s of WAL — risks filling the disk", cellStr(row, 0), humanMB(float64(retained)/1024/1024))})
		}
	}

	// Риск переполнения ID транзакций (wraparound).
	if res := q(`SELECT max(age(datfrozenxid)) FROM pg_database`); res != nil && len(res.Rows) > 0 {
		if age := cellInt(res.Rows[0], 0); age > 1500000000 {
			fs = append(fs, finding{explain.Critical, "Transaction ID wraparound approaching",
				fmt.Sprintf("oldest datfrozenxid age is %d (limit ~2.1B) — ensure autovacuum keeps up", age)})
		}
	}

	// Топ запросов по суммарному времени (pg_stat_statements, если установлен). Имя
	// столбца зависит от версии: total_time до PostgreSQL 13, total_exec_time с 13.
	totalCol := "total_exec_time"
	if r.serverVersion() > 0 && r.serverVersion() < 130000 {
		totalCol = "total_time"
	}
	if res := q(fmt.Sprintf(`SELECT round(%s)::bigint, calls
		FROM pg_stat_statements ORDER BY %s DESC LIMIT 1`, totalCol, totalCol)); res != nil && len(res.Rows) > 0 {
		fs = append(fs, finding{explain.Info, "pg_stat_statements available",
			fmt.Sprintf("top query: %d ms total over %d calls — use it to target optimization", cellInt(res.Rows[0], 0), cellInt(res.Rows[0], 1))})
	}

	// Не показываем чистый результат, если часть проверок не выполнилась: настоящая
	// ошибка запроса (а не отсутствие расширения) означает неполную диагностику.
	if errored > 0 {
		fs = append(fs, finding{explain.Warning, "Some checks could not be evaluated",
			fmt.Sprintf("%d diagnostic query(ies) errored on this shard (not a missing extension) — results are incomplete", errored)})
	}

	return fs, nil
}

// doctorAll запускает проверки на всех выбранных шардах (параллельно) и агрегирует:
// каждая находка показывается со сколькими и какими шардами она связана.
func (r *REPL) doctorAll() error {
	type shardFindings struct {
		label string
		fs    []finding
		err   error
	}
	// Прогреваем версию сервера ДО фан-аута: дальше горутины только читают
	// кэш, без гонки на записи r.serverVer.
	r.serverVersion()
	out := make([]shardFindings, len(r.targets))
	sem := make(chan struct{}, r.cfg.ProbeConcurrency(len(r.targets)))
	var wg sync.WaitGroup
	for i, s := range r.targets {
		wg.Add(1)
		sem <- struct{}{} // слот берём ДО запуска: иначе на N целей разом стартует N горутин
		go func(i int, s cluster.Shard) {
			defer wg.Done()
			defer func() { <-sem }()
			fs, err := r.doctorChecks(s)
			out[i] = shardFindings{s.Label, fs, err}
		}(i, s)
	}
	wg.Wait()

	// Агрегация по (severity, title) с сохранением порядка первого появления. Метки
	// шардов хранятся как множество (один шард может поднять один и тот же title
	// несколько раз и не должен раздувать счёт), а детали по объектам собираются с
	// дедупликацией, чтобы имена индексов/таблиц/слотов не терялись.
	type detail struct{ shard, text string }
	type agg struct {
		sev, title string
		shards     []string
		shardSeen  map[string]bool
		details    []detail
		detailSeen map[string]bool
	}
	idx := map[string]int{}
	var aggs []agg
	var unreachable []string
	reached := 0
	for _, sf := range out {
		if sf.err != nil {
			unreachable = append(unreachable, sf.label)
			continue
		}
		reached++
		for _, f := range sf.fs {
			key := f.sev + "\x00" + f.title
			gi, ok := idx[key]
			if !ok {
				gi = len(aggs)
				idx[key] = gi
				aggs = append(aggs, agg{
					sev: f.sev, title: f.title,
					shardSeen:  map[string]bool{},
					detailSeen: map[string]bool{},
				})
			}
			g := &aggs[gi]
			if !g.shardSeen[sf.label] {
				g.shardSeen[sf.label] = true
				g.shards = append(g.shards, sf.label)
			}
			if d := strings.TrimSpace(f.detail); d != "" {
				// Ключ по shard+text, чтобы одинаковое имя объекта на разных шардах
				// сохранялось (с меткой шарда), а не схлопывалось в одну строку.
				dk := sf.label + "\x00" + d
				if !g.detailSeen[dk] {
					g.detailSeen[dk] = true
					g.details = append(g.details, detail{sf.label, d})
				}
			}
		}
	}
	sevRank := map[string]int{explain.Critical: 0, explain.Warning: 1, explain.Info: 2}
	sort.SliceStable(aggs, func(i, j int) bool {
		if sevRank[aggs[i].sev] != sevRank[aggs[j].sev] {
			return sevRank[aggs[i].sev] < sevRank[aggs[j].sev]
		}
		return len(aggs[i].shards) > len(aggs[j].shards)
	})

	fmt.Fprintf(r.out, "%s across %d shards (%d reachable)\n", sevColor("doctor"), len(r.targets), reached)
	if len(unreachable) > 0 {
		fmt.Fprintf(r.out, "  %s unreachable: %s\n", sevTag(explain.Critical), strings.Join(trunc(unreachable, 12), ", "))
	}
	if len(aggs) == 0 && len(unreachable) == 0 {
		fmt.Fprintf(r.out, "  %s all clear across %d shards\n", sevTag(explain.Info), reached)
		return nil
	}
	for _, a := range aggs {
		scope := "all shards"
		if len(a.shards) < reached {
			scope = fmt.Sprintf("%d/%d: %s", len(a.shards), reached, strings.Join(trunc(a.shards, 12), ", "))
		}
		fmt.Fprintf(r.out, "\n  %s %s\n     %s\n", sevTag(a.sev), a.title, scope)
		shown := a.details
		if len(shown) > 8 {
			shown = shown[:8]
		}
		multiShard := len(a.shards) > 1
		for _, d := range shown {
			if multiShard && d.shard != "" {
				fmt.Fprintf(r.out, "     - [%s] %s\n", d.shard, d.text)
			} else {
				fmt.Fprintf(r.out, "     - %s\n", d.text)
			}
		}
		if len(a.details) > 8 {
			fmt.Fprintf(r.out, "     … and %d more\n", len(a.details)-8)
		}
	}
	return nil
}

func (r *REPL) renderDoctor(label string, fs []finding) {
	crit, warn, info := 0, 0, 0
	for _, f := range fs {
		switch f.sev {
		case explain.Critical:
			crit++
		case explain.Warning:
			warn++
		default:
			info++
		}
	}
	status := "OK"
	switch {
	case crit > 0:
		status = "CRITICAL"
	case warn > 0:
		status = "DEGRADED"
	}
	fmt.Fprintf(r.out, "%s [%s]   status: %s   (CRITICAL %d, WARNING %d, INFO %d)\n",
		sevColor("doctor"), label, colorStatus(status), crit, warn, info)

	if len(fs) == 0 {
		fmt.Fprintln(r.out, "no issues detected by the light checks.")
		return
	}
	rank := map[string]int{explain.Critical: 0, explain.Warning: 1, explain.Info: 2}
	sort.SliceStable(fs, func(i, j int) bool { return rank[fs[i].sev] < rank[fs[j].sev] })
	for _, f := range fs {
		fmt.Fprintf(r.out, "\n  %s %s\n     %s\n", sevTag(f.sev), f.title, f.detail)
	}
}
