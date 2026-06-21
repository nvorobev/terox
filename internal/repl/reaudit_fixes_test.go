package repl

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"terox/internal/cluster"
	"terox/internal/db"
	"terox/internal/migration"
	"terox/internal/store"
)

// TestChecksumMismatch — enforced checksum-mismatch (F11): миграция с тем же именем,
// но другим содержимым блокируется (если не --force).
func TestChecksumMismatch(t *testing.T) {
	r := &REPL{service: "svc", storage: "sto", applied: &store.Applied{C: map[string]map[string]string{
		"svc/sto": {"001.sql": migration.Checksum("CREATE TABLE t (id int);")},
	}}}
	if !r.checksumMismatch("001.sql", "CREATE TABLE t (id bigint);") {
		t.Error("different content under same name must be a mismatch (blocked without --force)")
	}
	if r.checksumMismatch("001.sql", "CREATE TABLE t (id int);") {
		t.Error("identical content must not be a mismatch")
	}
	if r.checksumMismatch("002.sql", "anything") {
		t.Error("unknown migration must not be a mismatch")
	}
	if (&REPL{}).checksumMismatch("001.sql", "x") {
		t.Error("nil applied must not be a mismatch")
	}
}

// TestShardMetaOf — пер-шардовая provenance в envelope (F13): для успешного шарда
// заполняются server_version/backend_pid/duration, для упавшего — sqlstate.
func TestShardMetaOf(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{ServerVersion: "16.13", BackendPID: 4242, Duration: 7 * time.Millisecond}},
		{Shard: cluster.Shard{Label: "rs002"}, Err: &pgconn.PgError{Code: "42501", Message: "denied"}},
	}
	meta := shardMetaOf(results)
	if len(meta) != 2 {
		t.Fatalf("expected 2 shard-meta entries, got %d", len(meta))
	}
	if !meta[0].OK || meta[0].ServerVersion != "16.13" || meta[0].BackendPID != 4242 || meta[0].DurationMS != 7 {
		t.Errorf("ok shard provenance wrong: %+v", meta[0])
	}
	if meta[1].OK || meta[1].SQLState != "42501" {
		t.Errorf("failed shard should carry sqlstate and not be ok: %+v", meta[1])
	}
}
