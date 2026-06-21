package repl

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"terox/internal/config"
)

// TestRecordHistorySkipsSecrets: утверждение с учёткой не попадает ни в память
// (tea), ни в файл истории.
func TestRecordHistorySkipsSecrets(t *testing.T) {
	dir := t.TempDir()
	hp := filepath.Join(dir, "hist")
	r := &REPL{out: io.Discard, cfg: &config.Config{}, histPath: hp, useTeaEditor: true}

	secret := "ALTER ROLE app WITH PASSWORD 'hunter2';"
	r.recordHistory(secret)
	for _, h := range r.history {
		if h == secret {
			t.Fatalf("secret leaked into in-memory history: %q", r.history)
		}
	}
	if data, err := os.ReadFile(hp); err == nil && len(data) > 0 {
		t.Fatalf("secret leaked into disk history: %q", data)
	}

	// Обычное утверждение в память записывается.
	ok := "select * from users where id = 5;"
	r.recordHistory(ok)
	if len(r.history) != 1 || r.history[0] != ok {
		t.Fatalf("expected the safe statement recorded once, got %v", r.history)
	}
}

// TestRecordHistoryMemoryGuardedByEditor: режим readline не наполняет память (чтобы
// последующий \editor tea лениво подгрузил полную историю с диска), а tea — наполняет.
func TestRecordHistoryMemoryGuardedByEditor(t *testing.T) {
	r := &REPL{out: io.Discard, cfg: &config.Config{}, useTeaEditor: false}
	r.recordHistory("select 1;")
	if r.history != nil {
		t.Errorf("readline mode must not populate in-memory history, got %v", r.history)
	}
	r.useTeaEditor = true
	r.recordHistory("select 2;")
	if len(r.history) != 1 || r.history[0] != "select 2;" {
		t.Errorf("tea mode should record in memory, got %v", r.history)
	}
}

// TestHistoryOffStopsRecording: \history off полностью отключает запись.
func TestHistoryOffStopsRecording(t *testing.T) {
	r := &REPL{out: io.Discard, cfg: &config.Config{}, useTeaEditor: true}
	r.runHistory([]string{"off"})
	r.recordHistory("select 1;")
	if len(r.history) != 0 {
		t.Errorf("history off must suppress recording, got %v", r.history)
	}
	r.runHistory([]string{"on"})
	r.recordHistory("select 2;")
	if len(r.history) != 1 {
		t.Errorf("history on must resume recording, got %v", r.history)
	}
}

// TestClearHistoryWipesMemoryAndDisk: \history clear очищает память и обрезает
// файл на диске.
func TestClearHistoryWipesMemoryAndDisk(t *testing.T) {
	dir := t.TempDir()
	hp := filepath.Join(dir, "hist")
	if err := os.WriteFile(hp, []byte("select 1;\nselect 2;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &REPL{out: io.Discard, cfg: &config.Config{}, histPath: hp, useTeaEditor: true,
		history: []string{"select 1;", "select 2;"}}
	r.runHistory([]string{"clear"})
	if len(r.history) != 0 {
		t.Errorf("clear must empty in-memory history, got %v", r.history)
	}
	if data, _ := os.ReadFile(hp); len(data) != 0 {
		t.Errorf("clear must truncate the disk history file, got %q", data)
	}
}
