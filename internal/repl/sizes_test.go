package repl

import (
	"bytes"
	"strings"
	"testing"
)

func TestSizesSQL(t *testing.T) {
	sql := sizesSQL(15)
	for _, want := range []string{"pg_total_relation_size", "pg_indexes_size", "n_dead_tup", "LIMIT 15"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sizesSQL missing %q:\n%s", want, sql)
		}
	}
	// Литеральный знак процента не должен оставлять %% в готовом SQL.
	if strings.Contains(sql, "%%") {
		t.Errorf("sizesSQL has an unformatted %%%%:\n%s", sql)
	}
}

func TestDoSizesInvalidN(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	if err := r.doSizes([]string{"-3"}); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Errorf("\\sizes -3 must error, got %v", err)
	}
	if err := r.doSizes([]string{"abc"}); err == nil {
		t.Errorf("\\sizes abc must error, got nil")
	}
}
