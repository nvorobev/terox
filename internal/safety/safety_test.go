package safety

import "testing"

func TestIsWrite(t *testing.T) {
	reads := []string{
		"SELECT 1",
		"select * from items where id = 5",
		"  -- comment\n SELECT now()",
		"/* block */ SELECT count(*) FROM t",
		"SHOW search_path",
		"TABLE items",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"EXPLAIN SELECT * FROM t",
		"VALUES (1),(2)",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("expected read-only: %q", q)
		}
	}

	writes := []string{
		"INSERT INTO t VALUES (1)",
		"update t set x = 1",
		"DELETE FROM t",
		"TRUNCATE t",
		"DROP TABLE t",
		"ALTER TABLE t ADD COLUMN x int",
		"CREATE INDEX i ON t(x)",
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		"with m as (update t set x=1 returning id) select * from m",
		"EXPLAIN ANALYZE INSERT INTO t VALUES (1)",
		"COPY t FROM STDIN",
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("expected write: %q", q)
		}
	}
}

func TestIsWriteSideEffectsAndSelectWrites(t *testing.T) {
	writes := []string{
		"select setval('s', 100)",
		"SELECT nextval('s')",
		"select pg_advisory_lock(1)",
		"select pg_try_advisory_lock(1)",
		"select lo_unlink(1234)",
		"SELECT pg_terminate_backend(123)",
		"select pg_cancel_backend(123)",
		"SELECT pg_reload_conf()",
		"SELECT * INTO new_table FROM users",
		"select a,b into temp newt from t",
		"SELECT id FROM t WHERE x=1 FOR UPDATE",
		"select * from t for share",
		"LOCK TABLE t IN ACCESS EXCLUSIVE MODE",
		"EXPLAIN ANALYSE INSERT INTO t VALUES (1)",
		"EXPLAIN (ANALYSE) DELETE FROM t",
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("expected write: %q", q)
		}
	}

	// Слова внутри строковых литералов не должны срабатывать.
	reads := []string{
		"SELECT * FROM events WHERE note = 'go into the void'",
		"select * from t where msg = 'please setval(now)'",
		"select id from t",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("expected read: %q", q)
		}
	}
}

func TestAllowlistUnknownLeadingKeyword(t *testing.T) {
	// Модель allowlist: любое ведущее ключевое слово, не доказуемо read-only,
	// считается потенциальной записью и требует подтверждения.
	writes := []string{
		"LOAD '/tmp/lib.so'",
		"load 'plugin'",
		"CHECKPOINT",
		"DISCARD ALL",
		"PREPARE p AS DELETE FROM t",
		"EXECUTE p",
		"DEALLOCATE p",
		"LISTEN chan",
		"NOTIFY chan",
		"SECURITY LABEL FOR x ON TABLE t IS 'y'",
		"IMPORT FOREIGN SCHEMA s FROM SERVER srv INTO local",
		"BEGIN",
		"COMMIT",
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("unknown/non-read leading keyword must be a potential write: %q", q)
		}
	}
	// Известные read-only формы остаются чтением.
	reads := []string{
		"SELECT 1", "SHOW all", "TABLE t", "VALUES (1)",
		"EXPLAIN SELECT 1", "WITH x AS (SELECT 1) SELECT * FROM x",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("known read-only form must stay a read: %q", q)
		}
	}
}

