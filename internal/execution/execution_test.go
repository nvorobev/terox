package execution

import (
	"testing"

	"terox/internal/safety"
)

// TestClassifyDelegates проверяет, что Classify — чистая делегация к safety.
func TestClassifyDelegates(t *testing.T) {
	cases := []string{
		"select 1",
		"select * from t where id = 1",
		"delete from t where id = 1",
		"delete from t",
		"truncate t",
		"select pg_advisory_lock(1)",
		"listen ch",
	}
	for _, sql := range cases {
		got := Classify(sql)
		want := safety.Classify(sql)
		if got.Level != want.Level || got.Write != want.Write || got.Unqualified != want.Unqualified {
			t.Errorf("Classify(%q) = %+v, want %+v", sql, got, want)
		}
	}
}

// TestPredicatesDelegate проверяет совпадение предикатов с safety.
func TestPredicatesDelegate(t *testing.T) {
	cases := []string{
		"select 1",
		"insert into t values (1)",
		"update t set x = 1",
		"delete from t",
		"delete from t where id = 1; truncate u",
	}
	for _, sql := range cases {
		if got, want := IsWrite(sql), safety.IsWrite(sql); got != want {
			t.Errorf("IsWrite(%q) = %v, want %v", sql, got, want)
		}
		if got, want := AnyUnqualifiedWrite(sql), safety.AnyUnqualifiedWrite(sql); got != want {
			t.Errorf("AnyUnqualifiedWrite(%q) = %v, want %v", sql, got, want)
		}
	}
}

// TestRiskLevelsWired проверяет, что реэкспортированные константы — те же
// значения уровня риска, что и в safety (через поведение Classify).
func TestRiskLevelsWired(t *testing.T) {
	if RiskReadOnly != safety.RiskReadOnly ||
		RiskVolatileSideEffect != safety.RiskVolatileSideEffect ||
		RiskWrite != safety.RiskWrite ||
		RiskUnknown != safety.RiskUnknown ||
		RiskUnqualifiedWrite != safety.RiskUnqualifiedWrite {
		t.Fatal("re-exported risk constants do not match safety values")
	}

	if d := Classify("select pg_advisory_lock(1)"); d.Level != RiskVolatileSideEffect {
		t.Errorf("expected RiskVolatileSideEffect, got %v", d.Level)
	}
	if d := Classify("select 1"); d.Level != RiskReadOnly {
		t.Errorf("expected RiskReadOnly, got %v", d.Level)
	}
	if d := Classify("truncate t"); d.Level != RiskUnqualifiedWrite {
		t.Errorf("expected RiskUnqualifiedWrite, got %v", d.Level)
	}
}

// TestLacksTopLevelOrderBy проверяет эвристику ORDER BY для предупреждения quorum.
func TestLacksTopLevelOrderBy(t *testing.T) {
	lacks := []string{
		"select id, name from users",
		"select id from users where id > 10 limit 5",
		"with a as (select x from t order by x) select * from a", // ORDER BY только в CTE
		"select rank() over (order by created_at) from t",        // ORDER BY только в окне
		"select 'order by' from t",                               // ORDER BY внутри литерала
	}
	for _, sql := range lacks {
		if !LacksTopLevelOrderBy(sql) {
			t.Errorf("LacksTopLevelOrderBy(%q) = false, want true", sql)
		}
	}
	has := []string{
		"select id from users order by id",
		"select id from users ORDER  BY id desc",
		"select id from t order by (a, b)",
		"update t set x = 1",      // не SELECT-форма → не предупреждаем
		"insert into t values(1)", // не SELECT-форма
	}
	for _, sql := range has {
		if LacksTopLevelOrderBy(sql) {
			t.Errorf("LacksTopLevelOrderBy(%q) = true, want false", sql)
		}
	}
}
