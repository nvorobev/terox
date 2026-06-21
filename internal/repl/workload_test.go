package repl

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"terox/internal/config"
	"terox/internal/db"
)

func TestStatementsSQLVersionColumns(t *testing.T) {
	// PG13+ -> *_exec_time.
	modern := statementsSQL(150000, "total", 20)
	if !strings.Contains(modern, "total_exec_time") || !strings.Contains(modern, "mean_exec_time") {
		t.Errorf("13+ must use *_exec_time:\n%s", modern)
	}
	// < 13 -> total_time/mean_time.
	old := statementsSQL(120000, "total", 20)
	if !strings.Contains(old, "total_time") || strings.Contains(old, "total_exec_time") {
		t.Errorf("pre-13 must use total_time (not total_exec_time):\n%s", old)
	}
}

func TestStatementsSQLOrderAndLimit(t *testing.T) {
	if !strings.Contains(statementsSQL(150000, "mean", 5), "ORDER BY mean_exec_time DESC") {
		t.Error("--mean should order by mean_exec_time")
	}
	if !strings.Contains(statementsSQL(150000, "calls", 5), "ORDER BY s.calls DESC") {
		t.Error("--calls should order by calls")
	}
	if !strings.Contains(statementsSQL(150000, "rows", 5), "ORDER BY s.rows DESC") {
		t.Error("--rows should order by rows")
	}
	if !strings.Contains(statementsSQL(150000, "total", 7), "LIMIT 7") {
		t.Error("limit not applied")
	}
	// Нулевой/отрицательный лимит -> дефолт 20.
	if !strings.Contains(statementsSQL(150000, "total", 0), "LIMIT 20") {
		t.Error("zero limit should default to 20")
	}
}

func TestStatementsQueryVersionHeaders(t *testing.T) {
	_, h13 := statementsQuery(150000, statementsOpts{})
	hs := strings.Join(h13, ",")
	for _, c := range []string{"max_ms", "stddev_ms", "wal_bytes", "db", "role"} {
		if !strings.Contains(hs, c) {
			t.Errorf("13+ headers should include %q, got %v", c, h13)
		}
	}
	_, h12 := statementsQuery(120000, statementsOpts{})
	if strings.Contains(strings.Join(h12, ","), "wal_bytes") {
		t.Errorf("pre-13 headers must not include wal_bytes, got %v", h12)
	}
}

func TestStatementsQueryFilters(t *testing.T) {
	sql, _ := statementsQuery(150000, statementsOpts{user: "app", db: "shop", queryid: "123"})
	for _, frag := range []string{"r.rolname = 'app'", "d.datname = 'shop'", "s.queryid::text = '123'", "WHERE"} {
		if !strings.Contains(sql, frag) {
			t.Errorf("filter SQL missing %q:\n%s", frag, sql)
		}
	}
	// Идентичность через join всегда присутствует.
	if !strings.Contains(sql, "pg_database") || !strings.Contains(sql, "pg_roles") {
		t.Errorf("identity joins missing:\n%s", sql)
	}
}

func TestStatementsDegradation(t *testing.T) {
	missingErr := &pgconn.PgError{Code: "42P01", Message: `relation "pg_stat_statements" does not exist`}
	preloadErr := &pgconn.PgError{Code: "55000", Message: "must be loaded via shared_preload_libraries"}
	deniedErr := &pgconn.PgError{Code: "42501", Message: "permission denied"}

	var buf bytes.Buffer
	r := &REPL{out: &buf, cfg: &config.Config{}}

	if !r.statementsDegradationNotice([]db.ShardResult{{Err: missingErr}}) {
		t.Error("all-missing should return true (skip table)")
	}
	if !strings.Contains(buf.String(), "shared_preload_libraries") {
		t.Errorf("missing hint expected, got %q", buf.String())
	}

	buf.Reset()
	if !r.statementsDegradationNotice([]db.ShardResult{{Err: preloadErr}}) {
		t.Error("55000 (not preloaded) should be treated as missing")
	}

	buf.Reset()
	if !r.statementsDegradationNotice([]db.ShardResult{{Err: deniedErr}}) {
		t.Error("all-denied should return true")
	}
	if !strings.Contains(buf.String(), "pg_read_all_stats") {
		t.Errorf("denied hint expected, got %q", buf.String())
	}

	buf.Reset()
	mixed := []db.ShardResult{{Result: &db.Result{}}, {Err: deniedErr}}
	if r.statementsDegradationNotice(mixed) {
		t.Error("partial availability should return false (still render)")
	}
}

// TestStatementsDanglingFlag: флаг --user/--db/--queryid последним токеном (без
// значения) раньше молча игнорировался; теперь \statements печатает ошибку
// "flag --X needs a value" и не выполняет запрос с потерянным фильтром.
func TestStatementsDanglingFlag(t *testing.T) {
	for _, flag := range []string{"--user", "--db", "--queryid"} {
		var buf bytes.Buffer
		// targets пуст: если бы парсинг прошёл, дошли бы до "no shard selected".
		r := &REPL{out: &buf, cfg: &config.Config{}}
		r.doStatements([]string{flag})
		out := buf.String()
		if !strings.Contains(out, "flag "+flag+" needs a value") {
			t.Errorf("dangling %s should error, got %q", flag, out)
		}
		if strings.Contains(out, "no shard selected") {
			t.Errorf("dangling %s must not fall through to execution, got %q", flag, out)
		}
	}
}

func TestSkewRatio(t *testing.T) {
	if skewRatio(0, 100) != 1 {
		t.Error("min<=0 should yield ratio 1")
	}
	if skewRatio(10, 50) != 5 {
		t.Errorf("50/10 should be 5, got %v", skewRatio(10, 50))
	}
}
