package repl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
)

func TestQueryWriteStillRejected(t *testing.T) {
	// Запись отклоняется read-only ещё до подключения к БД (наличие $N в тексте
	// ничего не меняет — headless query не биндит параметры).
	var buf bytes.Buffer
	err := Query(&config.Config{}, "svc/st", "delete from t where id = $1", QueryOptions{}, &buf)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("write must be rejected read-only, got %v", err)
	}
}

func TestQueryRejectsUnknownMode(t *testing.T) {
	var buf bytes.Buffer
	err := Query(&config.Config{}, "svc/st", "select 1", QueryOptions{Mode: "bogus"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "unknown --mode") {
		t.Fatalf("expected unknown-mode error, got %v", err)
	}
}

func TestQueryPerShardRejectsCSV(t *testing.T) {
	var buf bytes.Buffer
	err := Query(&config.Config{}, "svc/st", "select 1", QueryOptions{Format: "csv", Mode: "per-shard"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "per-shard") {
		t.Fatalf("expected per-shard+csv rejection, got %v", err)
	}
}

func TestFirstSuccessResult(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Position: 0, Label: "rs001"}, Err: &pgconn.PgError{Code: "42501", Message: "denied"}},
		{Shard: cluster.Shard{Position: 1, Label: "rs002"}, Result: &db.Result{Columns: []string{"id"}, Rows: [][]any{{int64(7)}}, IsSelect: true}},
	}
	fr := firstSuccessResult(results)
	if fr == nil || fr.Shard.Label != "rs002" {
		t.Fatalf("first success = %v, want rs002", fr)
	}
	// Все упали -> nil.
	if firstSuccessResult([]db.ShardResult{{Err: &pgconn.PgError{Code: "x"}}}) != nil {
		t.Error("all-failed should return nil")
	}
}

// TestContextSnapshotRestoresWriteMode: snapshot/restore контекста должен
// возвращать и writeMode — bindStorage сбрасывает его при смене storage, поэтому
// отмена \use/\connect не должна молча оставлять запись выключенной.
func TestContextSnapshotRestoresWriteMode(t *testing.T) {
	r := &REPL{service: "svc", storage: "st", writeMode: true}
	snap := r.snapshotContext()
	// Эмулируем то, что делает bindStorage при смене контекста.
	r.writeMode = false
	r.storage = "other"
	r.restoreContext(snap)
	if !r.writeMode {
		t.Error("restoreContext must restore writeMode=true after a cancelled switch")
	}
	if r.storage != "st" {
		t.Errorf("restoreContext must restore storage, got %q", r.storage)
	}
}

func TestBuildPerShard(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Position: 0, Label: "rs001"}, Result: &db.Result{Columns: []string{"id"}, Rows: [][]any{{int64(1)}}, IsSelect: true}},
		{Shard: cluster.Shard{Position: 1, Label: "rs002"}, Err: &pgconn.PgError{Code: "42501", Message: "permission denied"}},
	}
	env := buildPerShard("svc/sto/all", results)
	if env.SchemaVersion != 1 || env.Mode != "per-shard" || env.Target != "svc/sto/all" {
		t.Errorf("envelope meta wrong: %+v", env)
	}
	if env.Shards.Total != 2 || env.Shards.OK != 1 || env.Shards.Failed != 1 {
		t.Errorf("shard summary = %+v", env.Shards)
	}
	if len(env.Results) != 2 {
		t.Fatalf("results len = %d", len(env.Results))
	}
	if env.Results[0].Error != nil || len(env.Results[0].Rows) != 1 {
		t.Errorf("ok shard malformed: %+v", env.Results[0])
	}
	if env.Results[1].Error == nil || env.Results[1].Error.SQLState != "42501" {
		t.Errorf("error shard malformed: %+v", env.Results[1])
	}
}
