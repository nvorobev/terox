package complete

import "testing"

// Производные таблицы (подзапрос в FROM): `FROM (SELECT …) alias`. Раньше псевдоним
// и его выводимые колонки не попадали в область видимости — `alias.` не дополнялся.
// F5+ (этап B): разбор списка SELECT подзапроса.

func TestCompleteDerivedTableColumns(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from (select id, name from users) u where u.", cat)
	for _, col := range []string{"id", "name"} {
		if !has(r, col) {
			t.Errorf("derived table alias should offer subquery output %q; got %v", col, displays(r))
		}
	}
}

func TestCompleteDerivedTablePartialColumn(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from (select id, name from users) sub where sub.na", cat)
	if !has(r, "name") {
		t.Errorf("partial derived column should complete name; got %v", displays(r))
	}
	if has(r, "id") {
		t.Errorf("partial 'na' must not offer id; got %v", displays(r))
	}
}

func TestCompleteDerivedTableAsAlias(t *testing.T) {
	cat := sampleCatalog()
	// Явный AS задаёт имя выводимой колонки.
	r := Complete("select * from (select id as oid, user_id from orders) o where o.", cat)
	for _, col := range []string{"oid", "user_id"} {
		if !has(r, col) {
			t.Errorf("AS-named derived column %q expected; got %v", col, displays(r))
		}
	}
	if has(r, "id") {
		t.Errorf("aliased column must surface as oid, not id; got %v", displays(r))
	}
}

func TestCompleteDerivedTableExplicitColumnList(t *testing.T) {
	cat := sampleCatalog()
	// Явный список псевдонимов колонок `alias(c1, c2)` перекрывает имена из SELECT.
	r := Complete("select * from (select id, name from users) u(uid, uname) where u.", cat)
	for _, col := range []string{"uid", "uname"} {
		if !has(r, col) {
			t.Errorf("explicit column alias %q expected; got %v", col, displays(r))
		}
	}
	if has(r, "id") || has(r, "name") {
		t.Errorf("explicit column list must replace inner names; got %v", displays(r))
	}
}

func TestCompleteDerivedTableLateral(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from users u, lateral (select id from orders) o where o.", cat)
	if !has(r, "id") {
		t.Errorf("LATERAL derived table should offer id; got %v", displays(r))
	}
}

func TestCompleteDerivedTableSkipsExpressions(t *testing.T) {
	cat := sampleCatalog()
	// count(*) и звезда не дают выводимого имени; простая колонка — даёт.
	r := Complete("select * from (select id, count(*) from orders group by id) o where o.", cat)
	if !has(r, "id") {
		t.Errorf("simple column id expected from derived table; got %v", displays(r))
	}
	if has(r, "count") {
		t.Errorf("expression count(*) must not become a derived column; got %v", displays(r))
	}
}
