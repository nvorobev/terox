package repl

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"terox/internal/cluster"
	"terox/internal/complete"
	"terox/internal/db"
	"terox/internal/ui"
)

// createIndexConcurrentlyRe — скромная локальная привязка к CREATE [UNIQUE] INDEX
// CONCURRENTLY (migration.concurrentlyRe неэкспортирован). Используется только для
// напоминания про \heal после сбойной нетранзакционной миграции — не для решений о
// безопасности.
var createIndexConcurrentlyRe = regexp.MustCompile(`(?is)create\s+(unique\s+)?index\s+concurrently`)

// hasCreateIndexConcurrently сообщает, есть ли среди операторов CREATE INDEX
// CONCURRENTLY (любой из них).
func hasCreateIndexConcurrently(statements []string) bool {
	for _, s := range statements {
		if createIndexConcurrentlyRe.MatchString(s) {
			return true
		}
	}
	return false
}

// anyExecError сообщает, упал ли хотя бы один шард.
func anyExecError(results []db.ExecResult) bool {
	for _, res := range results {
		if res.Err != nil {
			return true
		}
	}
	return false
}

// warnInvalidIndexLeftover напоминает, что сбойный CREATE INDEX CONCURRENTLY мог
// оставить INVALID-индекс, и предлагает \heal.
func (r *REPL) warnInvalidIndexLeftover() {
	fmt.Fprintln(r.out, ui.Danger.Render("⚠ a failed CREATE INDEX CONCURRENTLY can leave an INVALID index behind"))
	fmt.Fprintln(r.out, "  it still takes up space and is ignored by the planner — run \\heal to find and drop the leftovers.")
}

// invalidIndexQuery находит остатки сбойного CREATE INDEX CONCURRENTLY: индексы с
// indisvalid = false в пользовательских схемах. Возвращает schema, index, table.
const invalidIndexQuery = `SELECT n.nspname, c.relname, t.relname FROM pg_index i ` +
	`JOIN pg_class c ON c.oid=i.indexrelid ` +
	`JOIN pg_class t ON t.oid=i.indrelid ` +
	`JOIN pg_namespace n ON n.oid=c.relnamespace ` +
	`WHERE NOT i.indisvalid AND n.nspname NOT IN ('pg_catalog','information_schema') ` +
	`ORDER BY 1,2`

// invalidIndex — один невалидный индекс на конкретном шарде.
type invalidIndex struct {
	schema string
	index  string
	table  string
}

// dropStmt строит команду удаления невалидного индекса. CONCURRENTLY — чтобы DROP
// не брал тяжёлую блокировку (как и сам CREATE INDEX CONCURRENTLY), IF EXISTS — на
// случай гонки/повторного запуска. Идентификаторы экранируются через QuoteIdent,
// чтобы имя объекта из БД нельзя было использовать для инъекции.
func (ix invalidIndex) dropStmt() string {
	return fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s.%s",
		complete.QuoteIdent(ix.schema, nil), complete.QuoteIdent(ix.index, nil))
}

// label — человекочитаемая ссылка "schema.index on table" для вывода.
func (ix invalidIndex) label() string {
	return fmt.Sprintf("%s.%s on %s", ix.schema, ix.index, ix.table)
}

// detectInvalidIndexes выполняет детектирующий read-only запрос на шарде и
// собирает невалидные индексы. Второй результат — флаг настоящей ошибки запроса
// (как в docQuery): безобидный пропуск (нет прав) даёт пустой список без ошибки.
func (r *REPL) detectInvalidIndexes(shard cluster.Shard) ([]invalidIndex, bool) {
	res, isErr := r.docQuery(shard, invalidIndexQuery)
	if res == nil {
		return nil, isErr
	}
	out := make([]invalidIndex, 0, len(res.Rows))
	for _, row := range res.Rows {
		out = append(out, invalidIndex{
			schema: cellStr(row, 0),
			index:  cellStr(row, 1),
			table:  cellStr(row, 2),
		})
	}
	return out, isErr
}

// doHeal — точка входа \heal. Без --apply это read-only диагностика (всегда
// разрешена). С --apply невалидные индексы удаляются (требует write-режим +
// подтверждение + строгий барьер на prod).
func (r *REPL) doHeal(args []string) error {
	apply := false
	for _, a := range args {
		switch a {
		case "--apply":
			apply = true
		default:
			return fmt.Errorf("usage: \\heal [--apply]")
		}
	}
	if len(r.targets) == 0 {
		return fmt.Errorf("no shard selected")
	}
	if apply {
		return r.healApply()
	}
	return r.healDiagnose()
}

