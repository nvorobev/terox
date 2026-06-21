package pgquery

import "strings"

// Kind классифицирует лексический токен.
type Kind int

const (
	Whitespace   Kind = iota
	Word              // идентификатор без кавычек или ключевое слово (буквы/цифры/_/$, Unicode)
	QuotedIdent       // "идентификатор" или U&"идентификатор"
	Number            // 42, 3.14, 1e9
	String            // '...', E'...', U&'...'
	DollarString      // $tag$...$tag$
	Param             // $1 — позиционный параметр
	Punct             // ( ) , . ; [ ]
	Operator          // последовательность операторов (+ - * / < > = ...)
	Comment           // -- строчный или /* блочный (вложенный)
	Backslash         // ведущий \ (мета-команда)
	Incomplete        // незавершённые строка/комментарий/qident/dollar в конце ввода
)

// Token — лексический токен со смещениями в байтах исходника. Конкатенация Text
// всех токенов равна исходной строке (лексер покрывает каждый байт).
type Token struct {
	Kind       Kind
	Text       string
	Start, End int
}

// Lex токенизирует s единым PostgreSQL-aware лексером. Последний токен —
// Incomplete, если ввод заканчивается внутри незавершённой строки/комментария/
// dollar-строки/кавыченного идентификатора (состояние доступно через TrailingStateOf).
func Lex(s string) []Token {
	var toks []Token
	i, n := 0, len(s)
	add := func(k Kind, start, end int) {
		toks = append(toks, Token{Kind: k, Text: s[start:end], Start: start, End: end})
	}
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			j := i
			for j < n && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r' || s[j] == '\f' || s[j] == '\v') {
				j++
			}
			add(Whitespace, i, j)
			i = j
		case c == '-' && i+1 < n && s[i+1] == '-':
			j := ScanLineComment(s, i)
			add(Comment, i, j)
			i = j
		case c == '/' && i+1 < n && s[i+1] == '*':
			end, ok := ScanBlockComment(s, i)
			if ok {
				add(Comment, i, end)
			} else {
				add(Incomplete, i, n) // незавершённый блочный комментарий
			}
			i = end
		case c == '\'':
			end, ok := ScanQuoted(s, i, false)
			emitQuoted(add, String, i, end, ok, n)
			i = end
		case c == '"':
			end, ok := ScanQuoted(s, i, false)
			emitQuoted(add, QuotedIdent, i, end, ok, n)
			i = end
		case IsEscapeStringStart(s, i+1):
			// Escape-строка E'...': кавычка сразу за отдельной буквой e/E. Используем
			// общий предикат (единый источник правил), а не инлайн-проверку: сюда
			// управление доходит лишь на границе токена, где e/E не хвост идентификатора.
			end, ok := ScanQuoted(s, i+1, true)
			emitQuoted(add, String, i, end, ok, n)
			i = end
		case IsUAmpStart(s, i):
			// Unicode-escape литерал/идентификатор U&'...' / U&"...": кавычка в i+2.
			k := String
			if s[i+2] == '"' {
				k = QuotedIdent
			}
			end, ok := ScanQuoted(s, i+2, false)
			emitQuoted(add, k, i, end, ok, n)
			i = end
		case c == '$':
			if tag, end, terminated, ok := ScanDollar(s, i); ok {
				if terminated {
					add(DollarString, i, end)
				} else {
					add(Incomplete, i, n)
				}
				i = end
				_ = tag
			} else if i+1 < n && s[i+1] >= '0' && s[i+1] <= '9' {
				j := i + 1
				for j < n && s[j] >= '0' && s[j] <= '9' {
					j++
				}
				add(Param, i, j)
				i = j
			} else {
				add(Operator, i, i+1)
				i++
			}
		case c >= '0' && c <= '9':
			j := i
			for j < n && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.' || s[j] == 'e' || s[j] == 'E' ||
				((s[j] == '+' || s[j] == '-') && j > i && (s[j-1] == 'e' || s[j-1] == 'E'))) {
				j++
			}
			add(Number, i, j)
			i = j
		case IsIdentStart(c):
			j := i
			for j < n && IsIdentCont(s[j]) {
				j++
			}
			add(Word, i, j)
			i = j
		case c == '(' || c == ')' || c == ',' || c == '.' || c == ';' || c == '[' || c == ']':
			add(Punct, i, i+1)
			i++
		case c == '\\':
			add(Backslash, i, i+1)
			i++
		case IsOpByte(c):
			j := i
			for j < n && IsOpByte(s[j]) {
				// Начало комментария обрывает набор оператора: в PostgreSQL
				// многосимвольный оператор не может содержать -- или /* — они
				// всегда начинают комментарий. Без этого `a=/*..*/b` заглотило бы
				// /* в оператор, и тело комментария «протекло» бы живыми токенами,
				// расходясь с байтовыми сканерами Split/Mask (нарушение инварианта
				// «один разбор»). Первый байт оператора началом комментария быть не
				// может — эти случаи перехватывают ветки -- и /* выше.
				if s[j] == '-' && j+1 < n && s[j+1] == '-' {
					break
				}
				if s[j] == '/' && j+1 < n && s[j+1] == '*' {
					break
				}
				j++
			}
			add(Operator, i, j)
			i = j
		default:
			add(Operator, i, i+1)
			i++
		}
	}
	return toks
}

// emitQuoted добавляет завершённый литерал указанного вида или Incomplete-токен.
func emitQuoted(add func(Kind, int, int), k Kind, start, end int, terminated bool, n int) {
	if terminated {
		add(k, start, end)
	} else {
		add(Incomplete, start, n)
	}
}

// TrailingState — лексическое состояние в конце ввода (для редактора/дополнения).
type TrailingState int

const (
	StateCode    TrailingState = iota
	StateString                // внутри незавершённой строки ('/E'/U&'/$tag$)
	StateComment               // внутри незавершённого комментария
	StateQIdent                // внутри незавершённого кавыченного идентификатора
)

// TrailingStateOf возвращает лексическое состояние в конце s. В строках/
// комментариях автодополнение SQL-объектов подавляется; в состоянии кавыченного
// идентификатора дополнение разрешено (с экранированием "").
func TrailingStateOf(s string) TrailingState {
	toks := Lex(s)
	if len(toks) == 0 {
		return StateCode
	}
	last := toks[len(toks)-1]
	if last.End != len(s) {
		return StateCode
	}
	switch last.Kind {
	case Incomplete:
		switch {
		case strings.HasPrefix(last.Text, "/*"):
			return StateComment
		case strings.HasPrefix(last.Text, `"`),
			strings.HasPrefix(strings.ToUpper(last.Text), `U&"`):
			return StateQIdent
		default:
			return StateString // ' E' U&' $tag$
		}
	case Comment:
		// Строчный комментарий без завершающего перевода строки — курсор внутри.
		if strings.HasPrefix(last.Text, "--") {
			return StateComment
		}
	}
	return StateCode
}
