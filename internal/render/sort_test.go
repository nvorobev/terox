package render

import "testing"

func rowsEqual(got [][]any, wantFirstCol []any) bool {
	if len(got) != len(wantFirstCol) {
		return false
	}
	for i := range got {
		if got[i][1] != wantFirstCol[i] {
			return false
		}
	}
	return true
}

func TestSortMergedNumeric(t *testing.T) {
	cols := []string{"shard", "id"}
	rows := [][]any{{"rs002", int64(30)}, {"rs001", int64(10)}, {"rs003", int64(20)}}
	if err := SortMerged(cols, rows, "id", false); err != nil {
		t.Fatal(err)
	}
	if !rowsEqual(rows, []any{int64(10), int64(20), int64(30)}) {
		t.Errorf("asc numeric sort wrong: %v", rows)
	}
	if err := SortMerged(cols, rows, "id", true); err != nil {
		t.Fatal(err)
	}
	if !rowsEqual(rows, []any{int64(30), int64(20), int64(10)}) {
		t.Errorf("desc numeric sort wrong: %v", rows)
	}
}

func TestSortMergedStringAndNulls(t *testing.T) {
	cols := []string{"shard", "name"}
	rows := [][]any{{"a", "banana"}, {"b", nil}, {"c", "apple"}}
	if err := SortMerged(cols, rows, "name", false); err != nil {
		t.Fatal(err)
	}
	// apple, banana, затем NULL в конце.
	if rows[0][1] != "apple" || rows[1][1] != "banana" || rows[2][1] != nil {
		t.Errorf("string sort with NULLS LAST wrong: %v", rows)
	}
	// desc тоже держит NULL в конце.
	if err := SortMerged(cols, rows, "name", true); err != nil {
		t.Fatal(err)
	}
	if rows[0][1] != "banana" || rows[1][1] != "apple" || rows[2][1] != nil {
		t.Errorf("desc string sort with NULLS LAST wrong: %v", rows)
	}
}

func TestSortMergedNumericStringsSortNumerically(t *testing.T) {
	// Числа в строковом виде (как из numeric) сортируются численно, не лексически.
	cols := []string{"shard", "n"}
	rows := [][]any{{"a", "100"}, {"b", "9"}, {"c", "20"}}
	if err := SortMerged(cols, rows, "n", false); err != nil {
		t.Fatal(err)
	}
	if rows[0][1] != "9" || rows[1][1] != "20" || rows[2][1] != "100" {
		t.Errorf("numeric-string sort should be 9,20,100 not lexical: %v", rows)
	}
}

func TestSortMergedUnknownColumn(t *testing.T) {
	if err := SortMerged([]string{"shard", "id"}, nil, "nope", false); err == nil {
		t.Error("expected error for unknown column")
	}
}

func TestAggregateSumsNumeric(t *testing.T) {
	// per-shard count() -> глобальная сумма; provenance-колонка отбрасывается.
	cols := []string{"shard", "count"}
	rows := [][]any{{"rs001", int64(3)}, {"rs002", int64(5)}}
	oc, or := Aggregate(cols, rows)
	if len(oc) != 1 || oc[0] != "count" {
		t.Fatalf("cols = %v, want [count]", oc)
	}
	if len(or) != 1 || or[0][0] != int64(8) {
		t.Errorf("sum = %v (%T), want int64(8)", or, or[0][0])
	}
}

func TestAggregateBigIntPrecision(t *testing.T) {
	// Сумма bigint > 2^53 точна (int64-аккумулятор), без потери и научной записи.
	_, or := Aggregate([]string{"shard", "total"}, [][]any{{"a", int64(9007199254740993)}, {"b", int64(1)}})
	if or[0][0] != int64(9007199254740994) {
		t.Errorf("bigint sum = %v (%T), want exact int64 9007199254740994", or[0][0], or[0][0])
	}
}

func TestAggregateRejectsSpecialFloatStrings(t *testing.T) {
	// Текст "NaN" — это строка, не число: не суммируется как numeric.
	_, or := Aggregate([]string{"shard", "label"}, [][]any{{"a", "NaN"}, {"b", "NaN"}})
	if or[0][0] != "NaN" {
		t.Errorf("special-float string aggregated as number: %v (%T)", or[0][0], or[0][0])
	}
}

func TestAggregateZeroRows(t *testing.T) {
	oc, or := Aggregate([]string{"shard", "name", "qty"}, nil)
	if len(oc) != 2 || len(or) != 0 {
		t.Errorf("zero input rows must yield zero rows, got cols=%v rows=%v", oc, or)
	}
}

func TestSortMergedSpecialFloatStringsAreText(t *testing.T) {
	cols := []string{"shard", "city"}
	rows := [][]any{{"a", "Inf"}, {"b", "Aspen"}, {"c", "Boston"}}
	if err := SortMerged(cols, rows, "city", false); err != nil {
		t.Fatal(err)
	}
	if rows[0][1] != "Aspen" || rows[1][1] != "Boston" || rows[2][1] != "Inf" {
		t.Errorf("'Inf' must sort lexically as a string, got %v", rows)
	}
}

func TestAggregateNonNumeric(t *testing.T) {
	cols := []string{"shard", "status"}
	// Совпадающее значение по всем шардам -> оно же.
	_, or := Aggregate(cols, [][]any{{"a", "ok"}, {"b", "ok"}})
	if or[0][0] != "ok" {
		t.Errorf("common value = %v, want ok", or[0][0])
	}
	// Разные значения -> пусто.
	_, or = Aggregate(cols, [][]any{{"a", "ok"}, {"b", "bad"}})
	if or[0][0] != "" {
		t.Errorf("mixed value = %v, want empty", or[0][0])
	}
}
