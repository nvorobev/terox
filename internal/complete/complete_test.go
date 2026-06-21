package complete

import (
	"strings"
	"testing"
)

func sampleCatalog() *Catalog {
	return &Catalog{
		SearchPath: []string{"public"},
		Schemas:    []string{"public", "archive", "pg_catalog"},
		Relations: []Relation{
			{Schema: "public", Name: "orders", Kind: "r"},
			{Schema: "public", Name: "users", Kind: "r"},
			{Schema: "public", Name: "Order Items", Kind: "r"}, // нужны кавычки
			{Schema: "public", Name: "user", Kind: "v"},        // зарезервированное слово
			{Schema: "public", Name: "orders_pkey", Kind: "i"}, // индекс: не в FROM
			{Schema: "archive", Name: "orders", Kind: "r"},
			{Schema: "archive", Name: "orders_v", Kind: "v"},    // представление (в drill: видно)
			{Schema: "archive", Name: "orders_idx", Kind: "i"},  // индекс (в drill: скрыт)
			{Schema: "pg_catalog", Name: "pg_class", Kind: "r"}, // системная таблица (скрыта)
		},
		Columns: []Column{
			{Schema: "public", Relation: "orders", Name: "id", Type: "integer"},
			{Schema: "public", Relation: "orders", Name: "created_at", Type: "timestamptz"},
			{Schema: "public", Relation: "orders", Name: "user_id", Type: "integer"},
			{Schema: "public", Relation: "users", Name: "id", Type: "integer"},
			{Schema: "public", Relation: "users", Name: "name", Type: "text"},
			{Schema: "archive", Relation: "orders", Name: "archived_at", Type: "timestamptz"},
		},
		Functions: []Function{
			{Schema: "pg_catalog", Name: "now", Signature: "()", MinArgs: 0},
			{Schema: "pg_catalog", Name: "current_user", NoParen: true},
			{Schema: "pg_catalog", Name: "count", Signature: "(\"any\")", Kind: "a", MinArgs: 1},
			{Schema: "pg_catalog", Name: "generate_series", Signature: "(int,int)", MinArgs: 2, RetSet: true},
			{Schema: "public", Name: "calc", Signature: "(int)", MinArgs: 1},  // пользовательская функция
			{Schema: "archive", Name: "arch_fn", Signature: "()", MinArgs: 0}, // в пределах схемы
		},
		Keywords: []string{"select", "from", "where", "and", "lateral"},
		Shards:   1,
	}
}

func displays(r Result) []string {
	out := make([]string, len(r.Candidates))
	for i, c := range r.Candidates {
		out[i] = c.Display
	}
	return out
}

func find(r Result, display string) (Candidate, bool) {
	for _, c := range r.Candidates {
		if c.Display == display {
			return c, true
		}
	}
	return Candidate{}, false
}

func has(r Result, display string) bool { _, ok := find(r, display); return ok }

func TestCompleteRelations(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from ", cat)
	for _, want := range []string{"orders", "users", "Order Items"} {
		if !has(r, want) {
			t.Errorf("FROM position missing %q; got %v", want, displays(r))
		}
	}
	if has(r, "orders_pkey") {
		t.Errorf("index must not be offered in FROM position: %v", displays(r))
	}
	// FROM также предлагает имена СХЕМ: можно выбрать схему, ввести '.' и углубиться
	// в её таблицы (навигация от схемы).
	if !has(r, "archive") {
		t.Errorf("FROM should offer schema 'archive' for drill-down: %v", displays(r))
	}
	// Встроенные функции показываются только по префиксу: пустой Tab в FROM не
	// засыпан set-returning функциями pg_catalog, но ввод префикса их открывает.
	if has(r, "generate_series") {
		t.Errorf("empty FROM should not dump pg_catalog set-returning functions: %v", displays(r))
	}
	if rg := Complete("select * from generate_se", cat); !has(rg, "generate_series") {
		t.Errorf("FROM should offer generate_series once a prefix is typed: %v", displays(rg))
	}
	// Скалярные функции вроде now() в FROM не предлагаются (не set-returning).
	if has(Complete("select * from no", cat), "now") {
		t.Errorf("scalar function now() must not be offered in FROM position even with a prefix")
	}
	// Кавычки при вставке.
	if c, _ := find(r, "Order Items"); c.Insert != `"Order Items"` {
		t.Errorf("space-name insert = %q, want quoted", c.Insert)
	}
	if c, _ := find(r, "user"); c.Insert != `"user"` {
		t.Errorf("reserved-name insert = %q, want quoted", c.Insert)
	}
	if c, _ := find(r, "orders"); c.Insert != "orders" {
		t.Errorf("plain insert = %q, want bare", c.Insert)
	}
}

