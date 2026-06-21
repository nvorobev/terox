package repl

import (
	"bytes"
	"strings"
	"testing"
)

// TestWatchRejectsStatementsSubcommands: \watch \statements snapshot|diff ведут
// baseline (r.lastWorkload) и, зацикленные, затирали бы его на каждом тике. Под
// \watch допускается только голый \statements; snapshot/diff отвергаются с ошибкой
// ДО входа в цикл (никакого DB-доступа).
func TestWatchRejectsStatementsSubcommands(t *testing.T) {
	cases := []struct {
		args []string
		raw  string
	}{
		{[]string{"\\statements", "snapshot"}, "\\watch \\statements snapshot"},
		{[]string{"\\statements", "diff"}, "\\watch \\statements diff"},
		{[]string{"\\workload", "snapshot"}, "\\watch \\workload snapshot"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		r := &REPL{out: &buf}
		err := r.doWatch(c.args, c.raw)
		if err == nil {
			t.Errorf("doWatch(%v) should reject snapshot/diff subcommand", c.args)
			continue
		}
		if !strings.Contains(err.Error(), "baseline") {
			t.Errorf("doWatch(%v) error should mention baseline clobbering, got %v", c.args, err)
		}
	}
}

// TestWatchUnwatchableMetaStillRejected: прочие meta-команды по-прежнему
// отвергаются общим сообщением; наша новая проверка snapshot/diff не подменяет его.
func TestWatchUnwatchableMetaStillRejected(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	err := r.doWatch([]string{"\\describe", "t"}, "\\watch \\describe t")
	if err == nil || !strings.Contains(err.Error(), "diagnostic command") {
		t.Fatalf("non-watchable meta should be rejected with the general message, got %v", err)
	}
}
