package repl

// Интеграционные тесты против живого PostgreSQL: проверяют SQL на реальном
// сервере. Включаются через TEROX_LIVE=1. Создают и удаляют собственные
// фикстуры public._terox_* на локальных шардах, не трогая items.*.
//
// Ожидаемое окружение: PostgreSQL на 127.0.0.1:55432 (хост через
// TEROX_LIVE_HOST), суперпользователь postgres/secret, базы shard_0,
// shard_1, shard_2.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
)

func auditLive(t *testing.T) {
	t.Helper()
	if os.Getenv("TEROX_LIVE") == "" {
		t.Skip("set TEROX_LIVE=1 to run live PostgreSQL integration tests")
	}
}

func auditHost() string {
	if h := os.Getenv("TEROX_LIVE_HOST"); h != "" {
		return h
	}
	return "127.0.0.1"
}

func auditShard(dbname string, pos int) cluster.Shard {
	return cluster.Shard{
		Host: auditHost(), Port: 55432, DB: dbname, User: "postgres",
		Password: "secret", SSLMode: "disable", Position: pos, Label: dbname,
	}
}

func auditCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 25*time.Second)
}

func mustExec(t *testing.T, m *db.Manager, s cluster.Shard, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := m.ExecOnce(ctx, s, sql); err != nil {
		t.Fatalf("setup exec failed on %s (%s): %v", s.DB, sql, err)
	}
}

func cleanupExec(m *db.Manager, s cluster.Shard, sql string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = m.ExecOnce(ctx, s, sql)
}

// TestLiveDescribeTableResolution проверяет, что describeTableSQL — валидный
// SQL и резолвит как голое имя (через search_path), так и имя с указанием
// схемы в колонки нужного отношения.
func TestLiveDescribeTableResolution(t *testing.T) {
	auditLive(t)
	m := db.NewManager()
	defer m.Close()
	ctx, cancel := auditCtx()
	defer cancel()
	s := auditShard("shard_0", 0)
	mustExec(t, m, s, `drop table if exists public._terox_desc; create table public._terox_desc(a int, b text)`)
	defer cleanupExec(m, s, `drop table if exists public._terox_desc`)

	for _, name := range []string{"_terox_desc", "public._terox_desc"} {
		tbl, err := parseTableArg(name)
		if err != nil {
			t.Fatalf("parseTableArg(%q): %v", name, err)
		}
		res, err := m.Exec(ctx, s, fmt.Sprintf(describeTableSQL, sqlLiteral(tbl)), true)
		if err != nil {
			t.Fatalf("\\d %s failed: %v", name, err)
		}
		if len(res.Rows) != 2 {
			t.Errorf("\\d %s returned %d columns, want 2", name, len(res.Rows))
		}
	}
}

// TestLiveExplainVersionAdaptive проверяет, что explainSQLFor строит EXPLAIN,
// который сервер принимает и для оценки, и для analyze, а вариант для
// неизвестной версии выполняется на любом сервере.
func TestLiveExplainVersionAdaptive(t *testing.T) {
	auditLive(t)
	m := db.NewManager()
	defer m.Close()
	ctx, cancel := auditCtx()
	defer cancel()
	s := auditShard("shard_0", 0)

	ver := 0
	if vr, e := m.Exec(ctx, s, `SELECT current_setting('server_version_num')::int`, true); e == nil && len(vr.Rows) > 0 && len(vr.Rows[0]) > 0 {
		ver = int(asInt64(vr.Rows[0][0]))
	}
	if ver == 0 {
		t.Fatal("could not read server_version_num")
	}
	for _, analyze := range []bool{false, true} {
		sql := explainSQLFor(explainOpts{analyze: analyze}, ver, "select count(*) from items.users")
		res, err := m.Exec(ctx, s, sql, true)
		if err != nil {
			t.Fatalf("EXPLAIN (analyze=%v) on server %d rejected the options: %v\nSQL: %s", analyze, ver, err, sql)
		}
		if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
			t.Fatalf("no plan returned for: %s", sql)
		}
	}
	// Вариант для неизвестной версии должен выполняться на любом сервере.
	if _, err := m.Exec(ctx, s, explainSQLFor(explainOpts{}, 0, "select 1"), true); err != nil {
		t.Fatalf("unknown-version EXPLAIN must still run: %v", err)
	}
}

// TestLiveDoctorChecksRunClean проверяет, что каждый диагностический запрос —
// валидный SQL на живом сервере; отсутствие расширения — это безобидный
// пропуск, а не ошибка.
func TestLiveDoctorChecksRunClean(t *testing.T) {
	auditLive(t)
	r := &REPL{mgr: db.NewManager(), cfg: &config.Config{}, out: &bytes.Buffer{}}
	defer r.mgr.Close()
	s := auditShard("shard_0", 0)
	r.targets = []cluster.Shard{s}
	fs, err := r.doctorChecks(s)
	if err != nil {
		t.Fatalf("doctorChecks errored: %v", err)
	}
	for _, f := range fs {
		if f.title == "Some checks could not be evaluated" {
			t.Errorf("a diagnostic query errored on the live server (not a missing extension): %s", f.detail)
		}
	}
}

