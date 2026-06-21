package complete

import "testing"

func issueNames(is []RelationIssue) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Qualified())
	}
	return out
}

func hasIssue(is []RelationIssue, qualified string) bool {
	for _, i := range is {
		if i.Qualified() == qualified {
			return true
		}
	}
	return false
}

func TestLintRelationsUnknown(t *testing.T) {
	cat := sampleCatalog()
	is := LintRelations("select * from no_such_table", cat)
	if !hasIssue(is, "no_such_table") {
		t.Errorf("unknown relation should be flagged; got %v", issueNames(is))
	}
}

func TestLintRelationsKnownClean(t *testing.T) {
	cat := sampleCatalog()
	if is := LintRelations("select * from users u join orders o on o.user_id = u.id", cat); len(is) != 0 {
		t.Errorf("known relations must not be flagged; got %v", issueNames(is))
	}
}

func TestLintRelationsUnknownSchema(t *testing.T) {
	cat := sampleCatalog()
	is := LintRelations("select * from nope.orders", cat)
	if !hasIssue(is, "nope.orders") {
		t.Errorf("unknown schema should be flagged; got %v", issueNames(is))
	}
	if is[0].Reason != "unknown-schema" {
		t.Errorf("reason should be unknown-schema; got %q", is[0].Reason)
	}
}

func TestLintRelationsSchemaQualifiedKnown(t *testing.T) {
	cat := sampleCatalog()
	if is := LintRelations("select * from archive.orders", cat); len(is) != 0 {
		t.Errorf("archive.orders exists and must not be flagged; got %v", issueNames(is))
	}
}

func TestLintRelationsSkipsCTE(t *testing.T) {
	cat := sampleCatalog()
	// CTE-имя не существует в каталоге, но ссылаться на него законно — не флагуем.
	sql := "with recent as (select * from orders) select * from recent"
	if is := LintRelations(sql, cat); len(is) != 0 {
		t.Errorf("CTE reference must not be flagged; got %v", issueNames(is))
	}
}

func TestLintRelationsSkipsDerived(t *testing.T) {
	cat := sampleCatalog()
	sql := "select * from (select id from users) sub where sub.id = 1"
	if is := LintRelations(sql, cat); len(is) != 0 {
		t.Errorf("derived-table alias must not be flagged; got %v", issueNames(is))
	}
}

func TestLintRelationsSkipsTableFunction(t *testing.T) {
	cat := sampleCatalog()
	// generate_series — табличная функция, не отношение; не флагуем.
	if is := LintRelations("select * from generate_series(1, 10) g", cat); len(is) != 0 {
		t.Errorf("table function must not be flagged as unknown relation; got %v", issueNames(is))
	}
}

func TestLintRelationsEmptyCatalog(t *testing.T) {
	// Нет снимка каталога — не диагностируем ничего (нечем сверять).
	if is := LintRelations("select * from whatever", &Catalog{}); len(is) != 0 {
		t.Errorf("empty catalog must produce no issues; got %v", issueNames(is))
	}
	if is := LintRelations("select * from whatever", nil); len(is) != 0 {
		t.Errorf("nil catalog must produce no issues; got %v", issueNames(is))
	}
}
