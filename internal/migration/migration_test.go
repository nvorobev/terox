package migration

import (
	"strings"
	"testing"
)

func TestBuildTransactional(t *testing.T) {
	body := "update items.users_locks_global set type_id = 4 where type_id = 3;"
	got, err := BuildTransactional(body, "_fa", "500ms", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Всё в одной транзакции через SET LOCAL: роль и таймаут откатываются на
	// COMMIT и не утекают к следующему клиенту пула.
	for _, want := range []string{
		`set local role _fa;`,
		"begin;",
		"set local statement_timeout = '500ms';",
		"commit;",
		body,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// Ровно одна транзакция (одна пара begin/commit) без утечки состояния сессии:
	// нет голого `set role` и нет `reset all` (SET LOCAL откатывается сам).
	if strings.Count(got, "begin;") != 1 {
		t.Errorf("expected exactly 1 begin:\n%s", got)
	}
	if strings.Contains(got, "reset all") {
		t.Errorf("should not use reset all (SET LOCAL self-reverts):\n%s", got)
	}
	if strings.Contains(got, "set role ") {
		t.Errorf("must use SET LOCAL ROLE, not session-level SET ROLE:\n%s", got)
	}
	// Порядок: begin, затем set local role, затем таймаут, затем тело.
	beginIdx := strings.Index(got, "begin;")
	roleIdx := strings.Index(got, "set local role")
	stIdx := strings.Index(got, "statement_timeout")
	bodyIdx := strings.Index(got, "update items")
	if !(beginIdx < roleIdx && roleIdx < stIdx && stIdx < bodyIdx) {
		t.Errorf("wrong ordering begin=%d role=%d st=%d body=%d", beginIdx, roleIdx, stIdx, bodyIdx)
	}
}

func TestBuildTransactionalOmitsEmpty(t *testing.T) {
	got, err := BuildTransactional("update t set x=1;", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "role") {
		t.Errorf("should omit role:\n%s", got)
	}
	if strings.Contains(got, "statement_timeout") {
		t.Errorf("should omit timeout:\n%s", got)
	}
	if strings.Count(got, "begin;") != 1 {
		t.Errorf("expected 1 begin:\n%s", got)
	}

	withLock, err := BuildTransactional("update t set x=1;", "_fa", "5s", "1s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(withLock, "set local lock_timeout = '1s';") {
		t.Errorf("expected lock_timeout:\n%s", withLock)
	}
}

func TestBuildTransactionalQuotesAndValidates(t *testing.T) {
	// Роль с двойной кавычкой внутри экранируется и не может вырваться из
	// выражения `set local role`.
	got, err := BuildTransactional("update t set x=1;", `we"ird`, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `set local role "we""ird";`) {
		t.Errorf("role not quoted/escaped:\n%s", got)
	}

	// Недопустимая роль (управляющий символ) и недопустимый таймаут — ошибки.
	if _, err := BuildTransactional("update t;", "ro\nle", "", ""); err == nil {
		t.Error("expected error for role with control character")
	}
	if _, err := BuildTransactional("update t;", "_fa", "5 seconds", ""); err == nil {
		t.Error("expected error for invalid statement_timeout")
	}
	if _, err := BuildTransactional("update t;", "_fa", "", "soon"); err == nil {
		t.Error("expected error for invalid lock_timeout")
	}
}

func TestIsNonTransactional(t *testing.T) {
	yes := []string{
		"CREATE INDEX CONCURRENTLY i ON t(a)",
		"create index concurrently if not exists i on t(a)",
		"DROP INDEX CONCURRENTLY i",
		"REINDEX INDEX CONCURRENTLY i",
		"VACUUM ANALYZE t",
		"vacuum (full) t",
		"CREATE DATABASE x",
		"DROP DATABASE x",
		"ALTER SYSTEM SET work_mem = '1GB'",
	}
	for _, s := range yes {
		if !IsNonTransactional(s) {
			t.Errorf("expected non-transactional: %q", s)
		}
	}

	no := []string{
		"UPDATE t SET x = 1",
		"CREATE INDEX i ON t(a)", // не concurrent
		"SELECT 1",
		"REFRESH MATERIALIZED VIEW CONCURRENTLY mv", // допустимо в транзакции
		"ALTER TABLE t ADD COLUMN a int",
		"-- vacuum in a comment\nSELECT 1",
	}
	for _, s := range no {
		if IsNonTransactional(s) {
			t.Errorf("expected transactional: %q", s)
		}
	}
}

func TestIsNonTransactionalNoPanicOnNonLetterLead(t *testing.T) {
	// Выражение, чей первый непробельный байт — не ASCII-буква (кавычки, цифра,
	// пунктуация), должно классифицироваться как транзакционное и не падать:
	// IsNonTransactional работает на произвольном SQL пользователя в пути записи.
	for _, s := range []string{
		`"my table" cluster something`,
		`42`,
		`(select 1)`,
		`"weird";`,
		``,
		`   `,
		`-- only a comment`,
	} {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("IsNonTransactional(%q) panicked: %v", s, rec)
				}
			}()
			if IsNonTransactional(s) {
				t.Errorf("expected transactional (false) for %q", s)
			}
			_ = Classify(s) // Classify прогоняет каждое разбитое выражение через IsNonTransactional
		}()
	}
}

