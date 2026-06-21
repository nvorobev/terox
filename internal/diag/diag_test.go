package diag

import (
	"strings"
	"testing"
)

func codes(ds []Diagnostic) map[string]Diagnostic {
	m := map[string]Diagnostic{}
	for _, d := range ds {
		m[d.Code] = d
	}
	return m
}

func TestAnalyzeRules(t *testing.T) {
	cases := []struct {
		sql     string
		want    []string
		notWant []string
	}{
		{"select * from t limit 10", []string{"select-star", "limit-without-order-by"}, nil},
		{"select id from t order by id limit 10", nil, []string{"limit-without-order-by"}},
		{"delete from t", []string{"unqualified-write"}, nil},
		{"delete from t where id = 1", nil, []string{"unqualified-write"}},
		{"select * from a, b", []string{"cartesian-product", "select-star"}, nil},
		{"select * from a, b where a.id = b.id", nil, []string{"cartesian-product"}},
		{"select id from t where x not in (select y from u)", []string{"not-in-nullable"}, nil},
		{"select 1; select 2", []string{"multi-statement"}, nil},
		// Ключевые слова внутри строкового литерала не должны срабатывать.
		{"select 'delete from everything' as note", nil, []string{"unqualified-write", "cartesian-product"}},
	}
	for _, c := range cases {
		got := codes(Analyze(c.sql))
		for _, w := range c.want {
			if _, ok := got[w]; !ok {
				t.Errorf("Analyze(%q) missing %q; got %v", c.sql, w, keys(got))
			}
		}
		for _, nw := range c.notWant {
			if _, ok := got[nw]; ok {
				t.Errorf("Analyze(%q) should NOT flag %q; got %v", c.sql, nw, keys(got))
			}
		}
	}
}

func TestAnalyzeRangesValid(t *testing.T) {
	sql := "select * from t limit 5"
	for _, d := range Analyze(sql) {
		if d.Start < 0 || d.End > len(sql) || d.Start > d.End {
			t.Errorf("diagnostic %q has invalid range [%d,%d] for len %d", d.Code, d.Start, d.End, len(sql))
		}
	}
}

// TestAnalyzeMultiStatementOffsets: в мульти-операторном вводе диагностики должны
// привязываться к КОНКРЕТНОМУ проблемному оператору, а не к первому совпадению/
// глобальной проверке WHERE.
func TestAnalyzeMultiStatementOffsets(t *testing.T) {
	// Первый оператор безопасен (с WHERE), второй — безусловный DELETE.
	sql := "update a set x=1 where id=1; delete from t"
	var uw *Diagnostic
	for _, d := range Analyze(sql) {
		if d.Code == "unqualified-write" {
			dd := d
			uw = &dd
		}
	}
	if uw == nil {
		t.Fatal("expected unqualified-write for the second statement")
	}
	if got := sql[uw.Start:uw.End]; !strings.EqualFold(got, "delete from") {
		t.Errorf("unqualified-write range = %q, want it to cover the offending DELETE", got)
	}

	// WHERE в первом операторе не должен скрыть декартово произведение во втором.
	got := codes(Analyze("select * from x where x.a = 1; select 1 from a, b"))
	if _, ok := got["cartesian-product"]; !ok {
		t.Errorf("cartesian in 2nd statement hidden by WHERE in 1st; got %v", keys(got))
	}
}

func TestAnalyzeNotInSubqueryConfidence(t *testing.T) {
	d := codes(Analyze("select id from t where x not in (select y from u)"))["not-in-nullable"]
	if d.Confidence != "medium" {
		t.Errorf("NOT IN (subquery) should be medium confidence, got %q", d.Confidence)
	}
}

func TestSeverityString(t *testing.T) {
	if Error.String() != "error" || Warning.String() != "warning" || Info.String() != "info" {
		t.Error("severity strings wrong")
	}
}

func keys(m map[string]Diagnostic) string {
	var k []string
	for c := range m {
		k = append(k, c)
	}
	return strings.Join(k, ",")
}
