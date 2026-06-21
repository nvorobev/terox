// Package migration собирает защитную обёртку миграции (один exec)
// и определяет операторы, которые должны выполняться вне транзакции.
package migration

import (
	"fmt"
	"regexp"
	"strings"

	"terox/internal/sqlsplit"
)

// pgDurationRe — грамматика длительности PostgreSQL для SET timeout (значение
// подставляется в `set local statement_timeout = '...'`). Некорректные значения
// отвергаются как блокирующая ошибка, чтобы опечатка не попала в SQL миграции.
var pgDurationRe = regexp.MustCompile(`^(0|[0-9]+(\.[0-9]+)?\s?(us|ms|s|min|h|d)?)$`)

// roleIdentRe — роль в виде простого строчного идентификатора, который можно
// писать без кавычек (частый случай, например _fa).
var roleIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// quoteRoleIdent формирует роль для `set local role`: без кавычек, если это
// простой строчный идентификатор (например _fa), иначе — в двойных кавычках с
// удвоением внутренних кавычек, чтобы роль с необычными символами (пробелы,
// кавычки, верхний регистр) не сломала оператор и не свелась к другой роли.
func quoteRoleIdent(role string) string {
	if roleIdentRe.MatchString(role) {
		return role
	}
	return `"` + strings.ReplaceAll(role, `"`, `""`) + `"`
}

// quoteLiteral оформляет s как строковый литерал SQL в одинарных кавычках
// (внутренние кавычки удваиваются) — дополнительная защита от шальной кавычки.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}

// validateRole отвергает роль с управляющими символами или NUL — такое имя
// роли PostgreSQL невозможно и оно бы испортило exec.
func validateRole(role string) error {
	for _, r := range role {
		if r == 0 || r < 0x20 {
			return fmt.Errorf("migration_role contains an invalid control character")
		}
	}
	return nil
}

// validateDuration отвергает непустое значение, не являющееся корректной
// длительностью PostgreSQL. Пустое значение означает "таймаут не задан".
func validateDuration(name, val string) error {
	val = strings.TrimSpace(val)
	if val == "" {
		return nil
	}
	if !pgDurationRe.MatchString(val) {
		return fmt.Errorf("%s %q is not a valid PostgreSQL duration (e.g. 500ms, 5s, 2min)", name, val)
	}
	return nil
}

// BuildTransactional оборачивает тело миграции в защитную обёртку и возвращает
// её одной строкой для отправки как ОДИН exec:
//
//	begin;
//	set local role "<role>";                    -- только если role != ""
//	set local statement_timeout = '<stmtTimeout>';
//	set local lock_timeout = '<lockTimeout>';   -- только если lockTimeout != ""
//
//	<body>
//
//	commit;
//
// Всё выполняется в ОДНОЙ транзакции через SET LOCAL, поэтому роль и таймауты
// действуют только на тело и автоматически сбрасываются на COMMIT/ROLLBACK.
// Это гарантирует, что повышенная роль не утечёт следующему клиенту на том же
// бэкенде из пула (pgbouncer в transaction mode по умолчанию не запускает
// reset-query).
//
// Роль экранируется как идентификатор, значения таймаутов валидируются, так что
// некорректный migration_role/таймаут — блокирующая ошибка. Тело НЕ должно
// содержать собственных begin/commit.
func BuildTransactional(body, role, stmtTimeout, lockTimeout string) (string, error) {
	role = strings.TrimSpace(role)
	if err := validateRole(role); err != nil {
		return "", err
	}
	if err := validateDuration("statement_timeout", stmtTimeout); err != nil {
		return "", err
	}
	if err := validateDuration("lock_timeout", lockTimeout); err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("begin;\n")
	if role != "" {
		fmt.Fprintf(&b, "set local role %s;\n", quoteRoleIdent(role))
	}
	if t := strings.TrimSpace(stmtTimeout); t != "" {
		fmt.Fprintf(&b, "set local statement_timeout = %s;\n", quoteLiteral(t))
	}
	if lt := strings.TrimSpace(lockTimeout); lt != "" {
		fmt.Fprintf(&b, "set local lock_timeout = %s;\n", quoteLiteral(lt))
	}
	b.WriteString("\n")
	body = strings.TrimSpace(body)
	b.WriteString(body)
	// Гарантируем, что тело завершается точкой с запятой и не сливается с
	// финальным commit. Проверяем хвост без комментариев (иначе концевой
	// строчный комментарий проглотил бы добавленную ';') и ставим терминатор на
	// отдельной строке, чтобы он не попал внутрь комментария.
	if !strings.HasSuffix(strings.TrimSpace(sqlsplit.Mask(body)), ";") {
		b.WriteString("\n;")
	}
	b.WriteString("\n\ncommit;\n")
	return b.String(), nil
}

