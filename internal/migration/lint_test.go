package migration

import (
	"strings"
	"testing"
)

func lintMessages(script string) string {
	var b strings.Builder
	for _, f := range Lint(script) {
		b.WriteString(f.Severity + ":" + f.Message + "\n")
	}
	return b.String()
}

func TestLintFlagsRiskyPatterns(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{"add-notnull-no-default", "ALTER TABLE t ADD COLUMN c int NOT NULL;", "ADD COLUMN NOT NULL without DEFAULT"},
		{"create-index-no-concurrently", "CREATE INDEX i ON t (a);", "CREATE INDEX without CONCURRENTLY"},
		{"add-fk-no-notvalid", "ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (a) REFERENCES o(id);", "ADD FOREIGN KEY without NOT VALID"},
		{"add-check-no-notvalid", "ALTER TABLE t ADD CONSTRAINT ck CHECK (a > 0);", "ADD CHECK constraint without NOT VALID"},
		{"alter-type-rewrite", "ALTER TABLE t ALTER COLUMN a TYPE bigint;", "ALTER COLUMN ... TYPE rewrites the table"},
		{"drop-no-if-exists", "DROP TABLE t;", "DROP without IF EXISTS"},
		{"truncate", "TRUNCATE TABLE t;", "TRUNCATE irreversibly discards all rows"},
		{"truncate-bare", "TRUNCATE t;", "TRUNCATE irreversibly discards all rows"},
		{"drop-column", "ALTER TABLE t DROP COLUMN c;", "ALTER TABLE ... DROP COLUMN is irreversible"},
		{"drop-schema", "DROP SCHEMA app CASCADE;", "DROP SCHEMA cascades into every contained object"},
		{"drop-database", "DROP DATABASE app;", "DROP DATABASE destroys an entire database"},
		{"rename-table", "ALTER TABLE t RENAME TO t2;", "ALTER ... RENAME breaks running deployments"},
		{"rename-column", "ALTER TABLE t RENAME COLUMN a TO b;", "ALTER ... RENAME breaks running deployments"},
	}
	for _, c := range cases {
		if got := lintMessages(c.sql); !strings.Contains(got, c.want) {
			t.Errorf("%s: Lint(%q) missing %q; got:\n%s", c.name, c.sql, c.want, got)
		}
	}
}

func TestLintAllowsSafePatterns(t *testing.T) {
	safe := []string{
		"ALTER TABLE t ADD COLUMN c int NOT NULL DEFAULT 0;",
		"CREATE INDEX CONCURRENTLY i ON t (a);",
		"ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (a) REFERENCES o(id) NOT VALID;",
		"ALTER TABLE t ADD CONSTRAINT ck CHECK (a > 0) NOT VALID;",
		"DROP TABLE IF EXISTS t;",
		"UPDATE t SET x = 1 WHERE id = 1;",
		"ALTER TABLE t ALTER COLUMN type SET NOT NULL;",
		"ALTER TABLE t ALTER COLUMN type DROP DEFAULT;",
	}
	for _, s := range safe {
		if fs := Lint(s); len(fs) != 0 {
			t.Errorf("Lint(%q) should be clean, got %+v", s, fs)
		}
	}
}

// TestLintDestructiveRulesPrecise: каждое новое destructive-правило не должно
// срабатывать на безопасном аналоге (нет ложных срабатываний по подстроке).
func TestLintDestructiveRulesPrecise(t *testing.T) {
	cases := []struct {
		name, sql, mustNotContain string
	}{
		// DELETE — не TRUNCATE.
		{"delete-not-truncate", "DELETE FROM t WHERE id = 1;", "TRUNCATE irreversibly discards all rows"},
		// DROP DEFAULT / DROP NOT NULL — не DROP COLUMN.
		{"drop-default-not-column", "ALTER TABLE t ALTER COLUMN a DROP DEFAULT;", "ALTER TABLE ... DROP COLUMN is irreversible"},
		{"drop-notnull-not-column", "ALTER TABLE t ALTER COLUMN a DROP NOT NULL;", "ALTER TABLE ... DROP COLUMN is irreversible"},
		// CREATE SCHEMA — не DROP SCHEMA.
		{"create-schema-not-drop", "CREATE SCHEMA app;", "DROP SCHEMA cascades into every contained object"},
		// DROP TABLE — это не DROP SCHEMA и не DROP DATABASE.
		{"drop-table-not-schema", "DROP TABLE app;", "DROP SCHEMA cascades into every contained object"},
		{"drop-table-not-database", "DROP TABLE app;", "DROP DATABASE destroys an entire database"},
		// ALTER без RENAME — не правило переименования.
		{"alter-add-not-rename", "ALTER TABLE t ADD COLUMN c int DEFAULT 0;", "ALTER ... RENAME breaks running deployments"},
	}
	for _, c := range cases {
		if got := lintMessages(c.sql); strings.Contains(got, c.mustNotContain) {
			t.Errorf("%s: Lint(%q) must NOT contain %q; got:\n%s", c.name, c.sql, c.mustNotContain, got)
		}
	}
}

// TestLintMaskingIgnoresStrings: ключевые слова внутри строкового литерала не должны
// триггерить правила (Mask их вычищает).
func TestLintMaskingIgnoresStrings(t *testing.T) {
	if fs := Lint("INSERT INTO audit(msg) VALUES ('CREATE INDEX foo on bar');"); len(fs) != 0 {
		t.Errorf("keyword inside a string literal must not trigger lint, got %+v", fs)
	}
}
