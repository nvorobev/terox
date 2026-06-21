package repl

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseFileArgsCheckFlag(t *testing.T) {
	o, err := parseFileArgs([]string{"--check", "m.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.check || o.path != "m.sql" {
		t.Errorf("--check not parsed: %+v", o)
	}
}

func TestMigrateCheckLintsFile(t *testing.T) {
	path := writeTempSQL(t, "CREATE INDEX i ON items (a);\nDROP TABLE old_items;")
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	o, err := parseFileArgs([]string{"--check", path})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.runMigrationFile(true, o); err != nil {
		t.Fatalf("\\migrate --check should not error offline: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CREATE INDEX without CONCURRENTLY") {
		t.Errorf("expected CREATE INDEX lint, got:\n%s", out)
	}
	if !strings.Contains(out, "DROP without IF EXISTS") {
		t.Errorf("expected DROP lint, got:\n%s", out)
	}
}

func TestMigrateCheckCleanFile(t *testing.T) {
	path := writeTempSQL(t, "UPDATE items SET x = 1 WHERE id = 1;")
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	o, _ := parseFileArgs([]string{"--check", path})
	if err := r.runMigrationFile(true, o); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "no risky online-migration patterns") {
		t.Errorf("clean migration should report no findings, got:\n%s", out)
	}
}
