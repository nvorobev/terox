package migration

import (
	"strings"
	"testing"
)

func TestSessionStateViolationForbidden(t *testing.T) {
	// Каждая конструкция session-scoped и должна отклоняться в оборачиваемом теле.
	cases := []struct {
		name string
		sql  string
		want string // подстрока, ожидаемая в причине
	}{
		{"session set search_path", "SET search_path = private, public", "search_path"},
		{"session set work_mem no spaces", "SET work_mem='10GB'", "work_mem"},
		{"session set row_security", "SET row_security = off", "row_security"},
		{"set session characteristics", "SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE", "session"},
		{"reset", "RESET search_path", "RESET"},
		{"listen", "LISTEN chan", "LISTEN"},
		{"unlisten", "UNLISTEN *", "UNLISTEN"},
		{"prepare stmt", "PREPARE p AS SELECT 1", "PREPARE"},
		{"deallocate", "DEALLOCATE ALL", "DEALLOCATE"},
		{"declare cursor", "DECLARE c CURSOR FOR SELECT 1", "CURSOR"},
		{"create temp", "CREATE TEMP TABLE tmp (id int)", "TEMP"},
		{"create temporary", "CREATE TEMPORARY TABLE tmp (id int)", "TEMP"},
		{"create global temp", "CREATE GLOBAL TEMPORARY TABLE tmp (id int)", "TEMP"},
		{"create local temp", "CREATE LOCAL TEMP TABLE tmp (id int)", "TEMP"},
		{"create or replace temp view", "CREATE OR REPLACE TEMP VIEW v AS SELECT 1", "TEMP"},
		{"create or replace temporary view", "CREATE OR REPLACE TEMPORARY VIEW v AS SELECT 1", "TEMP"},
		{"create or replace temp sequence", "CREATE OR REPLACE TEMP SEQUENCE s", "TEMP"},
		{"load shared library", "LOAD 'auto_explain'", "LOAD"},
		{"advisory lock in DO block", "DO $$ BEGIN PERFORM pg_advisory_lock(1); END $$", "advisory"},
		{"set_config in DO block", "DO $$ BEGIN PERFORM set_config('search_path','x',false); END $$", "set_config"},
		{"discard", "DISCARD TEMP", "DISCARD"},
		{"session advisory lock", "SELECT pg_advisory_lock(42)", "advisory"},
		{"session advisory unlock", "SELECT pg_advisory_unlock(42)", "advisory"},
		{"try advisory lock", "SELECT pg_try_advisory_lock(42)", "advisory"},
		{"quoted advisory lock", `SELECT "pg_advisory_lock"(42)`, "advisory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SessionStateViolation(tc.sql)
			if got == "" {
				t.Fatalf("SessionStateViolation(%q) = \"\", want a violation", tc.sql)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("reason %q does not mention %q", got, tc.want)
			}
			// Каждая причина обязана назвать безопасную альтернативу (\i verbatim).
			if !strings.Contains(got, "\\i") {
				t.Errorf("reason %q must name the safe alternative (\\i verbatim)", got)
			}
		})
	}
}

func TestSessionStateViolationAllowed(t *testing.T) {
	// Транзакционно-безопасные конструкции НЕ должны отклоняться.
	cases := []struct {
		name string
		sql  string
	}{
		{"set local", "SET LOCAL search_path = private"},
		{"set transaction", "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE"},
		{"set constraints", "SET CONSTRAINTS ALL DEFERRED"},
		{"plain dml", "UPDATE t SET x = 1 WHERE id = 5"},
		{"plain ddl", "ALTER TABLE t ADD COLUMN c int"},
		{"create table", "CREATE TABLE t (id int)"},
		{"create unlogged", "CREATE UNLOGGED TABLE t (id int)"},
		{"create or replace function", "CREATE OR REPLACE FUNCTION f() RETURNS int AS 'select 1' LANGUAGE sql"},
		{"create or replace view (non-temp)", "CREATE OR REPLACE VIEW v AS SELECT 1"},
		{"xact advisory lock in DO block", "DO $$ BEGIN PERFORM pg_advisory_xact_lock(1); END $$"},
		{"notify is transactional", "NOTIFY chan, 'hi'"},
		{"xact advisory lock", "SELECT pg_advisory_xact_lock(42)"},
		{"try xact advisory lock", "SELECT pg_try_advisory_xact_lock(42)"},
		{"search_path inside literal", "INSERT INTO logs(msg) VALUES ('SET search_path = evil')"},
		{"listen inside literal", "INSERT INTO logs(msg) VALUES ('please LISTEN to me')"},
		{"temp word in column default", "CREATE TABLE t (note text DEFAULT 'create temp table')"},
		{"advisory in comment", "SELECT 1 -- pg_advisory_lock(1)"},
		{"set column is dml not guc", "UPDATE t SET search_path = 1 WHERE id = 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionStateViolation(tc.sql); got != "" {
				t.Errorf("SessionStateViolation(%q) = %q, want \"\" (allowed)", tc.sql, got)
			}
		})
	}
}

