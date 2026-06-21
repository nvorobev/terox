package complete

import "testing"

// Регрессия P2 (аудит 2026-06-24): для schema.relation. completion обязан брать
// колонки ИМЕННО указанной схемы, а не одноимённой таблицы из search_path. В
// sampleCatalog и public.orders, и archive.orders существуют, search_path=[public].

func TestSchemaQualifiedRelationColumnsUsesNamedSchema(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select archive.orders.", cat)
	if !has(r, "archived_at") {
		t.Errorf("archive.orders. must offer archive.orders columns (archived_at); got %v", displays(r))
	}
	for _, leaked := range []string{"user_id", "created_at"} {
		if has(r, leaked) {
			t.Errorf("schema-qualified archive.orders. leaked public.orders column %q: %v", leaked, displays(r))
		}
	}
}

// Кавычки снимаются в identName до classifyQualified, поэтому явный qualifier в
// кавычках должен вести себя так же, как без них.
func TestQuotedSchemaQualifiedRelationColumnsUsesNamedSchema(t *testing.T) {
	cat := sampleCatalog()
	r := Complete(`select "archive"."orders".`, cat)
	if !has(r, "archived_at") {
		t.Errorf(`"archive"."orders". must offer archive.orders columns; got %v`, displays(r))
	}
	if has(r, "user_id") {
		t.Errorf(`quoted schema-qualified relation leaked public.orders column: %v`, displays(r))
	}
}

// Защита от регрессии: schema. (один квалификатор) по-прежнему углубляется в
// отношения и функции схемы, а не выдаёт колонки.
func TestSchemaDrillStillSuggestsRelationsAndFunctions(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select archive.", cat)
	if !has(r, "orders") {
		t.Errorf("schema. drill must list relations (orders); got %v", displays(r))
	}
	if !has(r, "arch_fn") {
		t.Errorf("schema. drill must list functions (arch_fn); got %v", displays(r))
	}
	if has(r, "archived_at") {
		t.Errorf("schema. drill must NOT list columns (archived_at); got %v", displays(r))
	}
}

// Защита от регрессии: alias. и голое relation. продолжают разрешаться в колонки
// своей области, а явный qualifier не ломает обычную квалификацию по псевдониму.
func TestAliasQualifiedColumnsStillResolve(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from orders o where o.", cat)
	if !has(r, "user_id") {
		t.Errorf("alias o. must resolve to public.orders columns (user_id); got %v", displays(r))
	}
	if has(r, "archived_at") {
		t.Errorf("alias o. (public.orders) leaked archive.orders column: %v", displays(r))
	}
}