func TestCompleteUnquotedExcludesQuoteNeeding(t *testing.T) {
	cat := sampleCatalog()
	// Ввод без кавычек "Order" не может вставить "Order Items" (невалидное
	// дополнение не выдаётся), поэтому имя исключается.
	r := Complete("select * from Order", cat)
	if has(r, "Order Items") {
		t.Errorf("must not offer quote-needing name for unquoted prefix: %v", displays(r))
	}
	// Но если кавычка открыта, имя предлагается с закрывающей кавычкой.
	r2 := Complete(`select * from "Ord`, cat)
	c, ok := find(r2, "Order Items")
	if !ok || c.Insert != `"Order Items"` {
		t.Errorf("quoted prefix should offer \"Order Items\"; got %v", displays(r2))
	}
}

func TestCompleteQuotedIdentifierCaseSensitive(t *testing.T) {
	// Часть в двойных кавычках — идентификатор с УЧЁТОМ регистра: "Fo совпадает
	// с точным "Foo", но не с lowercase foobar, а "fo не совпадает с "Foo".
	cat := &Catalog{
		SearchPath: []string{"public"},
		Schemas:    []string{"public"},
		Relations: []Relation{
			{Schema: "public", Name: "Foo", Kind: "r"}, // нужны кавычки (верхний регистр)
			{Schema: "public", Name: "foobar", Kind: "r"},
		},
	}
	r := Complete(`select * from "Fo`, cat)
	if c, ok := find(r, "Foo"); !ok || c.Insert != `"Foo"` {
		t.Errorf(`quoted "Fo should offer "Foo"; got %v`, displays(r))
	}
	if has(r, "foobar") {
		t.Errorf(`quoted "Fo must NOT match foobar; got %v`, displays(r))
	}
	if has(Complete(`select * from "fo`, cat), "Foo") {
		t.Errorf(`quoted "fo must NOT match the uppercase "Foo"`)
	}
}

func TestCompleteColumnsScoped(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from orders where cr", cat)
	if !has(r, "created_at") {
		t.Errorf("WHERE should complete scoped columns; got %v", displays(r))
	}
	if has(r, "archived_at") {
		t.Errorf("out-of-scope column leaked: %v", displays(r))
	}
}

func TestCompleteAliasColumns(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from orders o where o.", cat)
	for _, want := range []string{"id", "created_at", "user_id"} {
		if !has(r, want) {
			t.Errorf("alias.col missing %q; got %v", want, displays(r))
		}
	}
	if has(r, "name") {
		t.Errorf("users column leaked into orders alias scope: %v", displays(r))
	}
}

func TestCompleteCTEInFrom(t *testing.T) {
	cat := sampleCatalog()
	// CTE, заданный через WITH, предлагается в позиции FROM рядом с таблицами.
	r := Complete("with recent as (select 1) select * from re", cat)
	if !has(r, "recent") {
		t.Errorf("FROM should offer the in-query CTE 'recent'; got %v", displays(r))
	}
	c, _ := find(r, "recent")
	if c.Detail != "CTE" {
		t.Errorf("CTE candidate detail = %q, want CTE", c.Detail)
	}
}

// TestCompleteMultiCTEAndColumnList: SELECT внутри тела одного CTE не обрывает
// разбор (последующие CTE не теряются), а список колонок `cte(col, ...)` перед AS
// распознаётся.
func TestCompleteMultiCTEAndColumnList(t *testing.T) {
	cat := sampleCatalog()
	// Два CTE; второй предлагается, даже если тело первого содержит SELECT.
	r := Complete("with a as (select 1), beta as (select 2) select * from be", cat)
	if !has(r, "beta") {
		t.Errorf("the second CTE 'beta' must be offered after a SELECT in the first body; got %v", displays(r))
	}
	// CTE со списком колонок: `cte(col) AS (...)`.
	r = Complete("with named(x, y) as (select 1, 2) select * from nam", cat)
	if !has(r, "named") {
		t.Errorf("a CTE with a column list must be recognized; got %v", displays(r))
	}
}

