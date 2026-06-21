// Package diag — статический диагностический движок по тексту SQL (Feature 6).
// Возвращает находки с СТАБИЛЬНЫМ кодом, серьёзностью, ДИАПАЗОНОМ-источником (байтовые
// смещения в исходном SQL), сообщением, доказательством, предложением и уровнем
// уверенности. ВАЖНО: проверки — ЭВРИСТИКИ (никогда не выдаём догадку за доказанный
// факт); каталого-зависимые проверки (unknown column, relation вне search_path,
// object не на всех шардах) — точка расширения, требующая снимка каталога.
package diag

import (
	"regexp"
	"strings"

	"terox/internal/safety"
	"terox/internal/sqlsplit"
)

// Severity — серьёзность находки.
type Severity int

const (
	Info Severity = iota
	Warning
	Error
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "error"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// Diagnostic — одна статическая находка. Start/End — байтовые смещения в ИСХОДНОМ
// SQL (для подсветки в редакторе); Confidence — high|medium|low (эвристика).
type Diagnostic struct {
	Code       string
	Severity   Severity
	Start, End int
	Message    string
	Evidence   string
	Suggestion string
	Confidence string
}

var (
	reUpdateDelete = regexp.MustCompile(`(?i)\b(update|delete\s+from|truncate)\b`)
	reLimit        = regexp.MustCompile(`(?i)\blimit\b`)
	reOrderBy      = regexp.MustCompile(`(?i)\border\s+by\b`)
	reNotIn        = regexp.MustCompile(`(?i)\bnot\s+in\s*\(`)
	reSelectStar   = regexp.MustCompile(`(?i)\bselect\s+\*`)
	reFrom         = regexp.MustCompile(`(?i)\bfrom\b`)
	reWhere        = regexp.MustCompile(`(?i)\bwhere\b`)
	reJoin         = regexp.MustCompile(`(?i)\bjoin\b`)
	reSubselect    = regexp.MustCompile(`(?i)\bnot\s+in\s*\(\s*select\b`)
)

// Analyze прогоняет статические правила по SQL и возвращает находки, отсортированные
// по позиции. Литералы/комментарии нейтрализуются общим лексером (sqlsplit.Mask,
// сохраняет длину), поэтому ключевые слова внутри строк не дают ложных срабатываний,
// а смещения совпадают с исходным SQL.
func Analyze(sql string) []Diagnostic {
	var out []Diagnostic
	masked := sqlsplit.Mask(sql) // длина сохраняется → смещения валидны в исходнике

	// Несколько top-level запросов в одном вводе — частый источник недоразумений.
	if stmts := sqlsplit.Split(sql); len(stmts) > 1 {
		out = append(out, Diagnostic{
			Code: "multi-statement", Severity: Info, Start: 0, End: 0,
			Message:    "input contains multiple statements",
			Evidence:   "found " + itoa(len(stmts)) + " top-level statements",
			Suggestion: "run them one at a time if you expected a single result set",
			Confidence: "high",
		})
	}

	// Безусловная запись (UPDATE/DELETE без WHERE, TRUNCATE) — затрагивает ВСЕ строки.
	if safety.AnyUnqualifiedWrite(sql) {
		// Подсветку привязываем к КОНКРЕТНОМУ безусловному оператору, а не к первому
		// UPDATE/DELETE во всём вводе: в мульти-операторном SQL первый из них может
		// быть безопасным (с WHERE), а без WHERE — следующий.
		s, e := 0, 0
		for _, rg := range stmtRanges(masked) {
			if !safety.IsUnqualifiedWrite(sql[rg[0]:rg[1]]) {
				continue
			}
			if loc := reUpdateDelete.FindStringIndex(masked[rg[0]:rg[1]]); loc != nil {
				s, e = rg[0]+loc[0], rg[0]+loc[1]
			}
			break
		}
		out = append(out, Diagnostic{
			Code: "unqualified-write", Severity: Error, Start: s, End: e,
			Message:    "statement modifies ALL rows (no top-level WHERE)",
			Evidence:   "UPDATE/DELETE without WHERE, or TRUNCATE",
			Suggestion: "add a WHERE clause, or confirm you intend to affect every row",
			Confidence: "high",
		})
	}

	// LIMIT без ORDER BY — недетерминированный набор строк между запусками.
	// Считаем ПО-ОПЕРАТОРНО и на глубине скобок 0: ORDER BY из подзапроса
	// (select ... (select ... order by) ... limit) или из соседнего оператора
	// (a order by ...; b limit ...) не гарантирует порядок внешнего LIMIT, поэтому
	// не должен глушить предупреждение. Скан по masked → строки/комментарии уже
	// нейтрализованы, смещения совпадают с исходником.
	for _, rg := range stmtRanges(masked) {
		seg := masked[rg[0]:rg[1]]
		loc := topLevelMatch(seg, reLimit)
		if loc == nil {
			continue
		}
		if topLevelMatch(seg, reOrderBy) != nil {
			continue // у этого оператора есть top-level ORDER BY — порядок стабилен
		}
		out = append(out, Diagnostic{
			Code: "limit-without-order-by", Severity: Warning, Start: rg[0] + loc[0], End: rg[0] + loc[1],
			Message:    "LIMIT without ORDER BY returns an arbitrary (non-deterministic) subset",
			Evidence:   "no top-level ORDER BY accompanies the LIMIT",
			Suggestion: "add ORDER BY for a stable result, or confirm any subset is acceptable",
			Confidence: "medium",
		})
	}

	// NOT IN (subquery) — если подзапрос вернёт NULL, всё условие даёт UNKNOWN и
	// результат пуст. Классическая ловушка; для NOT IN со списком значений — слабее.
	if loc := reNotIn.FindStringIndex(masked); loc != nil {
		conf, msg := "low", "NOT IN with a nullable right-hand side has surprising NULL semantics"
		if reSubselect.MatchString(masked) {
			conf = "medium"
			msg = "NOT IN (subquery): a single NULL from the subquery makes the whole predicate return no rows"
		}
		out = append(out, Diagnostic{
			Code: "not-in-nullable", Severity: Warning, Start: loc[0], End: loc[1],
			Message:    msg,
			Evidence:   "NOT IN (...)",
			Suggestion: "prefer NOT EXISTS (or filter NULLs) when the column/subquery can be NULL",
			Confidence: conf,
		})
	}

	// SELECT * — широкие строки/хрупкость к изменению схемы (информационно).
	if loc := reSelectStar.FindStringIndex(masked); loc != nil {
		out = append(out, Diagnostic{
			Code: "select-star", Severity: Info, Start: loc[0], End: loc[1],
			Message:    "SELECT * fetches every column and is fragile to schema changes",
			Evidence:   "leading SELECT *",
			Suggestion: "list the columns you need",
			Confidence: "low",
		})
	}

	// Возможное декартово произведение: FROM со списком таблиц через запятую и без
	// WHERE/JOIN. Проверяем ПО-ОПЕРАТОРНО, чтобы WHERE/JOIN из соседнего оператора
	// не скрыл реальное декартово произведение (и наоборот).
	for _, rg := range stmtRanges(masked) {
		seg := masked[rg[0]:rg[1]]
		loc := reFrom.FindStringIndex(seg)
		if loc == nil {
			continue
		}
		if commaBeforeClause(seg[loc[1]:]) && !reWhere.MatchString(seg) && !reJoin.MatchString(seg) {
			out = append(out, Diagnostic{
				Code: "cartesian-product", Severity: Warning, Start: rg[0] + loc[0], End: rg[0] + loc[1],
				Message:    "comma-separated FROM tables with no WHERE/JOIN condition — likely a Cartesian product",
				Evidence:   "FROM a, b … without a join condition",
				Suggestion: "add a join condition (WHERE a.x = b.y or an explicit JOIN ... ON)",
				Confidence: "medium",
			})
			break
		}
	}

	return out
}

// stmtRanges возвращает байтовые диапазоны top-level операторов в masked (а значит
// и в исходнике — Mask сохраняет длину). Границами служат top-level ';' — внутри
// литералов/комментариев они уже нейтрализованы Mask, поэтому простого скана
// достаточно. Для одиночного оператора возвращает один диапазон на весь ввод.
func stmtRanges(masked string) [][2]int {
	var ranges [][2]int
	start := 0
	for i := 0; i < len(masked); i++ {
		if masked[i] == ';' {
			ranges = append(ranges, [2]int{start, i})
			start = i + 1
		}
	}
	if start < len(masked) {
		ranges = append(ranges, [2]int{start, len(masked)})
	}
	return ranges
}

// topLevelMatch возвращает [start,end] ПЕРВОГО вхождения re на глубине скобок 0 в
// seg (или nil). seg — masked-фрагмент одного оператора: содержимое строк/комментариев
// уже нейтрализовано, поэтому скобки внутри литералов не искажают глубину.
func topLevelMatch(seg string, re *regexp.Regexp) []int {
	for _, loc := range re.FindAllStringIndex(seg, -1) {
		if parenDepthAt(seg, loc[0]) == 0 {
			return loc
		}
	}
	return nil
}

// parenDepthAt считает глубину незакрытых '(' в seg[:idx] (закрывающая на нуле не
// уводит в минус — устойчиво к несбалансированным скобкам).
func parenDepthAt(seg string, idx int) int {
	depth := 0
	for i := 0; i < idx && i < len(seg); i++ {
		switch seg[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

// commaBeforeClause сообщает, есть ли запятая в части FROM до следующего предложения
// (WHERE/GROUP/ORDER/…) на глубине скобок 0 — т.е. список таблиц через запятую.
func commaBeforeClause(fromTail string) bool {
	depth := 0
	low := strings.ToLower(fromTail)
	for i := 0; i < len(low); i++ {
		switch low[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				return true
			}
		}
		if depth == 0 {
			for _, kw := range []string{"where", "group", "order", "having", "limit", "union"} {
				if strings.HasPrefix(low[i:], kw) && isWordBoundary(low, i, len(kw)) {
					return false
				}
			}
		}
	}
	return false
}

func isWordBoundary(s string, i, n int) bool {
	if i > 0 && isWordByte(s[i-1]) {
		return false
	}
	if i+n < len(s) && isWordByte(s[i+n]) {
		return false
	}
	return true
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
