// Package execution — единая точка входа для решения об ИСПОЛНИТЕЛЬНОМ РИСКЕ
// SQL (read-only / волатильный побочный эффект / запись / безусловная запись).
//
// Это «execution-decision facade»: главный архитектурный инвариант аудита
// (раздел 4.1) — UI НЕ должен сам решать, безопасен ли SQL. Раньше REPL,
// headless и сопутствующие call-site вызывали `safety.IsWrite` /
// `safety.AnyUnqualifiedWrite` / `safety.Classify` напрямую, размазывая
// решение read-vs-write по слою UI. Этот пакет собирает решение в один вход:
// REPL, headless и будущий API классифицируют SQL только через `execution`.
//
// Сейчас это тонкая, поведенчески идентичная делегация к `internal/safety`
// (никакой новой логики, чистый pass-through), поэтому единственный источник
// истины о риске остаётся в `safety`. По мере выделения полноценного
// ExecutionPlanner (аудит 4.2) этот пакет дорастёт до владельца планирования
// исполнения: `Request`, `Plan`, `Policy`, `Event`, `Outcome`, fan-out
// scheduler и canary/batch execution. Эти типы — будущая работа и здесь
// намеренно пока не объявлены.
package execution

import (
	"regexp"
	"strings"

	"terox/internal/safety"
	"terox/internal/sqlsplit"
)

// RiskLevel — уровень исполнительного риска SQL. Реэкспорт safety.RiskLevel,
// чтобы потребителям хватало одного импорта (`execution`).
type RiskLevel = safety.RiskLevel

// Уровни исполнительного риска (реэкспорт safety.Risk*). Перечислены все пять
// уровней, чтобы call-site не тянули `internal/safety` ради констант.
const (
	// RiskReadOnly — явно read-only форма.
	RiskReadOnly = safety.RiskReadOnly
	// RiskVolatileSideEffect — read-форма с волатильным побочным эффектом.
	RiskVolatileSideEffect = safety.RiskVolatileSideEffect
	// RiskWrite — ограниченная запись (есть WHERE / конкретная цель).
	RiskWrite = safety.RiskWrite
	// RiskUnknown — ведущее слово не доказуемо read-only.
	RiskUnknown = safety.RiskUnknown
	// RiskUnqualifiedWrite — безусловная запись (UPDATE/DELETE без WHERE, TRUNCATE).
	RiskUnqualifiedWrite = safety.RiskUnqualifiedWrite
)

// Decision — решение об исполнительном риске. Реэкспорт safety.Decision
// (Level / Write / Unqualified / Reasons).
type Decision = safety.Decision

// Classify — единый вход классификации исполнительного риска SQL. Делегирует
// safety.Classify без изменения поведения.
func Classify(sql string) Decision { return safety.Classify(sql) }

// IsWrite сообщает, является ли sql (или может быть) пишущим запросом.
// Делегирует safety.IsWrite.
func IsWrite(sql string) bool { return safety.IsWrite(sql) }

// AnyUnqualifiedWrite сообщает, содержит ли скрипт хотя бы одну безусловную
// запись. Делегирует safety.AnyUnqualifiedWrite.
func AnyUnqualifiedWrite(script string) bool { return safety.AnyUnqualifiedWrite(script) }

var (
	// selectLeadRe — запрос «формы SELECT» (читающий набор строк): select / with /
	// table / values на старте (после маскирования и в нижнем регистре).
	selectLeadRe = regexp.MustCompile(`^(select|with|table|values)\b`)
	// orderByRe — ключевое слово ORDER BY (любой пробел между словами).
	orderByRe = regexp.MustCompile(`order\s+by\b`)
)

// LacksTopLevelOrderBy сообщает, является ли sql запросом формы SELECT БЕЗ ORDER BY
// верхнего уровня. Эвристика (не гарантия): литералы/комментарии маскируются общим
// лексером, а скобочные подзапросы вырезаются, поэтому ORDER BY внутри подзапроса
// или window-функции `OVER (ORDER BY …)` не считается верхнеуровневым. Нужна для
// честного UX `--mode quorum`: без стабильного ORDER BY PostgreSQL не гарантирует
// порядок строк, и одинаковые данные на разных шардах могут выглядеть как расхождение.
func LacksTopLevelOrderBy(sql string) bool {
	clean := strings.TrimSpace(strings.ToLower(sqlsplit.Mask(sql)))
	for strings.HasPrefix(clean, "(") {
		clean = strings.TrimSpace(clean[1:])
	}
	if !selectLeadRe.MatchString(clean) {
		return false
	}
	return !orderByRe.MatchString(stripParenGroups(clean))
}

// stripParenGroups удаляет содержимое всех скобочных групп (на любой глубине),
// оставляя текст верхнего уровня, чтобы ключевое слово внутри подзапроса/скобок
// не принималось за верхнеуровневое.
func stripParenGroups(s string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteByte(s[i])
			}
		}
	}
	return b.String()
}
