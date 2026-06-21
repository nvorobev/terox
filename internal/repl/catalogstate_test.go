package repl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"terox/internal/complete"
)

func TestSegState(t *testing.T) {
	if st := segState(nil); st.Status != complete.StatusLoaded {
		t.Errorf("nil err -> %v, want loaded", st.Status)
	}
	if st := segState(&pgconn.PgError{Code: "42501", Message: "permission denied for table"}); st.Status != complete.StatusForbidden || st.Error != "permission denied" {
		t.Errorf("42501 -> %+v, want forbidden/permission denied", st)
	}
	if st := segState(&pgconn.PgError{Code: "57014", Message: "canceling statement"}); st.Status != complete.StatusTimeout {
		t.Errorf("57014 -> %v, want timeout", st.Status)
	}
	if st := segState(context.DeadlineExceeded); st.Status != complete.StatusTimeout {
		t.Errorf("deadline -> %v, want timeout", st.Status)
	}
	if st := segState(errors.New("connection refused")); st.Status != complete.StatusFailed {
		t.Errorf("plain -> %v, want failed", st.Status)
	}
}

func TestCoverageSegState(t *testing.T) {
	if st := coverageSegState(0, 3); st.Status != complete.StatusFailed {
		t.Errorf("0/3 -> %v, want failed", st.Status)
	}
	if st := coverageSegState(2, 3); st.Status != complete.StatusPartial || st.ShardsOK != 2 || st.ShardsN != 3 {
		t.Errorf("2/3 -> %+v, want partial 2/3", st)
	}
	if st := coverageSegState(3, 3); st.Status != complete.StatusLoaded {
		t.Errorf("3/3 -> %v, want loaded", st.Status)
	}
}

func TestSegmentIssues(t *testing.T) {
	segs := map[string]complete.LoadState{
		"relations":   {Status: complete.StatusLoaded},
		"functions":   {Status: complete.StatusForbidden, Error: "permission denied"},
		"coverage":    {Status: complete.StatusPartial, ShardsOK: 17, ShardsN: 32},
		"search_path": {Status: complete.StatusPending},
	}
	issues := segmentIssues(segs)
	// loaded и pending пропускаются; остаются functions и coverage, отсортированы.
	if len(issues) != 2 {
		t.Fatalf("expected 2 issues, got %v", issues)
	}
	joined := strings.Join(issues, "\n")
	if !strings.Contains(joined, "functions: forbidden (permission denied)") {
		t.Errorf("missing functions issue: %v", issues)
	}
	if !strings.Contains(joined, "coverage: partial [17/32]") {
		t.Errorf("missing coverage issue with shard count: %v", issues)
	}
	// Сортировка: coverage идёт раньше functions.
	if issues[0] != "coverage: partial [17/32]" {
		t.Errorf("issues not sorted: %v", issues)
	}
}

func TestDegradedNotice(t *testing.T) {
	// Все загружены -> заметки нет.
	if n := degradedNotice(map[string]complete.LoadState{
		"relations": {Status: complete.StatusLoaded},
		"functions": {Status: complete.StatusLoaded},
	}); n != "" {
		t.Errorf("all-loaded should give no notice, got %q", n)
	}
	// pending тоже не считается деградацией.
	if n := degradedNotice(map[string]complete.LoadState{
		"enums": {Status: complete.StatusPending},
	}); n != "" {
		t.Errorf("pending should give no notice, got %q", n)
	}
	// Деградировавшие сегменты перечислены, причина и подсказка присутствуют.
	n := degradedNotice(map[string]complete.LoadState{
		"relations":  {Status: complete.StatusLoaded},
		"functions":  {Status: complete.StatusForbidden, Error: "permission denied"},
		"extensions": {Status: complete.StatusTimeout},
	})
	for _, want := range []string{"degraded", "functions forbidden", "extensions timeout", "permission denied", "\\completion status"} {
		if !strings.Contains(n, want) {
			t.Errorf("degradedNotice missing %q in %q", want, n)
		}
	}
	// Сегменты отсортированы (extensions раньше functions).
	if strings.Index(n, "extensions") > strings.Index(n, "functions") {
		t.Errorf("segments not sorted: %q", n)
	}
}

func TestStatusString(t *testing.T) {
	cases := map[complete.Status]string{
		complete.StatusPending:   "pending",
		complete.StatusLoaded:    "loaded",
		complete.StatusPartial:   "partial",
		complete.StatusForbidden: "forbidden",
		complete.StatusTimeout:   "timeout",
		complete.StatusFailed:    "failed",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", st, got, want)
		}
	}
}