// healDiagnose печатает невалидные индексы по шардам и готовый DROP для каждого
// (read-only — никаких записей).
func (r *REPL) healDiagnose() error {
	fmt.Fprintf(r.out, "%s scanning %d shard(s) [%s] for invalid indexes\n",
		sevColor("heal"), len(r.targets), r.targetLabel)
	found := 0
	for _, shard := range r.targets {
		if err := r.reachable(shard); err != nil {
			fmt.Fprintf(r.out, "\n  [%s] unreachable: %v\n", shard.Label, oneLine(err.Error()))
			continue
		}
		ixs, errored := r.detectInvalidIndexes(shard)
		if errored {
			fmt.Fprintf(r.out, "\n  [%s] could not check (query errored)\n", shard.Label)
			continue
		}
		if len(ixs) == 0 {
			continue
		}
		found += len(ixs)
		fmt.Fprintf(r.out, "\n  [%s] %d invalid index(es):\n", shard.Label, len(ixs))
		for _, ix := range ixs {
			fmt.Fprintf(r.out, "    - %s\n", ix.label())
			fmt.Fprintf(r.out, "        %s;\n", ix.dropStmt())
		}
	}
	if found == 0 {
		fmt.Fprintln(r.out, "no invalid indexes")
		return nil
	}
	fmt.Fprintln(r.out, ui.Dim.Render("run \\heal --apply to drop them (requires \\write on)"))
	return nil
}

// healApply удаляет невалидные индексы на каждом шарде. Строго за write-режимом +
// подтверждением; на prod — дополнительный барьер (ввод 'drop'), т.к. DROP INDEX
// CONCURRENTLY идёт ВНЕ транзакции и без защитной обёртки. По каждому шарду строит
// его собственный набор DROP-команд и выполняет их на ЭТОМ шарде нетранзакционным
// путём (ExecScript), как в execWrite.
func (r *REPL) healApply() error {
	if !r.writeMode {
		return fmt.Errorf("\\heal --apply drops indexes — enable write mode first (\\write on)")
	}

	// Заново детектим невалидные индексы по каждому шарду: набор мог измениться с
	// последней диагностики, а удалять нужно ровно то, что есть СЕЙЧАС на шарде.
	perShard := make(map[string][]invalidIndex)
	var order []cluster.Shard
	total := 0
	for _, shard := range r.targets {
		if err := r.reachable(shard); err != nil {
			fmt.Fprintf(r.out, "  [%s] unreachable: %v\n", shard.Label, oneLine(err.Error()))
			continue
		}
		ixs, errored := r.detectInvalidIndexes(shard)
		if errored {
			fmt.Fprintf(r.out, "  [%s] could not check (query errored) — skipping\n", shard.Label)
			continue
		}
		if len(ixs) == 0 {
			continue
		}
		perShard[shard.Label] = ixs
		order = append(order, shard)
		total += len(ixs)
	}
	if total == 0 {
		fmt.Fprintln(r.out, "no invalid indexes")
		return nil
	}

	// Показываем, что именно будет удалено и на каком шарде.
	fmt.Fprintf(r.out, "%s will DROP %d invalid index(es) across %d shard(s):\n",
		ui.Danger.Render("⚠ heal --apply"), total, len(order))
	for _, shard := range order {
		for _, ix := range perShard[shard.Label] {
			fmt.Fprintf(r.out, "  [%s] %s;\n", shard.Label, ix.dropStmt())
		}
	}
	fmt.Fprintln(r.out, ui.Dim.Render("  DROP INDEX CONCURRENTLY runs UNPROTECTED (outside any transaction; only the client migration_timeout applies)."))

	// Обычное подтверждение записи.
	if !r.confirmWrite() {
		fmt.Fprintln(r.out, "cancelled")
		return nil
	}
	// На prod — дополнительный строгий барьер (по образцу 'unprotected' в execWrite).
	if r.prod {
		fmt.Fprintf(r.out, "%s dropping indexes on PROD across %d shard(s) [%s] — this is irreversible.\n",
			ui.Danger.Render("⚠ PROD"), len(order), r.targetLabel)
		if strings.TrimSpace(r.readLine("Type 'drop' to proceed: ")) != "drop" {
			fmt.Fprintln(r.out, "cancelled")
			return nil
		}
	}

	// Прерываемый контекст + клиентский дедлайн migration_timeout, как в runForEach:
	// DROP INDEX CONCURRENTLY не может использовать серверный statement_timeout.
	ctx, cancel := interruptible()
	defer cancel()
	timeout := time.Duration(r.cfg.MigrationTimeout)

	var okShards, failShards []string
	for _, shard := range order {
		ixs := perShard[shard.Label]
		stmts := make([]string, len(ixs))
		for i, ix := range ixs {
			stmts[i] = ix.dropStmt()
		}
		cctx, c2 := contextWithOptionalTimeout(ctx, timeout)
		_, err := r.mgr.ExecScript(cctx, shard, stmts)
		c2()
		if err != nil {
			failShards = append(failShards, shard.Label)
			fmt.Fprintf(r.out, "  [%s] error: %v\n", shard.Label, oneLine(err.Error()))
			continue
		}
		okShards = append(okShards, shard.Label)
		fmt.Fprintf(r.out, "  [%s] dropped %d index(es)\n", shard.Label, len(ixs))
	}

	sort.Strings(okShards)
	sort.Strings(failShards)
	fmt.Fprintf(r.out, "%s healed %d shard(s)", sevColor("heal"), len(okShards))
	if len(failShards) > 0 {
		fmt.Fprintf(r.out, ", %d shard(s) had errors (%s)", len(failShards), strings.Join(failShards, ", "))
	}
	fmt.Fprintln(r.out)
	return nil
}