func TestCompleteClauseContinuation(t *testing.T) {
	cat := sampleCatalog()
	// После завершённой ссылки на таблицу предлагается "where" и другие ключевые слова клауз.
	if r := Complete("select * from items w", cat); !has(r, "where") {
		t.Errorf("'from items w' should offer 'where'; got %v", displays(r))
	}
	if r := Complete("select * from orders o ", cat); !has(r, "where") || !has(r, "group by") {
		t.Errorf("after a table+alias, expected continuation keywords; got %v", displays(r))
	}
	// В непосредственной позиции таблицы (сразу после FROM) ключевые слова НЕ предлагаются.
	if r := Complete("select * from ", cat); has(r, "where") {
		t.Errorf("immediate FROM slot should not offer 'where'; got %v", displays(r))
	}
	// После UPDATE <table> предлагается SET.
	if r := Complete("update orders ", cat); !has(r, "set") {
		t.Errorf("'update orders ' should offer 'set'; got %v", displays(r))
	}
}

func TestCompleteUpdateSetColumns(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("update orders set ", cat)
	if !has(r, "created_at") || !has(r, "user_id") {
		t.Errorf("UPDATE SET should complete target columns; got %v", displays(r))
	}
}

func TestCompleteSchemaQualified(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from archive.", cat)
	if !has(r, "orders") {
		t.Errorf("schema. should list its relations; got %v", displays(r))
	}
	if has(r, "users") {
		t.Errorf("public.users leaked into archive. scope: %v", displays(r))
	}
}

func TestCompleteCreateTableNewName(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("create table ", cat)
	if len(r.Candidates) != 0 {
		t.Errorf("CREATE TABLE <new name> must offer nothing; got %v", displays(r))
	}
}

func TestCompleteTokenSequence(t *testing.T) {
	cat := sampleCatalog()
	if r := Complete("select * from t order ", cat); !has(r, "by") || len(r.Candidates) != 1 {
		t.Errorf("ORDER -> BY; got %v", displays(r))
	}
	if r := Complete("select * from t where x is ", cat); !has(r, "null") || !has(r, "not") {
		t.Errorf("IS -> null/not/...; got %v", displays(r))
	}
	if r := Complete("select id from t for ", cat); !has(r, "update") || !has(r, "share") {
		t.Errorf("FOR -> update/share/...; got %v", displays(r))
	}
}

func TestCompleteTopLevelAndKeywordCase(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("SEL", cat)
	c, ok := find(r, "SELECT")
	if !ok {
		t.Fatalf("top-level SELECT (uppercased) missing; got %v", displays(r))
	}
	if c.Insert != "SELECT" {
		t.Errorf("keyword case policy: insert = %q, want SELECT", c.Insert)
	}
	// нижний регистр остаётся нижним
	r2 := Complete("sel", cat)
	if c2, _ := find(r2, "select"); c2.Insert != "select" {
		t.Errorf("lowercase keyword insert = %q, want select", c2.Insert)
	}
}

func TestCompleteSuppressInStringComment(t *testing.T) {
	cat := sampleCatalog()
	for _, head := range []string{"select * from t where x = 'lit", "select 1 -- comm", "select $$ body"} {
		if r := Complete(head, cat); len(r.Candidates) != 0 {
			t.Errorf("no completion inside string/comment for %q; got %v", head, displays(r))
		}
	}
}

func TestCompleteReplaceStart(t *testing.T) {
	cat := sampleCatalog()
	head := "select * from ord"
	r := Complete(head, cat)
	if r.ReplaceStart != len("select * from ") {
		t.Errorf("ReplaceStart = %d, want %d", r.ReplaceStart, len("select * from "))
	}
	// каждый Insert кандидата продолжает введённый префикс
	prefix := head[r.ReplaceStart:]
	for _, c := range r.Candidates {
		if len(c.Insert) < len(prefix) {
			t.Errorf("candidate %q shorter than prefix %q", c.Insert, prefix)
		}
	}
}