// SessionGuards строит набор `SET LOCAL ...` операторов, которые задают роль и
// таймауты для тела защищённого write/COPY ВНУТРИ уже открытой транзакции
// (BEGIN ... COMMIT). В отличие от BuildTransactional (который сам сшивает
// begin/тело/commit в один exec простого протокола), здесь нужны только сами
// guard-операторы — их выполняет вызывающий на conn-уровневой транзакции, где
// тело — это COPY ... FROM STDIN (его нельзя засунуть в один Exec со скриптом).
//
// Возвращаются операторы в порядке role → statement_timeout → lock_timeout;
// пустые значения пропускаются. Роль экранируется как идентификатор, длительности
// валидируются — некорректный role/timeout даёт блокирующую ошибку (как в
// BuildTransactional), а не молча неэкранированный SQL. SET LOCAL откатывается на
// COMMIT/ROLLBACK, поэтому роль/таймауты не утекают следующему клиенту пула.
func SessionGuards(role, stmtTimeout, lockTimeout string) ([]string, error) {
	role = strings.TrimSpace(role)
	if err := validateRole(role); err != nil {
		return nil, err
	}
	if err := validateDuration("statement_timeout", stmtTimeout); err != nil {
		return nil, err
	}
	if err := validateDuration("lock_timeout", lockTimeout); err != nil {
		return nil, err
	}
	var out []string
	if role != "" {
		out = append(out, "set local role "+quoteRoleIdent(role))
	}
	if t := strings.TrimSpace(stmtTimeout); t != "" {
		out = append(out, "set local statement_timeout = "+quoteLiteral(t))
	}
	if lt := strings.TrimSpace(lockTimeout); lt != "" {
		out = append(out, "set local lock_timeout = "+quoteLiteral(lt))
	}
	return out, nil
}

var (
	// Привязка к реальной грамматике, чтобы "concurrently" внутри литерала или в
	// не-индексном операторе не совпадало. CONCURRENTLY идёт сразу после ключевого
	// слова объекта (CREATE [UNIQUE] INDEX CONCURRENTLY ..., DROP INDEX
	// CONCURRENTLY ..., REINDEX {INDEX|TABLE|...} CONCURRENTLY ...).
	concurrentlyRe = regexp.MustCompile(`(?is)^\s*(create\s+(unique\s+)?index\s+concurrently|drop\s+index\s+concurrently|reindex\s+(\([^)]*\)\s+)?(index|table|schema|database|system)\s+concurrently)\b`)
	// REINDEX уровня кластера (SYSTEM/DATABASE/SCHEMA) нельзя выполнить в
	// транзакции даже без CONCURRENTLY; REINDEX TABLE/INDEX — можно.
	reindexClusterRe = regexp.MustCompile(`(?is)^\s*reindex\s+(\([^)]*\)\s+)?(system|database|schema)\b`)
	// ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY нельзя в транзакции.
	detachConcurrentlyRe = regexp.MustCompile(`(?is)^\s*alter\s+table\s+.*\bdetach\s+partition\b.*\bconcurrently\b`)
	// ALTER DATABASE ... SET TABLESPACE физически перемещает файлы; не транзакционно.
	alterDBTablespaceRe = regexp.MustCompile(`(?is)^\s*alter\s+database\s+\S+\s+set\s+tablespace\b`)
	// Голый CLUSTER без имени таблицы (возможно VERBOSE или список опций в
	// скобках, например CLUSTER (VERBOSE)) перекластеризует всю БД и не может
	// выполняться в транзакции; CLUSTER <table> — может.
	clusterBareRe = regexp.MustCompile(`(?is)^\s*cluster\b\s*(\([^)]*\)|verbose\b)?\s*;?\s*$`)
	leadingWordRe = regexp.MustCompile(`^\s*([a-zA-Z]+)`)
	twoWordRe     = regexp.MustCompile(`^\s*([a-zA-Z]+)\s+([a-zA-Z]+)`)
)

