package repl

import (
	"bytes"
	"strings"
	"testing"
)

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "abd", 1},
		{"activity", "actvity", 1}, // пропущенная буква
		{"explain", "explian", 2},  // перестановка = 2 для обычного Левенштейна
		{"", "abc", 3},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSuggestMeta(t *testing.T) {
	// Опечатка близко к реальной команде -> предлагается она.
	if got := suggestMeta("\\actvity"); !strings.Contains(got, "\\activity") {
		t.Errorf("suggestMeta(\\actvity) = %q, want it to include \\activity", got)
	}
	if got := suggestMeta("\\explian"); !strings.Contains(got, "\\explain") {
		t.Errorf("suggestMeta(\\explian) = %q, want it to include \\explain", got)
	}
	// Совсем непохожее -> без подсказок.
	if got := suggestMeta("\\zzzzzzz"); got != "" {
		t.Errorf("suggestMeta(\\zzzzzzz) = %q, want empty", got)
	}
	// Не больше трёх кандидатов.
	if got := suggestMeta("\\dx"); strings.Count(got, ",") > 2 {
		t.Errorf("suggestMeta should cap at 3 candidates, got %q", got)
	}
}

func TestSearchHelp(t *testing.T) {
	// Поиск по намерению находит связанные команды по подстроке.
	matches := searchHelp("lock")
	found := false
	for _, e := range matches {
		if e.names[0] == "locks" {
			found = true
		}
	}
	if !found {
		t.Errorf("searchHelp(\"lock\") must include \\locks, got %d matches", len(matches))
	}
	// Бессмысленный запрос -> ничего.
	if got := searchHelp("zzzzzz"); len(got) != 0 {
		t.Errorf("searchHelp(nonsense) = %d matches, want 0", len(got))
	}
}

// TestPrintHelpKeywordFallback: \help <keyword> без точного совпадения печатает
// связанные команды, а не глухое "no help".
func TestPrintHelpKeywordFallback(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	r.printHelp([]string{"lock"})
	out := buf.String()
	if !strings.Contains(out, "\\locks") || !strings.Contains(out, "related") {
		t.Errorf("\\help lock should list related commands, got:\n%s", out)
	}
}

func TestDoRepeatNoPreviousQuery(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	if err := r.doRepeat(nil, false); err == nil || !strings.Contains(err.Error(), "no previous query") {
		t.Errorf("doRepeat without a prior query must error, got %v", err)
	}
}

func TestDoRepeatSelectorWithoutStorage(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf, lastQuery: "select 1"}
	if err := r.doRepeat([]string{"0,1"}, false); err == nil || !strings.Contains(err.Error(), "no storage selected") {
		t.Errorf("doRepeat with a selector but no storage must error, got %v", err)
	}
}
