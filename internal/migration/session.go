package migration

import (
	"regexp"
	"strings"

	"terox/internal/sqlsplit"
)

// SessionStatePolicy: терминология аудита для классов конструкций, выполняемых в
// оборачиваемом теле миграции/записи:
//
//   - forbidden    — session-scoped команды, переживающие COMMIT: session SET,
//     RESET, LISTEN/UNLISTEN, PREPARE/DEALLOCATE, DECLARE CURSOR, TEMP-объекты,
//     session-advisory-блокировки, DISCARD;
//   - localizable  — GUC, которые можно записать как SET LOCAL (откатывается на
//     COMMIT) — здесь это уже разрешённая форма `SET LOCAL ...`;
//   - allowed      — обычный транзакционный SQL без session state.
//
// Безопасная первая версия (этот файл) ОТКЛОНЯЕТ forbidden-конструкции в
// transaction-pooling профиле (обёрнутое тело), а не переписывает их молча. Это
// нужно потому, что обёртка terox оборачивает тело в BEGIN/COMMIT и применяет
// роль/таймауты только через SET LOCAL: обычный (не LOCAL) SET, TEMP-объекты,
// LISTEN, prepared statements и cursors привязаны к backend session и при
// PgBouncer transaction pooling утекли бы следующему клиенту того же бэкенда.
//
// Граница транзакции (BEGIN/COMMIT/ROLLBACK, SET ROLE, защищённые таймауты,
// set_config) проверяется отдельно в HasTxControl; здесь — именно session state.

var (
	// sessionAdvisoryLockRe ловит session-уровневые advisory-блокировки
	// (pg_advisory_lock, pg_advisory_unlock*, pg_try_advisory_lock*), которые НЕ
	// освобождаются на конце транзакции и потому утекают на backend. Транзакционные
	// варианты (pg_advisory_xact_lock*) исключены грамматически: после `advisory_`
	// у них идёт `xact`, а не `lock`/`unlock`, поэтому `(un)?lock` их не матчит.
	// Сканируется по выводу MaskKeepQuoted, чтобы имя функции в кавычках/U&
	// ("pg_advisory_lock") тоже распознавалось.
	sessionAdvisoryLockRe = regexp.MustCompile(`(?is)\bpg_(try_)?advisory_(un)?lock\w*\s*\(`)
)

// SessionStateViolation возвращает человекочитаемую причину (с названием
// конструкции, объяснением и безопасной альтернативой), если script содержит
// session-scoped конструкцию, небезопасную в оборачиваемом теле. Пустая строка —
// нарушений нет.
//
// Применяется только к WRAP-пути (\migrate и интерактивные записи), где terox сам
// оборачивает тело. Дословный путь \i <file> сессией владеет оператор и не
// проходит этот фильтр.
//
// Ложные срабатывания — безопасное направление: миграция лишь отклоняется с
// подсказкой выполнить её дословно через \i. Содержимое литералов и комментариев
// нейтрализуется Mask, поэтому ключевые слова внутри строк/комментариев не
// считаются командами.
func SessionStateViolation(script string) string {
	for _, stmt := range sqlsplit.Split(script) {
		if r := sessionStateViolationSingle(stmt); r != "" {
			return r
		}
	}
	return ""
}

const sessionStateAdvice = " It is session state that survives COMMIT and would leak to the next " +
	"client on the same pooled backend (PgBouncer transaction pooling). " +
	"Use a transaction-scoped form (e.g. SET LOCAL) in the body, or run the file " +
	"verbatim with \\i <file> where you own the session, role and timeouts."

