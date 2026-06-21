package repl

import (
	"io"
	"strings"
	"testing"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
)

func res(cols []string, rows [][]any) *db.Result {
	return &db.Result{Columns: cols, Rows: rows, IsSelect: true}
}

func TestPartialErrorMessage(t *testing.T) {
	if got := (&PartialError{Failed: 1, Total: 3}).Error(); !strings.Contains(got, "1 of 3 shard(s) failed") {
		t.Errorf("failed-only message wrong: %q", got)
	}
	if got := (&PartialError{Truncated: 2, Total: 4, RowCap: 100}).Error(); !strings.Contains(got, "hit the 100-row cap") {
		t.Errorf("truncated-only message wrong: %q", got)
	}
	both := (&PartialError{Failed: 1, Truncated: 1, Total: 3, RowCap: 100}).Error()
	if !strings.Contains(both, "failed") || !strings.Contains(both, "cap") {
		t.Errorf("combined message wrong: %q", both)
	}
}

func TestColumnNameDrift(t *testing.T) {
	same := []db.ShardResult{
		{Shard: cluster.Shard{Label: "a"}, Result: res([]string{"id", "name"}, nil)},
		{Shard: cluster.Shard{Label: "b"}, Result: res([]string{"id", "name"}, nil)},
	}
	if msg := columnNameDrift(same); msg != "" {
		t.Errorf("identical columns should not drift, got %q", msg)
	}
	diff := []db.ShardResult{
		{Shard: cluster.Shard{Label: "a"}, Result: res([]string{"id", "name"}, nil)},
		{Shard: cluster.Shard{Label: "b"}, Result: res([]string{"id", "email"}, nil)},
	}
	if msg := columnNameDrift(diff); msg == "" {
		t.Error("differing column names should be reported")
	}
}

func TestQuorumAgreement(t *testing.T) {
	rows := [][]any{{int64(42)}}
	// 2 из 3 согласны (большинство).
	majority := []db.ShardResult{
		{Shard: cluster.Shard{Label: "a"}, Result: res([]string{"v"}, rows)},
		{Shard: cluster.Shard{Label: "b"}, Result: res([]string{"v"}, rows)},
		{Shard: cluster.Shard{Label: "c"}, Result: res([]string{"v"}, [][]any{{int64(99)}})},
	}
	rep, agree, responding := quorumAgreement(majority)
	if rep == nil || agree != 2 || responding != 3 {
		t.Errorf("expected majority 2/3, got agree=%d responding=%d rep=%v", agree, responding, rep)
	}
	// Полное расхождение: 1-1-1, большинства нет.
	split := []db.ShardResult{
		{Shard: cluster.Shard{Label: "a"}, Result: res([]string{"v"}, [][]any{{int64(1)}})},
		{Shard: cluster.Shard{Label: "b"}, Result: res([]string{"v"}, [][]any{{int64(2)}})},
		{Shard: cluster.Shard{Label: "c"}, Result: res([]string{"v"}, [][]any{{int64(3)}})},
	}
	_, agree2, responding2 := quorumAgreement(split)
	if agree2*2 > responding2 {
		t.Errorf("1-1-1 split must have no majority, got agree=%d/%d", agree2, responding2)
	}
}

func TestQueryMergeSortRequiresOrderBy(t *testing.T) {
	err := Query(&config.Config{}, "svc/sto", "select 1", QueryOptions{Mode: "merge-sort"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires --order-by") {
		t.Errorf("merge-sort without --order-by should error, got %v", err)
	}
}
