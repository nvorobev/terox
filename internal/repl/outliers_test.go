package repl

import (
	"bytes"
	"strings"
	"testing"

	"terox/internal/explain"
)

// TestReportTimeOutliersTwoShards проверяет: при двух шардах шард, который в 100 раз
// медленнее, помечается как выброс (сравнение идёт с более быстрым шардом).
func TestReportTimeOutliersTwoShards(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	plans := []shardPlan{
		{label: "s0", root: &explain.Root{ExecutionTime: 1}},
		{label: "s1", root: &explain.Root{ExecutionTime: 100}},
	}
	r.reportTimeOutliers(plans)
	out := buf.String()
	if !strings.Contains(out, "outliers") || !strings.Contains(out, "s1") {
		t.Errorf("100ms vs 1ms across two shards should flag s1 as an outlier; got:\n%s", out)
	}
	// Два близких по времени шарда не помечаются как выброс.
	buf.Reset()
	r.reportTimeOutliers([]shardPlan{
		{label: "s0", root: &explain.Root{ExecutionTime: 50}},
		{label: "s1", root: &explain.Root{ExecutionTime: 55}},
	})
	if strings.Contains(buf.String(), "outliers") {
		t.Errorf("50ms vs 55ms must not be flagged; got:\n%s", buf.String())
	}
}
