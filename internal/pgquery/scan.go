// Package pgquery — единый PostgreSQL-aware лексер terox: ОДИН набор лексических
// правил (строки/комментарии/dollar-quote/U&-escape/идентификаторы) и ОДИН набор
// типов токенов, на которых строятся все потребители — sqlsplit (split/mask),
// complete (контекст автодополнения), а через них safety и migration. Раньше эти
// правила были продублированы в нескольких лексерах, которые могли разойтись на
// одном и том же SQL (P2-3); теперь источник истины один.
//
// Допущение: standard_conforming_strings=on (обычный литерал '...' не
// интерпретирует обратный слеш; экранирование слешем — только в E'...'). Это
// дефолт PostgreSQL начиная с 9.1. Юникод-escape (U&'...'/U&"...") распознаётся;
// нестандартный UESCAPE не обрабатывается (классификатор — UX-страховка, а не
// граница безопасности; см. DecodeUEscaped).
package pgquery

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// IsIdentByte сообщает, может ли c быть частью ASCII-идентификатора (_/буква/цифра).
func IsIdentByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// IsIdentStart сообщает, может ли c начинать идентификатор без кавычек. Не-ASCII
// байты (>= 0x80, продолжение UTF-8 буквы Unicode) допускаются, чтобы
// Unicode-идентификаторы токенизировались как одно слово.
func IsIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// IsIdentCont сообщает, может ли c продолжать идентификатор без кавычек.
// PostgreSQL допускает '$' внутри идентификатора (но не в начале).
func IsIdentCont(c byte) bool {
	return IsIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

// IsOpByte сообщает, является ли c символом оператора PostgreSQL.
func IsOpByte(c byte) bool {
	switch c {
	case '+', '-', '*', '/', '<', '>', '=', '~', '!', '@', '#', '%', '^', '&', '|', '`', '?', ':':
		return true
	}
	return false
}

// IsEscapeStringStart сообщает, что s[i] — открывающая кавычка escape-строки
// E'...' (т.е. перед ней стоит отдельная буква e/E, а не хвост идентификатора).
// Только при таком префиксе обратный слеш экранирует внутри литерала.
func IsEscapeStringStart(s string, i int) bool {
	return i < len(s) && s[i] == '\'' && i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') &&
		(i < 2 || !IsIdentCont(s[i-2]))
}

// IsUAmpStart сообщает, что с s[i] начинается Unicode-escape литерал/идентификатор
// PostgreSQL: U&'...' или U&"..." (префикс U&/u& перед кавычкой, и слева не
// идентификаторный байт, чтобы fooU&'..' не распознавался ошибочно).
func IsUAmpStart(s string, i int) bool {
	return i+2 < len(s) && (s[i] == 'u' || s[i] == 'U') && s[i+1] == '&' &&
		(s[i+2] == '"' || s[i+2] == '\'') && (i == 0 || !IsIdentByte(s[i-1]))
}

// DollarTag возвращает dollar-quote тег, начинающийся в s[i] ("$$" или "$func$"),
// если он там есть. Тег подчиняется правилам некавыченного идентификатора (не
// может начинаться с цифры), но PostgreSQL допускает в нём не-латинские буквы и
// буквы с диакритикой — поэтому принимаются не-ASCII байты (>= 0x80). Так "$1$" —
// это позиционный параметр $1 и '$', а $тег$/$café$ — настоящие теги.
func DollarTag(s string, i int) (string, bool) {
	if i >= len(s) || s[i] != '$' {
		return "", false
	}
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[i : j+1], true
		}
		if j == i+1 && c >= '0' && c <= '9' {
			return "", false
		}
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80) {
			return "", false
		}
	}
	return "", false
}

// ScanDollar сканирует dollar-quoted строку, начинающуюся в s[i]. Возвращает тег,
// индекс сразу за закрывающим тегом и признак завершённости. Если s[i] не
// открывает dollar-quote, ok=false. Незавершённая строка читается до конца ввода
// (terminated=false) — безопасное направление для редактора.
func ScanDollar(s string, i int) (tag string, end int, terminated, ok bool) {
	tag, ok = DollarTag(s, i)
	if !ok {
		return "", i, false, false
	}
	j := i + len(tag)
	if idx := strings.Index(s[j:], tag); idx >= 0 {
		return tag, j + idx + len(tag), true, true
	}
	return tag, len(s), false, true
}