// TestLiveDoctorAllNoRace гоняет \doctor --all по нескольким шардам параллельно;
// под -race ловит гонку на кэше версии сервера, если бы её не прогревали заранее.
func TestLiveDoctorAllNoRace(t *testing.T) {
	auditLive(t)
	r := &REPL{mgr: db.NewManager(), cfg: &config.Config{}, out: &bytes.Buffer{}}
	defer r.mgr.Close()
	r.targets = []cluster.Shard{auditShard("shard_0", 0), auditShard("shard_1", 1), auditShard("shard_2", 2)}
	if err := r.doctorAll(); err != nil {
		t.Fatalf("doctorAll: %v", err)
	}
}

// TestLiveDiffDriftAndIdentical проверяет, что многомерные запросы doDiff
// (резолвинг через to_regclass + колонки/индексы/ограничения/триггеры/таблица)
// выполняются на живом сервере и верно сообщают о расхождении, а затем об
// идентичности двух шардов.
func TestLiveDiffDriftAndIdentical(t *testing.T) {
	auditLive(t)
	buf := &bytes.Buffer{}
	m := db.NewManager()
	r := &REPL{mgr: m, cfg: &config.Config{}, out: buf}
	defer m.Close()
	s0, s1 := auditShard("shard_0", 0), auditShard("shard_1", 1)
	r.targets = []cluster.Shard{s0, s1}

	mustExec(t, m, s0, `drop table if exists public._terox_diff; create table public._terox_diff(a int)`)
	mustExec(t, m, s1, `drop table if exists public._terox_diff; create table public._terox_diff(a int, b text)`)
	defer cleanupExec(m, s0, `drop table if exists public._terox_diff`)
	defer cleanupExec(m, s1, `drop table if exists public._terox_diff`)

	buf.Reset()
	if err := r.doDiff([]string{"public._terox_diff"}); err != nil {
		t.Fatalf("doDiff (drift): %v", err)
	}
	if !strings.Contains(buf.String(), "DRIFT") {
		t.Errorf("expected DRIFT for differing columns, got:\n%s", buf.String())
	}

	// Выравниваем шарды — diff должен сообщить об идентичности.
	mustExec(t, m, s0, `alter table public._terox_diff add column b text`)
	buf.Reset()
	if err := r.doDiff([]string{"public._terox_diff"}); err != nil {
		t.Fatalf("doDiff (identical): %v", err)
	}
	if !strings.Contains(buf.String(), "identical") {
		t.Errorf("expected identical after aligning columns, got:\n%s", buf.String())
	}
}

// TestLiveCatalogUnionAcrossShards проверяет, что отношение, существующее
// только на не-первом шарде, попадает в каталог автодополнения, построенный
// по обоим шардам.
func TestLiveCatalogUnionAcrossShards(t *testing.T) {
	auditLive(t)
	m := db.NewManager()
	defer m.Close()
	s0, s1 := auditShard("shard_0", 0), auditShard("shard_1", 1)

	cleanupExec(m, s0, `drop table if exists public._terox_only1`) // гарантируем отсутствие на shard_0
	mustExec(t, m, s1, `drop table if exists public._terox_only1; create table public._terox_only1(x int)`)
	defer cleanupExec(m, s1, `drop table if exists public._terox_only1`)

	cat, err := buildCatalog(m, []cluster.Shard{s0, s1}, 4)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	if !cat.HasRelation("public", "_terox_only1") {
		t.Error("a relation present only on shard_1 must appear in the union catalog")
	}
}

