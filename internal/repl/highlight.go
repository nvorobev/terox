package repl

import (
	"strings"

	"github.com/chzyer/readline"

	"terox/internal/complete"
	"terox/internal/ui"
)

// ANSI-цвета для подсветки ввода (нулевой видимой ширины, чтобы не сбивать
// расчёт курсора readline, который идёт по неокрашенному буферу).
const (
	cReset   = "\x1b[0m"
	cKeyword = "\x1b[36m" // голубой
	cString  = "\x1b[32m" // зелёный
	cNumber  = "\x1b[33m" // жёлтый
	cComment = "\x1b[90m" // серый
	cMeta    = "\x1b[35m" // пурпурный
	cGhost   = "\x1b[90m" // тусклый серый (встроенная подсказка)
)

// ghostMaxLine ограничивает позицию встроенных подсказок, оставляя место для
// промпта, чтобы подсказка не выходила за правый край терминала.
const ghostMaxLine = 40

// hlKeywords — набор однословных SQL-ключевых слов для подсветки при вводе.
var hlKeywords = func() map[string]bool {
	words := []string{
		"select", "from", "where", "order", "by", "group", "having", "limit",
		"offset", "join", "left", "right", "inner", "outer", "cross", "full",
		"on", "using", "and", "or", "not", "in", "exists", "any", "all",
		"between", "like", "ilike", "is", "null", "insert", "into", "values",
		"update", "set", "delete", "returning", "with", "recursive", "union",
		"intersect", "except", "distinct", "as", "asc", "desc", "nulls", "first",
		"last", "case", "when", "then", "else", "end", "cast", "begin", "commit",
		"rollback", "savepoint", "explain", "analyze", "verbose", "create",
		"table", "index", "view", "unique", "concurrently", "alter", "drop",
		"add", "column", "primary", "key", "foreign", "references", "check",
		"default", "for", "share", "window", "partition", "over", "filter",
		"truncate", "vacuum", "reindex", "grant", "revoke", "role", "true",
		"false", "exists", "ilike",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()

// highlightSQL раскрашивает токены s ANSI-цветами тем же лексером, что и
// автодополнение (complete.Lex), поэтому подсветка и дополнение одинаково видят
// границы строк/комментариев/dollar-quote. Каждый исходный байт встречается
// ровно один раз; добавляются лишь цветовые коды нулевой ширины.
func highlightSQL(s string) string {
	var b strings.Builder
	toks := complete.Lex(s)
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch t.Kind {
		case complete.TBackslash:
			// Мета-команда: \cmd в начале строки или после пробела, красится целиком.
			if (t.Start == 0 || (i > 0 && toks[i-1].Kind == complete.TWhitespace)) &&
				i+1 < len(toks) && toks[i+1].Kind == complete.TWord {
				b.WriteString(cMeta + t.Text + toks[i+1].Text + cReset)
				i++
			} else {
				b.WriteString(t.Text)
			}
		case complete.TWord:
			if hlKeywords[strings.ToLower(t.Text)] {
				b.WriteString(cKeyword + t.Text + cReset)
			} else {
				b.WriteString(t.Text)
			}
		case complete.TNumber, complete.TParam:
			b.WriteString(cNumber + t.Text + cReset)
		case complete.TString, complete.TDollar:
			b.WriteString(cString + t.Text + cReset)
		case complete.TComment:
			b.WriteString(cComment + t.Text + cReset)
		case complete.TIncomplete:
			col := cString
			if strings.HasPrefix(t.Text, "/*") {
				col = cComment
			}
			b.WriteString(col + t.Text + cReset)
		default:
			// TQIdent (идентификатор), TOp, TPunct, TWhitespace — без цвета.
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// painter реализует readline.Painter: подсвечивает введённую строку и, если
// включён режим подсказок и курсор в конце, дописывает тусклую встроенную
// подсказку-"призрак" (принимается по Tab). После призрака идут backspace,
// чтобы курсор терминала остался сразу за реальным текстом.
type painter struct {
	r    *REPL
	comp *completer
}

func (p *painter) Paint(line []rune, pos int) []rune {
	if !ui.Enabled {
		return line
	}
	out := highlightSQL(string(line))
	// Призрак должен оставаться левее правого края терминала: если промпт + строка
	// + призрак доходят до края, замыкающие backspace не пересекают перенос и
	// перерисовка ломается. Бюджет считается по ширине терминала, а при неизвестной
	// ширине берётся ghostMaxLine.
	if p.r.suggest && pos == len(line) && len(line) > 0 {
		avail := ghostMaxLine - len(line)
		if w := readline.GetScreenWidth(); w > 0 {
			avail = w - p.r.promptWidth() - len(line) - 1
		}
		if ghost, annot := p.comp.ghostHint(string(line)); ghost != "" {
			gw := len([]rune(ghost))
			if gw <= avail {
				shown := cGhost + ghost
				vis := gw
				// Аннотация (сигнатура/тип, покрытие, "+N") только для показа;
				// выводится, если тоже влезает, затем backspace по всему, чтобы
				// курсор остался сразу за реальным текстом.
				if aw := len([]rune(annot)); aw > 0 && gw+aw <= avail {
					shown += cReset + cComment + annot
					vis += aw
				}
				out += shown + cReset + strings.Repeat("\b", vis)
			}
		}
	}
	return []rune(out)
}

// promptWidth — видимая ширина промпта (первой строки), используется для
// расчёта бюджета встроенного призрака относительно правого края терминала.
func (r *REPL) promptWidth() int {
	return len([]rune(r.contextLabel()+" "+r.statusPlain())) + len(" => ")
}
