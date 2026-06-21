package repl

import (
	"fmt"

	"terox/internal/migration"
	"terox/internal/ui"
)

// printMigrationLint печатает результаты migration-aware линтера (\migrate --check и
// в составе dry-run): опасные при онлайн-миграции паттерны (блокировки, переписывание
// таблицы, неидемпотентность). Возвращает число замечаний.
func (r *REPL) printMigrationLint(content string) int {
	findings := migration.Lint(content)
	if len(findings) == 0 {
		fmt.Fprintln(r.out, ui.Dim.Render("migration lint: no risky online-migration patterns (heuristic, no DB)"))
		return 0
	}
	fmt.Fprintln(r.out, ui.Service.Render("migration lint:"))
	for _, f := range findings {
		head := fmt.Sprintf("  [%s] %s — %s", f.Severity, f.Stmt, f.Message)
		if f.Severity == "warning" {
			fmt.Fprintln(r.out, ui.Danger.Render(head))
		} else {
			fmt.Fprintln(r.out, head)
		}
		fmt.Fprintf(r.out, "     fix: %s\n", f.Hint)
	}
	fmt.Fprintln(r.out, ui.Dim.Render("static heuristics — not a substitute for testing the migration"))
	return len(findings)
}
