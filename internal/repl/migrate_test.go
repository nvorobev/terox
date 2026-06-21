package repl

import "testing"

func TestParseFileArgsDefaultDryRun(t *testing.T) {
	// По умолчанию миграция в режиме dry-run (безопасность): без флага только превью.
	o, err := parseFileArgs([]string{"m.sql"})
	if err != nil || o.path != "m.sql" || !o.dryRun {
		t.Errorf("default must be dry-run; path=%q dry=%v err=%v", o.path, o.dryRun, err)
	}
	// --allowed реально применяет миграцию.
	if o, _ := parseFileArgs([]string{"--allowed", "m.sql"}); o.dryRun {
		t.Error("--allowed must disable dry-run")
	}
	// --dry-run явно задаёт режим dry-run.
	if o, _ := parseFileArgs([]string{"--dry-run", "m.sql"}); !o.dryRun {
		t.Error("--dry-run must be dry-run")
	}
	// Отсутствующий файл — ошибка независимо от флагов.
	if _, err := parseFileArgs([]string{"--allowed"}); err == nil {
		t.Error("missing file must error")
	}
}

func TestParseFileArgsRolloutFlags(t *testing.T) {
	o, err := parseFileArgs([]string{"--allowed", "--resume", "--canary", "--batch", "3", "m.sql"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !o.resume || !o.canary || o.batch != 3 || o.dryRun {
		t.Errorf("rollout flags not parsed: %+v", o)
	}
	if !o.staged() {
		t.Error("resume/canary/batch should mark staged()")
	}
	// --force и негативный батч.
	if o, _ := parseFileArgs([]string{"--force", "m.sql"}); !o.force {
		t.Error("--force not parsed")
	}
	if _, err := parseFileArgs([]string{"--batch", "0", "m.sql"}); err == nil {
		t.Error("--batch 0 must error")
	}
	if _, err := parseFileArgs([]string{"--batch"}); err == nil {
		t.Error("--batch without value must error")
	}
}