// IsNonTransactional сообщает, что оператор не может выполняться внутри блока
// транзакции и потому должен идти отдельным autocommit-exec (без обёртки
// роли/таймаутов). Набор намеренно консервативен.
func IsNonTransactional(stmt string) bool {
	// Нейтрализуем комментарии и содержимое литералов/идентификаторов, чтобы
	// ключевые слова внутри них (например, дефолт колонки 'run concurrently
	// later') не совпадали, а идентификатор в кавычках (например, ALTER DATABASE
	// "my db" ...) оставался одним токеном для регулярок.
	clean := strings.TrimSpace(sqlsplit.Mask(stmt))
	if clean == "" {
		return false
	}
	low := strings.ToLower(clean)

	// CREATE/DROP/REINDEX ... CONCURRENTLY (REFRESH MATERIALIZED VIEW
	// CONCURRENTLY допустим в транзакции и исключён грамматической привязкой в
	// concurrentlyRe). Плюс REINDEX уровня кластера, DETACH PARTITION
	// CONCURRENTLY и ALTER DATABASE ... SET TABLESPACE.
	if concurrentlyRe.MatchString(clean) || reindexClusterRe.MatchString(clean) ||
		detachConcurrentlyRe.MatchString(clean) || alterDBTablespaceRe.MatchString(clean) {
		return true
	}

	first := ""
	if fm := leadingWordRe.FindStringSubmatch(low + " "); fm != nil {
		first = fm[1]
	}
	if first == "vacuum" {
		return true
	}
	// Голый CLUSTER (без таблицы) — включая CLUSTER VERBOSE и CLUSTER (VERBOSE) —
	// перекластеризует всю БД и не может выполняться в транзакции.
	if clusterBareRe.MatchString(low) {
		return true
	}

	m := twoWordRe.FindStringSubmatch(low)
	if m != nil {
		switch m[1] + " " + m[2] {
		case "create database", "drop database",
			"create tablespace", "drop tablespace",
			"alter system", "discard all":
			return true
		}
	}
	return false
}

// Plan описывает, как должен выполняться скрипт.
type Plan struct {
	// NonTransactional равно true, когда ВСЕ операторы должны выполняться вне
	// транзакции (каждый отдельным autocommit-exec, без обёртки).
	NonTransactional bool
	// Mixed равно true, когда скрипт смешивает нетранзакционные операторы
	// (CONCURRENTLY/VACUUM/...) с транзакционными. Такой файл нельзя выполнить
	// безопасно: вызывающий должен его отклонить и попросить автора разделить.
	Mixed      bool
	Statements []string
}

var (
	// protectedSetRe ловит SET/RESET, меняющий настройку, на которую опирается
	// обёртка: роль / session authorization или таймауты statement/lock.
	// Необязательный квалификатор LOCAL/SESSION учитывается, так что `SET LOCAL
	// ROLE` и `SET LOCAL statement_timeout` ловятся наравне с голыми формами, и
	// RESET ALL (сбрасывающий SET LOCAL-таймауты обёртки) тоже включён.
	protectedSetRe = regexp.MustCompile(`(?is)^(set|reset)\s+(local\s+|session\s+)?(role|session\s+authorization|authorization|statement_timeout|lock_timeout|all)\b`)
	// setConfigCallRe ловит реальный вызов функции set_config(...), возможно с
	// квалификацией схемой (pg_catalog.set_config). В обёрнутой миграции ЛЮБОЙ
	// set_config отклоняется: он может незаметно сменить
	// role/session_authorization/statement_timeout/lock_timeout, а имя защищённой
	// настройки можно спрятать в кавычках, dollar-quoted, E-строке или U&-литерале,
	// которые регулярка по имени не распознает надёжно. Этими настройками владеет
	// обёртка через SET LOCAL; миграция, которой действительно нужен set_config,
	// должна выполняться дословно через \i, где роль/таймауты задаёт оператор.
	// Проверяется по выводу MaskKeepQuoted, чтобы имя функции в кавычках/U&
	// (например "set_config", U&"set_config") тоже распознавалось.
	setConfigCallRe = regexp.MustCompile(`(?is)\bset_config\s*\(`)
	// doBlockTxControlRe ловит явный COMMIT/ROLLBACK, который DO-блок или тело
	// процедуры могут выполнить сами (PG11+). Mask обнуляет dollar-quoted тело,
	// поэтому сканируется сырой оператор; ложное совпадение внутри литерала лишь
	// отклоняет миграцию — это безопасное направление.
	doBlockTxControlRe = regexp.MustCompile(`(?is)\b(commit|rollback)\b`)
)