func TestHasTxControlDoBlockCommit(t *testing.T) {
	// DO-блок с собственным COMMIT/ROLLBACK (PG11+) отклоняется: Mask затирает
	// тело в долларовых кавычках, поэтому сканируется исходное выражение.
	for _, s := range []string{
		"DO $$ BEGIN COMMIT; END $$;",
		"do $$ begin rollback; end $$",
	} {
		if !HasTxControl(s) {
			t.Errorf("expected HasTxControl=true for %q", s)
		}
	}
	// Обычный DO-блок без управления транзакцией остаётся разрешённым.
	if HasTxControl("DO $$ BEGIN PERFORM 1; END $$;") {
		t.Error("plain DO block without COMMIT/ROLLBACK should be allowed")
	}
}

func TestForbiddenOperation(t *testing.T) {
	// DROP DATABASE запрещён безусловно — во всех формах и позициях.
	for _, s := range []string{
		"DROP DATABASE foo",
		"drop database if exists foo",
		"  DROP   DATABASE bar;",
		"/* c */ DROP DATABASE baz",
		"CREATE TABLE t(i int);\nDROP DATABASE x;",
	} {
		if op := ForbiddenOperation(s); op != "DROP DATABASE" {
			t.Errorf("expected DROP DATABASE forbidden for %q, got %q", s, op)
		}
	}
	// Похожие, но безопасные/несовпадающие конструкции не блокируются.
	for _, s := range []string{
		"DROP TABLE foo",
		"DROP SCHEMA s CASCADE",
		"INSERT INTO log VALUES ('drop database x')", // литерал маскируется
		"-- DROP DATABASE x",                         // только комментарий
		"SELECT 1",
	} {
		if op := ForbiddenOperation(s); op != "" {
			t.Errorf("expected no forbidden op for %q, got %q", s, op)
		}
	}
}

func TestClassify(t *testing.T) {
	tx := Classify("set role _fa;\nbegin;\nupdate t set x=1;\ncommit;")
	if tx.NonTransactional || tx.Mixed {
		t.Error("expected transactional plan")
	}

	cc := Classify("CREATE INDEX CONCURRENTLY i1 ON t(a);\nCREATE INDEX CONCURRENTLY i2 ON t(b);")
	if !cc.NonTransactional {
		t.Error("expected non-transactional plan")
	}
	if len(cc.Statements) != 2 {
		t.Errorf("expected 2 statements, got %d", len(cc.Statements))
	}
}

