package diag

import (
	"strings"
	"testing"
)

// Регрессия P2 (аудит 2026-06-24): правило limit-without-order-by должно считаться
// по top-level оператору и на глубине скобок 0, а не по всему вводу. Иначе ORDER BY
// из подзапроса или соседнего оператора маскирует недетерминированный внешний LIMIT.

func TestLimitWithoutTopLevelOrderByIgnoresNestedOrder(t *testing.T) {
	got := codes(Analyze("select * from (select * from t order by id) s limit 10"))
	if _, ok := got["limit-without-order-by"]; !ok {
		t.Fatalf("nested ORDER BY must not suppress top-level LIMIT warning; got %v", keys(got))
	}
}

func TestLimitWithTopLevelOrderByNoDiagnostic(t *testing.T) {
	got := codes(Analyze("select * from t order by id limit 10"))
	if _, ok := got["limit-without-order-by"]; ok {
		t.Fatalf("top-level ORDER BY present — must NOT warn; got %v", keys(got))
	}
}

func TestLimitWithoutOrderByPerStatement(t *testing.T) {
	sql := "select * from a order by id; select * from b limit 10"
	got := codes(Analyze(sql))
	d, ok := got["limit-without-order-by"]
	if !ok {
		t.Fatalf("ORDER BY in the first statement must not hide LIMIT in the second; got %v", keys(got))
	}
	// Подсветка обязана указывать на LIMIT во ВТОРОМ операторе (после ';').
	semi := strings.IndexByte(sql, ';')
	if d.Start <= semi {
		t.Errorf("diagnostic anchored at %d, want it past the ';' at %d (second statement)", d.Start, semi)
	}
	if got := sql[d.Start:d.End]; !strings.EqualFold(got, "limit") {
		t.Errorf("diagnostic span = %q, want the LIMIT keyword", got)
	}
}

func TestNestedLimitDoesNotPoisonOuterStatement(t *testing.T) {
	// Внешний оператор имеет top-level ORDER BY, а LIMIT спрятан в подзапросе:
	// внешний результат детерминирован, ложного предупреждения быть не должно.
	got := codes(Analyze("select * from (select * from t limit 10) s order by id"))
	if _, ok := got["limit-without-order-by"]; ok {
		t.Fatalf("nested LIMIT under a top-level ORDER BY must not warn; got %v", keys(got))
	}
}
