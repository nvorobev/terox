package repl

import (
	"strings"
	"testing"

	"terox/internal/config"
)

func TestRoleIsStorageDriven(t *testing.T) {
	// Роль для записи — строго настроенная роль хранилища.
	r := &REPL{cfg: &config.Config{}, migrationRole: "store_w"}
	if got := r.role(); got != "store_w" {
		t.Errorf("role should be the storage's migration_role, got %q", got)
	}
	// Без настроенной роли роль не выдаётся, даже на prod-хранилище (роль задаётся
	// на каждое хранилище, не выводится из prod).
	r = &REPL{cfg: &config.Config{}, prod: true}
	if got := r.role(); got != "" {
		t.Errorf("an unconfigured role must stay empty even on prod, got %q", got)
	}
}

func TestParseTableArg(t *testing.T) {
	ok := map[string]string{
		"users":         "users",
		"items.users":   "items.users",
		`"My Table"`:    `"My Table"`,
		`public."User"`: `public."User"`,
		"select":        `"select"`, // зарезервированное слово берётся в кавычки
	}
	for in, want := range ok {
		got, err := parseTableArg(in)
		if err != nil {
			t.Errorf("parseTableArg(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseTableArg(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"users; drop table users",
		"users WHERE 1=1",
		"users(",
		"1users",
		"a.b.c",
		"",
		"items.",
		`"unterminated`,
	}
	for _, in := range bad {
		if _, err := parseTableArg(in); err == nil {
			t.Errorf("parseTableArg(%q) should have errored", in)
		}
	}
}

func TestBuildCountQuery(t *testing.T) {
	r := &REPL{}
	// Обычная таблица и безопасное условие.
	q, err := r.buildCountQuery([]string{"items.users", "id", "=", "5"})
	if err != nil {
		t.Fatal(err)
	}
	if q != "SELECT count(*) AS count FROM items.users WHERE id = 5" {
		t.Errorf("unexpected query: %q", q)
	}

	// Префикс "where" отбрасывается.
	q, err = r.buildCountQuery([]string{"t", "where", "x", ">", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(q, "WHERE x > 0") {
		t.Errorf("where not handled: %q", q)
	}

	// Разделитель операторов в условии отвергается.
	if _, err := r.buildCountQuery([]string{"t", "1=1; drop table t"}); err == nil {
		t.Error("multi-statement condition should be rejected")
	}
	// ';' внутри строкового литерала разрешён (это не разделитель).
	if _, err := r.buildCountQuery([]string{"t", "name", "=", "'a;b'"}); err != nil {
		t.Errorf("';' inside a literal should be allowed: %v", err)
	}
	// Инъекция в имя таблицы отвергается до построения SQL.
	if _, err := r.buildCountQuery([]string{"t; drop table t"}); err == nil {
		t.Error("injected table should be rejected")
	}
}

func TestCountArgsPreservesWhereSpacing(t *testing.T) {
	r := &REPL{}
	// Двойные пробелы внутри строкового литерала в WHERE сохраняются дословно.
	args, err := r.countArgs([]string{"users", "where", "name", "=", "'a  b'"}, `\count users where name = 'a  b'`)
	if err != nil {
		t.Fatal(err)
	}
	q, err := r.buildCountQuery(args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "'a  b'") {
		t.Errorf("double space in literal collapsed: %q", q)
	}
	// Без WHERE — только таблица, без условия.
	args, err = r.countArgs([]string{"users"}, `\count users`)
	if err != nil {
		t.Fatal(err)
	}
	q, err = r.buildCountQuery(args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q, "WHERE") {
		t.Errorf("unexpected WHERE for table-only \\count: %q", q)
	}
}

func TestExplainSQLForVersionGating(t *testing.T) {
	// PostgreSQL 11 (110000): без SETTINGS (с 12) и без WAL (с 13).
	if got := explainSQLFor(explainOpts{}, 110000, "select 1"); strings.Contains(got, "SETTINGS") {
		t.Errorf("PG11 estimate must not include SETTINGS: %q", got)
	}
	if got := explainSQLFor(explainOpts{analyze: true}, 110000, "select 1"); strings.Contains(got, "SETTINGS") || strings.Contains(got, "WAL") {
		t.Errorf("PG11 analyze must not include SETTINGS/WAL: %q", got)
	}
	// PostgreSQL 12 (120000): SETTINGS есть, WAL нет.
	got := explainSQLFor(explainOpts{analyze: true}, 120000, "select 1")
	if !strings.Contains(got, "SETTINGS") || strings.Contains(got, "WAL") {
		t.Errorf("PG12 analyze should have SETTINGS but not WAL: %q", got)
	}
	// PostgreSQL 13+ (130000): оба.
	got = explainSQLFor(explainOpts{analyze: true}, 130000, "select 1")
	if !strings.Contains(got, "SETTINGS") || !strings.Contains(got, "WAL") {
		t.Errorf("PG13 analyze should have SETTINGS and WAL: %q", got)
	}
	// Неизвестная версия (0): версионные опции опускаются, чтобы EXPLAIN не падал
	// на старом сервере.
	if got := explainSQLFor(explainOpts{analyze: true}, 0, "select 1"); strings.Contains(got, "SETTINGS") || strings.Contains(got, "WAL") {
		t.Errorf("unknown version must omit SETTINGS/WAL: %q", got)
	}
	// PostgreSQL 17 (170000): SERIALIZE/MEMORY при analyze, если запрошены.
	got17 := explainSQLFor(explainOpts{analyze: true, serialize: true, memory: true}, 170000, "select 1")
	if !strings.Contains(got17, "SERIALIZE TEXT") || !strings.Contains(got17, "MEMORY") {
		t.Errorf("PG17 analyze --serialize --memory should include both: %q", got17)
	}
	// До 17 — опускаются.
	if got := explainSQLFor(explainOpts{analyze: true, serialize: true, memory: true}, 160000, "select 1"); strings.Contains(got, "SERIALIZE") || strings.Contains(got, "MEMORY") {
		t.Errorf("pre-17 must omit SERIALIZE/MEMORY: %q", got)
	}
	// GENERIC_PLAN (16+) без analyze.
	if got := explainSQLFor(explainOpts{genericPlan: true}, 160000, "select 1"); !strings.Contains(got, "GENERIC_PLAN") {
		t.Errorf("PG16 --generic-plan should include GENERIC_PLAN: %q", got)
	}
}

func TestParseDMLRejectsMultiRelation(t *testing.T) {
	// UPDATE..FROM / DELETE..USING нельзя превратить в точный count(*)-предпросмотр,
	// поэтому parseDML их отклоняет.
	for _, sql := range []string{
		"update t set x = o.v from other o where o.id = t.id",
		"delete from t using other o where o.id = t.id",
	} {
		if _, _, ok := parseDML(sql); ok {
			t.Errorf("parseDML(%q) should decline a multi-relation DML", sql)
		}
	}
	// Простой UPDATE/DELETE разбирается (предпросмотр имеет смысл). FROM внутри
	// подзапроса не считается клаузой верхнего уровня.
	for _, sql := range []string{
		"update t set x = 1 where id = 5",
		"delete from t where id = 5",
		"update t set x = (select v from other where id = t.id) where id = 5",
	} {
		if _, _, ok := parseDML(sql); !ok {
			t.Errorf("parseDML(%q) should parse a single-relation DML", sql)
		}
	}
}

func TestParseDMLAliasAndWhitespace(t *testing.T) {
	// Алиас таблицы сохраняется (иначе WHERE с "f.*" не резолвился бы в предпросмотре),
	// а ведущий глагол распознаётся через любой пробел, включая перевод строки.
	for _, tc := range []struct{ sql, table, where string }{
		{"update foo as f set x=1 where f.id=2", "foo as f", "f.id=2"},
		{"delete from foo f where f.id=2", "foo f", "f.id=2"},
		{"update\nfoo\nset x=1 where id=3", "foo", "id=3"},
		{"DELETE\nFROM foo WHERE id=4", "foo", "id=4"},
		{"update only foo set x=1", "only foo", ""},
	} {
		table, where, ok := parseDML(tc.sql)
		if !ok {
			t.Errorf("parseDML(%q) ok=false, want parse", tc.sql)
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(table), tc.table) {
			t.Errorf("parseDML(%q) table=%q, want %q", tc.sql, table, tc.table)
		}
		if strings.TrimSpace(where) != tc.where {
			t.Errorf("parseDML(%q) where=%q, want %q", tc.sql, where, tc.where)
		}
	}
}

func TestHasTopLevelClause(t *testing.T) {
	if !hasTopLevelClause("update t set x=1 from o where o.id=t.id", "from") {
		t.Error("top-level FROM not detected")
	}
	if hasTopLevelClause("update t set x=(select v from o) where id=1", "from") {
		t.Error("FROM inside a subquery must not count as top-level")
	}
	if hasTopLevelClause("update t set fromage=1 where id=1", "from") {
		t.Error("'from' inside an identifier must not match")
	}
}

func TestParseOnOffStrictArity(t *testing.T) {
	if _, err := parseOnOff([]string{"on", "extra"}, false); err == nil {
		t.Error("parseOnOff must reject extra arguments (CLI-01)")
	}
	if v, err := parseOnOff([]string{"on"}, false); err != nil || !v {
		t.Errorf("parseOnOff([on]) = %v,%v", v, err)
	}
	if v, err := parseOnOff(nil, false); err != nil || !v {
		t.Errorf("parseOnOff(nil) should toggle to true, got %v,%v", v, err)
	}
}

func TestIsSensitiveStatement(t *testing.T) {
	for _, sql := range []string{
		"ALTER USER app WITH PASSWORD 'secret'",
		"create role r login password 'p'",
		"SET PASSWORD TO 'x'",
	} {
		if !isSensitiveStatement(sql) {
			t.Errorf("isSensitiveStatement(%q) should be true", sql)
		}
	}
	if isSensitiveStatement("select * from users where id = 5") {
		t.Error("an ordinary read must not be flagged sensitive")
	}
}

func TestMetaSQLTail(t *testing.T) {
	c := &completer{}
	cases := []struct {
		head     string
		wantOK   bool
		contains string
	}{
		{`\watch 5s select id from t`, true, "select id from t"},
		{`\watch select id from t`, true, "select id from t"},
		{`\save q select id from t`, true, "select id from t"},
		{`\save q`, false, ""}, // имя ещё вводится
		{`\count users where st`, true, "select * from users where st"},
		{`\count users`, false, ""}, // токен таблицы ещё вводится
		{`\dt`, false, ""},          // не команда с SQL-хвостом
	}
	for _, tc := range cases {
		got, ok := c.metaSQLTail(tc.head)
		if ok != tc.wantOK {
			t.Errorf("metaSQLTail(%q) ok=%v, want %v (got %q)", tc.head, ok, tc.wantOK, got)
			continue
		}
		if ok && !strings.Contains(got, tc.contains) {
			t.Errorf("metaSQLTail(%q) = %q, want to contain %q", tc.head, got, tc.contains)
		}
	}
}

func TestValidBaselineName(t *testing.T) {
	for _, ok := range []string{"baseline", "user-q1", "plan.v2", "a_b"} {
		if err := validBaselineName(ok); err != nil {
			t.Errorf("validBaselineName(%q) unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"../etc/passwd", "/abs", "a/b", "..", ".hidden", "", `a\b`} {
		if err := validBaselineName(bad); err == nil {
			t.Errorf("validBaselineName(%q) should have errored", bad)
		}
	}
}