func TestClassifyMixedRejected(t *testing.T) {
	// Файл, смешивающий транзакционный блок с выражением CONCURRENTLY, помечается
	// как Mixed, чтобы вызывающий его отклонил (разбиение сломало бы begin/commit).
	mixed := Classify("begin;\nupdate t set x=1;\ncommit;\nCREATE INDEX CONCURRENTLY i ON t(a);")
	if !mixed.Mixed {
		t.Errorf("expected Mixed plan, got %+v", mixed)
	}
	if mixed.NonTransactional {
		t.Error("Mixed plan should not be NonTransactional")
	}
}

func TestDiscardVariants(t *testing.T) {
	if !IsNonTransactional("DISCARD ALL") {
		t.Error("DISCARD ALL should be non-transactional")
	}
	for _, s := range []string{"DISCARD PLANS", "discard sequences", "DISCARD TEMP"} {
		if IsNonTransactional(s) {
			t.Errorf("%s can run in a transaction — should be transactional", s)
		}
	}
}

func TestNonTransactionalExtra(t *testing.T) {
	nonTx := []string{
		"REINDEX DATABASE shard_0",
		"reindex schema public",
		"REINDEX SYSTEM shard_0",
		"REINDEX (VERBOSE) DATABASE shard_0",
		"ALTER TABLE p DETACH PARTITION c CONCURRENTLY",
		"ALTER TABLE ONLY p DETACH PARTITION c CONCURRENTLY",
		"ALTER DATABASE shard_0 SET TABLESPACE fast",
		"CLUSTER",
		"CLUSTER VERBOSE",
	}
	for _, s := range nonTx {
		if !IsNonTransactional(s) {
			t.Errorf("expected non-transactional: %q", s)
		}
	}
	tx := []string{
		"REINDEX TABLE items",
		"REINDEX INDEX items_pkey",
		"ALTER TABLE p DETACH PARTITION c",
		"ALTER TABLE p DETACH PARTITION c FINALIZE",
		"ALTER DATABASE shard_0 SET statement_timeout = '5s'",
		"ALTER DATABASE shard_0 RENAME TO x",
		"CLUSTER items USING items_pkey",
		"CLUSTER VERBOSE items",
	}
	for _, s := range tx {
		if IsNonTransactional(s) {
			t.Errorf("expected transactional: %q", s)
		}
	}
}

func TestClusterParenthesizedOption(t *testing.T) {
	// CLUSTER (VERBOSE) без имени таблицы нельзя выполнять в транзакции.
	for _, s := range []string{
		"CLUSTER (VERBOSE)", "cluster (verbose);", "CLUSTER (VERBOSE, ANALYZE)",
		"CLUSTER ( verbose )", "CLUSTER", "CLUSTER VERBOSE",
	} {
		if !IsNonTransactional(s) {
			t.Errorf("expected non-transactional (bare cluster): %q", s)
		}
	}
	for _, s := range []string{
		"CLUSTER items", "CLUSTER VERBOSE items", "CLUSTER (VERBOSE) items",
		"CLUSTER items USING items_pkey",
	} {
		if IsNonTransactional(s) {
			t.Errorf("expected transactional (cluster of a table): %q", s)
		}
	}
}

func TestAlterDatabaseQuotedNameTablespace(t *testing.T) {
	// Имя базы в кавычках (с пробелом или без) тоже распознаётся.
	for _, s := range []string{
		`ALTER DATABASE "my db" SET TABLESPACE fast`,
		`ALTER DATABASE "mydb" SET TABLESPACE fast`,
		`alter database shard_0 set tablespace fast`,
	} {
		if !IsNonTransactional(s) {
			t.Errorf("expected non-transactional: %q", s)
		}
	}
}

func TestConcurrentlyNotMatchedInLiteral(t *testing.T) {
	noMatch := []string{
		"CREATE TABLE t (note text DEFAULT 'run concurrently later')",
		"CREATE VIEW v AS SELECT 'concurrently' AS x",
		"INSERT INTO t VALUES ('drop index concurrently foo')",
	}
	for _, s := range noMatch {
		if IsNonTransactional(s) {
			t.Errorf("false positive — should be transactional: %q", s)
		}
	}
	// Настоящий REINDEX ... CONCURRENTLY распознаётся.
	if !IsNonTransactional("REINDEX TABLE CONCURRENTLY t") {
		t.Error("REINDEX TABLE CONCURRENTLY should be non-transactional")
	}
}

