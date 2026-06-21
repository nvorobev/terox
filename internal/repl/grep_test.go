package repl

import (
	"bytes"
	"strings"
	"testing"
)

func grepREPL(buf *bytes.Buffer) *REPL {
	return &REPL{out: buf, lastCols: []string{"id", "status"}, lastRows: [][]any{
		{int64(1), "ok"},
		{int64(2), "FAILED"},
		{int64(3), "ok"},
	}}
}

func TestDoGrepSubstring(t *testing.T) {
	var buf bytes.Buffer
	r := grepREPL(&buf)
	if err := r.doGrep([]string{"fail"}); err != nil { // регистронезависимо
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "1 of 3 rows match") {
		t.Errorf("expected footer '1 of 3 rows match', got:\n%s", out)
	}
}

func TestDoGrepInvert(t *testing.T) {
	var buf bytes.Buffer
	r := grepREPL(&buf)
	if err := r.doGrep([]string{"-v", "ok"}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "1 of 3 rows match") {
		t.Errorf("-v ok should keep the 1 non-matching row, got:\n%s", out)
	}
}

func TestDoGrepNoResult(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	if err := r.doGrep([]string{"x"}); err == nil || !strings.Contains(err.Error(), "nothing to filter") {
		t.Errorf("grep without a prior result must error, got %v", err)
	}
}

func TestDoGrepUsage(t *testing.T) {
	var buf bytes.Buffer
	r := grepREPL(&buf)
	if err := r.doGrep(nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("grep without a pattern must show usage, got %v", err)
	}
}
