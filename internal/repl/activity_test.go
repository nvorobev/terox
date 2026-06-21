package repl

import (
	"io"
	"strings"
	"testing"

	"terox/internal/config"
)

func TestActivitySQL(t *testing.T) {
	// По умолчанию idle-сессии скрыты.
	def := activitySQL(false)
	if !strings.Contains(def, "state IS DISTINCT FROM 'idle'") {
		t.Errorf("default \\activity must hide idle sessions:\n%s", def)
	}
	if !strings.Contains(def, "pg_backend_pid()") {
		t.Errorf("must exclude own backend")
	}
	// --all показывает всё (без фильтра idle).
	all := activitySQL(true)
	if strings.Contains(all, "state IS DISTINCT FROM 'idle'") {
		t.Errorf("--all must not filter idle:\n%s", all)
	}
}

func TestBlockersAndLocksSQL(t *testing.T) {
	if !strings.Contains(blockersSQL(), "pg_blocking_pids") {
		t.Error("blockers must use pg_blocking_pids")
	}
	if !strings.Contains(locksSQL(), "pg_locks") {
		t.Error("locks must read pg_locks")
	}
}

func TestLongtxSQL(t *testing.T) {
	sql := longtxSQL(30)
	if !strings.Contains(sql, "interval '30 seconds'") {
		t.Errorf("threshold not embedded as interval:\n%s", sql)
	}
	if !strings.Contains(sql, "pg_backend_pid()") {
		t.Error("must exclude own backend")
	}
}

// TestDiagnosticCommandsCompletable гарантирует, что новые диагностические
// команды попадают в tab-дополнение (metaCommands) и в справку (helpEntries) —
// иначе они недоступны для подсказки / \help.
func TestDiagnosticCommandsCompletable(t *testing.T) {
	want := []string{"\\activity", "\\blockers", "\\locks", "\\longtx", "\\statements", "\\workload", "\\cancel", "\\terminate"}
	inMeta := map[string]bool{}
	for _, c := range metaCommands {
		inMeta[c] = true
	}
	inHelp := map[string]bool{}
	for _, e := range helpEntries {
		for _, n := range e.names {
			inHelp["\\"+n] = true
		}
	}
	for _, c := range want {
		if !inMeta[c] {
			t.Errorf("%s missing from metaCommands (tab-completion)", c)
		}
		if !inHelp[c] {
			t.Errorf("%s missing from helpEntries (\\help)", c)
		}
	}
}

func TestSignalBackendGuards(t *testing.T) {
	r := &REPL{out: io.Discard, cfg: &config.Config{}} // targets nil
	// Нет аргумента / лишние аргументы -> usage.
	if err := r.doSignalBackend(nil, false); err == nil {
		t.Error("empty args should error")
	}
	// Невалидный pid.
	if err := r.doSignalBackend([]string{"abc"}, false); err == nil || !strings.Contains(err.Error(), "invalid pid") {
		t.Errorf("non-numeric pid should error with 'invalid pid', got %v", err)
	}
	// Корректный pid, но не выбран ровно один шард -> отказ до обращения к БД.
	if err := r.doSignalBackend([]string{"12345"}, true); err == nil || !strings.Contains(err.Error(), "single backend") {
		t.Errorf("multi/zero shard should refuse before hitting DB, got %v", err)
	}
}
