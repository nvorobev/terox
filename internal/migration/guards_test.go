package migration

import (
	"strings"
	"testing"
)

func TestSessionGuards(t *testing.T) {
	// role + оба таймаута: порядок role → statement → lock, всё через SET LOCAL.
	g, err := SessionGuards("_fa", "5s", "2s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{
		"set local role _fa",
		"set local statement_timeout = '5s'",
		"set local lock_timeout = '2s'",
	}
	if len(g) != len(want) {
		t.Fatalf("got %d guards %v, want %d", len(g), g, len(want))
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("guard[%d] = %q, want %q", i, g[i], want[i])
		}
	}
}

func TestSessionGuardsEmptyValuesSkipped(t *testing.T) {
	// Пустые значения пропускаются: без роли и без lock_timeout — только statement.
	g, err := SessionGuards("", "300ms", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g) != 1 || g[0] != "set local statement_timeout = '300ms'" {
		t.Fatalf("got %v, want a single statement_timeout guard", g)
	}
	// Совсем пусто — нет guard-ов.
	g, err = SessionGuards("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g) != 0 {
		t.Fatalf("got %v, want no guards", g)
	}
}

func TestSessionGuardsQuotingAndValidation(t *testing.T) {
	// Роль со спецсимволами — в кавычках с удвоением, не «голым» идентификатором.
	g, err := SessionGuards(`we"ird`, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(g) != 1 || g[0] != `set local role "we""ird"` {
		t.Fatalf("got %v, want quoted role", g)
	}
	// Управляющий символ в роли — блокирующая ошибка (как в BuildTransactional).
	if _, err := SessionGuards("bad\x01role", "", ""); err == nil {
		t.Error("control char in role should be rejected")
	}
	// Некорректная длительность — блокирующая ошибка, чтобы опечатка не ушла в SQL.
	if _, err := SessionGuards("", "5 furlongs", ""); err == nil {
		t.Error("invalid statement_timeout should be rejected")
	}
	if _, err := SessionGuards("", "", "abc"); err == nil {
		t.Error("invalid lock_timeout should be rejected")
	}
}

func TestSessionGuardsMatchBuildTransactional(t *testing.T) {
	// Guard-операторы должны совпадать с тем, что BuildTransactional вставляет в
	// обёртку, чтобы COPY-путь и обычная запись имели одинаковую защиту.
	g, err := SessionGuards("_fa", "5s", "2s")
	if err != nil {
		t.Fatal(err)
	}
	built, err := BuildTransactional("update t set x=1;", "_fa", "5s", "2s")
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(built)
	for _, stmt := range g {
		if !strings.Contains(low, stmt) {
			t.Errorf("BuildTransactional output missing guard %q\n%s", stmt, built)
		}
	}
}