func TestSessionStateViolationCallProcedures(t *testing.T) {
	// CALL процедуры со session-scoped эффектом в теле (через set_config / session
	// advisory lock) отклоняется — Mask скрывает тело, поэтому сканируется сырой текст.
	bad := []struct{ name, sql string }{
		{"call set_config", "CALL run('x', set_config('search_path','y',false))"},
		{"call advisory lock", "CALL maintenance(pg_advisory_lock(7))"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got := SessionStateViolation(tc.sql); got == "" {
				t.Errorf("SessionStateViolation(%q) = \"\", want a refusal", tc.sql)
			}
		})
	}
	// Обычный CALL без session-state — допускается.
	ok := []string{
		"CALL refresh_totals()",
		"CALL archive_old_rows(30)",
	}
	for _, sql := range ok {
		if got := SessionStateViolation(sql); got != "" {
			t.Errorf("plain CALL %q wrongly refused: %q", sql, got)
		}
	}
}

// TestSessionStateDollarInKeyword фиксирует поведение единого лексера на границе
// «ключевое слово или идентификатор» (P2-3): SET/LISTEN с разделителем (пробел/
// таб) — настоящие команды и отклоняются; а SET$role/LISTEN$x — это ИДЕНТИФИКАТОРЫ
// ('$' допустим в идентификаторе PostgreSQL), не команды, поэтому НЕ нарушения. При
// этом обе проверки (SessionStateViolation и HasTxControl) согласованы — ни одна не
// видит скрытой команды там, где её нет.
func TestSessionStateDollarInKeyword(t *testing.T) {
	// Настоящие команды (с разделителем) — отклоняются.
	for _, s := range []string{"SET role = 'admin'", "SET\trole = 'admin'", "SET search_path = x", "LISTEN chan"} {
		if SessionStateViolation(s) == "" {
			t.Errorf("real command %q must be refused", s)
		}
	}
	// Идентификаторы set$role / listen$x — не команды; обе проверки согласны (нет нарушения).
	for _, s := range []string{"SET$role = 'admin'", "LISTEN$x"} {
		if got := SessionStateViolation(s); got != "" {
			t.Errorf("identifier %q is not a command; SessionStateViolation=%q", s, got)
		}
		if HasTxControl(s) {
			t.Errorf("identifier %q is not a command; HasTxControl should be false", s)
		}
	}
}

func TestSessionStateViolationExtraFalsePositives(t *testing.T) {
	// Идентификаторы/значения, ВЫГЛЯДЯЩИЕ как ключевые слова, но не команды.
	ok := []string{
		"INSERT INTO settings(name) VALUES ('search_path')",
		"UPDATE t SET listen = true WHERE id = 1",      // listen как столбец
		"SELECT prepare_date FROM orders WHERE id = 1", // prepare_* как столбец
		"SELECT * FROM declarations WHERE discard = false",
		"UPDATE config SET reset_at = now() WHERE id = 2", // reset_* как столбец
		"INSERT INTO audit(action) VALUES ('LOAD complete')",
	}
	for _, sql := range ok {
		if got := SessionStateViolation(sql); got != "" {
			t.Errorf("false positive: SessionStateViolation(%q) = %q, want allowed", sql, got)
		}
	}
}

func TestSessionStateViolationMultiStatement(t *testing.T) {
	// Нарушение в любом операторе скрипта (даже не первом) ловится.
	script := "UPDATE t SET x = 1 WHERE id = 1;\nSET search_path = evil;\n"
	if got := SessionStateViolation(script); got == "" || !strings.Contains(got, "search_path") {
		t.Errorf("multi-statement script: got %q, want search_path violation", got)
	}
	// Чисто безопасный многооператорный скрипт не отклоняется.
	ok := "UPDATE t SET x = 1 WHERE id = 1;\nSET LOCAL statement_timeout = '5s';\nINSERT INTO t VALUES (1);\n"
	if got := SessionStateViolation(ok); got != "" {
		t.Errorf("safe multi-statement script rejected: %q", got)
	}
}

func TestSessionStateViolationQuotedIdentifierGuc(t *testing.T) {
	// SET с именем GUC в кавычках всё ещё session-scoped → отклоняется
	// (консервативно: кавыченный токен не равен local/transaction/constraints).
	if got := SessionStateViolation(`SET "work_mem" = '1GB'`); got == "" {
		t.Errorf("quoted-identifier session SET should be refused")
	}
}

func TestSessionStateViolationCTE(t *testing.T) {
	// Изменяющий данные CTE сам по себе безопасен (транзакционен).
	if got := SessionStateViolation(
		"WITH d AS (DELETE FROM users WHERE id < 10 RETURNING *) INSERT INTO archive SELECT * FROM d"); got != "" {
		t.Errorf("data-modifying CTE wrongly flagged as session state: %q", got)
	}
}