func TestHiddenCommentSeparatorNotExploitable(t *testing.T) {
	// ';' внутри комментария не разделяет операторы — sqlsplit и PostgreSQL
	// видят один оператор. "SELECT 1 /*; */ DELETE FROM t" — единственный
	// оператор с ведущим SELECT без маркера записи, значит это READ: выполняется
	// в read-only транзакции (DELETE блокируется БД), а текст всё равно невалиден.
	// Пути к незащищённой записи нет. Тест фиксирует: если это всё же сочтут
	// записью, должен сработать строгий гейт (AnyUnqualifiedWrite).
	s := "SELECT 1 /*; */ DELETE FROM t"
	if IsWrite(s) && !AnyUnqualifiedWrite(s) {
		t.Errorf("if %q is treated as a write it must also be gated as unqualified", s)
	}
	// Для выполнения двух операторов нужен настоящий ';' — он разбивается и
	// проходит через гейт.
	if !AnyUnqualifiedWrite("SELECT 1; DELETE FROM t") {
		t.Error("a real separator exposes the trailing unqualified DELETE")
	}
}

func TestAnyUnqualifiedWriteMultiStatement(t *testing.T) {
	// Завершающий безусловный DML после чтения запускает строгий гейт.
	if !AnyUnqualifiedWrite("SELECT 1; DELETE FROM t") {
		t.Error("SELECT 1; DELETE FROM t (no WHERE) must be an unqualified write")
	}
	if !AnyUnqualifiedWrite("select 1; truncate t") {
		t.Error("trailing TRUNCATE must be an unqualified write")
	}
	if AnyUnqualifiedWrite("SELECT 1; DELETE FROM t WHERE id = 5") {
		t.Error("a qualified trailing DELETE is not unqualified")
	}
	// Классификатор одного оператора пропускает многооператорный случай.
	if IsUnqualifiedWrite("SELECT 1; DELETE FROM t") {
		t.Error("single-statement classifier sees the leading SELECT, so it is not unqualified on its own")
	}
}

func TestIsUnqualifiedWrite(t *testing.T) {
	unqualified := []string{
		"update items set name = 'x'",
		"UPDATE items SET name = upper(name)",
		"delete from items",
		"DELETE FROM items",
		"truncate items",
		"update items set x = (select max(y) from other where id = 5)",               // WHERE только в подзапросе
		"merge into target t using source s on t.id = s.id when matched then delete", // MERGE без верхнеуровневого WHERE
		"MERGE INTO target t USING source s ON t.id = s.id WHEN MATCHED THEN UPDATE SET x = 1",
	}
	for _, q := range unqualified {
		if !IsUnqualifiedWrite(q) {
			t.Errorf("expected unqualified: %q", q)
		}
	}

	qualified := []string{
		"update items set name = 'x' where item_id = 5",
		"delete from items where item_id > 100",
		"select * from items",                                  // не запись
		"insert into items values (1)",                         // insert не затрагивает все строки
		"update items set note = 'no where here' where id = 1", // 'where' есть и в литерале
		// MERGE только со вставкой новых строк — обычная запись, не безусловная.
		"merge into target t using source s on t.id = s.id when not matched then insert (id) values (s.id)",
	}
	for _, q := range qualified {
		if IsUnqualifiedWrite(q) {
			t.Errorf("expected qualified/safe: %q", q)
		}
	}
}

