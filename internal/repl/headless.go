package repl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
	"terox/internal/execution"
	"terox/internal/explain"
	"terox/internal/export"
	"terox/internal/migration"
	"terox/internal/render"
	"terox/internal/store"
)

// MigrateOptions — флаги поэтапной раскатки для неинтерактивного предпросмотра.
type MigrateOptions struct {
	Canary bool
	Batch  int
	Resume bool
}

// MigratePreview неинтерактивно показывает ТОЧНЫЙ payload миграции и план поэтапной
// раскатки для .sql-файла, НИЧЕГО НЕ ПРИМЕНЯЯ и НЕ ПОДКЛЮЧАЯСЬ к БД (offline). Для
// валидации миграций в CI: ловит mixed-файл, собственное управление транзакцией,
// session-state нарушение, дрейф контрольной суммы по локальному ledger и печатает
// этапы. Реальное применение остаётся за интерактивным `\migrate --allowed` —
// headless apply намеренно не поддерживается (это write-путь на множество шардов).
func MigratePreview(cfg *config.Config, target, path string, opts MigrateOptions, out io.Writer) error {
	parts := strings.SplitN(target, "/", 3)
	if len(parts) < 2 {
		return fmt.Errorf("target must be service/storage[/selector], got %q", target)
	}
	svc, ok := cfg.Services[parts[0]]
	if !ok || svc == nil {
		return fmt.Errorf("unknown service %q", parts[0])
	}
	st, ok := svc.Storages[parts[1]]
	if !ok || st == nil {
		return fmt.Errorf("unknown or empty storage %q in service %q", parts[1], parts[0])
	}
	targets, err := resolveTarget(cfg, target)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)
	plan := migration.Classify(content)

	// Те же проверки firewall, что и на пути применения (\migrate оборачивает тело):
	// dry-run обязан отклонять то же, что отклонит --allowed.
	if plan.Mixed {
		return fmt.Errorf("mixed file: CONCURRENTLY/VACUUM cannot share a transaction — split into separate files")
	}
	if migration.HasTxControl(content) {
		return fmt.Errorf("refused: file carries its own BEGIN/COMMIT/ROLLBACK or SET ROLE (would bypass the protective wrapper); use \\i to run it verbatim")
	}
	if reason := migration.SessionStateViolation(content); reason != "" {
		return fmt.Errorf("%s", strings.TrimPrefix(reason, "refused: "))
	}

	// Точный payload, который ушёл бы на каждый шард.
	if plan.NonTransactional {
		fmt.Fprintln(out, "-- non-transactional: each statement runs as its own exec --")
		for i, s := range plan.Statements {
			fmt.Fprintf(out, "-- [%d/%d] --\n%s;\n", i+1, len(plan.Statements), s)
		}
	} else {
		built, err := migration.BuildTransactional(content, st.MigrationRole, cfg.StatementTimeout, cfg.LockTimeout)
		if err != nil {
			return fmt.Errorf("refused: %v", err)
		}
		fmt.Fprintln(out, "-- exact exec terox would send to each shard --")
		fmt.Fprint(out, built)
		if !strings.HasSuffix(built, "\n") {
			fmt.Fprintln(out)
		}
	}

	// План поэтапной раскатки по ЛОКАЛЬНОМУ ledger (без обращения к БД).
	labels := make([]string, len(targets))
	for i, s := range targets {
		labels[i] = s.Label
	}
	ctx := parts[0] + "/" + parts[1]
	appliedSet := map[string]bool{}
	if applied, err := store.LoadApplied(); err == nil && applied != nil {
		appliedSet = migration.AppliedSetFromShards(applied.Shards(ctx, fileBase(path)))
		if prev, ok := applied.Checksum(ctx, fileBase(path)); ok && prev != migration.Checksum(content) {
			fmt.Fprintf(out, "-- WARNING: checksum drift — %s was applied before with different content --\n", fileBase(path))
		}
	}
	pl := migration.PlanRollout(labels, appliedSet, opts.Resume, opts.Canary, opts.Batch)
	fmt.Fprintf(out, "rollout plan: %d pending, %d skipped, %d stage(s)\n", len(pl.Pending), len(pl.Skipped), len(pl.Stages))
	for i, stage := range pl.Stages {
		kind := "batch"
		if i == 0 && opts.Canary {
			kind = "canary"
		}
		fmt.Fprintf(out, "  stage %d (%s): %s\n", i+1, kind, strings.Join(stage, ", "))
	}
	fmt.Fprintln(out, "-- dry-run preview (offline, no DB). Apply interactively with \\migrate --allowed. --")
	return nil
}

