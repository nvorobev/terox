package repl

import (
	"errors"
	"testing"
	"time"

	"terox/internal/cluster"
	"terox/internal/db"
)

func lagResult(label string, v any, err error) db.ShardResult {
	sr := db.ShardResult{Shard: cluster.Shard{Label: label}, Err: err}
	if err == nil {
		sr.Result = &db.Result{Rows: [][]any{{v}}, IsSelect: true}
	}
	return sr
}

func TestMaxReplayLagSecondsTakesMax(t *testing.T) {
	results := []db.ShardResult{
		lagResult("rs001", "1.5", nil),
		lagResult("rs002", "12.25", nil),
		lagResult("rs003", "0", nil),
	}
	lag, ok := maxReplayLagSeconds(results)
	if !ok {
		t.Fatal("expected ok with numeric values")
	}
	if lag != 12.25 {
		t.Errorf("max lag should be 12.25; got %v", lag)
	}
}

func TestMaxReplayLagSecondsNoData(t *testing.T) {
	// Все шарды с ошибкой → ok=false → lag-gating не срабатывает (консервативно).
	results := []db.ShardResult{
		lagResult("rs001", nil, errors.New("permission denied")),
	}
	if _, ok := maxReplayLagSeconds(results); ok {
		t.Error("error-only shards must yield ok=false")
	}
}

func TestMaxReplayLagSecondsSkipsUnparseable(t *testing.T) {
	results := []db.ShardResult{
		lagResult("rs001", "n/a", nil), // не число — пропускается
		lagResult("rs002", "3.0", nil),
	}
	lag, ok := maxReplayLagSeconds(results)
	if !ok || lag != 3.0 {
		t.Errorf("should skip unparseable and take 3.0; got lag=%v ok=%v", lag, ok)
	}
}

func TestParseFileArgsMaxLag(t *testing.T) {
	o, err := parseFileArgs([]string{"--max-lag", "30s", "m.sql"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if o.maxLag != 30*time.Second {
		t.Errorf("maxLag should be 30s; got %v", o.maxLag)
	}
	if o.path != "m.sql" {
		t.Errorf("path should be m.sql; got %q", o.path)
	}
}

func TestParseFileArgsMaxLagInvalid(t *testing.T) {
	if _, err := parseFileArgs([]string{"--max-lag", "soon", "m.sql"}); err == nil {
		t.Error("invalid duration must error")
	}
	if _, err := parseFileArgs([]string{"--max-lag"}); err == nil {
		t.Error("missing duration must error")
	}
}