// TestExplainAnalyzeUnqualifiedWrite: EXPLAIN ANALYZE РЕАЛЬНО выполняет вложенный
// запрос, поэтому безусловный DML под ним должен требовать усиленного
// подтверждения (RiskUnqualifiedWrite), а не обычного. Обычный EXPLAIN (без
// ANALYZE) ничего не исполняет и остаётся read-only.
func TestExplainAnalyzeUnqualifiedWrite(t *testing.T) {
	unqualified := []string{
		"EXPLAIN ANALYZE TRUNCATE t",
		"explain analyze delete from t",
		"EXPLAIN ANALYZE UPDATE t SET x = 1",
		"EXPLAIN (ANALYZE) DELETE FROM t",
		"EXPLAIN (ANALYZE, VERBOSE) UPDATE t SET x = 1",
		"EXPLAIN ANALYZE VERBOSE DELETE FROM t",
		"EXPLAIN ANALYSE TRUNCATE t", // британское написание
	}
	for _, q := range unqualified {
		if !IsUnqualifiedWrite(q) {
			t.Errorf("expected unqualified (EXPLAIN ANALYZE executes): %q", q)
		}
		if !IsWrite(q) {
			t.Errorf("expected write: %q", q)
		}
	}

	notUnqualified := []string{
		"EXPLAIN ANALYZE DELETE FROM t WHERE id = 5", // есть WHERE
		"EXPLAIN ANALYZE SELECT 1",                   // не запись
		"EXPLAIN TRUNCATE t",                         // без ANALYZE — не исполняется
		"EXPLAIN DELETE FROM t",                      // без ANALYZE — не исполняется
	}
	for _, q := range notUnqualified {
		if IsUnqualifiedWrite(q) {
			t.Errorf("expected NOT unqualified: %q", q)
		}
	}

	// Обычный EXPLAIN (без ANALYZE) запросом-на-запись не считается.
	for _, q := range []string{"EXPLAIN TRUNCATE t", "EXPLAIN DELETE FROM t", "EXPLAIN SELECT 1"} {
		if IsWrite(q) {
			t.Errorf("plain EXPLAIN must be read-only: %q", q)
		}
	}
}

func TestLiteralAwareClassification(t *testing.T) {
	// Маркер комментария внутри литерала — это текст, а не комментарий:
	// настоящие DELETE/WHERE всё равно видны.
	if !IsWrite("WITH x AS (SELECT 'oops --' AS c) DELETE FROM t USING x WHERE t.id=1") {
		t.Error("writing CTE hidden behind a '--' inside a literal must be a write")
	}
	if IsUnqualifiedWrite("UPDATE t SET note = 'see -- WHERE clause' WHERE id = 5") {
		t.Error("real WHERE must survive a '--' inside a literal (qualified)")
	}
	// Вызов с побочными эффектами не должен прятаться за маркером комментария в литерале.
	if !IsWrite("SELECT 'note -- ' AS a, pg_terminate_backend(123) AS b") {
		t.Error("pg_terminate_backend after a '--' literal must be a write")
	}
	// Тело dollar-quoted / E-строки — это данные: 'where' внутри не настоящий
	// WHERE, поэтому UPDATE по всем строкам помечается безусловным.
	if !IsUnqualifiedWrite("update t set note = $$ where $$") {
		t.Error("dollar-quoted 'where' is not a real WHERE — all-rows update")
	}
	if !IsUnqualifiedWrite(`update t set note = E'a\'where here'`) {
		t.Error("E-string escaped quote must not expose a fake WHERE")
	}
}

func TestIsUnqualifiedWithLeadingCTE(t *testing.T) {
	if !IsUnqualifiedWrite("with c as (select 1) update t set x=1") {
		t.Error("WITH ... UPDATE with no WHERE is an all-rows write")
	}
	if !IsUnqualifiedWrite("with c as (select 1) delete from t") {
		t.Error("WITH ... DELETE with no WHERE is an all-rows write")
	}
	if IsUnqualifiedWrite("with c as (select 1) update t set x=1 where id=5") {
		t.Error("WITH ... UPDATE ... WHERE is qualified")
	}
}

func TestIsWriteCTEWithSelectInto(t *testing.T) {
	writes := []string{
		"WITH c AS (SELECT 1) SELECT * INTO newt FROM c",
		"WITH c AS (SELECT 1 AS x) SELECT * FROM c FOR SHARE",
		"with d as (delete from t returning *) select * from d",
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("expected write: %q", q)
		}
	}
	if IsWrite("WITH c AS (SELECT 1) SELECT * FROM c") {
		t.Error("plain WITH ... SELECT should be a read")
	}
}

