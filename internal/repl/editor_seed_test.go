package repl

import (
	"strings"
	"testing"
)

// TestEditInEditorSeeds: editInEditor пред-заполняет временный файл seed'ом и
// возвращает итог. Подменяем редактор на no-op `true` — он оставляет файл как есть,
// поэтому возвращается ровно seed (проверяет round-trip пред-заполнения).
func TestEditInEditorSeeds(t *testing.T) {
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "true")
	r := &REPL{}
	got, err := r.editInEditor("select * from big_table where id = 1;")
	if err != nil {
		t.Fatalf("editInEditor: %v", err)
	}
	if strings.TrimSpace(got) != "select * from big_table where id = 1;" {
		t.Errorf("seed not round-tripped, got %q", got)
	}
}

// TestEditInEditorEmptySeed: пустой seed не пишет ничего и возвращает пустую строку.
func TestEditInEditorEmptySeed(t *testing.T) {
	t.Setenv("VISUAL", "true")
	t.Setenv("EDITOR", "true")
	r := &REPL{}
	got, err := r.editInEditor("")
	if err != nil {
		t.Fatalf("editInEditor: %v", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Errorf("empty seed should yield empty content, got %q", got)
	}
}
