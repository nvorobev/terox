package safety

import (
	"strings"

	"terox/internal/sqlsplit"
)

// RiskLevel — уровень исполнительного риска SQL (ExecutionRisk). Заменяет булеву
// модель «безопасно/опасно» на градацию. ВАЖНО: это ЭВРИСТИКА — UX-подсказка для
// подтверждений и подсветки, а НЕ доказанная граница безопасности. Настоящие границы:
// (1) права роли подключения, (2) server-enforced read-only транзакция. Поэтому тип
// называется RiskLevel, а не «safe/unsafe».
type RiskLevel int

const (
	// RiskReadOnly — явно read-only форма (select/show/table/values/fetch/explain
	// без analyze/with без записи).
	RiskReadOnly RiskLevel = iota
	// RiskVolatileSideEffect — read-форма, вызывающая волатильную функцию с побочными
	// эффектами (advisory locks, pg_terminate_backend, dblink, lo_*, …), которую
	// read-only транзакция НЕ блокирует. Эвристика — единственная защита.
	RiskVolatileSideEffect
	// RiskWrite — изменяет данные/схему, но ограничено (есть WHERE / конкретная цель).
	RiskWrite
	// RiskUnknown — ведущее слово не доказуемо read-only (LISTEN, PREPARE, LOAD,
	// CHECKPOINT, голый BEGIN, …) — консервативно ограждается как потенциальная запись.
	RiskUnknown
	// RiskUnqualifiedWrite — UPDATE/DELETE без WHERE верхнего уровня или TRUNCATE:
	// затрагивает ВСЕ строки. Высший уровень — требует усиленного подтверждения.
	RiskUnqualifiedWrite
)

func (r RiskLevel) String() string {
	switch r {
	case RiskReadOnly:
		return "read-only"
	case RiskVolatileSideEffect:
		return "volatile-side-effect"
	case RiskWrite:
		return "write"
	case RiskUnknown:
		return "unknown"
	case RiskUnqualifiedWrite:
		return "unqualified-write"
	default:
		return "unknown"
	}
}

// Decision — результат классификации риска: единый объект решения вместо «голого
// bool». Несёт уровень риска, признаки write/unqualified (для выбора подтверждения) и
// человекочитаемые причины (какое слово/функция сработали) — чтобы UI объяснял отказ,
// а не просто блокировал.
type Decision struct {
	Level       RiskLevel
	Write       bool     // путь записи: нужен режим \write и подтверждение
	Unqualified bool     // безусловная запись: усиленное подтверждение
	Reasons     []string // почему так классифицировано (для объяснения пользователю)
}

// Classify — единая точка классификации риска SQL (может быть несколько запросов).
// Write согласован с IsWrite, Unqualified — с AnyUnqualifiedWrite, а Level/Reasons
// дают более богатую градацию для подтверждений и подсветки.
func Classify(sql string) Decision {
	level, reasons := riskLevel(sql)
	return Decision{
		Level:       level,
		Write:       IsWrite(sql),
		Unqualified: AnyUnqualifiedWrite(sql),
		Reasons:     reasons,
	}
}

// riskLevel вычисляет максимальный уровень риска по всем запросам ввода и собирает
// причины.
func riskLevel(sql string) (RiskLevel, []string) {
	stmts := sqlsplit.Split(sql)
	if len(stmts) == 0 {
		stmts = []string{sql}
	}
	level := RiskReadOnly
	var reasons []string
	for _, s := range stmts {
		l, rs := classifyOne(s)
		if l > level {
			level = l
		}
		reasons = append(reasons, rs...)
	}
	return level, reasons
}

// classifyOne классифицирует один запрос.
func classifyOne(s string) (RiskLevel, []string) {
	clean := sanitize(s)
	if clean == "" {
		return RiskReadOnly, nil
	}
	for strings.HasPrefix(clean, "(") {
		clean = strings.TrimSpace(clean[1:])
	}
	if fn := sideEffectMatch(s, clean); fn != "" {
		return RiskVolatileSideEffect, []string{"calls volatile side-effecting function " + fn + "() — a read-only transaction does NOT block it"}
	}
	if !isWriteSingle(s) {
		return RiskReadOnly, nil
	}
	first := leadingWord.FindString(clean)
	if IsUnqualifiedWrite(s) {
		switch first {
		case "truncate":
			return RiskUnqualifiedWrite, []string{"TRUNCATE removes ALL rows"}
		default:
			return RiskUnqualifiedWrite, []string{first + " affects ALL rows — no top-level WHERE"}
		}
	}
	if first == "" {
		return RiskUnknown, []string{"statement is not provably read-only"}
	}
	if writeKeywords[first] || first == "with" || first == "select" || first == "explain" || first == "merge" {
		return RiskWrite, []string{first + " modifies data or schema"}
	}
	return RiskUnknown, []string{first + " is not provably read-only — treated as a potential write"}
}

// sideEffectMatch возвращает имя сработавшей волатильной функции с побочным эффектом
// (или "") — учитывает и обычный замаскированный текст, и вариант с видимыми
// идентификаторами в кавычках (имя функции в "…").
func sideEffectMatch(raw, clean string) string {
	if m := sideEffectRe.FindString(clean); m != "" {
		return trimFnName(m)
	}
	if m := sideEffectRe.FindString(strings.ToLower(sqlsplit.MaskKeepQuoted(raw))); m != "" {
		return trimFnName(m)
	}
	return ""
}

// trimFnName убирает хвостовую "(" (и пробел) из совпадения регэкспа "name(".
func trimFnName(m string) string {
	m = strings.TrimSpace(m)
	m = strings.TrimSuffix(m, "(")
	return strings.TrimSpace(m)
}
