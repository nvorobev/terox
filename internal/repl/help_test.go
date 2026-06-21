package repl

import "testing"

func TestHelpRegistry(t *testing.T) {
	// Каждая запись корректно заполнена.
	for _, e := range helpEntries {
		if len(e.names) == 0 || e.syntax == "" || e.summary == "" || e.category == "" {
			t.Errorf("malformed help entry: %+v", e)
		}
	}
	// Псевдонимы разрешаются (с обратным слешем и без, любой регистр).
	for _, q := range []string{"explain", "\\explain", "EXPLAIN", "s", "mig", "?"} {
		if lookupHelp(q) == nil {
			t.Errorf("lookupHelp(%q) = nil", q)
		}
	}
	if lookupHelp("nosuchcommand") != nil {
		t.Error("unknown command should not resolve")
	}
}
