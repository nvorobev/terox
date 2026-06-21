package safety

import (
	"strings"
	"testing"
)

func TestClassifyLevels(t *testing.T) {
	cases := []struct {
		sql   string
		level RiskLevel
		write bool
		unq   bool
	}{
		{"select 1", RiskReadOnly, false, false},
		{"select * from t where id = 1", RiskReadOnly, false, false},
		{"show all", RiskReadOnly, false, false},
		{"delete from t where id = 1", RiskWrite, true, false},
		{"update t set x = 1 where id = 2", RiskWrite, true, false},
		{"delete from t", RiskUnqualifiedWrite, true, true},
		{"truncate t", RiskUnqualifiedWrite, true, true},
		{"update t set x = 1", RiskUnqualifiedWrite, true, true},
		{"select pg_advisory_lock(1)", RiskVolatileSideEffect, true, false},
		{"select dblink_exec('...')", RiskVolatileSideEffect, true, false},
		{"listen ch", RiskUnknown, true, false},
		{"insert into t values (1)", RiskWrite, true, false},
		{"merge into t using s on t.id = s.id when matched then delete", RiskUnqualifiedWrite, true, true},
		{"merge into t using s on t.id = s.id when matched then update set x = 1", RiskUnqualifiedWrite, true, true},
		{"merge into t using s on t.id = s.id when not matched then insert (id) values (s.id)", RiskWrite, true, false},
	}
	for _, c := range cases {
		d := Classify(c.sql)
		if d.Level != c.level {
			t.Errorf("Classify(%q).Level = %v, want %v", c.sql, d.Level, c.level)
		}
		if d.Write != c.write {
			t.Errorf("Classify(%q).Write = %v, want %v", c.sql, d.Write, c.write)
		}
		if d.Unqualified != c.unq {
			t.Errorf("Classify(%q).Unqualified = %v, want %v", c.sql, d.Unqualified, c.unq)
		}
	}
}

// TestClassifyWriteMatchesIsWrite — Decision.Write обязан совпадать с IsWrite
// (единый источник истины, не расходящиеся реализации).
func TestClassifyWriteMatchesIsWrite(t *testing.T) {
	for _, sql := range []string{
		"select 1", "delete from t", "with c as (delete from t returning *) select * from c",
		"select pg_advisory_lock(1)", "explain analyze delete from t", "vacuum", "set x = 1",
		"select 1; delete from t",
	} {
		if Classify(sql).Write != IsWrite(sql) {
			t.Errorf("Classify(%q).Write (%v) != IsWrite (%v)", sql, Classify(sql).Write, IsWrite(sql))
		}
	}
}

func TestClassifyReasons(t *testing.T) {
	if rs := Classify("delete from t").Reasons; len(rs) == 0 || !strings.Contains(rs[0], "ALL rows") {
		t.Errorf("unqualified delete should explain ALL rows, got %v", rs)
	}
	if rs := Classify("select pg_advisory_lock(1)").Reasons; len(rs) == 0 || !strings.Contains(rs[0], "pg_advisory_lock") {
		t.Errorf("side-effect should name the function, got %v", rs)
	}
}

func TestRiskLevelString(t *testing.T) {
	want := map[RiskLevel]string{
		RiskReadOnly:           "read-only",
		RiskVolatileSideEffect: "volatile-side-effect",
		RiskWrite:              "write",
		RiskUnknown:            "unknown",
		RiskUnqualifiedWrite:   "unqualified-write",
		RiskLevel(99):          "unknown", // ветка default
	}
	for lvl, s := range want {
		if got := lvl.String(); got != s {
			t.Errorf("RiskLevel(%d).String() = %q, want %q", lvl, got, s)
		}
	}
}

func TestClassifyExplainAnalyzeWrite(t *testing.T) {
	// EXPLAIN ANALYZE DELETE действительно выполняет запись → ведущее слово explain,
	// уровень write (покрывает explain-ветку RiskWrite).
	d := Classify("explain analyze delete from t where id = 1")
	if !d.Write || d.Level != RiskWrite {
		t.Errorf("explain analyze delete should be RiskWrite, got %+v", d)
	}
}

func TestClassifyQuotedSideEffect(t *testing.T) {
	// Имя функции в кавычках обходит обычный Mask — покрывает MaskKeepQuoted-путь.
	d := Classify(`select "dblink_exec"('x')`)
	if d.Level != RiskVolatileSideEffect {
		t.Errorf("quoted side-effect function should be detected, got %+v", d)
	}
}

func TestClassifyMultiStatementMax(t *testing.T) {
	// Самый опасный из нескольких запросов определяет уровень.
	d := Classify("select 1; delete from t")
	if d.Level != RiskUnqualifiedWrite || !d.Write || !d.Unqualified {
		t.Errorf("multi-statement should take the max risk, got %+v", d)
	}
}
