// Package sqlsplit разбивает SQL-скрипт на отдельные операторы и нейтрализует
// содержимое литералов/комментариев для пословного анализа. Лексические правила
// (строки/комментарии/dollar-quote/U&) берутся из единого лексера internal/pgquery
// — здесь только высокоуровневые операции split/mask поверх его примитивов, чтобы
// safety, migration и completion видели один и тот же разбор SQL (P2-3).
package sqlsplit

import (
	"strings"

	"terox/internal/pgquery"
)

// Split возвращает непустые операторы верхнего уровня из src. Точки с запятой
// внутри одинарных/двойных кавычек, dollar-quoted строк ($tag$...$tag$),
// строчных (--) и блочных (/* */) комментариев разделителями не считаются.
func Split(src string) []string {
	var stmts []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			stmts = append(stmts, s)
		}
		cur.Reset()
	}
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '-' && i+1 < n && src[i+1] == '-':
			j := pgquery.ScanLineComment(src, i)
			cur.WriteString(src[i:j])
			i = j
		case c == '/' && i+1 < n && src[i+1] == '*':
			end, _ := pgquery.ScanBlockComment(src, i)
			cur.WriteString(src[i:end])
			i = end
		case c == '\'':
			end, _ := pgquery.ScanQuoted(src, i, pgquery.IsEscapeStringStart(src, i))
			cur.WriteString(src[i:end])
			i = end
		case c == '"':
			end, _ := pgquery.ScanQuoted(src, i, false)
			cur.WriteString(src[i:end])
			i = end
		case pgquery.IsIdentStart(c):
			// Идентификатор поглощается целиком (включая '$' внутри/после него),
			// иначе '$' в "A$$" был бы ошибочно принят за начало dollar-quote — как и
			// решает единый лексер (PostgreSQL: '$' допустим в идентификаторе).
			j := i + 1
			for j < n && pgquery.IsIdentCont(src[j]) {
				j++
			}
			cur.WriteString(src[i:j])
			i = j
		case c == '$':
			if _, end, _, ok := pgquery.ScanDollar(src, i); ok {
				cur.WriteString(src[i:end])
				i = end
			} else {
				cur.WriteByte(c)
				i++
			}
		case c == ';':
			flush()
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	flush()
	return stmts
}

// InStringOrComment сообщает, попадает ли КОНЕЦ s внутрь незавершённого
// строкового литерала ('...'/E'...'), dollar-quoted строки, кавыченного
// идентификатора ("..."), строчного или блочного комментария. В такой позиции
// курсора автодополнение SQL-объектов следует подавлять.
//
// Делегирует единому лексеру pgquery (TrailingStateOf): «конец ввода внутри
// литерала/комментария?» — тот же вопрос, поэтому ответ совпадает с тем, что видят
// completion и Split, и не может разойтись (P2-3).
func InStringOrComment(s string) bool {
	return pgquery.TrailingStateOf(s) != pgquery.StateCode
}