func TestHasTxControl(t *testing.T) {
	yes := []string{
		"begin;\nupdate t set x=1;\ncommit;",
		"set role _fa;\nupdate t set x=1;",
		"START TRANSACTION; update t set x=1; commit;",
		// END/ABORT — синонимы COMMIT/ROLLBACK; в середине тела они бы тихо
		// сломали защитную обёртку.
		"update t set x=1;\nend;",
		"update t set x=1;\nABORT;",
		// SET SESSION AUTHORIZATION — смена роли, переживающая COMMIT.
		"set session authorization postgres;\nupdate t set x=1;",
		// Формы SET LOCAL откатываются на COMMIT, но перекрывают собственные
		// SET LOCAL обёртки до конца тела; таймауты и роль нельзя тихо менять
		// внутри обёрнутой миграции.
		"set local role hacker;\nupdate t set x=1;",
		"set local statement_timeout = 0;\nupdate big set x=1;",
		"set local lock_timeout = '0';\nalter table t add column y int;",
		"reset role;\nupdate t set x=1;",
		"reset all;\nupdate t set x=1;",
		// set_config(...) — функциональная форма SET для тех же настроек.
		"select set_config('statement_timeout','0',true);\nupdate big set x=1;",
		"select set_config('role','postgres',false);\nupdate t set x=1;",
		// Любой set_config отклоняется: имя защищённой настройки можно спрятать в
		// литералах с кавычками/долларами/E/U&, которые регэксп по настройке не
		// читает, а само имя функции можно взять в кавычки или указать со схемой.
		`select "set_config"('statement_timeout','0',true);` + "\nupdate big set x=1;",
		"select set_config($x$statement_timeout$x$,$x$0$x$,true);\nupdate big set x=1;",
		"select pg_catalog.set_config('statement_timeout','0',true);\nupdate big set x=1;",
		`select U&"set_config"('statement_timeout','0',true);` + "\nupdate big set x=1;",
		// Незащищённая настройка (search_path) тоже отклоняется: обёрнутая
		// миграция вообще не должна вызывать set_config (для дословного запуска \i).
		"select set_config('search_path','public',true);\nupdate t set x=1 where id=1;",
		// PREPARE TRANSACTION завершает транзакцию обёртки через двухфазный коммит.
		"update t set x=1;\nprepare transaction 'leak';",
	}
	for _, s := range yes {
		if !HasTxControl(s) {
			t.Errorf("expected tx control: %q", s)
		}
	}
	// Текст 'set_config' внутри строкового литерала не считается (нет вызова).
	if HasTxControl("update t set note = 'call set_config(''role'',...)' where id=1") {
		t.Error("literal mentioning set_config should not report tx control")
	}
	if HasTxControl("update t set x = 1 where id = 5") {
		t.Error("plain body should not report tx control")
	}
	// 'set role' внутри литерала не считается.
	if HasTxControl("update t set note = 'set role hacker' where id=1") {
		t.Error("literal 'set role' should not report tx control")
	}
	// 'END' как завершитель CASE (не выражение) не должно давать ложное
	// срабатывание: leadingWordRe берёт первое слово выражения, здесь UPDATE.
	if HasTxControl("update t set x = case when a then 1 else 2 end where id=1") {
		t.Error("CASE ... END should not report tx control")
	}
}

func TestBuildTransactionalTrailingComment(t *testing.T) {
	// Тело, чья последняя строка — незавершённое выражение и строчный комментарий,
	// всё равно даёт завершённое выражение перед финальным commit.
	got, err := BuildTransactional("update t set x=1 -- the important update", "_fa", "5s", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "\ncommit;") {
		t.Errorf("missing trailing commit:\n%s", got)
	}
	// Добавленный завершитель должен быть на своей строке (не внутри комментария).
	if !strings.Contains(got, "-- the important update\n;") {
		t.Errorf("terminator should follow the comment on a new line:\n%s", got)
	}
}