// resolveTarget раскрывает строку "service/storage[/selector]" в список шардов
// для запроса (по умолчанию селектор "all").
func resolveTarget(cfg *config.Config, target string) ([]cluster.Shard, error) {
	parts := strings.SplitN(target, "/", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("target must be service/storage[/selector], got %q", target)
	}
	svc, ok := cfg.Services[parts[0]]
	if !ok || svc == nil {
		return nil, fmt.Errorf("unknown service %q", parts[0])
	}
	st, ok := svc.Storages[parts[1]]
	if !ok || st == nil {
		return nil, fmt.Errorf("unknown or empty storage %q in service %q", parts[1], parts[0])
	}
	shards, err := cluster.Expand(st)
	if err != nil {
		return nil, err
	}
	sel := "all"
	if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
		sel = parts[2]
	}
	targets, _, err := cluster.ParseSelector(shards, sel)
	return targets, err
}

// QueryOptions — параметры неинтерактивного query.
type QueryOptions struct {
	Format  string // table|json|csv|envelope (пусто = table)
	OrderBy string // col[:asc|:desc] — глобальная сортировка по шардам
	Mode    string // union(default)|aggregate|first-success|per-shard
}

// Query неинтерактивно выполняет read-only запрос к цели и пишет объединённый
// результат (с колонкой "shard" при нескольких шардах) в заданном формате
// (json, csv или table). Для скриптов и CI. Ошибки шардов идут в stderr; если
// упал хотя бы один шард, результат неполный и возвращается ошибка (частичный
// результат всё равно пишется).
func Query(cfg *config.Config, target, sql string, opts QueryOptions, out io.Writer) error {
	sql = strings.TrimSuffix(strings.TrimSpace(sql), ";")
	if sql == "" {
		return fmt.Errorf("no query given")
	}
	sortCol, sortDesc := parseOrderBy(opts.OrderBy)
	// Явно отклоняем неизвестный формат, а не молча откатываемся к table-рендеру.
	format := strings.ToLower(strings.TrimSpace(opts.Format))
	switch format {
	case "", "table", "json", "csv", "envelope":
	default:
		return fmt.Errorf("unknown --format %q (want table, json, csv, or envelope)", format)
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	switch mode {
	case "", "union", "union-by-name":
		// union-by-name — явное имя текущего поведения по умолчанию (типизированное
		// объединение по имени колонки); отличается от гипотетического union-by-position.
		mode = "union"
	case "strict", "merge-sort", "quorum", "aggregate", "first-success", "per-shard":
	default:
		return fmt.Errorf("unknown --mode %q (want union, union-by-name, strict, merge-sort, quorum, aggregate, first-success, or per-shard)", mode)
	}
	if mode == "per-shard" && format == "csv" {
		return fmt.Errorf("--mode per-shard does not support --format csv (use the default union mode, which has a shard column)")
	}
	if mode == "merge-sort" && sortCol == "" {
		return fmt.Errorf("--mode merge-sort requires --order-by <col[:asc|:desc]> (the global sort key)")
	}
	// В режимах с самонесущими ошибками (envelope/per-shard) не дублируем в stderr.
	quiet := format == "envelope" || mode == "per-shard"
	if execution.IsWrite(sql) {
		return fmt.Errorf("non-interactive query is read-only; writes are not allowed here")
	}
	// quorum сравнивает строки по позициям: без верхнеуровневого ORDER BY PostgreSQL
	// не гарантирует порядок, и одинаковые данные на разных шардах могут выглядеть
	// как расхождение. Предупреждаем (не блокируем) — честный UX, как советует аудит.
	if mode == "quorum" && !quiet && execution.LacksTopLevelOrderBy(sql) {
		fmt.Fprintln(os.Stderr, "quorum: SELECT has no top-level ORDER BY — row order is not guaranteed across shards; add a stable ORDER BY or identical data may look divergent")
	}
	targets, err := resolveTarget(cfg, target)
	if err != nil {
		return err
	}
	mgr := db.NewManager()
	defer mgr.Close()
	mgr.SetReadTimeout(cfg.StatementTimeout)

	timeout := time.Duration(cfg.QueryTimeout)
	// Ограничиваем материализацию на шард, чтобы огромный результат не вызвал OOM;
	// при срабатывании лимита результат неполный и об этом сообщается ниже.
	const headlessRowCap = 100000
	results := mgr.FanoutProgress(context.Background(), targets, sql, true, cfg.Concurrency(len(targets)), timeout, headlessRowCap, nil)
	// Полный срез ответов ВСЕХ шардов — сохраняем ДО схлопывания в режимах
	// first-success/quorum, чтобы envelope-метаданные (shards/errors/shard_meta)
	// отражали реальный веер, а не единственного представителя.
	allResults := results

	// first-success: оставляем только первый успешный результат (любой шард).
	// Режим намеренно завершается с кодом 0, как только хоть один шард ответил, но
	// НЕ молча: сообщаем в stderr, сколько шардов упало до успеха, чтобы CI не
	// принял exit 0 за «все шарды здоровы».
	if mode == "first-success" {
		fr := firstSuccessResult(results)
		if fr == nil {
			return fmt.Errorf("--mode first-success: no shard returned a result")
		}
		failedBefore := 0
		for _, sr := range results {
			if sr.Err != nil {
				failedBefore++
			}
		}
		if failedBefore > 0 {
			fmt.Fprintf(os.Stderr, "first-success: %d of %d shard(s) failed before a success (exit 0 by design)\n", failedBefore, len(results))
		}
		results = []db.ShardResult{*fr}
	}

	// quorum: диагностическое чтение — шарды ДОЛЖНЫ согласиться на одном результате.
	// Выводим результат большинства; при отсутствии большинства — ошибка (расхождение).
	// Запись в quorum невозможна (headless query уже отверг IsWrite выше).
	if mode == "quorum" {
		rep, agree, okCount := quorumAgreement(results)
		if rep == nil {
			return fmt.Errorf("--mode quorum: no shard returned a result")
		}
		if agree*2 <= okCount {
			return fmt.Errorf("--mode quorum: no majority — only %d of %d responding shard(s) agree on the result", agree, okCount)
		}
		if agree < okCount && !quiet {
			fmt.Fprintf(os.Stderr, "quorum: %d of %d responding shard(s) agree; %d diverge\n", agree, okCount, okCount-agree)
		}
		results = []db.ShardResult{*rep}
	}

	failed, truncated := 0, 0
	for _, sr := range results {
		if sr.Err != nil {
			failed++
			if !quiet {
				fmt.Fprintf(os.Stderr, "shard %s: %v\n", sr.Shard.Label, sr.Err)
			}
			continue
		}
		if sr.Result != nil && sr.Result.Truncated {
			truncated++
			if !quiet {
				fmt.Fprintf(os.Stderr, "shard %s: result capped at %d rows\n", sr.Shard.Label, headlessRowCap)
			}
		}
	}

	if mode == "per-shard" {
		if err := writePerShard(out, format, target, results); err != nil {
			return err
		}
	} else {
		// strict: одинаковые имена И типы колонок на всех шардах обязательны —
		// падаем сразу при дрейфе вместо тихого предупреждения (Feature 3/13).
		if mode == "strict" {
			if msg := columnNameDrift(results); msg != "" {
				return fmt.Errorf("--mode strict: %s", msg)
			}
			if d := render.DetectTypeDrift(results); len(d) > 0 {
				return fmt.Errorf("--mode strict: type drift across shards: %s", strings.Join(d, "; "))
			}
		}
		cols, rows := render.Merge(results)
		if mode == "aggregate" {
			cols, rows = render.Aggregate(cols, rows)
		}
		// Глобальная сортировка (per-shard ORDER BY не даёт общего порядка).
		if sortCol != "" {
			if err := render.SortMerged(cols, rows, sortCol, sortDesc); err != nil {
				return err
			}
		}
		switch format {
		case "json":
			if err := export.WriteJSON(out, cols, rows); err != nil {
				return err
			}
		case "csv":
			if err := export.WriteCSV(out, cols, rows); err != nil {
				return err
			}
		case "envelope":
			if err := export.WriteEnvelope(out, buildEnvelope(target, results, allResults, cols, rows)); err != nil {
				return err
			}
		default:
			// Сортировка/агрегация рендерятся из объединённых (cols, rows), чтобы
			// порядок/свёртка применились и в табличном выводе; иначе — обычный путь.
			if sortCol != "" || mode == "aggregate" {
				render.AnyTable(out, cols, rows, "")
			} else if len(results) == 1 {
				render.Single(out, results[0].Result, 0, false)
			} else {
				render.Multi(out, results, 0)
			}
		}
	}
	if failed > 0 || truncated > 0 {
		pe := &PartialError{Failed: failed, Truncated: truncated, Total: len(results), RowCap: headlessRowCap}
		// Полный провал — обычная ошибка (exit 1). Частичный успех (часть шардов
		// ответила) — отдельный PartialError, который main маппит в exit 2, чтобы CI
		// отличал «часть данных есть» от «не подключились вовсе».
		if failed == len(results) {
			return fmt.Errorf("all %d shard(s) failed — result is empty", len(results))
		}
		return pe
	}
	return nil
}

// PartialError означает частичный успех многошардового запроса: часть шардов
// вернула результат, часть упала или упёрлась в лимит строк. main маппит его в
// отдельный exit code (2), чтобы автоматизация отличала неполный результат от
// полного провала или ошибки конфигурации.
type PartialError struct {
	Failed    int // шардов с ошибкой
	Truncated int // шардов, упёршихся в лимит строк
	Total     int // всего шардов
	RowCap    int // лимит строк на шард
}

func (e *PartialError) Error() string {
	switch {
	case e.Failed > 0 && e.Truncated > 0:
		return fmt.Sprintf("%d of %d shard(s) failed and %d hit the %d-row cap — result is incomplete", e.Failed, e.Total, e.Truncated, e.RowCap)
	case e.Failed > 0:
		return fmt.Sprintf("%d of %d shard(s) failed — result is incomplete", e.Failed, e.Total)
	default:
		return fmt.Sprintf("%d of %d shard(s) hit the %d-row cap — result is incomplete (narrow the query)", e.Truncated, e.Total, e.RowCap)
	}
}

// columnNameDrift возвращает непустое сообщение, если успешные шарды вернули разные
// НАБОРЫ имён колонок (strict-режим требует идентичной схемы). Сравнение по полному
// списку имён в порядке появления.
func columnNameDrift(results []db.ShardResult) string {
	var ref []string
	var refLabel string
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil || !sr.Result.IsSelect {
			continue
		}
		if ref == nil {
			ref = sr.Result.Columns
			refLabel = sr.Shard.LabelDB()
			continue
		}
		if !equalStrings(ref, sr.Result.Columns) {
			return fmt.Sprintf("column names differ across shards: %s has %v, %s has %v",
				refLabel, ref, sr.Shard.LabelDB(), sr.Result.Columns)
		}
	}
	return ""
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// quorumAgreement группирует успешные шарды по отпечатку результата (колонки+строки)
// и возвращает представителя крупнейшей группы, размер согласия и число ответивших
// шардов. rep == nil, если не ответил ни один шард.
func quorumAgreement(results []db.ShardResult) (rep *db.ShardResult, agree, responding int) {
	groups := map[string][]int{}
	for i := range results {
		if results[i].Err != nil || results[i].Result == nil {
			continue
		}
		responding++
		fp := resultFingerprint(results[i].Result)
		groups[fp] = append(groups[fp], i)
	}
	best := -1
	for _, idxs := range groups {
		if len(idxs) > agree {
			agree = len(idxs)
			best = idxs[0]
		}
	}
	if best < 0 {
		return nil, 0, responding
	}
	return &results[best], agree, responding
}

// resultFingerprint — детерминированный отпечаток результата для сравнения на
// согласие в quorum (имена колонок + значения строк).
func resultFingerprint(r *db.Result) string {
	return fmt.Sprintf("%v|%v", r.Columns, r.Rows)
}

// firstSuccessResult возвращает первый успешный результат шарда (по порядку
// позиций) или nil, если ни один не вернул результат.
func firstSuccessResult(results []db.ShardResult) *db.ShardResult {
	for i := range results {
		if results[i].Err == nil && results[i].Result != nil {
			return &results[i]
		}
	}
	return nil
}

// buildPerShard собирает per-shard конверт (отдельный набор строк на шард).
func buildPerShard(target string, results []db.ShardResult) export.PerShardEnvelope {
	env := export.PerShardEnvelope{SchemaVersion: 1, Target: target, Mode: "per-shard"}
	env.Shards.Total = len(results)
	for _, sr := range results {
		sj := export.ShardResultJSON{Shard: sr.Shard.LabelDB()}
		if sr.Err != nil {
			env.Shards.Failed++
			info := db.ClassifyError(sr.Err)
			sj.Error = &export.ShardError{Shard: sr.Shard.LabelDB(), SQLState: info.SQLState, Severity: info.Severity, Message: info.Message, Kind: info.Kind}
		} else {
			env.Shards.OK++
			if sr.Result != nil {
				sj.Columns = sr.Result.Columns
				sj.Rows = export.RowValues(sr.Result.Rows)
				sj.ServerVersion = sr.Result.ServerVersion
				sj.BackendPID = sr.Result.BackendPID
				sj.DurationMS = sr.Result.Duration.Milliseconds()
				if sr.Result.Truncated {
					sj.Truncated = true
					env.Shards.Truncated++
				}
			}
		}
		env.Results = append(env.Results, sj)
	}
	return env
}

// writePerShard выводит результат каждого шарда отдельно. table — помеченными
// блоками; json/envelope — per-shard конвертом (csv отклонён ранее).
func writePerShard(out io.Writer, format, target string, results []db.ShardResult) error {
	switch format {
	case "json", "envelope":
		return export.WritePerShardEnvelope(out, buildPerShard(target, results))
	default:
		for _, sr := range results {
			status := "ok"
			if sr.Err != nil {
				status = "FAIL"
			}
			fmt.Fprintf(out, "== shard %s (%s) ==\n", sr.Shard.LabelDB(), status)
			if sr.Err != nil {
				info := db.ClassifyError(sr.Err)
				if info.SQLState != "" {
					fmt.Fprintf(out, "  [%s] %s\n", info.SQLState, info.Message)
				} else {
					fmt.Fprintf(out, "  %s\n", info.Message)
				}
				continue
			}
			render.Single(out, sr.Result, 0, false)
		}
		return nil
	}
}

// parseOrderBy разбирает значение --order-by: "col" или "col:desc"/"col:asc" в
// (имя колонки, по-убыванию). Двоеточие трактуется как направление только если
// суффикс ровно asc/desc, иначе считается частью имени.
func parseOrderBy(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		switch strings.ToLower(strings.TrimSpace(s[i+1:])) {
		case "desc":
			return strings.TrimSpace(s[:i]), true
		case "asc":
			return strings.TrimSpace(s[:i]), false
		}
	}
	return s, false
}