func TestCandidateDetail(t *testing.T) {
	cat := sampleCatalog()
	// сигнатура функции передаётся в Detail (в позиции выражения, не FROM)
	r := Complete("select generate_series", cat)
	if c, ok := find(r, "generate_series"); !ok || c.Detail != "(int,int)" {
		t.Errorf("function Detail = %q, want (int,int)", c.Detail)
	}
	// тип колонки передаётся в Detail
	r2 := Complete("update orders set ", cat)
	if c, ok := find(r2, "created_at"); !ok || c.Detail != "timestamptz" {
		t.Errorf("column Detail = %q, want timestamptz", c.Detail)
	}
}

func TestSearchPathPrecedence(t *testing.T) {
	cat := sampleCatalog() // search_path=[public]; orders есть и в public, и в archive
	// Неквалифицированный алиас 'orders' разрешается в первую по search_path схему (public).
	r := Complete("select * from orders o where o.", cat)
	if !has(r, "user_id") { // колонка public.orders
		t.Errorf("expected public.orders columns (user_id); got %v", displays(r))
	}
	if has(r, "archived_at") { // колонка archive.orders
		t.Errorf("archive.orders column leaked despite search_path precedence: %v", displays(r))
	}
}

func TestLazyColumns(t *testing.T) {
	// Каталог с отношениями, но без загруженных колонок (ленивая загрузка).
	cat := &Catalog{
		SearchPath: []string{"public"},
		Schemas:    []string{"public", "archive"},
		Relations: []Relation{
			{Schema: "public", Name: "orders", Kind: "r"},
			{Schema: "archive", Name: "orders", Kind: "r"},
		},
	}
	// Хост спрашивает, какие колонки нужны этому дополнению.
	refs := NeededColumns("select * from orders o where o.", cat)
	if len(refs) != 1 || refs[0] != (RelRef{Schema: "public", Name: "orders"}) {
		t.Fatalf("NeededColumns = %v, want [{public orders}]", refs)
	}
	if cat.ColumnsLoaded("", "orders") {
		t.Error("columns should not be loaded yet")
	}
	// До загрузки кандидатов-колонок нет.
	if r := Complete("select * from orders o where o.", cat); len(r.Candidates) != 0 {
		t.Errorf("expected no columns before load; got %v", displays(r))
	}
	// Хост подгружает их лениво.
	cat.SetColumns("public", "orders", []Column{
		{Schema: "public", Relation: "orders", Name: "id", Type: "integer"},
		{Schema: "public", Relation: "orders", Name: "total", Type: "numeric"},
	})
	if !cat.ColumnsLoaded("", "orders") {
		t.Error("columns should be loaded now")
	}
	r := Complete("select * from orders o where o.", cat)
	if !has(r, "id") || !has(r, "total") {
		t.Errorf("expected lazily-loaded columns; got %v", displays(r))
	}
	// В позиции FROM колонки не нужны.
	if refs := NeededColumns("select * from ", cat); len(refs) != 0 {
		t.Errorf("FROM position needs no columns; got %v", refs)
	}
}

func TestCoverageBadge(t *testing.T) {
	cat := sampleCatalog()
	cat.Shards = 3
	cat.Coverage = map[string]int{"col:public.orders.user_id": 2}
	r := Complete("select * from orders where user_", cat)
	c, ok := find(r, "user_id")
	if !ok || c.Coverage != "[2/3]" {
		t.Errorf("coverage badge = %q, want [2/3]; got %v", c.Coverage, displays(r))
	}
}

func TestCompleteHidesSystemObjects(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from ", cat)
	if has(r, "pg_class") {
		t.Errorf("system table pg_class must be hidden in FROM: %v", displays(r))
	}
	if has(r, "pg_catalog") {
		t.Errorf("system schema pg_catalog must be hidden: %v", displays(r))
	}
	// Пользовательская схема предлагается.
	if !has(r, "archive") {
		t.Errorf("user schema 'archive' should be offered: %v", displays(r))
	}
	// Системный режим их показывает.
	cat.IncludeSystem = true
	r = Complete("select * from ", cat)
	if !has(r, "pg_catalog") || !has(r, "pg_class") {
		t.Errorf("system mode should reveal pg_catalog/pg_class: %v", displays(r))
	}
}

