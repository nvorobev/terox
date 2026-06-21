package repl

import (
	"fmt"
	"strings"
	"time"

	"terox/internal/advisor"
	"terox/internal/explain"
)

// Feature 8 / R-NEW-3: index advisor поверх EXPLAIN (без ANALYZE — запрос не
// выполняется). Чистая эвристика анализа плана вынесена в пакет terox/internal/advisor;
// здесь остаётся только REPL-команда \advise, связывающая её с БД и выводом.
//
// Покрываемые источники предложений (см. internal/advisor):
//   - filter   — Seq Scan с Filter (равенства → ведущие столбцы, диапазон → хвост);
//   - join     — Hash/Merge/Index Cond и Join Filter (столбцы join по Seq-Scan стороне);
//   - sort     — Sort Key над одиночной Seq-Scan таблицей (индекс под ORDER BY);
//   - group    — Group Key (Sorted/Hashed Aggregate) над одиночной Seq-Scan таблицей.

// doAdvise реализует \advise <query>: EXPLAIN запроса (без выполнения) и
// эвристические предложения индексов из плана (фильтры, join, sort, group).
func (r *REPL) doAdvise(raw string) error {
	query := strings.TrimSpace(rawTail(raw, 1))
	if query == "" {
		return fmt.Errorf("usage: \\advise <query>  — suggests indexes from the query plan (does not run it)")
	}
	if len(r.targets) == 0 {
		return fmt.Errorf("no shard selected")
	}
	target := r.targets[0] // canary: один шард
	ctx, cancel := interruptible()
	defer cancel()
	cctx, c2 := contextWithOptionalTimeout(ctx, time.Duration(r.cfg.QueryTimeout))
	defer c2()

	// VERBOSE — чтобы EXPLAIN отдавал per-node "Schema" (иначе schema-aware код
	// мёртв и одноимённые таблицы из разных схем смешиваются).
	res, err := r.mgr.Exec(cctx, target, "EXPLAIN (VERBOSE, FORMAT JSON) "+query, true)
	if err != nil {
		return fmt.Errorf("advise: %v", err)
	}
	if res == nil || len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return fmt.Errorf("advise: no plan returned")
	}
	planJSON, err := toJSON(res.Rows[0][0])
	if err != nil {
		return err
	}
	root, err := explain.Parse(planJSON)
	if err != nil {
		return err
	}

	props := advisor.DedupeOverlap(advisor.CollectProposals(&root.Plan))
	if len(props) == 0 {
		fmt.Fprintf(r.out, "index advisor (%s): no filtered scans, join probes, or sort/group keys on a sequential scan — nothing to suggest\n", target.LabelDB())
		return nil
	}

	fmt.Fprintf(r.out, "index advisor for the plan on %s (heuristic — limited live statistics):\n", target.LabelDB())

	// Кеши по таблице: полные столбцы существующих индексов и оценка размера.
	idxCache := map[string][][]string{}
	existingOf := func(schema, table string) [][]string {
		key := schema + "." + table
		if m, ok := idxCache[key]; ok {
			return m
		}
		var cols [][]string
		if ir, err := r.mgr.Exec(cctx, target, advisor.IndexColumnsSQL(schema, table), true); err == nil && ir != nil {
			cols = advisor.ParseIndexColumns(ir.Rows)
		}
		idxCache[key] = cols
		return cols
	}
	type tstat struct {
		rows int64
		size string
		ok   bool
	}
	statCache := map[string]tstat{}
	statOf := func(schema, table string) tstat {
		key := schema + "." + table
		if s, ok := statCache[key]; ok {
			return s
		}
		var s tstat
		if sr, err := r.mgr.Exec(cctx, target, advisor.TableStatsSQL(schema, table), true); err == nil && sr != nil && len(sr.Rows) > 0 && len(sr.Rows[0]) >= 3 {
			s.rows = asInt64(sr.Rows[0][0])
			s.size = str(sr.Rows[0][2])
			s.ok = true
		}
		statCache[key] = s
		return s
	}

	shown, skipped := 0, 0
	const maxShown = 20
	const tinyRows = 1000
	for _, p := range props {
		existing := existingOf(p.Schema, p.Table)
		// Полностью покрыто существующим индексом (cols — префикс индекса) → избыточно.
		if advisor.CoveredByExisting(p.Cols, existing) {
			skipped++
			continue
		}
		st := statOf(p.Schema, p.Table)
		// Крошечная таблица: индекс почти не ускоряет, но добавляет write/storage cost.
		if st.ok && st.rows >= 0 && st.rows < tinyRows {
			skipped++
			continue
		}
		if shown >= maxShown {
			continue
		}
		shown++
		suggest, rollback := p.IndexDDL()
		fmt.Fprintf(r.out, "  • [%s/%s] %s\n", p.Kind, p.Confidence, p.Evidence)
		if st.ok {
			fmt.Fprintf(r.out, "    table:    ~%d row(s), %s heap — a new index also costs storage and slows writes\n", st.rows, st.size)
		}
		// Селективность ведущего столбца из pg_stats (если таблица проанализирована).
		if cs, err := r.mgr.Exec(cctx, target, advisor.ColumnStatsSQL(p.Schema, p.Table, p.Cols[0]), true); err == nil && cs != nil && len(cs.Rows) > 0 && len(cs.Rows[0]) > 0 {
			if note := advisor.SelectivityNote(asFloat64(cs.Rows[0][0])); note != "" {
				fmt.Fprintf(r.out, "    stats:    %s\n", note)
			}
		}
		if advisor.SharesLeadingColumn(p.Cols, existing) {
			fmt.Fprintf(r.out, "    note:     an existing index shares the leading column; this composite may still help\n")
		}
		if p.LikeLead {
			fmt.Fprintf(r.out, "    opclass:  leading column uses LIKE — for left-anchored patterns add a text_pattern_ops opclass (… (%s text_pattern_ops)) outside the C locale\n", advisor.QuoteIdentDDL(p.Cols[0]))
		}
		fmt.Fprintf(r.out, "    suggest:  %s\n", suggest)
		fmt.Fprintf(r.out, "    rollback: %s\n", rollback)
		fmt.Fprintf(r.out, "    validate: after CREATE, EXPLAIN the query — expect an Index Scan using %s instead of a Seq Scan\n", p.IndexName())
	}
	if shown == 0 {
		fmt.Fprintf(r.out, "  all candidate columns are already covered by an index or sit on tiny tables — no suggestion\n")
		return nil
	}
	if skipped > 0 {
		fmt.Fprintf(r.out, "  (%d candidate(s) skipped: already covered by an index, or on a table under %d rows)\n", skipped, tinyRows)
	}
	if len(props) > maxShown {
		fmt.Fprintf(r.out, "  (showing %d of %d proposals)\n", maxShown, len(props))
	}
	fmt.Fprintf(r.out, "  shards:   analyzed on canary shard %s only; plans/stats can differ — re-check on all %d selected shard(s)\n", target.LabelDB(), len(r.targets))
	fmt.Fprintln(r.out, "  heuristic: confidence is from plan shape + table size only — still verify selectivity (pg_stats), multi-column order and write/storage cost; one heuristic is not enough")
	return nil
}
