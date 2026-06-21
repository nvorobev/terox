package repl

import (
	"errors"
	"testing"

	"terox/internal/cluster"
	"terox/internal/db"
)

// row собирает строку pg_stat_statements в порядке колонок без версионных полей:
// queryid, db, role, calls, total_ms, …, query (минимум 5 колонок, query последняя).
func wlRow(qid string, calls int64, totalMs string, query string) []any {
	return []any{qid, "shopdb", "app", calls, totalMs, query}
}

func wlResults(rows ...[]any) []db.ShardResult {
	return []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Rows: rows, IsSelect: true}},
	}
}

func TestBuildWorkloadSnapshotAggregatesShards(t *testing.T) {
	// Один и тот же queryid на двух шардах суммируется.
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Rows: [][]any{wlRow("100", 10, "200", "select 1")}, IsSelect: true}},
		{Shard: cluster.Shard{Label: "rs002"}, Result: &db.Result{Rows: [][]any{wlRow("100", 5, "100", "select 1")}, IsSelect: true}},
	}
	snap := buildWorkloadSnapshot(results)
	st, ok := snap.stats["100"]
	if !ok {
		t.Fatal("queryid 100 missing from snapshot")
	}
	if st.calls != 15 {
		t.Errorf("calls should sum to 15; got %d", st.calls)
	}
	if st.totalMs != 300 {
		t.Errorf("total_ms should sum to 300; got %v", st.totalMs)
	}
	if st.meanMs() != 20 {
		t.Errorf("mean should be 300/15=20; got %v", st.meanMs())
	}
	if snap.shards != 2 {
		t.Errorf("snapshot should record 2 shards; got %d", snap.shards)
	}
}

func TestBuildWorkloadSnapshotSkipsErrShards(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Err: errors.New("connection refused")},
		{Shard: cluster.Shard{Label: "rs002"}, Result: &db.Result{Rows: [][]any{wlRow("7", 3, "30", "select 2")}, IsSelect: true}},
	}
	snap := buildWorkloadSnapshot(results)
	if len(snap.stats) != 1 || snap.shards != 1 {
		t.Errorf("error shard must be skipped; stats=%d shards=%d", len(snap.stats), snap.shards)
	}
}

func TestDiffWorkloadRegression(t *testing.T) {
	before := buildWorkloadSnapshot(wlResults(
		wlRow("100", 10, "100", "select 1"), // mean 10
		wlRow("200", 10, "50", "select 2"),  // mean 5
	))
	after := buildWorkloadSnapshot(wlResults(
		wlRow("100", 20, "400", "select 1"), // mean 20 → регрессия 2x, +300ms
		wlRow("200", 10, "50", "select 2"),  // без изменений → опускается
		wlRow("300", 5, "25", "select 3"),   // новый
	))
	deltas := diffWorkload(before, after)
	if len(deltas) != 2 {
		t.Fatalf("expected 2 changed queryids (100 regressed, 300 new); got %d: %+v", len(deltas), deltas)
	}
	// Отсортировано по приросту суммарного времени: 100 (+300) раньше 300 (+25).
	if deltas[0].queryid != "100" {
		t.Errorf("biggest total-time delta should be queryid 100; got %s", deltas[0].queryid)
	}
	if deltas[0].callsDelta != 10 || deltas[0].totalDelta != 300 {
		t.Errorf("queryid 100 delta wrong: calls=%d total=%v", deltas[0].callsDelta, deltas[0].totalDelta)
	}
	if r := deltas[0].meanRatio(); r != 2 {
		t.Errorf("queryid 100 mean ratio should be 2.0 (20/10); got %v", r)
	}
	if !deltas[1].isNew || deltas[1].queryid != "300" {
		t.Errorf("queryid 300 should be flagged new; got %+v", deltas[1])
	}
	if deltas[1].meanRatio() != 0 {
		t.Errorf("new queryid has no before-mean ratio; got %v", deltas[1].meanRatio())
	}
}

func TestDiffWorkloadNoChange(t *testing.T) {
	snap := buildWorkloadSnapshot(wlResults(wlRow("100", 10, "100", "select 1")))
	if d := diffWorkload(snap, snap); len(d) != 0 {
		t.Errorf("identical snapshots must show no change; got %+v", d)
	}
}