func TestSchemaDrillSelectableAndFunctions(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from archive.", cat)
	if !has(r, "orders") || !has(r, "orders_v") {
		t.Errorf("drill should list the schema's tables and views: %v", displays(r))
	}
	if has(r, "orders_idx") {
		t.Errorf("drill must NOT list indexes/primary keys: %v", displays(r))
	}
	if !has(r, "arch_fn") {
		t.Errorf("drill should list the schema's functions: %v", displays(r))
	}
	if has(r, "now") {
		t.Errorf("pg_catalog now() must not leak into archive. drill: %v", displays(r))
	}
	// Порядок: отношения (таблицы/представления) перед функциями.
	ri, fi := -1, -1
	for i, c := range r.Candidates {
		if c.Display == "orders" {
			ri = i
		}
		if c.Display == "arch_fn" {
			fi = i
		}
	}
	if ri < 0 || fi < 0 || ri > fi {
		t.Errorf("relations should sort before functions: %v", displays(r))
	}
	// Живой ghost показывает ПЕРВОГО кандидата — после "schema." это должна быть
	// ТАБЛИЦА, не индекс/первичный ключ. (Таблицы идут перед представлениями и функциями.)
	if len(r.Candidates) == 0 {
		t.Fatal("schema drill returned no candidates")
	}
	first := r.Candidates[0]
	if first.Kind != KRelation || !strings.HasPrefix(first.Detail, "table") {
		t.Errorf("ghost (first candidate) after schema. must be a table, got %q (detail %q)", first.Display, first.Detail)
	}
	if first.Display != "orders" {
		t.Errorf("first table should be 'orders' (alphabetical), got %q", first.Display)
	}
}

func TestSystemFunctionsPrefixGated(t *testing.T) {
	cat := sampleCatalog()
	// Пустой Tab в позиции выражения не должен вываливать функции pg_catalog.
	r := Complete("select ", cat)
	if has(r, "now") {
		t.Errorf("now() should not appear on an empty Tab: %v", displays(r))
	}
	// Функция пользовательской схемы предлагается всегда.
	if !has(r, "calc") {
		t.Errorf("user function calc should be offered: %v", displays(r))
	}
	// После ввода префикса встроенная функция доступна.
	r = Complete("select no", cat)
	if !has(r, "now") {
		t.Errorf("now() should appear after typing 'no': %v", displays(r))
	}
}

func TestCompleteSelectLeadsTopLevel(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("s", cat)
	if len(r.Candidates) == 0 || r.Candidates[0].Display != "select" {
		t.Errorf("typing 's' should lead with select (not the shorter 'set'); got %v", displays(r))
	}
}

func TestCompleteSelectListOffersFrom(t *testing.T) {
	cat := sampleCatalog()
	// После завершённого элемента select ввод "f" ведёт к FROM (а не к функции).
	r := Complete("select id f", cat)
	if len(r.Candidates) == 0 || r.Candidates[0].Display != "from" {
		t.Errorf("'select id f' should lead with FROM; got %v", displays(r))
	}
	// Сразу после SELECT (новый слот выражения) FROM не приоритетное продолжение —
	// элемент select ещё пишется.
	r2 := Complete("select f", cat)
	if c, ok := find(r2, "from"); ok && c.score >= explicitKeywordScore {
		t.Errorf("'select f' must not prioritize FROM in the expression slot; got %v", displays(r2))
	}
}

func TestCompleteForwardAlias(t *testing.T) {
	cat := sampleCatalog() // у public.orders колонки id, created_at, user_id
	// Курсор сразу после "select o.", а FROM/алиас находятся СПРАВА.
	line := "select o. from orders o limit 1"
	pos := len("select o.")
	r := CompleteAt(line, pos, cat)
	for _, want := range []string{"id", "created_at", "user_id"} {
		if !has(r, want) {
			t.Errorf("forward alias: 'select o.|' should complete orders columns; missing %q; got %v", want, displays(r))
		}
	}
	// Колонка из другой, несвязанной таблицы не должна просачиваться.
	if has(r, "archived_at") {
		t.Errorf("unrelated column leaked into forward-alias scope: %v", displays(r))
	}
	// Прямой алиас с указанием схемы: "select p." при "from items.products p".
	line2 := "select p. from public.orders p"
	r2 := CompleteAt(line2, len("select p."), cat)
	if !has(r2, "user_id") {
		t.Errorf("schema-qualified forward alias should resolve columns; got %v", displays(r2))
	}
}
