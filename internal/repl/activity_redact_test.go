package repl

import (
	"strings"
	"testing"

	"terox/internal/cluster"
	"terox/internal/db"
)

func TestMaskQueryColumn(t *testing.T) {
	results := []db.ShardResult{
		{Shard: cluster.Shard{Label: "a"}, Result: &db.Result{
			Columns:  []string{"pid", "query"},
			Rows:     [][]any{{int64(1), "update users set pw = 'secret123' where id = 42"}},
			IsSelect: true,
		}},
	}
	maskQueryColumn(results, 1)
	got, _ := results[0].Result.Rows[0][1].(string)
	if strings.Contains(got, "secret123") {
		t.Errorf("string literal must be masked, got %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "update users set pw") {
		t.Errorf("non-literal SQL should be preserved, got %q", got)
	}
}

func TestHasRaw(t *testing.T) {
	if !hasRaw([]string{"--raw"}) || hasRaw([]string{"--all"}) {
		t.Error("hasRaw should detect --raw only")
	}
}

func TestMaskQueryTextNumbers(t *testing.T) {
	// Маскированный вывод (по умолчанию для \activity и т.п.) не должен светить ни
	// строковые, ни числовые литералы — числа тоже бывают PII (ID, телефоны, суммы).
	got := maskQueryText("update t set bal = 5000 where ssn = 123456789 and note = 'x'")
	for _, leak := range []string{"5000", "123456789", "x"} {
		if strings.Contains(got, leak) {
			t.Errorf("masked output must not leak %q, got %q", leak, got)
		}
	}
	// Цифра, примыкающая к идентификатору (col1), частью числа не считается.
	if !strings.Contains(maskQueryText("select col1 from t"), "col1") {
		t.Errorf("identifier col1 must be preserved, got %q", maskQueryText("select col1 from t"))
	}
}
