package repl

import (
	"fmt"
	"strings"

	"terox/internal/complete"
	"terox/internal/diag"
	"terox/internal/ui"
)

// doLint реализует \lint <sql> — статическая диагностика ДО выполнения (без БД):
// безусловная запись, LIMIT без ORDER BY, NOT IN с nullable, SELECT *, возможное
// декартово произведение, несколько запросов. Это ЭВРИСТИКИ с уровнем уверенности,
// не замена EXPLAIN. Если уже загружен снимок каталога — добавляются
// каталого-зависимые находки (ссылки на неизвестные отношения/схемы, Feature 6+).
func (r *REPL) doLint(raw string) error {
	query := strings.TrimSpace(rawTail(raw, 1))
	if query == "" {
		return fmt.Errorf("usage: \\lint <sql> — static pre-execution diagnostics (no DB)")
	}
	ds := diag.Analyze(query)
	rel := complete.LintRelations(query, r.completeCatalog())
	if len(ds) == 0 && len(rel) == 0 {
		fmt.Fprintln(r.out, "no static issues found (heuristic check — not a guarantee; run \\explain for a measured plan)")
		return nil
	}
	for _, ri := range rel {
		msg := fmt.Sprintf("  [error/high] %s %q not found in catalog", lintReason(ri.Reason), ri.Qualified())
		fmt.Fprintln(r.out, ui.Danger.Render(msg))
		fmt.Fprintf(r.out, "     fix:      check the name/schema, or reload the catalog (\\completion reload) if it changed\n")
	}
	for _, d := range ds {
		head := fmt.Sprintf("  [%s/%s] %s", d.Severity, d.Confidence, d.Message)
		switch d.Severity {
		case diag.Error:
			fmt.Fprintln(r.out, ui.Danger.Render(head))
		default:
			fmt.Fprintln(r.out, head)
		}
		if d.Evidence != "" {
			fmt.Fprintf(r.out, "     evidence: %s\n", d.Evidence)
		}
		if d.Suggestion != "" {
			fmt.Fprintf(r.out, "     fix:      %s\n", d.Suggestion)
		}
	}
	fmt.Fprintln(r.out, ui.Dim.Render("static heuristics — not a substitute for EXPLAIN or actually running the query"))
	return nil
}

// lintReason переводит код находки каталого-зависимого линта в человекочитаемую
// часть сообщения.
func lintReason(code string) string {
	switch code {
	case "unknown-schema":
		return "schema of"
	default:
		return "relation"
	}
}