func TestIsWriteLeadingParensAndMultiStatement(t *testing.T) {
	if !IsWrite("(with d as (delete from t returning *) select * from d)") {
		t.Error("parenthesized writing CTE should be a write")
	}
	if IsWrite("((select 1))") {
		t.Error("parenthesized select should be a read")
	}
	if !IsWrite("SELECT 1; DROP TABLE t") {
		t.Error("multi-statement with trailing DROP should be a write")
	}
	if IsWrite("select 1; select 2") {
		t.Error("multi-statement pure reads should be a read")
	}
}

// TestIsWriteQuotedSideEffectFunctions проверяет: имя функции с побочными
// эффектами в двойных кавычках не проскакивает в read-only транзакцию
// (Mask заменяет содержимое кавычек-идентификатора на 'x').
func TestIsWriteQuotedSideEffectFunctions(t *testing.T) {
	writes := []string{
		`SELECT "pg_terminate_backend"(123)`,
		`select "pg_cancel_backend"(123)`,
		`SELECT public."dblink_exec"('host=h dbname=d', 'DROP TABLE important')`,
		`select "dblink"('h', 'select 1')`,
		`SELECT "pg_reload_conf"()`,
		`select "lo_unlink"(42)`,
		`SELECT pg_catalog."pg_read_file"('/etc/passwd')`,
		`SELECT "pg_terminate_backend" (123)`, // пробел перед скобкой
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("quoted side-effect call must be a write: %q", q)
		}
	}
	// Имя с побочными эффектами только внутри строкового литерала — это данные,
	// а не вызов, и остаётся чтением.
	reads := []string{
		`SELECT 'pg_terminate_backend(1)' AS note`,
		`SELECT col AS "pg_terminate_backend" FROM t`, // алиас столбца в кавычках, не вызов
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("non-call occurrence must stay a read: %q", q)
		}
	}
}

// TestIsWriteUnicodeEscapedSideEffect проверяет: функция с побочными эффектами
// за Unicode-экранированным идентификатором U&"..." распознаётся как запись
// (сервер декодирует \XXXX в реальное имя).
func TestIsWriteUnicodeEscapedSideEffect(t *testing.T) {
	writes := []string{
		`SELECT U&"pg_terminate_backend"(123)`,               // U& без экранирования
		`SELECT U&"pg_termin\0061te_backend"(123)`,           // \0061 = 'a'
		`select u&"pg_termin\0061te_backend"(123)`,           // строчное u&
		`SELECT U&"pg_cancel_backend"(123)`,                  //
		`SELECT U&"\0070g_terminate_backend"(123)`,           // \0070 = 'p'
		`SELECT U&"dblink_ex\+000065c"('h', 'DROP TABLE t')`, // \+000065 = 'e'
		`SELECT pg_catalog.U&"pg_read_file"('/etc/passwd')`,  // с указанием схемы
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("Unicode-escaped side-effect call must be a write: %q", q)
		}
	}
	// Строковый литерал U&'...' (не идентификатор) с именем функции — это данные,
	// а не вызов, и остаётся чтением.
	if IsWrite(`SELECT U&'pg_terminate_backend(1)' AS note`) {
		t.Error("Unicode-escaped string literal must stay a read")
	}
}