// buildEnvelope собирает стабильный машиночитаемый конверт из результатов веера:
// объединённые колонки/строки (та же multiset-схема, что и в Merge), сводку по
// шардам и пер-шардовые ошибки с SQLSTATE/категорией. Чистая функция (cols/rows
// уже посчитаны Merge), поэтому тестируется без БД.
//
// dataResults описывает ВЫВОД (схема/строки) — в режимах first-success/quorum это
// один представитель; metaResults — ВЕСЬ веер (сводка/ошибки/provenance по всем
// шардам), чтобы envelope не врал про упавшие шарды при схлопывании результата.
func buildEnvelope(target string, dataResults, metaResults []db.ShardResult, cols []string, rows [][]any) export.Envelope {
	schema := render.MergedSchema(dataResults, cols)
	env := export.Envelope{
		SchemaVersion: 1,
		Target:        target,
		Columns:       cols,
		Rows:          export.RowValues(rows),
		Warnings:      render.DetectTypeDrift(dataResults),
		Schema:        columnsToJSON(schema),
		SchemaCheck:   render.SchemaCheckStatus(schema),
		ShardMeta:     shardMetaOf(metaResults),
	}
	env.RowCount = len(env.Rows)
	env.Shards.Total = len(metaResults)
	for _, sr := range metaResults {
		if sr.Err != nil {
			env.Shards.Failed++
			info := db.ClassifyError(sr.Err)
			env.Errors = append(env.Errors, export.ShardError{
				Shard:    sr.Shard.LabelDB(),
				SQLState: info.SQLState,
				Severity: info.Severity,
				Message:  info.Message,
				Kind:     info.Kind,
			})
			continue
		}
		env.Shards.OK++
		if sr.Result != nil && sr.Result.Truncated {
			env.Shards.Truncated++
			env.Truncated = true
		}
	}
	return env
}

