package repl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"terox/internal/db"
	"terox/internal/render"
	"terox/internal/sqlsplit"
)

// Feature 10: живые диагностические команды (read-only) по pg_stat_* представлениям.
// Все SELECT'ы выполняются read-only веером по текущим целям и рендерятся одной
// таблицей с ведущей колонкой shard. cancel/terminate действуют на ОДИН шард
// (pid привязан к конкретному backend) и требуют подтверждения.

// shortQuery нормализует и обрезает текст запроса до 60 символов на стороне
// сервера, чтобы вывод оставался однострочным.
const shortQueryExpr = `left(regexp_replace(coalesce(query,''), '\s+', ' ', 'g'), 60)`

// activitySQL строит запрос pg_stat_activity. all=true включает idle-сессии.
func activitySQL(all bool) string {
	where := `pid <> pg_backend_pid() AND backend_type = 'client backend'`
	if !all {
		// По умолчанию скрываем простаивающие сессии (state='idle'), оставляя
		// активные и "idle in transaction" (потенциальные блокировщики).
		where += ` AND state IS DISTINCT FROM 'idle'`
	}
	return `SELECT pid,
       coalesce(usename,'') AS usr,
       coalesce(application_name,'') AS app,
       coalesce(host(client_addr),'local') AS client,
       coalesce(state,'') AS state,
       coalesce(round(extract(epoch FROM now()-state_change))::int::text,'') AS state_age_s,
       coalesce(round(extract(epoch FROM now()-xact_start))::int::text,'') AS xact_age_s,
       coalesce(wait_event_type,'') AS wait_type,
       coalesce(wait_event,'') AS wait,
       ` + shortQueryExpr + ` AS query
FROM pg_stat_activity
WHERE ` + where + `
ORDER BY xact_start NULLS LAST, state_change`
}

var activityHeaders = []string{"pid", "user", "app", "client", "state", "state_age_s", "xact_age_s", "wait_type", "wait", "query"}

// blockersSQL показывает заблокированные backend'ы и кто их блокирует
// (pg_blocking_pids), плюс флаг by_autovacuum — заблокирован ли запрос воркером
// автовакуума (его не отменишь как обычный backend; стоит дождаться/настроить).
func blockersSQL() string {
	return `SELECT a.pid,
       coalesce(a.usename,'') AS usr,
       pg_blocking_pids(a.pid)::text AS blocked_by,
       EXISTS (SELECT 1 FROM pg_stat_activity v
               WHERE v.pid = ANY(pg_blocking_pids(a.pid))
                 AND v.backend_type = 'autovacuum worker')::text AS by_autovacuum,
       coalesce(a.wait_event_type,'') AS wait_type,
       coalesce(a.wait_event,'') AS wait,
       coalesce(round(extract(epoch FROM now()-a.xact_start))::int::text,'') AS xact_age_s,
       ` + shortQueryExpr + ` AS query
FROM pg_stat_activity a
WHERE cardinality(pg_blocking_pids(a.pid)) > 0
ORDER BY a.xact_start`
}

var blockersHeaders = []string{"pid", "user", "blocked_by", "by_autovacuum", "wait_type", "wait", "xact_age_s", "query"}

// locksSQL — сводка по блокировкам (режим/тип/granted) с количеством.
func locksSQL() string {
	return `SELECT mode, locktype, granted::text AS granted, count(*)::text AS n
FROM pg_locks
GROUP BY mode, locktype, granted
ORDER BY granted, count(*) DESC`
}

var locksHeaders = []string{"mode", "locktype", "granted", "n"}

// longtxSQL — транзакции (включая idle in transaction) старше thresholdSeconds.
func longtxSQL(thresholdSeconds int) string {
	return fmt.Sprintf(`SELECT pid,
       coalesce(usename,'') AS usr,
       coalesce(state,'') AS state,
       round(extract(epoch FROM now()-xact_start))::int::text AS xact_age_s,
       coalesce(wait_event_type,'') AS wait_type,
       %s AS query
FROM pg_stat_activity
WHERE xact_start IS NOT NULL
  AND now()-xact_start > interval '%d seconds'
  AND pid <> pg_backend_pid()
ORDER BY xact_start`, shortQueryExpr, thresholdSeconds)
}

var longtxHeaders = []string{"pid", "user", "state", "xact_age_s", "wait_type", "query"}

// runDiagQuery выполняет read-only диагностический SELECT по всем целям и
// рендерит объединённую таблицу (ведущая колонка shard). headers — без "shard".
func (r *REPL) runDiagQuery(sql string, headers []string) {
	if len(r.targets) == 0 {
		fmt.Fprintln(r.out, "no shard selected")
		return
	}
	r.renderDiagResults(headers, r.fanoutRead(sql))
}

