package explain

import (
	"fmt"
	"strings"
)

// capability — флаги классов полей EXPLAIN, доступных в плане; правила, которым
// они нужны, помечаются как "не оценено", если данных нет (например, в плане без
// ANALYZE).
type capability int

const (
	capActual   capability = 1 << iota // EXPLAIN ANALYZE: фактические строки/время/spill
	capBuffers                         // EXPLAIN (BUFFERS)
	capIOTiming                        // track_io_timing → I/O Read/Write Time
	capWAL                             // EXPLAIN (WAL), PG13+
)

func capsOf(root *Root) capability {
	var c capability
	if root.Plan.ActualTotalTime != nil {
		c |= capActual
	}
	if root.Plan.SharedReadBlocks != nil || root.Plan.TempWrittenBlocks != nil {
		c |= capBuffers
	}
	if root.Plan.IOReadTime != nil {
		c |= capIOTiming
	}
	if root.Plan.WALRecords != nil {
		c |= capWAL
	}
	return c
}

func capName(c capability) string {
	switch {
	case c&capActual != 0:
		return "EXPLAIN ANALYZE (actual rows/time)"
	case c&capWAL != 0:
		return "EXPLAIN (WAL)"
	case c&capIOTiming != 0:
		return "track_io_timing"
	case c&capBuffers != 0:
		return "EXPLAIN (BUFFERS)"
	}
	return "more plan fields"
}

// rule — одна диагностика со стабильным id, гейтом по версии/полям и
// оценщиком по узлу или по корню. nil от оценщика означает "не сработало".
type rule struct {
	id       string
	severity string
	minVer   int        // требуемый server_version_num (0 = любая)
	needs    capability // нужные правилу возможности
	node     func(e *engine, n *Node) *Finding
	root     func(e *engine) []Finding
}

// engine прогоняет набор правил по одному плану.
type engine struct {
	root      *Root
	exec      float64
	serverVer int
	caps      capability
	findings  []Finding
	notEval   map[string]string
	parent    map[*Node]*Node // потомок -> родитель, для правил, учитывающих родителя
}

// parentOf возвращает родителя n в дереве плана (nil для корня).
func (e *engine) parentOf(n *Node) *Node {
	if e.parent == nil {
		return nil
	}
	return e.parent[n]
}

func (e *engine) share(n *Node) float64 {
	if e.exec <= 0 {
		return 0
	}
	s := exclusiveTime(n) / e.exec
	if s > 1 { // время по воркерам может суммарно превышать wall-clock
		s = 1
	}
	return s
}

// skip возвращает причину, по которой правило не может выполниться (слишком
// старая версия или нет нужной возможности), либо "" если может.
func (e *engine) skip(r rule) string {
	if r.minVer > 0 && e.serverVer > 0 && e.serverVer < r.minVer {
		return fmt.Sprintf("needs PostgreSQL %d+", r.minVer/10000)
	}
	if miss := r.needs &^ e.caps; miss != 0 {
		return "needs " + capName(miss)
	}
	return ""
}

func (e *engine) run() {
	for _, r := range rules {
		if reason := e.skip(r); reason != "" {
			e.notEval[r.id] = reason
		}
	}
	for _, r := range rules {
		if r.root == nil {
			continue
		}
		if _, sk := e.notEval[r.id]; sk {
			continue
		}
		for _, f := range r.root(e) {
			f.RuleID = r.id
			if f.Severity == "" {
				f.Severity = r.severity
			}
			e.findings = append(e.findings, f)
		}
	}
	// Строим индекс потомок->родитель один раз, чтобы правила могли узнать
	// положение узла в дереве.
	e.parent = map[*Node]*Node{}
	var index func(n *Node)
	index = func(n *Node) {
		for i := range n.Plans {
			e.parent[&n.Plans[i]] = n
			index(&n.Plans[i])
		}
	}
	index(&e.root.Plan)

	var walk func(n *Node)
	walk = func(n *Node) {
		for _, r := range rules {
			if r.node == nil {
				continue
			}
			if _, sk := e.notEval[r.id]; sk {
				continue
			}
			if f := r.node(e, n); f != nil {
				f.RuleID = r.id
				if f.Severity == "" {
					f.Severity = r.severity
				}
				if f.Node == "" {
					f.Node = nodeLabel(n)
				}
				e.findings = append(e.findings, *f)
			}
		}
		for i := range n.Plans {
			walk(&n.Plans[i])
		}
	}
	walk(&e.root.Plan)
}

