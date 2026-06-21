package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terox/internal/cluster"
	"terox/internal/complete"
	"terox/internal/config"
	"terox/internal/store"
)

func testCompleter() *completer {
	cfg := &config.Config{Services: map[string]*config.Service{
		"item":    {Storages: map[string]*config.Storage{"prod_item": {}, "cold": {}}},
		"billing": {Storages: map[string]*config.Storage{"main": {}}},
	}}
	r := &REPL{
		cfg:     cfg,
		service: "item",
		shards:  []cluster.Shard{{Label: "rs01"}, {Label: "rs02"}},
		queries: &store.Queries{M: map[string]string{"top": "select 1", "recent": "select 2"}},
	}
	return &completer{r: r}
}

func flatten(src [][]string) []string {
	var out []string
	for _, s := range src {
		out = append(out, s...)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestSuggestionsSuppressInStringOrComment(t *testing.T) {
	c := testCompleter() // нет целей БД -> каталог nil; движок всё равно прерывается
	for _, head := range []string{"where note = 'lit", "select 1 -- comm", "do $$ beg"} {
		if subs, _ := c.suggestions(head, len(head)); len(subs) != 0 {
			t.Errorf("expected no completion inside string/comment for %q, got %v", head, subs)
		}
	}
}

func TestSuggestionsCreateTableNoExistingRelations(t *testing.T) {
	c := testCompleter()
	if subs, _ := c.suggestions("create table ", len("create table ")); len(subs) != 0 {
		t.Errorf("CREATE TABLE should not suggest relations, got %v", subs)
	}
}

func TestMetaArgCompletion(t *testing.T) {
	c := testCompleter()
	cases := []struct {
		head string
		want []string // подмножество, которое обязательно должно присутствовать
	}{
		{"\\use ", []string{"billing", "item"}},
		{"\\c ", []string{"cold", "prod_item"}},
		{"\\c prod_item ", []string{"all", "rs01", "rs02"}},
		{"\\shard ", []string{"all", "rs01", "rs02"}},
		{"\\s ", []string{"all", "rs01", "rs02"}},
		{"\\g ", []string{"all", "rs01", "rs02"}},
		{"\\gx ", []string{"all", "rs01", "rs02"}},
		{"\\run ", []string{"recent", "top"}},
		{"\\write ", []string{"on", "off"}},
		{"\\timeout ", []string{"500ms", "off"}},
		{"\\compare ", []string{"billing/main", "item/cold", "item/prod_item"}},
		{"\\export ", []string{"csv", "json"}},
	}
	for _, tc := range cases {
		got := flatten(c.metaArgs(tc.head))
		for _, w := range tc.want {
			if !contains(got, w) {
				t.Errorf("metaArgs(%q) missing %q; got %v", tc.head, w, got)
			}
		}
	}
	// Наличие '/' в цели \compare останавливает дополнение (без двойной вставки).
	if got := c.metaArgs("\\compare item/"); len(flatten(got)) != 0 {
		t.Errorf("\\compare with slash should not complete, got %v", got)
	}
}

func TestPathCandidates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "migration.sql"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := pathCandidates(dir + "/m")
	if !contains(got, "migration.sql") {
		t.Errorf("expected migration.sql, got %v", got)
	}
	if contains(got, "sub/") {
		t.Errorf("prefix 'm' should not match 'sub', got %v", got)
	}
	all := pathCandidates(dir + "/")
	if !contains(all, "sub/") {
		t.Errorf("directory should be listed with trailing slash, got %v", all)
	}
}

func TestFuncToken(t *testing.T) {
	cases := []struct {
		name    string
		minArgs int64
		want    string
	}{
		{"current_user", 0, "current_user"}, // спецслово без скобок
		{"current_date", 0, "current_date"}, // спецслово без скобок
		{"session_user", 0, "session_user"}, // спецслово без скобок
		{"now", 0, "now()"},                 // функция без аргументов
		{"current_database", 0, "current_database()"},
		{"pg_backend_pid", 0, "pg_backend_pid()"},
		{"coalesce", 2, "coalesce("}, // принимает аргументы
		{"jsonb_build_object", 2, "jsonb_build_object("},
	}
	for _, tc := range cases {
		if got := funcToken(tc.name, tc.minArgs); got != tc.want {
			t.Errorf("funcToken(%q,%d) = %q, want %q", tc.name, tc.minArgs, got, tc.want)
		}
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	if got := longestCommonPrefix([]string{"_id", "_name"}); got != "_" {
		t.Errorf("got %q want _", got)
	}
	if got := longestCommonPrefix([]string{"abc", "abd"}); got != "ab" {
		t.Errorf("got %q want ab", got)
	}
	if got := longestCommonPrefix([]string{"x", "y"}); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

func TestSuffixes(t *testing.T) {
	src := [][]string{{"select", "set", "show"}}
	got := suffixes("se", src)
	// "select" -> "lect", "set" -> "t"; "show" исключается
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestHighlightSQLKeyword(t *testing.T) {
	out := highlightSQL("select * from items where id = 5")
	if !strings.Contains(out, cKeyword+"select"+cReset) {
		t.Errorf("select not highlighted: %q", out)
	}
	if !strings.Contains(out, cNumber+"5"+cReset) {
		t.Errorf("number not highlighted: %q", out)
	}
	// Каждый исходный символ сохраняется (убираем ANSI и сравниваем).
	stripped := stripANSI(out)
	if stripped != "select * from items where id = 5" {
		t.Errorf("text mangled: %q", stripped)
	}
}

func TestHighlightSQLString(t *testing.T) {
	out := highlightSQL("where note = 'select from'")
	if !strings.Contains(out, cString+"'select from'"+cReset) {
		t.Errorf("string literal not highlighted as one unit: %q", out)
	}
}

func TestHighlightDollarAndBlockComment(t *testing.T) {
	out := highlightSQL("do $$ select from $$ x")
	if !strings.Contains(out, cString+"$$ select from $$"+cReset) {
		t.Errorf("dollar-quoted string not highlighted as one unit: %q", out)
	}
	if stripANSI(out) != "do $$ select from $$ x" {
		t.Errorf("text mangled: %q", stripANSI(out))
	}
	out2 := highlightSQL("a /* drop;table */ b")
	if !strings.Contains(out2, cComment+"/* drop;table */"+cReset) {
		t.Errorf("block comment not highlighted as one unit: %q", out2)
	}
	if stripANSI(out2) != "a /* drop;table */ b" {
		t.Errorf("text mangled: %q", stripANSI(out2))
	}
}

func TestCurrentWord(t *testing.T) {
	cases := map[string]string{
		"extract(ep":                   "ep",   // внутри вызова функции
		"select * from items where t.": "t.",   // с точкой (целиком)
		"select t.co":                  "t.co", // частичное с точкой
		"\\co":                         "\\co", // мета-команда
		"select co":                    "co",   // обычный идентификатор
		"a, b":                         "b",    // после запятой
		"func(a, bc":                   "bc",   // после запятой в аргументах
		"":                             "",     // пусто
		"select ":                      "",     // пробел в конце
		"where x = 'lit":               "lit",  // внутри литерала (по возможности)
	}
	for head, want := range cases {
		if got := currentWord(head); got != want {
			t.Errorf("currentWord(%q) = %q, want %q", head, got, want)
		}
	}
}

func TestCompleteGrammarSpecials(t *testing.T) {
	// Сквозной тест: движок дополнения предлагает спецслова грамматики
	// выражений в позиции выражения.
	cat := &complete.Catalog{SearchPath: []string{"public"}, Schemas: []string{"public"}}
	r := complete.Complete("select * from t where x = ", cat)
	want := []string{"true", "false", "null", "extract("}
	have := map[string]bool{}
	for _, c := range r.Candidates {
		have[strings.ToLower(c.Insert)] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("Complete at an expression position is missing grammar special %q", w)
		}
	}
}

func TestParseInterval(t *testing.T) {
	if d, ok := parseInterval("5s"); !ok || d != 5*time.Second {
		t.Errorf("5s => %v,%v", d, ok)
	}
	if d, ok := parseInterval("3"); !ok || d != 3*time.Second {
		t.Errorf("3 => %v,%v", d, ok)
	}
	if _, ok := parseInterval("nope"); ok {
		t.Error("nope should not parse")
	}
	// Неположительные интервалы отвергаются (иначе \watch зациклится).
	for _, s := range []string{"0s", "-1s", "0", "-5"} {
		if _, ok := parseInterval(s); ok {
			t.Errorf("%q should be rejected", s)
		}
	}
}

func TestLooksLikeInterval(t *testing.T) {
	for _, s := range []string{"5s", "2m", "0s", "-1s", "10", "1.5h"} {
		if !looksLikeInterval(s) {
			t.Errorf("%q should look like an interval", s)
		}
	}
	for _, s := range []string{"select", "count(*)", "x5"} {
		if looksLikeInterval(s) {
			t.Errorf("%q should NOT look like an interval", s)
		}
	}
}

func TestTokenizeArgs(t *testing.T) {
	got := tokenizeArgs(`\migrate -n "/path with space/m.sql"`)
	want := []string{`\migrate`, "-n", "/path with space/m.sql"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
	// Одинарные кавычки и экранированные двойные кавычки.
	if g := tokenizeArgs(`\export json 'a b.json'`); g[2] != "a b.json" {
		t.Errorf("single-quote path = %q", g[2])
	}
	if g := tokenizeArgs(`\x "a\"b"`); g[1] != `a"b` {
		t.Errorf("escaped quote = %q", g[1])
	}
}

func TestSavedQueryParams(t *testing.T) {
	sql := "select * from items where id = :id and name = :name and note = ':id not a param' order by id::int"
	got := queryParams(sql)
	if len(got) != 2 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("queryParams = %v, want [id name] (literal :id and ::cast ignored)", got)
	}
	bound := applyParams(sql, map[string]string{
		"id":   paramLiteral("5"),
		"name": paramLiteral("o'brien"),
	})
	want := "select * from items where id = 5 and name = 'o''brien' and note = ':id not a param' order by id::int"
	if bound != want {
		t.Errorf("applyParams:\n got %q\nwant %q", bound, want)
	}
	if queryParams("select 1") != nil {
		t.Error("no params should yield nil")
	}
}

func TestWriteLeaseResetOnContextSwitch(t *testing.T) {
	cfg := &config.Config{Services: map[string]*config.Service{
		"s": {Storages: map[string]*config.Storage{
			"a": {HostTemplate: "127.0.0.1", DBTemplate: "d", Port: 5432, Count: 1},
			"b": {HostTemplate: "127.0.0.1", DBTemplate: "d", Port: 5432, Count: 1},
		}},
	}}
	var buf strings.Builder
	r := &REPL{cfg: cfg, out: &buf}
	if err := r.bindStorage("s", "a"); err != nil {
		t.Fatal(err)
	}
	r.writeMode = true
	if err := r.bindStorage("s", "b"); err != nil { // смена хранилища
		t.Fatal(err)
	}
	if r.writeMode {
		t.Error("write mode must reset to read-only on storage switch")
	}
	if !strings.Contains(buf.String(), "read-only") {
		t.Errorf("expected a reset notice, got %q", buf.String())
	}
	// Повторная привязка ТОГО ЖЕ хранилища сохраняет режим записи.
	r.writeMode = true
	if err := r.bindStorage("s", "b"); err != nil {
		t.Fatal(err)
	}
	if !r.writeMode {
		t.Error("same-storage rebind must keep write mode")
	}
}

func TestParseOnOff(t *testing.T) {
	if v, _ := parseOnOff(nil, true); v != false {
		t.Error("no args toggles true->false")
	}
	if v, _ := parseOnOff([]string{"on"}, false); v != true {
		t.Error("on -> true")
	}
	if v, _ := parseOnOff([]string{"OFF"}, true); v != false {
		t.Error("OFF -> false")
	}
	if _, err := parseOnOff([]string{"maybe"}, true); err == nil {
		t.Error("invalid value must error, not silently keep state")
	}
}

func TestParseExplainArgs(t *testing.T) {
	cases := []struct {
		in    []string
		mode  string
		anly  bool
		query string
	}{
		{[]string{"select", "1"}, "auto", false, "select 1"},
		{[]string{"analyze", "select", "1"}, "auto", true, "select 1"},
		{[]string{"--all", "select", "*", "from", "t"}, "all", false, "select * from t"},
		{[]string{"analyze", "--first", "select", "1;"}, "first", true, "select 1"},
		{[]string{"--shard", "rs05", "select", "1"}, "shard", false, "select 1"},
		{[]string{"--sample", "4", "select", "1"}, "sample", false, "select 1"},
		{[]string{"--outliers", "select", "1"}, "outliers", false, "select 1"},
	}
	for _, c := range cases {
		// raw — это соединённые аргументы; запрос вырезается из raw
		// с сохранением внутренних пробелов.
		o, q, err := parseExplainArgs(c.in, strings.Join(c.in, " "))
		if err != nil {
			t.Errorf("%v: %v", c.in, err)
			continue
		}
		if o.mode != c.mode || o.analyze != c.anly || q != c.query {
			t.Errorf("%v => mode=%q analyze=%v query=%q; want %q/%v/%q", c.in, o.mode, o.analyze, q, c.mode, c.anly, c.query)
		}
	}
	if _, _, err := parseExplainArgs([]string{"--sample", "x", "select"}, "--sample x select"); err == nil {
		t.Error("--sample x must error")
	}
	// Внутренние пробелы в строковом литерале сохраняются (не схлопываются):
	// остаток вырезается из raw, а не пересобирается через strings.Fields.
	raw := "analyze select * from t where name = 'a   b'"
	_, q, err := parseExplainArgs(strings.Fields(raw), raw)
	if err != nil {
		t.Fatal(err)
	}
	if q != "select * from t where name = 'a   b'" {
		t.Errorf("string-literal spacing collapsed: %q", q)
	}
}

func TestParseMaxRows(t *testing.T) {
	if n, err := parseMaxRows("100"); err != nil || n != 100 {
		t.Errorf("100 => %d,%v", n, err)
	}
	for _, s := range []string{"unlimited", "all", "0"} {
		if n, err := parseMaxRows(s); err != nil || n != 0 {
			t.Errorf("%q => %d,%v want 0", s, n, err)
		}
	}
	for _, s := range []string{"-5", "abc", "1.5"} {
		if _, err := parseMaxRows(s); err == nil {
			t.Errorf("%q must error", s)
		}
	}
}

// stripANSI убирает ANSI-escape-последовательности для проверок.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
