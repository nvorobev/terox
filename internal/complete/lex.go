// Package complete — типизированный контекстный движок автодополнения SQL:
// лёгкий контекстный парсер по токенам, типизированная модель каталога (схема/
// отношение/колонка/функция), ранкер и рендерер, экранирующий идентификаторы при
// необходимости. Пакет чистый (без зависимостей от БД и readline): REPL собирает
// снимок каталога и вызывает Complete.
//
// Лексер — единый pgquery: complete лишь переэкспортирует его типы токенов под
// привычными для пакета именами (T*), чтобы все подсистемы terox видели один и
// тот же разбор SQL (P2-3). Раньше здесь была отдельная копия лексера.
package complete

import "terox/internal/pgquery"

// Kind/Token — типы токенов единого лексера pgquery (алиасы).
type (
	Kind  = pgquery.Kind
	Token = pgquery.Token
)

// Виды токенов (исторические имена complete -> канонические pgquery).
const (
	TWhitespace = pgquery.Whitespace
	TWord       = pgquery.Word
	TQIdent     = pgquery.QuotedIdent
	TNumber     = pgquery.Number
	TString     = pgquery.String
	TDollar     = pgquery.DollarString
	TParam      = pgquery.Param
	TPunct      = pgquery.Punct
	TOp         = pgquery.Operator
	TComment    = pgquery.Comment
	TBackslash  = pgquery.Backslash
	TIncomplete = pgquery.Incomplete
)

// Lex токенизирует s единым лексером pgquery.
func Lex(s string) []Token { return pgquery.Lex(s) }

// TrailingState — лексическое состояние в конце ввода (алиас pgquery).
type TrailingState = pgquery.TrailingState

const (
	StateCode    = pgquery.StateCode
	StateString  = pgquery.StateString
	StateComment = pgquery.StateComment
	StateQIdent  = pgquery.StateQIdent
)

// TrailingStateOf возвращает лексическое состояние в конце s.
func TrailingStateOf(s string) TrailingState { return pgquery.TrailingStateOf(s) }
