package repl

import (
	"bytes"
	"strings"
	"testing"
)

// TestReportSettingsDiffOnlyOneSide: параметр, присутствующий только на одной
// стороне, раньше молча терялся (\compare показывал лишь общие отличия). Теперь
// он должен выводиться как only in A / only in B по образцу reportMapDiff.
func TestReportSettingsDiffOnlyOneSide(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	a := map[string]string{"work_mem": "4MB", "jit": "on"}
	b := map[string]string{"work_mem": "4MB"} // jit отсутствует на B
	r.reportSettingsDiff(a, b, true)
	out := buf.String()
	if !strings.Contains(out, "jit: only in A") {
		t.Errorf("expected only-in-A line for jit, got:\n%s", out)
	}

	buf.Reset()
	r.reportSettingsDiff(map[string]string{"work_mem": "4MB"},
		map[string]string{"work_mem": "4MB", "jit": "off"}, true)
	out = buf.String()
	if !strings.Contains(out, "jit: only in B") {
		t.Errorf("expected only-in-B line for jit, got:\n%s", out)
	}
}

// TestReportSettingsDiffValueDiff: различающиеся общие значения по-прежнему
// показываются как A x / B y (регрессия на старое поведение).
func TestReportSettingsDiffValueDiff(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	r.reportSettingsDiff(map[string]string{"work_mem": "4MB"},
		map[string]string{"work_mem": "16MB"}, true)
	if !strings.Contains(buf.String(), "work_mem: A 4MB / B 16MB") {
		t.Errorf("expected value diff line, got:\n%s", buf.String())
	}
}

// TestReportSettingsDiffUnavailable: при недоступной секции config (реальная
// ошибка чтения pg_settings) не должно быть "config differences: none" —
// это маскировало бы реальную разницу.
func TestReportSettingsDiffUnavailable(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	r.reportSettingsDiff(map[string]string{}, map[string]string{}, false)
	out := buf.String()
	if strings.Contains(out, "none") {
		t.Errorf("unavailable config must not read as 'none', got:\n%s", out)
	}
	if !strings.Contains(out, "unavailable") {
		t.Errorf("expected an 'unavailable' notice, got:\n%s", out)
	}
}