// shardMetaOf собирает пер-шардовую provenance (Feature 13): версия сервера, backend
// PID, длительность, SQLSTATE и статус каждого шарда.
func shardMetaOf(results []db.ShardResult) []export.ShardMetaJSON {
	out := make([]export.ShardMetaJSON, 0, len(results))
	for _, sr := range results {
		m := export.ShardMetaJSON{Shard: sr.Shard.LabelDB()}
		if sr.Err != nil {
			info := db.ClassifyError(sr.Err)
			m.SQLState = info.SQLState
		} else if sr.Result != nil {
			m.OK = true
			m.ServerVersion = sr.Result.ServerVersion
			m.BackendPID = sr.Result.BackendPID
			m.DurationMS = sr.Result.Duration.Milliseconds()
		}
		out = append(out, m)
	}
	return out
}

// columnsToJSON конвертирует типизированную схему db.Column в export.ColumnJSON
// (Feature 3) — отдельный тип, чтобы export не зависел от пакета db.
func columnsToJSON(schema []db.Column) []export.ColumnJSON {
	out := make([]export.ColumnJSON, len(schema))
	for i, c := range schema {
		out[i] = export.ColumnJSON{
			Name:       c.Name,
			TypeOID:    c.DataTypeOID,
			TypeName:   c.TypeName,
			Typmod:     c.TypeModifier,
			Occurrence: c.Occurrence,
			Synthetic:  c.Synthetic,
		}
	}
	return out
}