// HasTxControl сообщает, содержит ли скрипт собственное управление транзакцией
// или отменяет гарантию обёртки — в этом случае оборачивать его НЕбезопасно и
// он должен быть отклонён (вызывающий превращает true в refuseTxControl).
// Распознаются формы:
//
//   - управление транзакцией: begin/commit/rollback/start/end/abort и
//     PREPARE TRANSACTION (двухфазная передача, завершающая транзакцию обёртки);
//   - переключения роли, выходящие за обёртку: SET [LOCAL|SESSION] ROLE,
//     SET SESSION AUTHORIZATION, RESET ROLE;
//   - изменения защищённых таймаутов: SET [LOCAL|SESSION]
//     statement_timeout/lock_timeout, их RESET или RESET ALL;
//   - те же мутации через set_config(...).
//
// END и ABORT — синонимы COMMIT и ROLLBACK в PostgreSQL; без их учёта `end;`
// посреди тела преждевременно зафиксировал бы защитную обёртку, сбросив
// `set local role` и выполнив остаток под повышенной сессионной ролью.
//
// Ложные срабатывания здесь — БЕЗОПАСНОЕ направление (миграция просто
// отклоняется), поэтому распознавание намеренно широкое.
func HasTxControl(script string) bool {
	for _, s := range sqlsplit.Split(script) {
		clean := strings.ToLower(strings.TrimSpace(sqlsplit.Mask(s)))
		first := ""
		if fm := leadingWordRe.FindStringSubmatch(clean + " "); fm != nil {
			first = fm[1]
		}
		switch first {
		case "begin", "commit", "rollback", "start", "end", "abort":
			return true
		case "prepare":
			// PREPARE TRANSACTION передаёт транзакцию обёртки как двухфазный commit,
			// обнуляя SET LOCAL роли/таймаутов.
			if m := twoWordRe.FindStringSubmatch(clean); m != nil && m[2] == "transaction" {
				return true
			}
		case "set", "reset":
			if protectedSetRe.MatchString(clean) {
				return true
			}
		case "do", "call":
			// DO-блок / тело процедуры могут сами сделать COMMIT/ROLLBACK (PG11+);
			// Mask скрывает dollar-quoted тело, поэтому сканируем сырой оператор.
			if doBlockTxControlRe.MatchString(strings.ToLower(s)) {
				return true
			}
		}
		// set_config(...) может встретиться внутри любого оператора (обычно SELECT)
		// и сменить роль/таймауты из выражения. Ищем вызов в тексте с
		// развёрнутыми кавычками и U&, чтобы "set_config", U&"set_config" и
		// pg_catalog.set_config распознавались, и отклоняем любое вхождение — имя
		// защищённой настройки можно спрятать в quoted/dollar/E/U&-литералах.
		if setConfigCallRe.MatchString(strings.ToLower(sqlsplit.MaskKeepQuoted(s))) {
			return true
		}
	}
	return false
}

// dropDatabaseRe ловит DROP DATABASE (в т.ч. IF EXISTS) в начале оператора.
var dropDatabaseRe = regexp.MustCompile(`(?s)^\s*drop\s+database\b`)

// ForbiddenOperation возвращает имя операции, которую terox запрещает БЕЗУСЛОВНО —
// её нельзя включить ни write-режимом, ни подтверждением. Сейчас это DROP DATABASE:
// необратимое уничтожение всей базы шарда, для которого нет безопасного сценария из
// интерактивного клиента. Пустая строка — запрещённых операций нет. Распознавание
// идёт по тем же маскированным стейтментам, что и HasTxControl, поэтому
// DROP DATABASE внутри строкового литерала или комментария не считается.
func ForbiddenOperation(script string) string {
	for _, s := range sqlsplit.Split(script) {
		clean := strings.ToLower(strings.TrimSpace(sqlsplit.Mask(s)))
		if dropDatabaseRe.MatchString(clean) {
			return "DROP DATABASE"
		}
	}
	return ""
}

// Classify разбивает скрипт и выбирает стратегию выполнения. Файл должен быть
// либо целиком транзакционным (один exec), либо целиком нетранзакционным
// (отдельные exec); смесь помечается Mixed, чтобы вызывающий мог её отклонить,
// а не тихо сломать транзакционное обрамление.
func Classify(script string) Plan {
	stmts := sqlsplit.Split(script)
	nonTx := 0
	for _, s := range stmts {
		if IsNonTransactional(s) {
			nonTx++
		}
	}
	switch {
	case nonTx == 0:
		return Plan{NonTransactional: false, Statements: stmts}
	case nonTx == len(stmts):
		return Plan{NonTransactional: true, Statements: stmts}
	default:
		return Plan{Mixed: true, Statements: stmts}
	}
}
