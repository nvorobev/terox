package advisor

import (
	"strings"
	"testing"
)

func TestFilterColumns(t *testing.T) {
	cases := []struct {
		filter string
		want   []string
	}{
		{"(status = 'new'::text)", []string{"status"}},
		{"(t.email = 'x'::text)", []string{"email"}},
		{"((a > 1) AND (b = 2))", []string{"b", "a"}}, // равенство первым
		{"(id > 100)", []string{"id"}},
		{"((x = 1) AND (x = 2))", []string{"x"}}, // дедуп
		// Левостороннее приведение varchar: PostgreSQL рендерит (col)::text —
		// извлекаем РЕАЛЬНЫЙ столбец, а не имя типа.
		{"((name)::text = 'x'::text)", []string{"name"}},
		{"((nm)::text ~~ 'a%'::text)", []string{"nm"}},
		{"((email)::text = 'a'::text)", []string{"email"}},
		// EXPLAIN VERBOSE квалифицирует столбец: ((rel.col)::text = …) -> col.
		{"((terox_dbg.email)::text = 'u1@x'::text)", []string{"email"}},
		{"(orders.id = 5)", []string{"id"}},
	}
	for _, c := range cases {
		got := filterColumns(c.filter)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("filterColumns(%q) = %v, want %v", c.filter, got, c.want)
		}
	}
}

func TestIndexColumnsSQL(t *testing.T) {
	s := IndexColumnsSQL("public", "users")
	if !strings.Contains(s, "unnest(i.indkey)") || !strings.Contains(s, "'public'") || !strings.Contains(s, "'users'") {
		t.Errorf("IndexColumnsSQL wrong:\n%s", s)
	}
	// Частичные/невалидные индексы и выражения (attnum=0) не должны учитываться.
	if !strings.Contains(s, "indpred IS NULL") || !strings.Contains(s, "indisvalid") || !strings.Contains(s, "indisready") || !strings.Contains(s, "k.attnum <> 0") {
		t.Errorf("must exclude partial/invalid/not-ready/expression indexes:\n%s", s)
	}
	if strings.Contains(IndexColumnsSQL("", "t"), "nspname =") {
		t.Error("no-schema variant must omit nspname filter")
	}
}

func TestParseIndexColumns(t *testing.T) {
	rows := [][]any{
		{"1001", "status", int64(1)},
		{"1001", "created_at", int64(2)},
		{"1002", "id", int64(1)},
	}
	got := ParseIndexColumns(rows)
	if len(got) != 2 || len(got[0]) != 2 || got[0][0] != "status" || got[0][1] != "created_at" || got[1][0] != "id" {
		t.Errorf("ParseIndexColumns wrong: %v", got)
	}
}

func TestDedupeOverlapAndCoverage(t *testing.T) {
	props := []Proposal{
		{Schema: "public", Table: "t", Cols: []string{"a"}, Kind: "filter"},
		{Schema: "public", Table: "t", Cols: []string{"a", "b"}, Kind: "filter"},
		{Schema: "public", Table: "t", Cols: []string{"c"}, Kind: "join"},
	}
	got := DedupeOverlap(props)
	// (a) поглощается (a,b); (c) остаётся.
	if len(got) != 2 {
		t.Fatalf("expected 2 proposals after overlap dedup, got %d: %+v", len(got), got)
	}
	// CoveredByExisting: (a,b) покрыто индексом (a,b,c), (a,d) — нет.
	existing := [][]string{{"a", "b", "c"}}
	if !CoveredByExisting([]string{"a", "b"}, existing) {
		t.Error("(a,b) should be covered by index (a,b,c)")
	}
	if CoveredByExisting([]string{"a", "d"}, existing) {
		t.Error("(a,d) is not a prefix of (a,b,c) — must not be covered")
	}
}

func TestSelectivityNote(t *testing.T) {
	if SelectivityNote(0) != "" {
		t.Error("no stats (0) should give empty note")
	}
	if !strings.Contains(SelectivityNote(-1), "100% distinct") {
		t.Errorf("n_distinct=-1 should read ~100%% distinct, got %q", SelectivityNote(-1))
	}
	if !strings.Contains(SelectivityNote(3), "LOW cardinality") {
		t.Errorf("n_distinct=3 should warn LOW cardinality, got %q", SelectivityNote(3))
	}
	if !strings.Contains(SelectivityNote(5000), "5000 distinct") {
		t.Errorf("n_distinct=5000 should report distinct count, got %q", SelectivityNote(5000))
	}
}

func TestColumnStatsSQL(t *testing.T) {
	s := ColumnStatsSQL("public", "users", "email")
	if !strings.Contains(s, "n_distinct") || !strings.Contains(s, "pg_stats") || !strings.Contains(s, "'email'") {
		t.Errorf("ColumnStatsSQL wrong:\n%s", s)
	}
}

func TestQuoteIdentDDL(t *testing.T) {
	if quoteIdentDDL("users") != "users" {
		t.Error("simple lowercase ident should not be quoted")
	}
	if quoteIdentDDL("MyTable") != `"MyTable"` {
		t.Errorf("mixed-case ident should be quoted, got %s", quoteIdentDDL("MyTable"))
	}
	if quoteIdentDDL(`we"ird`) != `"we""ird"` {
		t.Errorf("embedded quote should be doubled, got %s", quoteIdentDDL(`we"ird`))
	}
}
