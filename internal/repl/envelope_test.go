package repl

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"terox/internal/cluster"
	"terox/internal/db"
	"terox/internal/export"
	"terox/internal/render"
)

func TestBuildEnvelope(t *testing.T) {
	results := []db.ShardResult{
		{
			Shard:  cluster.Shard{Position: 0, Label: "rs001"},
			Result: &db.Result{Columns: []string{"id", "id"}, Rows: [][]any{{int64(1), int64(2)}}, IsSelect: true},
		},
		{
			Shard: cluster.Shard{Position: 1, Label: "rs002"},
			Err:   &pgconn.PgError{Code: "42501", Message: "permission denied", Severity: "ERROR"},
		},
	}
	cols, rows := render.Merge(results)
	env := buildEnvelope("svc/sto/all", results, results, cols, rows)

	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
	}
	if env.Target != "svc/sto/all" {
		t.Errorf("target = %q", env.Target)
	}
	if env.Shards.Total != 2 || env.Shards.OK != 1 || env.Shards.Failed != 1 {
		t.Errorf("shards = %+v", env.Shards)
	}
	if len(env.Errors) != 1 || env.Errors[0].SQLState != "42501" || env.Errors[0].Kind != "server" {
		t.Errorf("errors = %+v", env.Errors)
	}
	// Дубли имён колонок сохраняются (shard + id + id).
	if len(env.Columns) != 3 || env.Columns[1] != "id" || env.Columns[2] != "id" {
		t.Errorf("columns = %v, want [shard id id]", env.Columns)
	}
	if env.RowCount != 1 || len(env.Rows) != 1 || len(env.Rows[0]) != 3 {
		t.Errorf("rows = %v (count %d)", env.Rows, env.RowCount)
	}
}

func TestBuildEnvelopeMetaCoversAllShards(t *testing.T) {
	// first-success/quorum схлопывают ВЫВОД до одного представителя, но envelope обязан
	// показывать весь веер: упавший шард должен попасть в shards.failed и errors.
	rep := db.ShardResult{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Columns: []string{"n"}, Rows: [][]any{{int64(150)}}, IsSelect: true}}
	all := []db.ShardResult{
		rep,
		{Shard: cluster.Shard{Label: "rs002"}, Err: &pgconn.PgError{Code: "08006", Message: "connection failure", Severity: "FATAL"}},
	}
	cols, rows := render.Merge([]db.ShardResult{rep}) // вывод — только представитель
	env := buildEnvelope("svc/sto/all", []db.ShardResult{rep}, all, cols, rows)
	if env.Shards.Total != 2 || env.Shards.OK != 1 || env.Shards.Failed != 1 {
		t.Errorf("envelope must report the full fan-out, got shards=%+v", env.Shards)
	}
	if len(env.Errors) != 1 || env.Errors[0].SQLState != "08006" {
		t.Errorf("failed shard must appear in errors, got %+v", env.Errors)
	}
	if env.RowCount != 1 { // строки берутся от представителя
		t.Errorf("rows should come from the representative, got %d", env.RowCount)
	}
}

func TestBuildEnvelopeTypedSchema(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Columns: []string{"id", "name"}, ColTypes: []uint32{23, 25}, ColMods: []int32{-1, -1}, Rows: [][]any{{int64(1), "a"}}, IsSelect: true}},
	}
	cols, rows := render.Merge(results)
	env := buildEnvelope("svc/sto/all", results, results, cols, rows)
	if env.SchemaCheck != "complete" {
		t.Errorf("schema_check should be complete, got %q", env.SchemaCheck)
	}
	if len(env.Schema) != len(cols) {
		t.Fatalf("schema should be parallel to columns (%d), got %d", len(cols), len(env.Schema))
	}
	// Найдём типизированную колонку id.
	var idCol *export.ColumnJSON
	for i := range env.Schema {
		if env.Schema[i].Name == "id" {
			idCol = &env.Schema[i]
		}
	}
	if idCol == nil || idCol.TypeName != "int4" {
		t.Errorf("id column should be typed int4, got %+v", idCol)
	}
}

func TestBuildEnvelopeTypeDrift(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Columns: []string{"id"}, ColTypes: []uint32{23}, Rows: [][]any{{int64(1)}}, IsSelect: true}},
		{Shard: cluster.Shard{Label: "rs002"}, Result: &db.Result{Columns: []string{"id"}, ColTypes: []uint32{20}, Rows: [][]any{{int64(2)}}, IsSelect: true}},
	}
	cols, rows := render.Merge(results)
	env := buildEnvelope("svc/sto/all", results, results, cols, rows)
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "differing types") {
		t.Errorf("envelope should carry a type-drift warning, got %v", env.Warnings)
	}
}

func TestWriteEnvelopeStableSchema(t *testing.T) {
	// Пустой успешный конверт: columns/rows/errors всегда присутствуют (не null).
	var buf bytes.Buffer
	if err := export.WriteEnvelope(&buf, export.Envelope{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	for _, k := range []string{"schema_version", "columns", "rows", "row_count", "shards", "errors", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("envelope missing stable key %q:\n%s", k, buf.String())
		}
	}
	// columns/rows/errors должны быть массивами, а не null.
	for _, k := range []string{"columns", "rows", "errors"} {
		if _, ok := m[k].([]any); !ok {
			t.Errorf("key %q should be an array, got %T", k, m[k])
		}
	}
}

func TestWriteEnvelopeCarriesSQLState(t *testing.T) {
	env := export.Envelope{
		SchemaVersion: 1,
		Columns:       []string{"shard", "id"},
		Rows:          [][]any{{"rs001", int64(1)}},
		Errors:        []export.ShardError{{Shard: "rs002", SQLState: "23505", Message: "dup", Kind: "server"}},
	}
	var buf bytes.Buffer
	if err := export.WriteEnvelope(&buf, env); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "23505") || !strings.Contains(out, `"sqlstate"`) {
		t.Errorf("envelope must expose SQLSTATE:\n%s", out)
	}
}
