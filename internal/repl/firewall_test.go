package repl

import (
	"bytes"
	"strings"
	"testing"

	"terox/internal/config"
)

// TestExecWriteRefusesSessionState: firewall session-state встроен в обёрнутый
// write-путь — SET, переживающий COMMIT, отклоняется до обращения к БД (mgr тут nil:
// доступ к нему вызвал бы panic), а сообщение называет конструкцию и безопасную замену.
func TestExecWriteRefusesSessionState(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{"session set", "SET search_path = evil", "search_path"},
		{"temp table", "CREATE TEMP TABLE t (id int)", "TEMP"},
		{"listen", "LISTEN chan", "LISTEN"},
		{"advisory lock", "SELECT pg_advisory_lock(1)", "advisory"},
		// DISCARD ALL — И session-state, И IsNonTransactional; backstop обязан отклонить
		// его до того, как non-transactional ветка выполнит его без защиты.
		{"discard all (non-transactional)", "DISCARD ALL", "DISCARD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := &REPL{out: &buf, cfg: &config.Config{}, writeMode: true}
			res, err := r.execWrite(tc.sql, true) // wrap=true → профиль transaction-pooling
			if res != nil {
				t.Errorf("expected no shard results (refused before exec), got %v", res)
			}
			if err == nil {
				t.Errorf("expected a refusal error (not silent nil), got nil")
			}
			out := buf.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("refusal %q does not mention %q", out, tc.want)
			}
			if !strings.Contains(out, "\\i") {
				t.Errorf("refusal %q must name the verbatim \\i alternative", out)
			}
		})
	}
}
