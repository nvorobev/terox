package repl

import (
	"fmt"
	"sort"
	"strconv"

	"terox/internal/db"
	"terox/internal/render"
	"terox/internal/ui"
)

// F9+: тренды нагрузки. \statements snapshot захватывает текущий pg_stat_statements
// (агрегат по шардам), \statements diff сравнивает с захваченным и показывает, какие
// queryid прибавили больше всего суммарного времени и регрессировала ли их средняя
// латентность. Логика diff — ЧИСТАЯ клиентская (без БД), полностью тестируема.
// queryid server-local, поэтому снапшот валиден лишь в пределах одного сервера/мажора.

// workloadStat — агрегированная по шардам статистика одного queryid в снимке.
type workloadStat struct {
	calls   int64
	totalMs float64
	query   string
}

// meanMs — средняя латентность (total/calls), 0 при отсутствии вызовов.
func (s workloadStat) meanMs() float64 {
	if s.calls <= 0 {
		return 0
	}
	return s.totalMs / float64(s.calls)
}

// workloadSnapshot — снимок нагрузки: queryid → агрегат по всем шардам.
type workloadSnapshot struct {
	stats  map[string]workloadStat
	shards int
}

// buildWorkloadSnapshot агрегирует строки pg_stat_statements (веер по шардам) в
// снимок. Индексы колонок queryid(0)/calls(3)/total_ms(4)/query(последняя)
// присутствуют при любой версии — версионные max/stddev/wal лишь вставляются между
// ними, не сдвигая эти (как и в statementsSkew).
func buildWorkloadSnapshot(results []db.ShardResult) workloadSnapshot {
	snap := workloadSnapshot{stats: map[string]workloadStat{}}
	for _, sr := range results {
		if sr.Err != nil || sr.Result == nil {
			continue
		}
		snap.shards++
		for _, row := range sr.Result.Rows {
			if len(row) < 5 {
				continue
			}
			qid := str(row[0])
			if qid == "" {
				continue
			}
			st := snap.stats[qid]
			st.calls += asInt64viaString(str(row[3]))
			tot, _ := strconv.ParseFloat(str(row[4]), 64)
			st.totalMs += tot
			if st.query == "" {
				st.query = str(row[len(row)-1])
			}
			snap.stats[qid] = st
		}
	}
	return snap
}

// workloadDelta — изменение нагрузки одного queryid между двумя снимками.
type workloadDelta struct {
	queryid    string
	isNew      bool
	callsDelta int64
	totalDelta float64 // прирост суммарного времени (ms) — главный sort-ключ
	meanBefore float64
	meanAfter  float64
	query      string
}

// meanRatio — во сколько раз изменилась средняя латентность (0, если до было 0 или запрос новый).
func (d workloadDelta) meanRatio() float64 {
	if d.meanBefore <= 0 {
		return 0
	}
	return d.meanAfter / d.meanBefore
}

// diffWorkload сравнивает снимки и возвращает изменения по queryid, отсортированные
// по приросту суммарного времени (самое затратное — первым). Запросы без изменений
// (те же calls и total) опускаются.
func diffWorkload(before, after workloadSnapshot) []workloadDelta {
	var out []workloadDelta
	for qid, a := range after.stats {
		b, ok := before.stats[qid]
		d := workloadDelta{
			queryid:    qid,
			isNew:      !ok,
			callsDelta: a.calls - b.calls,
			totalDelta: a.totalMs - b.totalMs,
			meanBefore: b.meanMs(),
			meanAfter:  a.meanMs(),
			query:      a.query,
		}
		if !d.isNew && d.callsDelta == 0 && d.totalDelta == 0 {
			continue
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].totalDelta != out[j].totalDelta {
			return out[i].totalDelta > out[j].totalDelta
		}
		return out[i].queryid < out[j].queryid
	})
	return out
}

// captureWorkload выполняет широкий срез pg_stat_statements и строит снимок
// (или nil, если расширение недоступно — подсказка уже напечатана).
func (r *REPL) captureWorkload() *workloadSnapshot {
	o := statementsOpts{orderBy: "total", limit: 0} // широкий срез: агрегируем сами
	sql, _ := statementsQuery(r.serverVersion(), o)
	results := r.fanoutRead(sql)
	if r.statementsDegradationNotice(results) {
		return nil
	}
	snap := buildWorkloadSnapshot(results)
	return &snap
}

// doWorkloadSnapshot реализует \statements snapshot — захват снимка нагрузки.
func (r *REPL) doWorkloadSnapshot() {
	if len(r.targets) == 0 {
		fmt.Fprintln(r.out, "no shard selected")
		return
	}
	snap := r.captureWorkload()
	if snap == nil {
		return
	}
	r.lastWorkload = snap
	fmt.Fprintf(r.out, "captured workload snapshot: %d queryid(s) across %d shard(s)\n", len(snap.stats), snap.shards)
	fmt.Fprintln(r.out, ui.Dim.Render("generate more workload, then \\statements diff to see regressions (queryid is server-local)"))
}

// doWorkloadDiff реализует \statements diff — сравнение текущей нагрузки с захваченным
// снимком.
func (r *REPL) doWorkloadDiff() {
	if r.lastWorkload == nil {
		fmt.Fprintln(r.out, "no snapshot captured yet — run \\statements snapshot first")
		return
	}
	if len(r.targets) == 0 {
		fmt.Fprintln(r.out, "no shard selected")
		return
	}
	after := r.captureWorkload()
	if after == nil {
		return
	}
	deltas := diffWorkload(*r.lastWorkload, *after)
	if len(deltas) == 0 {
		fmt.Fprintln(r.out, "no workload change since the snapshot")
		return
	}
	headers := []string{"queryid", "calls_d", "total_ms_d", "mean_before", "mean_after", "mean_ratio", "query"}
	var rows [][]string
	const limit = 20
	for i, d := range deltas {
		if i >= limit {
			break
		}
		ratio := "—"
		switch {
		case d.isNew:
			ratio = "new"
		case d.meanRatio() > 0:
			ratio = fmt.Sprintf("%.2fx", d.meanRatio())
		}
		rows = append(rows, []string{
			d.queryid,
			fmt.Sprintf("%+d", d.callsDelta),
			fmt.Sprintf("%+.1f", d.totalDelta),
			fmt.Sprintf("%.2f", d.meanBefore),
			fmt.Sprintf("%.2f", d.meanAfter),
			ratio,
			d.query,
		})
	}
	title := fmt.Sprintf("%d queryid(s) changed since snapshot (by added total time)", len(deltas))
	render.Table(r.out, headers, rows, title)
	r.statementsCaveats()
}