func sessionStateViolationSingle(stmt string) string {
	clean := strings.ToLower(strings.TrimSpace(sqlsplit.Mask(stmt)))
	if clean == "" {
		return ""
	}
	// Убираем ведущие скобки (например, "(select pg_advisory_lock(1))").
	for strings.HasPrefix(clean, "(") {
		clean = strings.TrimSpace(clean[1:])
	}
	words := strings.Fields(clean)
	if len(words) == 0 {
		return ""
	}
	first := trimWord(words[0])
	second := ""
	if len(words) > 1 {
		second = trimWord(words[1])
	}

	switch first {
	case "set":
		// SET LOCAL — транзакционно (откатывается на COMMIT) → разрешено.
		// SET TRANSACTION / SET CONSTRAINTS — характеристики текущей транзакции →
		// разрешены. Любой другой SET (включая SET SESSION ...) — session GUC.
		switch second {
		case "local", "transaction", "constraints":
			return ""
		}
		name := second
		if name == "" || name == "=" {
			name = "session parameter"
		}
		return "refused: SET " + name + " is a session-level parameter." + sessionStateAdvice
	case "reset":
		return "refused: RESET " + second + " changes a session parameter." + sessionStateAdvice
	case "listen", "unlisten":
		return "refused: " + strings.ToUpper(first) + " binds to the backend session." + sessionStateAdvice
	case "prepare":
		// PREPARE TRANSACTION — двухфазная фиксация (это управление транзакцией,
		// ловится HasTxControl), а не prepared statement.
		if second == "transaction" {
			return ""
		}
		return "refused: PREPARE creates a session prepared statement." + sessionStateAdvice
	case "deallocate":
		return "refused: DEALLOCATE manipulates session prepared statements." + sessionStateAdvice
	case "declare":
		return "refused: DECLARE … CURSOR creates a cursor bound to the session/transaction." + sessionStateAdvice
	case "discard":
		return "refused: DISCARD resets session state." + sessionStateAdvice
	case "create":
		// CREATE [OR REPLACE] [GLOBAL|LOCAL] [TEMP|TEMPORARY] … — пропускаем
		// необязательные модификаторы (в любом порядке) и смотрим на ключевое слово
		// объекта. TEMP/TEMPORARY-объект (таблица/представление/последовательность)
		// живёт в session-local pg_temp и переживает COMMIT; UNLOGGED и обычные —
		// постоянные → разрешены.
		if w := firstNonModifier(words[1:]); w == "temp" || w == "temporary" {
			return "refused: CREATE TEMP/TEMPORARY creates a temporary object bound to the backend session." + sessionStateAdvice
		}
	case "load":
		// LOAD '<library>' загружает разделяемую библиотеку в текущий backend;
		// её session-хуки и дефолты GUC переживают COMMIT.
		return "refused: LOAD loads a shared library into the backend session." + sessionStateAdvice
	}

	// DO/анонимный блок и CALL процедуры: тело dollar-quoted, и Mask его обнуляет,
	// поэтому session advisory lock / set_config внутри тела обычным сканом не
	// видно. Сканируем СЫРОЙ оператор (как HasTxControl для commit/rollback);
	// ложное совпадение внутри литерала лишь отклоняет миграцию — безопасное направление.
	if first == "do" || first == "call" {
		raw := strings.ToLower(stmt)
		if sessionAdvisoryLockRe.MatchString(raw) {
			return "refused: a session-level advisory lock (pg_advisory_lock/…) inside a DO/CALL block is not released at transaction end." + sessionStateAdvice
		}
		if setConfigCallRe.MatchString(raw) {
			return "refused: set_config(...) inside a DO/CALL block can change a session parameter." + sessionStateAdvice
		}
	}

	// Session-уровневые advisory-блокировки могут прятаться внутри любого
	// выражения (обычно SELECT pg_advisory_lock(1)). Сканируем сырой оператор с
	// раскрытыми кавычками/U&, чтобы имя функции в "..." тоже ловилось.
	if sessionAdvisoryLockRe.MatchString(strings.ToLower(sqlsplit.MaskKeepQuoted(stmt))) {
		return "refused: a session-level advisory lock (pg_advisory_lock/…) is not released at transaction end." + sessionStateAdvice
	}
	return ""
}

// firstNonModifier возвращает первое слово, не являющееся необязательным
// модификатором CREATE (or/replace/global/local), чтобы добраться до ключевого
// слова объекта (например temp/temporary) независимо от того, сколько
// модификаторов ему предшествует (CREATE OR REPLACE TEMP VIEW, CREATE GLOBAL
// TEMP TABLE, …).
func firstNonModifier(words []string) string {
	for _, w := range words {
		switch trimWord(w) {
		case "or", "replace", "global", "local":
			continue
		default:
			return trimWord(w)
		}
	}
	return ""
}

// trimWord убирает завершающую пунктуацию токена, прилипшую к слову при
// разбиении по пробелам (например, "search_path=" → "search_path", "public," →
// "public", "table(" → "table", "work_mem='" → "work_mem").
func trimWord(s string) string {
	return strings.TrimRight(s, ",;=()'\"")
}
