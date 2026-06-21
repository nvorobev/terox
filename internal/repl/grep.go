package repl

import (
	"fmt"
	"strings"

	"terox/internal/render"
)

// doGrep (\grep [-v] <pattern>) фильтрует строки последнего результата по подстроке
// (без учёта регистра) БЕЗ перезапроса БД — работает над r.lastRows/r.lastCols в
// памяти. Совпадение ищется в любой ячейке строки; -v инвертирует (строки БЕЗ
// совпадения). Удобно после fan-out по многим строкам, когда нужен один id/маркер.
func (r *REPL) doGrep(args []string) error {
	if len(r.lastCols) == 0 {
		return fmt.Errorf("nothing to filter — run a query first")
	}
	invert := false
	if len(args) > 0 && args[0] == "-v" {
		invert = true
		args = args[1:]
	}
	pat := strings.ToLower(strings.TrimSpace(strings.Join(args, " ")))
	if pat == "" {
		return fmt.Errorf("usage: \\grep [-v] <pattern>  — filter the last result in memory")
	}
	matched := make([][]any, 0, len(r.lastRows))
	for _, row := range r.lastRows {
		hit := false
		for _, cell := range row {
			if strings.Contains(strings.ToLower(str(cell)), pat) {
				hit = true
				break
			}
		}
		if hit != invert { // invert=true оставляет строки без совпадения
			matched = append(matched, row)
		}
	}
	footer := fmt.Sprintf("%d of %d rows match", len(matched), len(r.lastRows))
	if r.lastTruncated {
		footer += " — of the fetched rows (the full result was truncated; raise \\maxrows)"
	}
	render.AnyTable(r.out, r.lastCols, matched, footer)
	return nil
}
