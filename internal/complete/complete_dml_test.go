package complete

import "testing"

// Эти контексты раньше были «мёртвыми ветками»: classify их объявлял, но
// currentClause никогда до них не доходил (insert-cols — курсор на глубине скобок;
// using/merge — отношение после USING). F5.

func TestCompleteInsertColumnList(t *testing.T) {
	cat := sampleCatalog()
	// Пустой список столбцов: предлагаются столбцы целевой таблицы.
	r := Complete("insert into orders (", cat)
	for _, col := range []string{"id", "created_at", "user_id"} {
		if !has(r, col) {
			t.Errorf("insert column list should offer %q; got %v", col, displays(r))
		}
	}
	// Не должно предлагать столбцы чужой таблицы.
	if has(r, "name") {
		t.Errorf("insert into orders(...) must not offer users.name; got %v", displays(r))
	}
	// Частичный второй столбец после запятой.
	r2 := Complete("insert into orders (id, cr", cat)
	if !has(r2, "created_at") {
		t.Errorf("partial second column should complete created_at; got %v", displays(r2))
	}
}

func TestCompleteInsertColumnListSchemaQualified(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("insert into archive.orders (", cat)
	if !has(r, "archived_at") {
		t.Errorf("insert into archive.orders(...) should offer archive.orders columns; got %v", displays(r))
	}
}

func TestCompleteDeleteUsingRelation(t *testing.T) {
	cat := sampleCatalog()
	// DELETE ... USING <отношение> — позиция таблицы, а не столбцов.
	r := Complete("delete from orders using us", cat)
	if !has(r, "users") {
		t.Errorf("DELETE ... USING should offer relations (users); got %v", displays(r))
	}
}

func TestCompleteDeleteUsingThenWhereColumns(t *testing.T) {
	cat := sampleCatalog()
	// После USING <rel> отношение попадает в scope, и WHERE <rel>.col работает.
	r := Complete("delete from orders o using users u where u.", cat)
	if !has(r, "name") {
		t.Errorf("alias from USING should resolve columns (users.name); got %v", displays(r))
	}
}

func TestCompleteMergeUsingRelation(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("merge into orders using us", cat)
	if !has(r, "users") {
		t.Errorf("MERGE ... USING should offer relations (users); got %v", displays(r))
	}
}

func TestCompleteReturningColumns(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("delete from orders where id = 1 returning ", cat)
	for _, col := range []string{"id", "created_at"} {
		if !has(r, col) {
			t.Errorf("RETURNING should offer target columns (%q); got %v", col, displays(r))
		}
	}
}

// JOIN ... USING (cols) НЕ должен предлагать отношения — это список столбцов.
func TestCompleteJoinUsingIsColumns(t *testing.T) {
	cat := sampleCatalog()
	r := Complete("select * from orders o join users u using (", cat)
	// Должны быть столбцы соединяемых отношений, а не таблицы.
	if has(r, "users") || has(r, "orders") {
		t.Errorf("JOIN ... USING (cols) must not offer relations; got %v", displays(r))
	}
	if !has(r, "id") {
		t.Errorf("JOIN ... USING (cols) should offer columns (id); got %v", displays(r))
	}
}