// TestExplainAnalyzeNotSubstring проверяет фикс ложного срабатывания на подстроку
// "analyze" в имени объекта: ANALYZE распознаётся только как ОПЦИЯ EXPLAIN, а не как
// любое вхождение. EXPLAIN DELETE FROM analyze_log — обычный EXPLAIN (read-only,
// ничего не исполняет), тогда как настоящие EXPLAIN ANALYZE <write> остаются записью.
func TestExplainAnalyzeNotSubstring(t *testing.T) {
	// Имя объекта содержит "analyze" — это НЕ опция, обычный EXPLAIN ничего не
	// исполняет → read-only и не безусловная запись.
	reads := []string{
		"EXPLAIN DELETE FROM analyze_log",
		"explain delete from analyze_log",
		"EXPLAIN SELECT * FROM analyze_log",
		"EXPLAIN UPDATE analyze_log SET x = 1",
		"EXPLAIN (VERBOSE) DELETE FROM analyze_log",
		"EXPLAIN TRUNCATE analyze_log",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("plain EXPLAIN over an object named *analyze* must be read-only: %q", q)
		}
		if IsUnqualifiedWrite(q) {
			t.Errorf("plain EXPLAIN over an object named *analyze* must not be unqualified: %q", q)
		}
	}

	// Настоящий EXPLAIN ANALYZE во всех формах опции ДОЛЖЕН исполнять вложенный
	// запрос → запись (и безусловная без WHERE).
	writes := []string{
		"EXPLAIN ANALYZE DELETE FROM users",
		"explain analyze delete from users",
		"EXPLAIN (ANALYZE) DELETE FROM users",
		"EXPLAIN (ANALYZE TRUE) DELETE FROM users",
		"EXPLAIN (ANALYZE on) DELETE FROM users",
		"EXPLAIN (ANALYZE, VERBOSE) DELETE FROM users",
		"EXPLAIN (VERBOSE, ANALYZE) DELETE FROM users",
		"EXPLAIN (ANALYSE) DELETE FROM users",
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("EXPLAIN ANALYZE <write> must execute → write: %q", q)
		}
		if !IsUnqualifiedWrite(q) {
			t.Errorf("EXPLAIN ANALYZE <unqualified write> must be unqualified: %q", q)
		}
	}
}

// TestPgFileSideEffects проверяет фикс denylist: мутирующие функции, которые НЕ
// блокируются read-only транзакцией, помечаются как запись.
func TestPgFileSideEffects(t *testing.T) {
	writes := []string{
		"SELECT pg_file_write('x', 'y')",
		"select pg_file_write('cfg', 'data', false)",
		"SELECT pg_file_unlink('x')",
		"select pg_stat_statements_reset()",
		`SELECT "pg_file_write"('x','y')`, // имя в кавычках не должно проскочить
	}
	for _, q := range writes {
		if !IsWrite(q) {
			t.Errorf("mutating side-effect function must be a write: %q", q)
		}
	}
	// Контроль: чтение pg_logdir_ls и обычные SELECT остаются чтением (не добавлены
	// в denylist, иначе легитимные read-запросы ложно требовали бы write-режим).
	reads := []string{
		"SELECT pg_logdir_ls()",
		"SELECT pg_sleep(1)",
		"SELECT * FROM t WHERE note = 'pg_file_write here'",
	}
	for _, q := range reads {
		if IsWrite(q) {
			t.Errorf("non-mutating / literal occurrence must stay a read: %q", q)
		}
	}
}

// TestIsUnqualifiedWriteDataModifyingCTE проверяет: DELETE/UPDATE по всем строкам
// внутри тела CTE запускает строгое подтверждение, даже если завершающий оператор —
// безобидный SELECT.
func TestIsUnqualifiedWriteDataModifyingCTE(t *testing.T) {
	unqualified := []string{
		"WITH deleted AS (DELETE FROM users RETURNING *) SELECT count(*) FROM deleted",
		"with u as (update accounts set balance = 0 returning id) select * from u",
		"WITH d AS (DELETE FROM t), e AS (SELECT 1) SELECT * FROM e",
	}
	for _, q := range unqualified {
		if !IsUnqualifiedWrite(q) {
			t.Errorf("data-modifying CTE without WHERE must be unqualified: %q", q)
		}
	}
	qualified := []string{
		"WITH deleted AS (DELETE FROM users WHERE id = 5 RETURNING *) SELECT count(*) FROM deleted",
		"with u as (update t set x=1 where id=1 returning id) select * from u",
		"WITH c AS (SELECT 1) SELECT * FROM c", // DML вообще нет
	}
	for _, q := range qualified {
		if IsUnqualifiedWrite(q) {
			t.Errorf("qualified or non-DML CTE must not be unqualified: %q", q)
		}
	}
}
