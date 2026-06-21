package repl

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestEditorEnterContinuesIncomplete: Enter на незавершённом операторе вставляет
// перенос строки и продолжает редактирование; завершение ";" отправляет весь буфер.
func TestEditorEnterContinuesIncomplete(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "select a,")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.done {
		t.Fatal("Enter on an incomplete statement must not submit")
	}
	if !strings.Contains(m.line(), "\n") {
		t.Errorf("Enter should insert a newline; got %q", m.line())
	}
	typeStr(m, "b from t;")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.done || m.submitted != "select a,\nb from t;" {
		t.Errorf("completing with ; should submit the whole buffer; done=%v sub=%q", m.done, m.submitted)
	}
}

// TestEditorEnterSubmitsMeta: мета-команда (без ";") отправляется по Enter.
func TestEditorEnterSubmitsMeta(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "\\dt")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.done || m.submitted != "\\dt" {
		t.Errorf("meta-command should submit on Enter; done=%v sub=%q", m.done, m.submitted)
	}
}

// TestEditorVerticalNavigation: ↑/↓ ходят по строкам буфера, сохраняя колонку (с
// клампом к длине строки).
func TestEditorVerticalNavigation(t *testing.T) {
	m := newTestEditor()
	typeStr(m, "aaa")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // незавершённый → перенос
	typeStr(m, "bbbbb")                      // input "aaa\nbbbbb", курсор row1 col5
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if row, col := m.cursorRowCol(); row != 0 || col != 3 {
		t.Errorf("Up should clamp to row0 col3, got row=%d col=%d", row, col)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if row, _ := m.cursorRowCol(); row != 1 {
		t.Errorf("Down should return to row1, got row=%d", row)
	}
}

// TestEditorUpAtTopGoesToHistory: на первой строке ↑ уходит в историю.
func TestEditorUpAtTopGoesToHistory(t *testing.T) {
	m := newTestEditor()
	m.hist = []string{"prev query"}
	m.histIdx = len(m.hist)
	typeStr(m, "abc")
	press(m, tea.KeyEsc) // на случай открытого меню
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.line() != "prev query" {
		t.Errorf("Up on the first line should recall history; got %q", m.line())
	}
}

func TestInputComplete(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"\\dt", true},
		{"select 1;", true},
		{"select 1; ", true},
		{"select 1", false},
		{"select ';'", false}, // ; внутри литерала не завершает
	}
	for _, c := range cases {
		m := newTestEditor()
		typeStr(m, c.in)
		if got := m.inputComplete(); got != c.want {
			t.Errorf("inputComplete(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
