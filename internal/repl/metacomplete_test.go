package repl

import (
	"io"
	"strings"
	"testing"

	"terox/internal/complete"
	"terox/internal/config"
)

func metaCompleterWithCat() *completer {
	cat := &complete.Catalog{
		SearchPath: []string{"public"},
		Schemas:    []string{"public", "items"},
		Relations: []complete.Relation{
			{Schema: "public", Name: "orders", Kind: "r"},
			{Schema: "items", Name: "users", Kind: "r"},
			{Schema: "items", Name: "items", Kind: "r"},
			{Schema: "items", Name: "users_idx", Kind: "i"}, // индекс: не предлагается
		},
		Reserved: map[string]bool{},
		Shards:   1,
	}
	r := &REPL{out: io.Discard, suggest: true, cfg: &config.Config{}}
	r.catalog = cat
	// Заполняем кэш первичных ключей, чтобы дополнение/валидация не обращались к БД.
	r.pkCache = map[string][]string{
		"items\x00items": {"item_id"},
		"\x00orders":     {"id"},
	}
	r.comp = newCompleter(r)
	return r.comp
}

func has(subs []string, want string) bool {
	for _, s := range subs {
		if s == want {
			return true
		}
	}
	return false
}

func TestMetaTableArgCompletion(t *testing.T) {
	c := metaCompleterWithCat()
	// \count <table> с пустым словом → полные имена таблиц/схем.
	subs, _ := c.suggestions(`\count `, len(`\count `))
	for _, want := range []string{"orders", "items.users", "items", "public"} {
		if !has(subs, want) {
			t.Errorf(`\count should suggest %q; got %v`, want, subs)
		}
	}
	// items нет в search_path → голое "users" не предлагается (только items.users).
	if has(subs, "users") {
		t.Errorf("bare non-search-path table 'users' should not be offered: %v", subs)
	}
	// индекс не предлагается как таблица.
	if has(subs, "items.users_idx") {
		t.Errorf("index should not be offered as a table: %v", subs)
	}
	// \find: "items." → переход к таблицам схемы (суффикс "users").
	subs2, _ := c.suggestions(`\find items.`, len(`\find items.`))
	if !has(subs2, "users") {
		t.Errorf(`\find items. should suggest 'users'; got %v`, subs2)
	}
	// \diff тоже дополняет таблицы.
	subs3, _ := c.suggestions(`\diff or`, len(`\diff or`))
	if !has(subs3, "ders") { // суффикс "orders" после "or"
		t.Errorf(`\diff or → should complete 'orders'; got %v`, subs3)
	}
}

func TestMetaPkeyArgCompletion(t *testing.T) {
	c := metaCompleterWithCat()
	// \find items.items. → столбец первичного ключа items.items.
	subs, _ := c.suggestions(`\find items.items.`, len(`\find items.items.`))
	if !has(subs, "item_id") {
		t.Errorf(`\find items.items. should drill to pkey 'item_id'; got %v`, subs)
	}
	// таблица из search_path: orders. → её первичный ключ "id".
	subs2, _ := c.suggestions(`\count orders.`, len(`\count orders.`))
	if !has(subs2, "id") {
		t.Errorf(`\count orders. should drill to pkey 'id'; got %v`, subs2)
	}
	// \diff не переходит к столбцам (принимает только таблицу).
	subs3, _ := c.suggestions(`\diff items.items.`, len(`\diff items.items.`))
	if has(subs3, "item_id") {
		t.Errorf(`\diff must not offer pkey columns; got %v`, subs3)
	}
}