// renderDiagResults рендерит уже выполненный fanout одной таблицей (ведущая
// колонка shard). Вынесено из runDiagQuery, чтобы вызывающий мог дополнительно
// проанализировать строки перед выводом.
func (r *REPL) renderDiagResults(headers []string, results []db.ShardResult) {
	full := append([]string{"shard"}, headers...)
	var rows [][]string
	okShards, fails, total := 0, 0, 0
	for _, sr := range results {
		if sr.Err != nil {
			fails++
			rec := make([]string, len(full))
			rec[0] = sr.Shard.LabelDB()
			if len(rec) > 1 {
				rec[1] = "ERR: " + oneLine(sr.Err.Error())
			}
			rows = append(rows, rec)
			continue
		}
		okShards++
		if sr.Result == nil {
			continue
		}
		for _, row := range sr.Result.Rows {
			rec := make([]string, len(full))
			rec[0] = sr.Shard.LabelDB()
			for i := range headers {
				if i < len(row) {
					rec[i+1] = str(row[i])
				}
			}
			rows = append(rows, rec)
			total++
		}
	}
	footer := fmt.Sprintf("%d row(s) across %d shard(s)", total, okShards)
	if fails > 0 {
		footer += fmt.Sprintf(" — %d shard(s) failed", fails)
	}
	render.Table(r.out, full, rows, footer)
}

// maskQueryText маскирует литералы в тексте запроса для безопасного показа/сохранения
// диагностики client-side. Строки, комментарии и идентификаторы в кавычках затирает
// sqlsplit.Mask; числовые литералы (ID, суммы, телефоны — потенциальный PII)
// дополнительно затираются здесь. Длина сохраняется. Цифра, примыкающая к букве/
// подчёркиванию/точке (как в идентификаторе col1 или версии v1.2), частью числового
// литерала не считается и не трогается.
func maskQueryText(s string) string {
	b := []byte(sqlsplit.Mask(s))
	for i := 0; i < len(b); {
		attached := i > 0 && (identByte(b[i-1]) || b[i-1] == '.')
		if b[i] >= '0' && b[i] <= '9' && !attached {
			for i < len(b) && ((b[i] >= '0' && b[i] <= '9') || b[i] == '.') {
				b[i] = ' '
				i++
			}
			continue
		}
		i++
	}
	return string(b)
}

// maskQueryColumn маскирует литералы (строки и числа) в колонке query результатов
// диагностики client-side — чтобы пароли/PII из живого текста запроса не
// отображались и не попадали в сохранённый вывод. Длина сохраняется (maskQueryText).
func maskQueryColumn(results []db.ShardResult, queryIdx int) {
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil {
			continue
		}
		for _, row := range sr.Result.Rows {
			if queryIdx < len(row) {
				if s, ok := row[queryIdx].(string); ok {
					row[queryIdx] = maskQueryText(s)
				}
			}
		}
	}
}

// runDiagQueryRedacted выполняет диагностический SELECT и по умолчанию маскирует
// колонку "query" (последнюю), чтобы не светить литералы. --raw отключает маскировку.
func (r *REPL) runDiagQueryRedacted(sql string, headers []string, raw bool) {
	if len(r.targets) == 0 {
		fmt.Fprintln(r.out, "no shard selected")
		return
	}
	results := r.fanoutRead(sql)
	if !raw && len(headers) > 0 && headers[len(headers)-1] == "query" {
		maskQueryColumn(results, len(headers)-1)
	}
	r.renderDiagResults(headers, results)
}

// doActivity (\activity [--all] [--raw]) — текущие backend'ы (по умолчанию без idle
// и с маскированными литералами в query; --raw показывает текст как есть).
func (r *REPL) doActivity(args []string) {
	all, raw := false, false
	for _, a := range args {
		switch a {
		case "--all", "-a":
			all = true
		case "--raw":
			raw = true
		}
	}
	r.runDiagQueryRedacted(activitySQL(all), activityHeaders, raw)
}

// doBlockers (\blockers [--raw]) — заблокированные backend'ы и блокировщики.
func (r *REPL) doBlockers(args []string) {
	r.runDiagQueryRedacted(blockersSQL(), blockersHeaders, hasRaw(args))
}

// doLocks (\locks) — сводка по блокировкам.
func (r *REPL) doLocks() { r.runDiagQuery(locksSQL(), locksHeaders) }

// hasRaw сообщает, передан ли флаг --raw.
func hasRaw(args []string) bool {
	for _, a := range args {
		if a == "--raw" {
			return true
		}
	}
	return false
}

// doLongtx (\longtx [duration] [--raw]) — долгие транзакции (по умолчанию > 1 минуты).
func (r *REPL) doLongtx(args []string) {
	seconds := 60
	raw := false
	for _, a := range args {
		if a == "--raw" {
			raw = true
			continue
		}
		d, ok := pgDurationToGo(a)
		if !ok || d <= 0 {
			fmt.Fprintf(r.out, "usage: \\longtx [duration] [--raw] (e.g. 30s, 5min); got %q\n", a)
			return
		}
		seconds = int(d.Seconds())
		if seconds < 1 {
			seconds = 1
		}
	}
	r.runDiagQueryRedacted(longtxSQL(seconds), longtxHeaders, raw)
}

