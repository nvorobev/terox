package execution

import (
	"strings"
	"testing"
)

func TestPlanRead(t *testing.T) {
	p := (Planner{}).Plan(Request{SQL: "SELECT 1", WriteMode: false})
	if p.Refused() {
		t.Fatalf("read should not be refused: %+v", p.Refusal)
	}
	if p.IsWrite {
		t.Fatal("SELECT 1 must not be a write")
	}
}

func TestPlanWriteWithoutWriteMode(t *testing.T) {
	p := (Planner{}).Plan(Request{SQL: "UPDATE t SET x=1 WHERE id=1", WriteMode: false})
	if !p.IsWrite {
		t.Fatal("UPDATE must be a write")
	}
	if !p.Refused() {
		t.Fatal("write with write mode off must be refused")
	}
	if p.Refusal.Code != RefuseReadOnly {
		t.Fatalf("want %q, got %q", RefuseReadOnly, p.Refusal.Code)
	}
}

func TestPlanWriteQualifiedAllowed(t *testing.T) {
	p := (Planner{}).Plan(Request{SQL: "UPDATE t SET x=1 WHERE id=1", WriteMode: true})
	if p.Refused() {
		t.Fatalf("qualified write in write mode must not be refused: %+v", p.Refusal)
	}
	if !p.IsWrite {
		t.Fatal("qualified UPDATE must be classified as a write")
	}
}

func TestPlanUnqualifiedWriteAllowed(t *testing.T) {
	for _, sql := range []string{
		"UPDATE t SET x=1",
		"DELETE FROM t",
		"TRUNCATE t",
		"SELECT 1; DELETE FROM t",
	} {
		p := (Planner{}).Plan(Request{SQL: sql, WriteMode: true})
		if p.Refused() {
			t.Fatalf("%q must not be refused: %+v", sql, p.Refusal)
		}
		if !p.Decision.Unqualified {
			t.Fatalf("%q must retain the unqualified-write risk marker", sql)
		}
	}
}

func TestPlanRefusesTxControl(t *testing.T) {
	p := (Planner{}).Plan(Request{SQL: "BEGIN; UPDATE t SET x=1 WHERE id=1; COMMIT;", WriteMode: true})
	if !p.Refused() {
		t.Fatal("statement with its own tx control must be refused")
	}
	if p.Refusal.Code != RefuseTxControl {
		t.Fatalf("want %q, got %q", RefuseTxControl, p.Refusal.Code)
	}
}

func TestPlanRefusesSessionState(t *testing.T) {
	// terox ходит через собственный pgxpool (transaction pooling), поэтому
	// session-scoped SET отклоняется всегда.
	p := (Planner{}).Plan(Request{SQL: "SET search_path = private, public", WriteMode: true})
	if !p.Refused() {
		t.Fatal("session SET must be refused")
	}
	if p.Refusal.Code != RefuseSessionState {
		t.Fatalf("want %q, got %q", RefuseSessionState, p.Refusal.Code)
	}
	if strings.TrimSpace(p.Refusal.Message) == "" {
		t.Fatal("session-state refusal must carry an explanatory message")
	}
}

func TestPlanWarnsVolatileSideEffect(t *testing.T) {
	// SELECT pg_terminate_backend(...) — read-форма, но волатильный побочный эффект,
	// который read-only транзакция не блокирует: план должен пометить запись и
	// вынести предупреждение.
	p := (Planner{}).Plan(Request{SQL: "SELECT pg_terminate_backend(123)", WriteMode: true})
	if p.Refused() {
		t.Fatalf("volatile side-effect must not be refused outright: %+v", p.Refusal)
	}
	if !p.IsWrite {
		t.Fatal("volatile side-effecting SELECT must be treated as a write")
	}
	if len(p.Warnings) == 0 {
		t.Fatal("volatile side-effect must produce a warning")
	}
}