func TestParsePkeyShorthand(t *testing.T) {
	cases := []struct {
		args                         []string
		ok                           bool
		schema, table, col, tableTok string
		cond                         string
	}{
		{[]string{"items.items.item_id=100"}, true, "items", "items", "item_id", "items.items", "item_id=100"},
		{[]string{"orders.id=5"}, true, "", "orders", "id", "orders", "id=5"},
		{[]string{"items.items.item_id", "=", "100"}, true, "items", "items", "item_id", "items.items", "item_id=100"},
		{[]string{"items.users", "id", "=", "200"}, false, "", "", "", "", ""}, // пробел в части таблицы → обычная форма
		{[]string{"items.users"}, false, "", "", "", "", ""},                   // нет оператора
		{[]string{"a.b.c.d=1"}, false, "", "", "", "", ""},                     // слишком много точек для таблицы
	}
	for _, tc := range cases {
		sh, ok := parsePkeyShorthand(tc.args)
		if ok != tc.ok {
			t.Errorf("parsePkeyShorthand(%v) ok=%v, want %v", tc.args, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if sh.schema != tc.schema || sh.table != tc.table || sh.col != tc.col ||
			sh.tableTok != tc.tableTok || sh.cond != tc.cond {
			t.Errorf("parsePkeyShorthand(%v) = %+v, want schema=%q table=%q col=%q tableTok=%q cond=%q",
				tc.args, sh, tc.schema, tc.table, tc.col, tc.tableTok, tc.cond)
		}
	}
}

func TestResolvePkeyShorthand(t *testing.T) {
	r := &REPL{out: io.Discard, pkCache: map[string][]string{"items\x00items": {"item_id"}}}
	// столбец первичного ключа → переписывается в [таблица, условие].
	got, err := r.resolvePkeyShorthand([]string{"items.items.item_id=100"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "items.items" || got[1] != "item_id=100" {
		t.Errorf("rewrite = %v, want [items.items item_id=100]", got)
	}
	// столбец не первичного ключа → отказ.
	if _, err := r.resolvePkeyShorthand([]string{"items.items.name=x"}); err == nil {
		t.Errorf("expected refusal for non-pkey column")
	}
	// обычная форма проходит без изменений.
	got2, err := r.resolvePkeyShorthand([]string{"items.items", "id=1"})
	if err != nil || len(got2) != 2 || got2[0] != "items.items" {
		t.Errorf("ordinary form should pass through; got %v err %v", got2, err)
	}
	// допустимо только простое литеральное значение; выражения/инъекции отклоняются.
	for _, bad := range [][]string{
		{"items.items.item_id=100", "or", "name='x'"},     // булев хвост
		{"items.items.item_id=1", "union", "select", "1"}, // эксфильтрация через UNION
		{"items.items.item_id=(select", "1)"},             // подзапрос
		{"items.items.item_id=1)or(1=1"},                  // разрыв скобок без пробелов
		{"items.items.item_id="},                          // пустое значение
	} {
		if _, err := r.resolvePkeyShorthand(bad); err == nil {
			t.Errorf("non-literal key value must be refused: %v", bad)
		}
	}
	// строковое значение в кавычках с 'and'/'or' внутри — допустимый литерал.
	if _, err := r.resolvePkeyShorthand([]string{"items.items.item_id='a or b'"}); err != nil {
		t.Errorf("and/or inside a string literal should be allowed: %v", err)
	}
	// числа, строки в кавычках и null/bool принимаются.
	for _, ok := range [][]string{
		{"items.items.item_id=100"}, {"items.items.item_id>=5"},
		{"items.items.item_id='abc'"}, {"items.items.item_id", "=", "null"},
	} {
		if _, err := r.resolvePkeyShorthand(ok); err != nil {
			t.Errorf("simple literal value should be allowed %v: %v", ok, err)
		}
	}
	// недописанная форма (без значения) → подсказка, а не общая ошибка.
	_, err = r.resolvePkeyShorthand([]string{"items.items.item_id"})
	if err == nil || !strings.Contains(err.Error(), "укажите значение") {
		t.Errorf("missing-value shorthand should hint to add a value; got %v", err)
	}
}

// TestCompletionUnicodeCursor проверяет: readline/tea передают курсор как индекс
// в РУНАХ, а дополнение и движок режут по БАЙТОВОМУ смещению. Многобайтовый
// префикс (кириллический литерал) не должен портить дополняемое слово.
func TestCompletionUnicodeCursor(t *testing.T) {
	c := metaCompleterWithCat()
	line := "select 'мир' from o" // 'мир' — 3 кириллические руны / 6 байт
	rl := []rune(line)
	out, _ := c.Do(rl, len(rl)) // readline передаёт Do индекс в рунах
	var subs []string
	for _, r := range out {
		subs = append(subs, string(r))
	}
	// в позиции FROM с префиксом "o" дополняется 'orders' (суффикс "rders").
	if !has(subs, "rders") {
		t.Errorf("unicode prefix broke FROM completion; got %v", subs)
	}
}