// doCancel (\cancel <pid>) и doTerminate (\terminate <pid>) шлют сигнал одному
// backend на ОДНОМ выбранном шарде (pid привязан к конкретному серверу). cancel
// мягко прерывает текущий запрос; terminate обрывает соединение и требует ввода
// 'yes'. Собственные backend'ы terox (application_name='terox') не трогаем.
func (r *REPL) doSignalBackend(args []string, terminate bool) error {
	verb := "cancel"
	fn := "pg_cancel_backend"
	if terminate {
		verb, fn = "terminate", "pg_terminate_backend"
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: \\%s <pid> (select a single shard first; pid is backend-local)", verb)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(args[0]))
	if err != nil || pid <= 0 {
		return fmt.Errorf("\\%s: invalid pid %q", verb, args[0])
	}
	if len(r.targets) != 1 {
		return fmt.Errorf("\\%s targets a single backend — narrow to one shard first (\\shard) ; current selection has %d shards", verb, len(r.targets))
	}
	target := r.targets[0]
	// Дедлайн на ПРОСМОТР backend'а (быстрый read). Сам сигнал ниже шлётся свежим
	// контекстом уже ПОСЛЕ подтверждения — иначе раздумье оператора над 'yes'
	// съело бы этот дедлайн и подтверждённый terminate упал бы по таймауту.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// Сначала показываем backend, на который собираемся воздействовать.
	info, err := r.mgr.Exec(ctx, target, fmt.Sprintf(`SELECT coalesce(usename,''), coalesce(application_name,''),
		coalesce(host(client_addr),'local'), coalesce(state,''),
		coalesce(round(extract(epoch FROM now()-xact_start))::int::text,''),
		pid = pg_backend_pid()
		FROM pg_stat_activity WHERE pid = %d`, pid), true)
	if err != nil {
		return fmt.Errorf("\\%s: %v", verb, err)
	}
	if info == nil || len(info.Rows) == 0 {
		return fmt.Errorf("\\%s: no backend with pid %d on %s", verb, pid, target.LabelDB())
	}
	row := info.Rows[0]
	// Запрос выбирает 6 колонок (usename..pid=pg_backend_pid()); защищаемся от
	// усечённой строки, чтобы row[0..4] не вызвали панику (ср. isSelf проверял len>5).
	if len(row) < 6 {
		return fmt.Errorf("\\%s: unexpected backend row for pid %d on %s", verb, pid, target.LabelDB())
	}
	usr, app, client, state, age := str(row[0]), str(row[1]), str(row[2]), str(row[3]), str(row[4])
	isSelf := str(row[5]) == "true"
	fmt.Fprintf(r.out, "%s on %s: pid=%d user=%s app=%s client=%s state=%s xact_age=%ss\n",
		strings.ToUpper(verb), target.LabelDB(), pid, usr, app, client, state, age)
	if isSelf {
		return fmt.Errorf("refused: pid %d is terox's own backend on this connection", pid)
	}
	if app == "terox" {
		return fmt.Errorf("refused: pid %d is a terox backend (application_name=terox) — not signaling our own tooling", pid)
	}
	if terminate {
		if strings.TrimSpace(r.readLine("type 'yes' to terminate this backend: ")) != "yes" {
			fmt.Fprintln(r.out, "cancelled")
			return nil
		}
	}
	// Свежий дедлайн на сам сигнал: отсчёт начинается после подтверждения, а не с
	// момента просмотра, поэтому долгое раздумье оператора его не отменяет.
	sigCtx, sigCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer sigCancel()
	res, err := r.mgr.Exec(sigCtx, target, fmt.Sprintf("SELECT %s(%d)", fn, pid), true)
	if err != nil {
		logOperatorAction(verb, target.LabelDB(), pid, "error: "+oneLine(err.Error()))
		return fmt.Errorf("\\%s: %v", verb, err)
	}
	ok := res != nil && len(res.Rows) > 0 && len(res.Rows[0]) > 0 && str(res.Rows[0][0]) == "true"
	if ok {
		fmt.Fprintf(r.out, "%s signal sent to pid %d on %s\n", verb, pid, target.LabelDB())
		logOperatorAction(verb, target.LabelDB(), pid, "sent (user="+usr+" app="+app+")")
	} else {
		fmt.Fprintf(r.out, "%s: server returned false for pid %d (already gone or insufficient privilege)\n", verb, pid)
		logOperatorAction(verb, target.LabelDB(), pid, "no-op (gone or denied)")
	}
	return nil
}

// logOperatorAction дописывает локальную аудит-запись о cancel/terminate
// (кто/когда/шард/pid/итог) в ~/.config/terox/operator.log (0600). Локальный журнал
// операторских действий, без сетевого побочного эффекта; сбой записи не критичен.
func logOperatorAction(verb, shard string, pid int, result string) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "terox", "operator.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\tshard=%s\tpid=%d\t%s\n", time.Now().Format(time.RFC3339), verb, shard, pid, result)
}