// TestLiveWriteStopOnError проверяет реальный fan-out записи: миграция,
// упавшая на одном шарде, по умолчанию прерывает остальные (stop-on-error), а
// при write_error_mode=continue применяется и к ним.
func TestLiveWriteStopOnError(t *testing.T) {
	auditLive(t)
	m := db.NewManager()
	defer m.Close()
	s0, s1, s2 := auditShard("shard_0", 0), auditShard("shard_1", 1), auditShard("shard_2", 2)

	// У таблицы на shard_1 есть CHECK, который INSERT нарушает — запись падает.
	mustExec(t, m, s0, `drop table if exists public._terox_w; create table public._terox_w(n int)`)
	mustExec(t, m, s1, `drop table if exists public._terox_w; create table public._terox_w(n int check (n < 0))`)
	mustExec(t, m, s2, `drop table if exists public._terox_w; create table public._terox_w(n int)`)
	defer cleanupExec(m, s0, `drop table if exists public._terox_w`)
	defer cleanupExec(m, s1, `drop table if exists public._terox_w`)
	defer cleanupExec(m, s2, `drop table if exists public._terox_w`)

	count := func(s cluster.Shard) int64 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := m.Exec(ctx, s, `select count(*) from public._terox_w`, true)
		if err != nil {
			t.Fatalf("count on %s: %v", s.DB, err)
		}
		return asInt64(res.Rows[0][0])
	}

	// Последовательный режим: stop-on-error детерминированно не даёт обработать
	// шард после сбоя (shard_2).
	r := &REPL{mgr: m, cfg: &config.Config{FanoutMode: "sequential"}, out: &bytes.Buffer{}, writeMode: true}
	r.targets = []cluster.Shard{s0, s1, s2}

	// По умолчанию — stop-on-error.
	r.execWrite("insert into public._terox_w values (5)", true)
	if got := count(s2); got != 0 {
		t.Errorf("stop-on-error: shard_2 should NOT have been written (got %d rows)", got)
	}

	// Режим continue применяет ко всем шардам несмотря на сбой на shard_1.
	mustExec(t, m, s0, `truncate public._terox_w`)
	mustExec(t, m, s2, `truncate public._terox_w`)
	r.cfg.WriteErrorMode = "continue"
	r.execWrite("insert into public._terox_w values (5)", true)
	if got := count(s2); got != 1 {
		t.Errorf("continue mode: shard_2 should have been written (got %d rows)", got)
	}
}

// TestLiveStorageDrivenRole проверяет роль записи на уровне хранилища: при
// заданном migration_role запись идёт под ней (set local role), без неё роль
// не устанавливается и запись идёт под подключившимся пользователем.
func TestLiveStorageDrivenRole(t *testing.T) {
	auditLive(t)
	m := db.NewManager()
	r := &REPL{mgr: m, cfg: &config.Config{}, out: &bytes.Buffer{}, writeMode: true}
	defer m.Close()
	s := auditShard("shard_0", 0)
	r.targets = []cluster.Shard{s}
	mustExec(t, m, s, `drop table if exists public._terox_role;
		create table public._terox_role(who text);
		grant insert on public._terox_role to _fa`)
	defer cleanupExec(m, s, `drop table if exists public._terox_role`)

	whoRan := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := m.Exec(ctx, s, `select who from public._terox_role order by ctid desc limit 1`, true)
		if err != nil {
			t.Fatalf("read who: %v", err)
		}
		if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
			t.Fatal("no row written")
		}
		return fmt.Sprint(res.Rows[0][0])
	}

	// Хранилище с заданной ролью — запись повышается до неё.
	r.migrationRole = "_fa"
	r.execWrite(`insert into public._terox_role select current_user`, true)
	if got := whoRan(); got != "_fa" {
		t.Errorf("with migration_role=_fa, write should run as _fa, got %q", got)
	}

	// Хранилище без роли (и без общего дефолта) — без `set role`; запись идёт
	// под подключившимся пользователем.
	r.migrationRole = ""
	r.execWrite(`insert into public._terox_role select current_user`, true)
	if got := whoRan(); got != "postgres" {
		t.Errorf("with no role configured, write should run as the connecting user, got %q", got)
	}
}

// TestLiveCountRawWherePreservesSpacing проверяет, что двойной пробел внутри
// строкового литерала в WHERE сохраняется, поэтому count считает только строку
// с точным пробелом, а не строки с одинарным.
func TestLiveCountRawWherePreservesSpacing(t *testing.T) {
	auditLive(t)
	m := db.NewManager()
	r := &REPL{mgr: m, cfg: &config.Config{}, out: &bytes.Buffer{}}
	defer m.Close()
	ctx, cancel := auditCtx()
	defer cancel()
	s0 := auditShard("shard_0", 0)
	r.targets = []cluster.Shard{s0}

	mustExec(t, m, s0, `drop table if exists public._terox_cnt;
		create table public._terox_cnt(note text);
		insert into public._terox_cnt values ('a  b'), ('a b'), ('a b')`)
	defer cleanupExec(m, s0, `drop table if exists public._terox_cnt`)

	args, err := r.countArgs([]string{"public._terox_cnt", "where", "note", "=", "'a  b'"},
		`\count public._terox_cnt where note = 'a  b'`)
	if err != nil {
		t.Fatal(err)
	}
	q, err := r.buildCountQuery(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Exec(ctx, s0, q, true)
	if err != nil {
		t.Fatalf("count query failed: %v\nSQL: %s", err, q)
	}
	if got := asInt64(res.Rows[0][0]); got != 1 {
		t.Errorf("UX-01: count = %d, want 1 (collapsed spacing would also match the two 'a b' rows)", got)
	}
}