// Plan выполняет EXPLAIN на первой цели и выдаёт структурный анализ в JSON
// (машиночитаемая диагностика для CI). analyze запускает сам запрос (только
// чтение).
func Plan(cfg *config.Config, target, sql string, analyze bool, out io.Writer) error {
	sql = strings.TrimSuffix(strings.TrimSpace(sql), ";")
	if sql == "" {
		return fmt.Errorf("no query given")
	}
	if analyze && execution.IsWrite(sql) {
		return fmt.Errorf("EXPLAIN ANALYZE would execute a writing query")
	}
	targets, err := resolveTarget(cfg, target)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return fmt.Errorf("no shards selected")
	}
	mgr := db.NewManager()
	defer mgr.Close()
	mgr.SetReadTimeout(cfg.StatementTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.QueryTimeout))
	defer cancel()
	// Читаем версию сервера до построения EXPLAIN, чтобы версионные опции
	// (SETTINGS 12+, WAL 13+) уходили только на сервер, который их поддерживает.
	ver := 0
	if vr, e := mgr.Exec(ctx, targets[0], `SELECT current_setting('server_version_num')::int`, true); e == nil && vr != nil && len(vr.Rows) > 0 && len(vr.Rows[0]) > 0 {
		ver = int(asInt64(vr.Rows[0][0]))
	}
	res, err := mgr.Exec(ctx, targets[0], explainSQLFor(explainOpts{analyze: analyze}, ver, sql), true)
	if err != nil {
		return err
	}
	if res == nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return fmt.Errorf("no plan returned")
	}
	planJSON, err := toJSON(res.Rows[0][0])
	if err != nil {
		return err
	}
	root, err := explain.Parse(planJSON)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Shard string `json:"shard"`
		*explain.Analysis
	}{targets[0].Label, explain.AnalyzeVersion(root, ver)})
}