// ScanQuoted сканирует литерал '...' или "..." начиная с кавычки s[i]. escString
// (для E'...') включает экранирование обратным слешем. Понимает экранирование
// удвоением кавычки. Возвращает индекс сразу за закрывающей кавычкой и признак
// завершённости (false — ввод кончился внутри литерала).
func ScanQuoted(s string, i int, escString bool) (end int, terminated bool) {
	q := s[i]
	j := i + 1
	for j < len(s) {
		ch := s[j]
		if escString && ch == '\\' && j+1 < len(s) {
			j += 2
			continue
		}
		if ch == q {
			if j+1 < len(s) && s[j+1] == q { // удвоенная кавычка — экранирование
				j += 2
				continue
			}
			return j + 1, true
		}
		j++
	}
	return len(s), false
}

// ScanLineComment сканирует строчный комментарий "-- ..." начиная с s[i] (на '-').
// Возвращает индекс перевода строки или конца ввода (сам '\n' не включается).
func ScanLineComment(s string, i int) int {
	j := i + 2
	for j < len(s) && s[j] != '\n' {
		j++
	}
	return j
}

// ScanBlockComment сканирует блочный комментарий "/* ... */" начиная с s[i] (на
// '/'). Комментарии PostgreSQL вложенные (docs §4.1.5): завершается на парном
// внешнем */. Возвращает индекс сразу за закрывающим */ и признак завершённости.
func ScanBlockComment(s string, i int) (end int, terminated bool) {
	depth, j := 1, i+2
	for j < len(s) && depth > 0 {
		if s[j] == '/' && j+1 < len(s) && s[j+1] == '*' {
			depth, j = depth+1, j+2
			continue
		}
		if s[j] == '*' && j+1 < len(s) && s[j+1] == '/' {
			depth, j = depth-1, j+2
			continue
		}
		j++
	}
	return j, depth == 0
}

// DecodeUEscaped читает Unicode-escaped строку/идентификатор PostgreSQL начиная с
// открывающей кавычки s[open] ('"' или '\”), декодируя \XXXX (4 hex) и \+XXXXXX
// (6 hex), сворачивая \\ в обратный слеш и удвоенную кавычку в одиночную.
// Возвращает декодированное содержимое и индекс сразу за закрывающей кавычкой.
// Нестандартный UESCAPE НЕ обрабатывается: пропущенное декодирование далее
// ловится правами, а не ложным завершением.
func DecodeUEscaped(s string, open int) (string, int) {
	quote := s[open]
	n := len(s)
	i := open + 1
	var b []byte
	for i < n {
		c := s[i]
		if c == quote {
			if i+1 < n && s[i+1] == quote { // удвоенная кавычка -> одиночная
				b = append(b, quote)
				i += 2
				continue
			}
			return string(b), i + 1
		}
		if c == '\\' && i+1 < n {
			if s[i+1] == '\\' { // \\ -> обратный слеш
				b = append(b, '\\')
				i += 2
				continue
			}
			if s[i+1] == '+' && i+8 <= n && AllHex(s[i+2:i+8]) { // \+XXXXXX
				if cp, err := strconv.ParseUint(s[i+2:i+8], 16, 32); err == nil {
					b = appendRune(b, rune(cp))
					i += 8
					continue
				}
			}
			if i+5 <= n && AllHex(s[i+1:i+5]) { // \XXXX
				if cp, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b = appendRune(b, rune(cp))
					i += 5
					continue
				}
			}
			b = append(b, '\\') // нераспознанный escape — оставляем обратный слеш
			i++
			continue
		}
		b = append(b, c)
		i++
	}
	return string(b), n // не завершено — читаем до конца (безопасное направление)
}

// AllHex сообщает, является ли каждый байт s hex-цифрой (и s непустая).
func AllHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// appendRune добавляет руну как UTF-8.
func appendRune(b []byte, r rune) []byte {
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], r)
	return append(b, buf[:n]...)
}