// Mask возвращает s с нейтрализованным содержимым строковых литералов,
// кавыченных идентификаторов и комментариев, чтобы определение ключевых слов/
// структуры не сбивалось текстом внутри них. Результат имеет ту же ДЛИНУ, что и s,
// и сохраняет каждый структурный байт вне литералов/комментариев (скобки, точки с
// запятой, операторы, ключевые слова), поэтому байтовые смещения совпадают.
//
// Содержимое '...'/E'...'/$tag$...$tag$ заменяется пробелами; содержимое "..."
// заменяется на 'x' (чтобы токен оставался одним словом без пробелов); комментарии
// — пробелами (переводы строк сохраняются). Сами разделители сохраняются.
func Mask(s string) string {
	out := make([]byte, len(s))
	n := len(s)
	blank := func(from, to int, fill byte) {
		for k := from; k < to; k++ {
			if s[k] == '\n' {
				out[k] = '\n'
			} else {
				out[k] = fill
			}
		}
	}
	i := 0
	for i < n {
		c := s[i]
		switch {
		case c == '-' && i+1 < n && s[i+1] == '-':
			j := pgquery.ScanLineComment(s, i)
			blank(i, j, ' ')
			i = j
		case c == '/' && i+1 < n && s[i+1] == '*':
			end, _ := pgquery.ScanBlockComment(s, i)
			blank(i, end, ' ')
			i = end
		case c == '\'' || c == '"':
			fill := byte(' ')
			if c == '"' {
				fill = 'x'
			}
			esc := c == '\'' && pgquery.IsEscapeStringStart(s, i)
			end, terminated := pgquery.ScanQuoted(s, i, esc)
			out[i] = c // открывающий разделитель
			contentEnd := end
			if terminated {
				contentEnd = end - 1
			}
			blank(i+1, contentEnd, fill)
			if terminated {
				out[end-1] = c // закрывающий разделитель
			}
			i = end
		case pgquery.IsIdentStart(c):
			// Идентификатор копируется дословно целиком (включая '$'), чтобы '$'
			// внутри/после него не был принят за dollar-quote (см. Split).
			j := i + 1
			for j < n && pgquery.IsIdentCont(s[j]) {
				j++
			}
			copy(out[i:j], s[i:j])
			i = j
		case c == '$':
			if tag, end, terminated, ok := pgquery.ScanDollar(s, i); ok {
				for k := 0; k < len(tag); k++ {
					out[i+k] = s[i+k] // открывающий тег
				}
				contentEnd := end
				if terminated {
					contentEnd = end - len(tag)
				}
				blank(i+len(tag), contentEnd, ' ')
				if terminated {
					for k := 0; k < len(tag); k++ {
						out[end-len(tag)+k] = s[end-len(tag)+k] // закрывающий тег
					}
				}
				i = end
			} else {
				out[i] = c
				i++
			}
		default:
			out[i] = c
			i++
		}
	}
	return string(out)
}

// MaskKeepQuoted нейтрализует строковые литералы ('...'/E'...'/$tag$...$tag$) и
// комментарии до одного пробела, как Mask, но выдаёт СОДЕРЖИМОЕ кавыченных
// идентификаторов "..." ДОСЛОВНО (кавычки убираются, "" сворачивается в ") и
// декодирует U&"..."-идентификаторы. Результат НЕ сохраняет байтовые смещения и
// нужен только для пословного сопоставления имён, где кавыченный идентификатор
// должен оставаться видимым (иначе SELECT "dblink_exec"(...) проскочил бы denylist).
func MaskKeepQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '-' && i+1 < n && s[i+1] == '-':
			i = pgquery.ScanLineComment(s, i)
			b.WriteByte(' ')
		case c == '/' && i+1 < n && s[i+1] == '*':
			i, _ = pgquery.ScanBlockComment(s, i)
			b.WriteByte(' ')
		case c == '\'':
			i, _ = pgquery.ScanQuoted(s, i, pgquery.IsEscapeStringStart(s, i))
			b.WriteByte(' ')
		case pgquery.IsUAmpStart(s, i):
			// U&"d\0061t" / U&'..': декодируем escape, чтобы имя функции, спрятанное
			// за \XXXX, восстановилось для denylist. Идентификатор показываем
			// декодированным; строковый литерал затираем как любой другой.
			decoded, ni := pgquery.DecodeUEscaped(s, i+2)
			if s[i+2] == '"' {
				b.WriteString(decoded)
			} else {
				b.WriteByte(' ')
			}
			i = ni
		case c == '"':
			i++
			for i < n {
				ch := s[i]
				if ch == '"' {
					if i+1 < n && s[i+1] == '"' { // удвоённая "" — экранированная кавычка
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(ch)
				i++
			}
		case pgquery.IsIdentStart(c):
			// Идентификатор (включая '$') выдаётся дословно: '$' внутри/после него не
			// dollar-quote. U&"..." уже перехвачен веткой выше.
			j := i + 1
			for j < n && pgquery.IsIdentCont(s[j]) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
		case c == '$':
			if _, end, _, ok := pgquery.ScanDollar(s, i); ok {
				i = end
				b.WriteByte(' ')
			} else {
				b.WriteByte(c)
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
