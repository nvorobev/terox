package sqlsplit

import (
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"single", "SELECT 1", 1},
		{"trailing semi", "SELECT 1;", 1},
		{"two", "SELECT 1; SELECT 2;", 2},
		{"semicolon in string", "SELECT ';'; SELECT 2", 2},
		{"semicolon in comment", "SELECT 1; -- a;b\nSELECT 2", 2},
		{"block comment", "SELECT 1 /* a;b */; SELECT 2", 2},
		{"dollar quoted func", "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql; SELECT 1", 2},
		{"empty statements", ";;SELECT 1;;", 1},
		{"escaped quote", "SELECT 'it''s; ok'; SELECT 2", 2},
		{"e-string backslash quote", `SELECT E'a\';b'; SELECT 2`, 2},
		{"e-string with semicolon", `UPDATE t SET note = E'a\'; b' WHERE id = 1`, 1},
		// При standard_conforming_strings=on обратный слэш в обычном литерале —
		// это просто символ; кавычка закрывает литерал, значит это ДВА оператора.
		{"plain backslash literal", `update t set p='C:\'; vacuum x;`, 2},
		{"plain backslash not escape", `select 'a\'; select 2`, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Split(tc.src)
			if len(got) != tc.want {
				t.Errorf("got %d statements, want %d: %#v", len(got), tc.want, got)
			}
		})
	}
}

func TestSplitNestedBlockComment(t *testing.T) {
	// Блочные комментарии PostgreSQL вложенные; ; внутри полностью закомментирован.
	got := Split("SELECT 1 /* outer /* inner; */ still comment; */ ; SELECT 2")
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d: %#v", len(got), got)
	}
}

func TestSplitDollarDigitTagNotQuote(t *testing.T) {
	// $1$ — это позиционный параметр $1 и затем '$', а не открытие dollar-quote,
	// поэтому следующий ; разделяет операторы.
	got := Split("SELECT $1$ ; SELECT 2")
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d: %#v", len(got), got)
	}
	// Настоящий тег dollar-quote подавляет внутренний ;.
	if g := Split("SELECT $tag$ a;b $tag$ ; SELECT 2"); len(g) != 2 {
		t.Fatalf("want 2 statements, got %d: %#v", len(g), g)
	}
}

func TestSplitNonASCIIDollarTag(t *testing.T) {
	// PostgreSQL допускает не-латинские буквы в теге dollar-quote ($тег$). Лексер
	// должен распознавать такой тег и не разбивать по ; внутри тела.
	if g := Split("SELECT $тег$ a;b;c $тег$ ; SELECT 2"); len(g) != 2 {
		t.Fatalf("non-ASCII dollar tag: want 2 statements, got %d: %#v", len(g), g)
	}
	if g := Split("SELECT $café$ x;y $café$ ; SELECT 2"); len(g) != 2 {
		t.Fatalf("accented dollar tag: want 2 statements, got %d: %#v", len(g), g)
	}
	// Mask нейтрализует тело не-ASCII dollar-quote (внутренний ; не структурный),
	// сохраняя длину.
	in := "SELECT $тег$ a;b $тег$"
	m := Mask(in)
	if len(m) != len(in) {
		t.Fatalf("Mask must preserve byte length: in=%d out=%d", len(in), len(m))
	}
	if strings.Contains(m, ";") {
		t.Errorf("Mask should neutralize ';' inside a non-ASCII dollar body: %q", m)
	}
}

func TestDollarInIdentifierNotQuote(t *testing.T) {
	// '$' — допустимый символ идентификатора, поэтому "A$$" — это идентификатор, а
	// НЕ открытие dollar-quote. Раньше байтовый обход ошибочно считал "$$" началом
	// незакрытой dollar-строки и расходился с лексером completion (P2-3).
	if got := InStringOrComment("A$$"); got {
		t.Errorf("A$$ is an identifier, must not be 'inside a string', got %v", got)
	}
	// Mask не трогает идентификатор (включая '$').
	if got := Mask("A$$"); got != "A$$" {
		t.Errorf("Mask(A$$) = %q, want A$$", got)
	}
	// '$$' после идентификатора не запускает dollar-quote, поэтому ; разделяет.
	if g := Split("func$$body$$ ; select 2"); len(g) != 2 {
		t.Fatalf("func$$body$$ is an identifier: want 2 statements, got %d: %#v", len(g), g)
	}
	// Настоящий dollar-quote (после пробела/в начале) по-прежнему подавляет ;.
	if g := Split("select $$a;b$$ ; select 2"); len(g) != 2 {
		t.Fatalf("real dollar-quote: want 2 statements, got %d: %#v", len(g), g)
	}
	if !InStringOrComment("select $$open") {
		t.Error("a real unterminated dollar-quote should be detected")
	}
}

func TestMask(t *testing.T) {
	sp := func(n int) string { return strings.Repeat(" ", n) }
	cases := []struct{ in, want string }{
		{"a 'b;c' d", "a '" + sp(3) + "' d"},        // нутро литерала затёрто
		{`E'a\'where'`, "E'" + sp(8) + "'"},         // E-строка: \' остаётся внутри
		{"x -- y;z\nq", "x " + sp(6) + "\nq"},       // строчный комментарий до конца строки
		{"a /* b;c */ d", "a " + sp(9) + " d"},      // блочный комментарий
		{"a /* /* n */ */ b", "a " + sp(13) + " b"}, // вложенный блочный комментарий
		{`"my db"`, `"xxxxx"`},                      // идентификатор в кавычках -> заглушка
		{"$$ a;b $$", "$$" + sp(5) + "$$"},          // тело dollar-quote
		{"$1$ x", "$1$ x"},                          // не dollar-quote
	}
	for _, tc := range cases {
		got := Mask(tc.in)
		if got != tc.want {
			t.Errorf("Mask(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if len(got) != len(tc.in) {
			t.Errorf("Mask(%q) changed length: %d != %d", tc.in, len(got), len(tc.in))
		}
	}
}

func TestInStringOrComment(t *testing.T) {
	inside := []string{
		"WHERE note = 'lit",
		`WHERE note = E'a\'`,
		"SELECT 1 -- a comment",
		"SELECT 1 /* open",
		"SELECT 1 /* /* nested",
		`SELECT "Order Ite`,
		"DO $$ begin",
		"INSERT INTO t VALUES ('a', 'b",
	}
	for _, s := range inside {
		if !InStringOrComment(s) {
			t.Errorf("expected inside string/comment: %q", s)
		}
	}
	outside := []string{
		"WHERE note = 'lit'",
		"SELECT 1 -- c\nSELECT ",
		"SELECT 1 /* c */ FROM ",
		`SELECT "Order Items" FROM `,
		"DO $$ begin end $$ ",
		"SELECT * FROM items WHERE id = ",
		"",
	}
	for _, s := range outside {
		if InStringOrComment(s) {
			t.Errorf("expected NOT inside string/comment: %q", s)
		}
	}
}

func TestSplitConcurrentIndexStaysSeparate(t *testing.T) {
	src := "CREATE INDEX CONCURRENTLY i1 ON t(a);\nCREATE INDEX CONCURRENTLY i2 ON t(b);\n"
	got := Split(src)
	if len(got) != 2 {
		t.Fatalf("want 2 statements, got %d: %#v", len(got), got)
	}
}
