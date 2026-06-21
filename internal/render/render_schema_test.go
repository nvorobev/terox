package render

import (
	"testing"

	"terox/internal/cluster"
	"terox/internal/db"
)

func TestMergedSchemaTypesAndSynthetic(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "rs001"}, Result: &db.Result{Columns: []string{"id", "name"}, ColTypes: []uint32{23, 25}, ColMods: []int32{-1, -1}, IsSelect: true}},
		{Shard: cluster.Shard{Label: "rs002"}, Result: &db.Result{Columns: []string{"id", "name"}, ColTypes: []uint32{23, 25}, ColMods: []int32{-1, -1}, IsSelect: true}},
	}
	// Объединённые колонки с ведущей provenance-колонкой "shard".
	schema := MergedSchema(results, []string{"shard", "id", "name"})
	if len(schema) != 3 {
		t.Fatalf("expected 3 columns, got %d", len(schema))
	}
	if !schema[0].Synthetic || schema[0].Name != "shard" {
		t.Errorf("first column should be synthetic provenance 'shard'; got %+v", schema[0])
	}
	if schema[1].TypeName != "int4" || schema[1].Synthetic {
		t.Errorf("id should be int4 and real; got %+v", schema[1])
	}
	if schema[2].TypeName != "text" {
		t.Errorf("name should be text; got %+v", schema[2])
	}
}

func TestSchemaCheckStatus(t *testing.T) {
	complete := []db.Column{{Name: "shard", Synthetic: true}, {Name: "id", DataTypeOID: 23}}
	if s := SchemaCheckStatus(complete); s != "complete" {
		t.Errorf("all real columns typed → complete, got %q", s)
	}
	partial := []db.Column{{Name: "id", DataTypeOID: 23}, {Name: "x", DataTypeOID: 0}}
	if s := SchemaCheckStatus(partial); s != "partial" {
		t.Errorf("some unknown OID → partial, got %q", s)
	}
	skipped := []db.Column{{Name: "shard", Synthetic: true}, {Name: "id", DataTypeOID: 0}}
	if s := SchemaCheckStatus(skipped); s != "skipped" {
		t.Errorf("no real column typed → skipped, got %q", s)
	}
}