func (e *engine) notEvaluatedList() []string {
	var out []string
	for id, reason := range e.notEval {
		out = append(out, id+": "+reason)
	}
	sortStrings(out)
	return out
}

// f — маленький конструктор Finding.
func f(title string, conf float64, impact string) *Finding {
	return &Finding{Title: title, Confidence: conf, Impact: impact}
}

// rules — набор правил с учётом версии. У каждого стабильный ID (Compare по нему
// сопоставляет находки) и структура свидетельство→гипотеза→проверка→действие.
var rules = []rule{
	{
		id: "seqscan-high-filter", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if !strings.Contains(n.NodeType, "Seq Scan") || n.RowsRemovedByFilter == nil || n.ActualRows == nil {
				return nil
			}
			lp := loops(n)
			removed, returned := *n.RowsRemovedByFilter*lp, *n.ActualRows*lp
			total := removed + returned
			if total <= 1000 || removed/total <= 0.9 {
				return nil
			}
			fd := f(fmt.Sprintf("Seq Scan on %s reads almost the whole table then filters it", n.RelationName), 0.75, "high")
			// Поднимаем до CRITICAL только если скан занимает заметное абсолютное
			// время, а не просто большую долю микросекундного запроса.
			if nodeTotalTime(n) > 100 && e.share(n) > 0.3 {
				fd.Severity = Critical
			}
			fd.Evidence = []string{fmt.Sprintf("read %.0f rows, kept %.0f (%.1f%% discarded by filter)", total, returned, 100*removed/total)}
			fd.Hypothesis = "the predicate lacks a selective usable index, so every row is read and filtered"
			fd.Checks = []string{"compare the filter columns against existing indexes (\\d " + relOr(n, "<table>") + ")", "is the table genuinely small? then a seq scan is fine"}
			fd.Actions = []string{filterIndexSuggestion(n)}
			return fd
		},
	},
	{
		id: "row-misestimate", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.ActualRows == nil {
				return nil
			}
			lp := loops(n)
			actual, est := *n.ActualRows, n.PlanRows
			if actual < 1 || est < 1 {
				return nil
			}
			ratio, dir := est/actual, "over"
			if actual > est {
				ratio, dir = actual/est, "under"
			}
			if ratio < 10 || (actual*lp <= 1000 && est*lp <= 1000) {
				return nil
			}
			fd := f(fmt.Sprintf("Planner %sestimated rows by %.0f× at %s", dir, ratio, nodeLabel(n)), 0.85, "high")
			fd.Evidence = []string{fmt.Sprintf("estimated %.0f, actual %.0f rows per loop", est, actual)}
			fd.Hypothesis = "stale or insufficient statistics (or correlated columns) — bad estimates lead to wrong join/scan choices"
			// Готовый ANALYZE/CREATE STATISTICS выдаём только для базовой таблицы;
			// для Join/Aggregate (без RelationName) ошибка идёт от дочернего скана,
			// поэтому указываем на него.
			if n.RelationName != "" {
				rel := quoteIdentSuggest(n.RelationName)
				fd.Checks = []string{"check pg_stats freshness / n_distinct for " + n.RelationName, "are the filtered columns correlated?"}
				fd.Actions = []string{"ANALYZE " + rel + ";", "CREATE STATISTICS ... (dependencies, mcv) ON col_a, col_b FROM " + rel + ";"}
			} else {
				fd.Checks = []string{"trace the misestimate to the base scan(s) feeding this node", "are the underlying columns stale or correlated?"}
				fd.Actions = []string{"ANALYZE the base tables under this node, and consider extended statistics (CREATE STATISTICS) on correlated columns"}
			}
			return fd
		},
	},
	{
		id: "nested-loop-many", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			// Большое число итераций относится к ВНУТРЕННЕЙ стороне Nested Loop (она
			// перевыполняется на каждую внешнюю строку). Срабатываем только там —
			// узел, повторяемый другим родителем (например Append), это не та проблема.
			if p := e.parentOf(n); p == nil || p.NodeType != "Nested Loop" {
				return nil
			}
			lp := loops(n)
			if lp < 1000 || nodeTotalTime(n) <= 0 || e.share(n) <= 0.2 {
				return nil
			}
			fd := f(fmt.Sprintf("%s executed %.0f times", nodeLabel(n), lp), 0.8, "high")
			fd.Evidence = []string{fmt.Sprintf("%.0f loops totalling %.0f ms%s", lp, nodeTotalTime(n), timeNote(e.share(n)))}
			fd.Hypothesis = "the inner side of a nested loop is re-executed many times"
			fd.Checks = []string{"is there an index for a hash/merge join key?", "could the outer side be filtered earlier?"}
			fd.Actions = []string{"add an index so the planner picks a hash/merge join, enable Memoize, or filter the outer side earlier"}
			return fd
		},
	},
	{
		id: "sort-spill", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.SortSpaceType != "Disk" && !strings.Contains(strings.ToLower(n.SortMethod), "external") {
				return nil
			}
			fd := f("Sort spilled to disk (insufficient work_mem)", 0.9, "high")
			ev := "sort used an external merge (spilled to disk)"
			if n.SortSpaceUsed != nil {
				ev = fmt.Sprintf("sort spilled to disk, %.0f MB of temp files", *n.SortSpaceUsed/1024)
			}
			fd.Evidence = []string{ev}
			fd.Hypothesis = "the sort set exceeded work_mem and was written to disk"
			fd.Checks = []string{"current work_mem (SHOW work_mem)", "could the result set be reduced before sorting?"}
			fd.Actions = []string{"raise work_mem for this statement only:\n  SET LOCAL work_mem = '256MB';"}
			return fd
		},
	},
	{
		id: "hash-spill", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.HashBatches == nil || *n.HashBatches <= 1 {
				return nil
			}
			fd := f(fmt.Sprintf("Hash build spilled into %d batches", *n.HashBatches), 0.9, "high")
			fd.Evidence = []string{fmt.Sprintf("%d batches — the hash table did not fit in work_mem", *n.HashBatches)}
			fd.Hypothesis = "the build side is large, or its row count was underestimated"
			fd.Checks = []string{"is there a row-misestimate feeding the hash?", "can the build side be filtered earlier?"}
			fd.Actions = []string{"reduce the build side with earlier filtering/indexing, raise work_mem locally, or fix the estimate"}
			return fd
		},
	},
	{
		id: "hashagg-spill", severity: Warning, minVer: 130000, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			spilled := (n.DiskUsageKB != nil && *n.DiskUsageKB > 0) || (n.HashAggBatch != nil && *n.HashAggBatch > 1)
			if !spilled {
				return nil
			}
			fd := f("Hash Aggregate spilled to disk", 0.9, "high")
			ev := "the GROUP BY hash table exceeded work_mem and spilled"
			if n.DiskUsageKB != nil {
				ev = fmt.Sprintf("GROUP BY hash spilled, %.0f MB on disk", *n.DiskUsageKB/1024)
			}
			fd.Evidence = []string{ev}
			fd.Hypothesis = "high group cardinality exceeded work_mem"
			fd.Checks = []string{"how many distinct groups does the GROUP BY produce?"}
			fd.Actions = []string{"raise work_mem locally (SET LOCAL work_mem), reduce group cardinality, or pre-aggregate"}
			return fd
		},
	},
	{
		id: "ios-heap-fetches", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.HeapFetches == nil || *n.HeapFetches <= 1000 {
				return nil
			}
			fd := f(fmt.Sprintf("Index Only Scan on %s did %.0f heap fetches", relOr(n, n.IndexName), *n.HeapFetches), 0.85, "medium")
			fd.Evidence = []string{fmt.Sprintf("%.0f heap fetches — the index-only benefit is lost", *n.HeapFetches)}
			fd.Hypothesis = "the visibility map is stale, so the scan still visits the heap"
			fd.Checks = []string{"last (auto)vacuum time for " + relOr(n, "<table>")}
			fd.Actions = []string{"VACUUM the table so the visibility map is current:\n  VACUUM (ANALYZE) " + relSuggest(n, "<table>") + ";"}
			return fd
		},
	},
	{
		id: "bitmap-recheck", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.RowsRemovedByRecheck == nil || *n.RowsRemovedByRecheck*loops(n) <= 1000 {
				return nil
			}
			fd := f(fmt.Sprintf("Bitmap recheck on %s discarded %.0f rows", n.RelationName, *n.RowsRemovedByRecheck*loops(n)), 0.75, "medium")
			fd.Evidence = []string{"the bitmap became lossy (not enough work_mem), so each candidate row is rechecked"}
			fd.Hypothesis = "the bitmap exceeded work_mem and went lossy"
			fd.Actions = []string{"raise work_mem locally, or make the index more selective for this predicate"}
			return fd
		},
	},
	{
		id: "late-join-filter", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.RowsRemovedByJoin == nil || n.ActualRows == nil {
				return nil
			}
			lp := loops(n)
			removed, kept := *n.RowsRemovedByJoin*lp, *n.ActualRows*lp
			total := removed + kept
			if total <= 10000 || removed/total <= 0.9 {
				return nil
			}
			fd := f(fmt.Sprintf("%s joined %.0f rows then discarded %.1f%% with a join filter", n.NodeType, total, 100*removed/total), 0.7, "medium")
			fd.Evidence = []string{fmt.Sprintf("%.0f rows joined, %.0f discarded afterwards", total, removed)}
			fd.Hypothesis = "work is wasted joining rows that are thrown away"
			fd.Actions = []string{"apply the filter before the join (push into a subquery/CTE or a scan predicate), or add a more selective join key"}
			return fd
		},
	},
	{
		id: "memoize-miss", severity: Info, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.CacheHits == nil || n.CacheMisses == nil {
				return nil
			}
			hits, misses := *n.CacheHits, *n.CacheMisses
			if total := hits + misses; total <= 1000 || misses/total <= 0.8 {
				return nil
			}
			fd := f(fmt.Sprintf("Memoize cache mostly missed (%.0f%% misses)", 100*(*n.CacheMisses)/(*n.CacheHits+*n.CacheMisses)), 0.6, "low")
			fd.Evidence = []string{"the cached inner result rarely repeats, so Memoize adds overhead without benefit"}
			fd.Hypothesis = "low repetition in the inner key, or a row-misestimate that chose Memoize"
			fd.Actions = []string{"consider a different join strategy; fixing estimates may drop Memoize"}
			return fd
		},
	},
	{
		id: "correlated-subplan", severity: Warning, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			isSub := n.NodeType == "CTE Scan" || strings.HasPrefix(n.SubplanName, "SubPlan") || strings.HasPrefix(n.ParentRelationship, "SubPlan")
			if !isSub || loops(n) < 100 || e.share(n) <= 0.1 {
				return nil
			}
			// Узел под Materialize — дешёвый rescan однажды посчитанного результата,
			// а не перевычисление на строку, поэтому не считаем его коррелированным.
			if p := e.parentOf(n); p != nil && p.NodeType == "Materialize" {
				return nil
			}
			fd := f(fmt.Sprintf("%s executed %.0f times", nodeLabel(n), loops(n)), 0.7, "high")
			fd.Evidence = []string{fmt.Sprintf("a correlated subquery / CTE is re-evaluated per outer row%s", timeNote(e.share(n)))}
			fd.Hypothesis = "a correlated SubPlan or multiply-scanned CTE runs once per outer row"
			fd.Actions = []string{"rewrite the correlated subquery as a JOIN or LATERAL, or aggregate it once into a CTE/temp table"}
			return fd
		},
	},
	{
		id: "expr-index-mismatch", severity: Info,
		node: func(e *engine, n *Node) *Finding {
			if !isScan(n) {
				return nil
			}
			cond := n.Filter + " " + n.IndexCond
			if !exprIndexHint(cond) {
				return nil
			}
			fd := f(fmt.Sprintf("Possible expression/type-cast mismatch at %s", nodeLabel(n)), 0.4, "medium")
			fd.Evidence = []string{"the predicate applies a function/cast to a column (" + strings.TrimSpace(condSnippet(cond)) + ")"}
			fd.Hypothesis = "a function/cast on the column can prevent a plain index from being used"
			fd.Actions = []string{"build a matching expression index, or compare the column without a function/cast:\n  CREATE INDEX CONCURRENTLY ON " + relSuggest(n, "<table>") + " ((lower(col)));"}
			return fd
		},
	},
	{
		id: "no-partition-pruning", severity: Info,
		node: func(e *engine, n *Node) *Finding {
			if (n.NodeType != "Append" && n.NodeType != "Merge Append") || len(n.Plans) < 8 {
				return nil
			}
			pruned := 0
			if n.SubplansRemoved != nil {
				pruned = *n.SubplansRemoved
			}
			if pruned != 0 {
				return nil
			}
			fd := f(fmt.Sprintf("%s scans %d partitions with no pruning", n.NodeType, len(n.Plans)), 0.6, "medium")
			fd.Evidence = []string{"no partitions were pruned"}
			fd.Hypothesis = "the query lacks a usable partition-key predicate (or its type mismatches)"
			fd.Actions = []string{"add a filter on the partition key (matching its type) so PostgreSQL can prune partitions"}
			return fd
		},
	},
	{
		id: "parallel-undersubscribed", severity: Info, needs: capActual,
		node: func(e *engine, n *Node) *Finding {
			if n.WorkersPlanned == nil || n.WorkersLaunched == nil || *n.WorkersLaunched >= *n.WorkersPlanned {
				return nil
			}
			fd := f(fmt.Sprintf("Only %d of %d planned parallel workers launched", *n.WorkersLaunched, *n.WorkersPlanned), 0.7, "medium")
			fd.Evidence = []string{"the cluster ran out of available parallel workers"}
			fd.Hypothesis = "max_parallel_workers exhausted by concurrent load"
			fd.Actions = []string{"check max_parallel_workers / max_parallel_workers_per_gather and concurrent load"}
			return fd
		},
	},
	{
		id: "wide-rows", severity: Info,
		node: func(e *engine, n *Node) *Finding {
			if n.PlanWidth < 2000 || n.ParentRelationship == "InitPlan" {
				return nil
			}
			fd := f(fmt.Sprintf("Wide rows at %s (~%d bytes/row)", nodeLabel(n), n.PlanWidth), 0.5, "low")
			fd.Evidence = []string{fmt.Sprintf("~%d bytes per row increases memory, sort and network cost", n.PlanWidth)}
			fd.Hypothesis = "more columns/data are selected than needed (SELECT *)"
			fd.Actions = []string{"select only the columns you need instead of SELECT *"}
			return fd
		},
	},
	{
		id: "io-time-dominant", severity: Warning, needs: capIOTiming,
		node: func(e *engine, n *Node) *Finding {
			if n.IOReadTime == nil || e.exec <= 0 {
				return nil
			}
			// Время I/O кумулятивно, поэтому сообщаем только на корне.
			if n != &e.root.Plan {
				return nil
			}
			ratio := *n.IOReadTime / e.exec
			if ratio > 1 { // I/O по воркерам кумулятивно и может превышать wall-clock
				ratio = 1
			}
			if ratio < 0.5 {
				return nil
			}
			fd := f(fmt.Sprintf("%.0f%% of execution time spent reading from disk", 100*ratio), 0.8, "high")
			fd.Node = "query"
			fd.Evidence = []string{fmt.Sprintf("%.0f ms of %.0f ms execution was I/O read time", *n.IOReadTime, e.exec)}
			fd.Hypothesis = "cold cache or slow storage — data not in shared_buffers/OS cache"
			fd.Checks = []string{"is shared_buffers / effective_cache_size sized for the working set?", "was the cache cold (first run)?"}
			fd.Actions = []string{"warm the cache or re-run; review shared_buffers and storage latency"}
			return fd
		},
	},
	{
		id: "wal-heavy", severity: Info, minVer: 130000, needs: capWAL,
		node: func(e *engine, n *Node) *Finding {
			if n != &e.root.Plan || n.WALBytes == nil || *n.WALBytes < 16*1024*1024 {
				return nil
			}
			fd := f(fmt.Sprintf("Query wrote %.0f MB of WAL", *n.WALBytes/1024/1024), 0.7, "medium")
			fd.Node = "query"
			ev := fmt.Sprintf("%.0f MB WAL", *n.WALBytes/1024/1024)
			if n.WALFPI != nil {
				ev += fmt.Sprintf(", %.0f full-page images", *n.WALFPI)
			}
			fd.Evidence = []string{ev}
			fd.Hypothesis = "a large write; full-page images spike right after a checkpoint"
			fd.Actions = []string{"batch the write, or align heavy writes away from checkpoints; review checkpoint/WAL settings"}
			return fd
		},
	},

	// --- правила уровня всего запроса (root) ---
	{
		id: "planning-dominates", severity: Warning, needs: capActual,
		root: func(e *engine) []Finding {
			if e.root.PlanningTime <= 5 || e.exec <= 0 || e.root.PlanningTime <= 2*e.exec {
				return nil
			}
			fd := f(fmt.Sprintf("Planning took %.1f ms but execution only %.1f ms", e.root.PlanningTime, e.exec), 0.8, "medium")
			fd.Node = "query"
			fd.Evidence = []string{"the query is cheap to run yet expensive to plan"}
			fd.Hypothesis = "many partitions/indexes, a very complex query, or generic-vs-custom prepared plans"
			fd.Actions = []string{"consider plan_cache_mode, or reduce partitions/indexes scanned"}
			return []Finding{*fd}
		},
	},
	{
		id: "jit-overhead", severity: Info, needs: capActual,
		root: func(e *engine) []Finding {
			if e.root.JIT == nil || e.root.JIT.Timing.Total <= 5 || e.exec <= 0 || e.root.JIT.Timing.Total <= 0.25*e.exec {
				return nil
			}
			fd := f(fmt.Sprintf("JIT spent %.1f ms (%.0f%% of execution)", e.root.JIT.Timing.Total, 100*e.root.JIT.Timing.Total/e.exec), 0.7, "medium")
			fd.Node = "query"
			fd.Evidence = []string{"JIT compilation cost more than it saved for this short query"}
			fd.Actions = []string{"raise jit_above_cost, or disable JIT for such queries:\n  SET LOCAL jit = off;"}
			return []Finding{*fd}
		},
	},
	{
		id: "trigger-overhead", severity: Info, needs: capActual,
		root: func(e *engine) []Finding {
			var t float64
			for _, tr := range e.root.Triggers {
				t += tr.Time
			}
			if t <= 0 || e.exec <= 0 || t <= 0.2*e.exec {
				return nil
			}
			fd := f(fmt.Sprintf("Triggers added %.1f ms (%.0f%% of execution)", t, 100*t/e.exec), 0.7, "medium")
			fd.Node = "query"
			fd.Evidence = []string{"AFTER/BEFORE triggers (often FK checks) are a significant part of the time"}
			fd.Actions = []string{"review trigger logic and ensure foreign-key columns are indexed on both sides"}
			return []Finding{*fd}
		},
	},
	{
		id: "planner-overrides", severity: Info,
		root: func(e *engine) []Finding {
			var out []Finding
			for k, v := range e.root.Settings {
				if strings.HasPrefix(k, "enable_") && v == "off" {
					fd := f(fmt.Sprintf("Planner setting %s = off", k), 0.9, "low")
					fd.Node = "query"
					fd.Evidence = []string{"a planner method is disabled in this session — the plan may not match production"}
					fd.Actions = []string{"re-check the plan with default settings (RESET " + k + ";) before drawing conclusions"}
					out = append(out, *fd)
				}
			}
			sortFindings(out)
			return out
		},
	},
}

func sortFindings(fs []Finding) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j-1].Title > fs[j].Title; j-- {
			fs[j-1], fs[j] = fs[j], fs[j-1]
		}
	}
}
