package repl

import (
	"bytes"
	"strings"
	"testing"

	"terox/internal/migration"
	"terox/internal/store"
)

func TestWarnChecksumDrift(t *testing.T) {
	mk := func() (*REPL, *bytes.Buffer) {
		var buf bytes.Buffer
		r := &REPL{out: &buf, service: "svc", storage: "sto",
			applied: &store.Applied{C: map[string]map[string]string{
				"svc/sto": {"001.sql": migration.Checksum("CREATE TABLE t (id int);")},
			}}}
		return r, &buf
	}

	// Изменённое содержимое под тем же именем -> предупреждение.
	r, buf := mk()
	r.warnChecksumDrift("001.sql", "CREATE TABLE t (id bigint);")
	if !strings.Contains(buf.String(), "checksum mismatch") {
		t.Errorf("expected checksum-drift warning, got:\n%s", buf.String())
	}

	// То же содержимое -> без предупреждения.
	r, buf = mk()
	r.warnChecksumDrift("001.sql", "CREATE TABLE t (id int);")
	if strings.Contains(buf.String(), "checksum mismatch") {
		t.Errorf("identical content must not warn, got:\n%s", buf.String())
	}

	// Неизвестная миграция (нет записи) -> без предупреждения.
	r, buf = mk()
	r.warnChecksumDrift("002.sql", "anything")
	if strings.Contains(buf.String(), "checksum mismatch") {
		t.Errorf("unknown migration must not warn, got:\n%s", buf.String())
	}

	// Nil-applied -> без паники, без предупреждения.
	(&REPL{out: &bytes.Buffer{}}).warnChecksumDrift("001.sql", "x")
}
