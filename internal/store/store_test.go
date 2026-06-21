package store

import "testing"

func TestQueriesPersist(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	q, err := LoadQueries()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Set("a", "select 1"); err != nil {
		t.Fatal(err)
	}

	// Перечитываем с диска и проверяем сохранность.
	q2, err := LoadQueries()
	if err != nil {
		t.Fatal(err)
	}
	if sql, ok := q2.Get("a"); !ok || sql != "select 1" {
		t.Errorf("got %q,%v want select 1", sql, ok)
	}
	if err := q2.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := q2.Get("a"); ok {
		t.Error("expected deletion")
	}
}

func TestAppliedLedger(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	a, err := LoadApplied()
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Record("item/prod", "001.sql", "shard_0", "2026-06-20")
	_ = a.Record("item/prod", "001.sql", "shard_1", "2026-06-20")

	a2, err := LoadApplied()
	if err != nil {
		t.Fatal(err)
	}
	shards := a2.Shards("item/prod", "001.sql")
	if len(shards) != 2 {
		t.Errorf("want 2 shards, got %d", len(shards))
	}
	if migs := a2.Migrations("item/prod"); len(migs) != 1 || migs[0] != "001.sql" {
		t.Errorf("unexpected migrations: %v", migs)
	}
}

func TestAppliedChecksums(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, err := LoadApplied()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Checksum("item/prod", "001.sql"); ok {
		t.Error("no checksum expected initially")
	}
	if err := a.RecordChecksum("item/prod", "001.sql", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	// Переживает перезагрузку (отдельный checksums.json, applied.json не затронут).
	a2, err := LoadApplied()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := a2.Checksum("item/prod", "001.sql")
	if !ok || got != "deadbeef" {
		t.Errorf("checksum reload = %q,%v; want deadbeef,true", got, ok)
	}
	// Record (метка времени) не портит чек-суммы и наоборот.
	if err := a2.Record("item/prod", "001.sql", "rs001", "ts"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a2.Checksum("item/prod", "001.sql"); got != "deadbeef" {
		t.Errorf("checksum clobbered by Record: %q", got)
	}
}
