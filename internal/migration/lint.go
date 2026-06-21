package migration

import (
	"regexp"
	"strings"

	"terox/internal/sqlsplit"
)

// LintFinding — одно замечание migration-aware линтера (статика, без БД).
type LintFinding struct {
	Severity string // warning|info
	Stmt     string // короткая метка оператора
	Message  string
	Hint     string // как переписать безопаснее
}

// migrationLintRule — правило: применяется к замаскированному оператору.
//
// Известное ограничение эвристики: предикаты when матчат ВЕСЬ оператор целиком,
// поэтому в многооператорном ALTER (несколько действий через запятую) безопасный
// модификатор у одной части глушит предупреждение для соседней. Например в
// "ALTER TABLE t ADD CONSTRAINT c CHECK (...) NOT VALID, DROP COLUMN x" наличие
// NOT VALID/DEFAULT в одной части видно правилу так же, как и DROP COLUMN в
// другой, и пересечение их признаков может скрыть замечание. Правила
// рассчитаны на типовой однодейственный ALTER на оператор.
type migrationLintRule struct {
	severity string
	when     func(masked string) bool
	message  string
	hint     string
}

var (
	reAddColumn       = regexp.MustCompile(`(?is)\badd\s+column\b`)
	reNotNull         = regexp.MustCompile(`(?is)\bnot\s+null\b`)
	reDefault         = regexp.MustCompile(`(?is)\bdefault\b`)
	reCreateIndex     = regexp.MustCompile(`(?is)\bcreate\s+(unique\s+)?index\b`)
	reConcurrently    = regexp.MustCompile(`(?is)\bconcurrently\b`)
	reAddForeignKey   = regexp.MustCompile(`(?is)\badd\s+(constraint\s+\S+\s+)?foreign\s+key\b`)
	reAddCheck        = regexp.MustCompile(`(?is)\badd\s+(constraint\s+\S+\s+)?check\b`)
	reNotValid        = regexp.MustCompile(`(?is)\bnot\s+valid\b`)
	reDropObject      = regexp.MustCompile(`(?is)^\s*drop\s+(table|index|view|materialized\s+view|sequence|type)\b`)
	reIfExists        = regexp.MustCompile(`(?is)\bif\s+exists\b`)
	reAlterColumnType = regexp.MustCompile(`(?is)\balter\s+column\b\s+("[^"]+"|\S+)\s+(set\s+data\s+type|type)\b`)
	reTruncate        = regexp.MustCompile(`(?is)^\s*truncate\b`)
	reDropColumn      = regexp.MustCompile(`(?is)\bdrop\s+column\b`)
	reDropSchema      = regexp.MustCompile(`(?is)^\s*drop\s+schema\b`)
	reDropDatabase    = regexp.MustCompile(`(?is)^\s*drop\s+database\b`)
	reRename          = regexp.MustCompile(`(?is)^\s*alter\s+\S.*\brename\b`)
)

var migrationLintRules = []migrationLintRule{
	{
		severity: "warning",
		when: func(m string) bool {
			return reAddColumn.MatchString(m) && reNotNull.MatchString(m) && !reDefault.MatchString(m)
		},
		message: "ADD COLUMN NOT NULL without DEFAULT",
		hint:    "fails on a non-empty table; add a DEFAULT (a constant default is metadata-only on PostgreSQL 11+) or backfill then SET NOT NULL",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reCreateIndex.MatchString(m) && !reConcurrently.MatchString(m) },
		message:  "CREATE INDEX without CONCURRENTLY",
		hint:     "locks the table against writes while it builds; use CREATE INDEX CONCURRENTLY (in a separate, non-wrapped file)",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reAddForeignKey.MatchString(m) && !reNotValid.MatchString(m) },
		message:  "ADD FOREIGN KEY without NOT VALID",
		hint:     "scans and locks both tables to validate existing rows; ADD ... NOT VALID, then VALIDATE CONSTRAINT in a later step",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reAddCheck.MatchString(m) && !reNotValid.MatchString(m) },
		message:  "ADD CHECK constraint without NOT VALID",
		hint:     "scans the whole table under lock; ADD CONSTRAINT ... CHECK (...) NOT VALID, then VALIDATE CONSTRAINT separately",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reAlterColumnType.MatchString(m) },
		message:  "ALTER COLUMN ... TYPE rewrites the table",
		hint:     "a full table rewrite under an exclusive lock; verify it is intended and sized for the maintenance window",
	},
	{
		severity: "info",
		when:     func(m string) bool { return reDropObject.MatchString(m) && !reIfExists.MatchString(m) },
		message:  "DROP without IF EXISTS",
		hint:     "not idempotent — a re-run (or resume after a partial rollout) fails; add IF EXISTS",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reTruncate.MatchString(m) },
		message:  "TRUNCATE irreversibly discards all rows",
		hint:     "instant, unlogged-style data loss and takes an ACCESS EXCLUSIVE lock; double-check it is intended, or DELETE in batches if rows must be removed online",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reDropColumn.MatchString(m) },
		message:  "ALTER TABLE ... DROP COLUMN is irreversible",
		hint:     "the column and its data are gone for good; confirm no deployed code still reads it, and consider keeping it (nullable/unused) until the old code is fully rolled out",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reDropSchema.MatchString(m) },
		message:  "DROP SCHEMA cascades into every contained object",
		hint:     "with CASCADE it destroys all tables/objects inside; verify the schema is empty (or intended for removal), and prefer IF EXISTS so a resumed rollout stays idempotent",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reDropDatabase.MatchString(m) },
		message:  "DROP DATABASE destroys an entire database",
		hint:     "catastrophic and irreversible; this almost never belongs in a migration — confirm the target and run it out-of-band against the right cluster",
	},
	{
		severity: "warning",
		when:     func(m string) bool { return reRename.MatchString(m) },
		message:  "ALTER ... RENAME breaks running deployments",
		hint:     "old code referencing the previous name fails immediately; add the new name alongside the old (view/column) and cut over after the deploy, instead of renaming in place",
	},
}

// commandLabel строит читаемую метку команды из ведущих слов (+CONCURRENTLY) для
// замечаний линтера.
func commandLabel(clean string) string {
	words := strings.Fields(strings.ToLower(clean))
	if len(words) == 0 {
		return "(empty)"
	}
	label := strings.ToUpper(trimWord(words[0]))
	if len(words) > 1 {
		switch words[0] {
		case "create", "drop", "alter", "refresh", "set", "reset", "reindex", "discard", "lock", "prepare":
			label += " " + strings.ToUpper(trimWord(words[1]))
		}
	}
	if concurrentlyRe.MatchString(clean) {
		label += " CONCURRENTLY"
	}
	return label
}

// Lint прогоняет migration-aware статические правила по каждому оператору скрипта и
// возвращает замечания о паттернах, опасных при онлайн-миграции (блокировки,
// переписывание таблицы, неидемпотентность). БД не требуется; это эвристика.
func Lint(script string) []LintFinding {
	var out []LintFinding
	for _, stmt := range sqlsplit.Split(script) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		masked := sqlsplit.Mask(stmt)
		label := commandLabel(strings.TrimSpace(masked))
		for _, rule := range migrationLintRules {
			if rule.when(masked) {
				out = append(out, LintFinding{Severity: rule.severity, Stmt: label, Message: rule.message, Hint: rule.hint})
			}
		}
	}
	return out
}
