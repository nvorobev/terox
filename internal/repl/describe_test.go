package repl

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestDescribeSQLFormatting(t *testing.T) {
	lit := sqlLiteral("public.users")
	for name, sql := range map[string]string{
		"columns":      describeTableSQL,
		"indexes":      describeIndexesSQL,
		"foreign_keys": describeForeignKeysSQL,
		"referenced":   describeReferencedBySQL,
		"checks":       describeChecksSQL,
		"size":         describeSizeSQL,
	} {
		got := fmt.Sprintf(sql, lit)
		if strings.Contains(got, "%!") {
			t.Errorf("%s: format verb mismatch:\n%s", name, got)
		}
		if !strings.Contains(got, "to_regclass('public.users')") {
			t.Errorf("%s: target not substituted:\n%s", name, got)
		}
	}
}

func TestDoDescribeNoTargets(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf}
	if err := r.doDescribe("users"); err == nil || !strings.Contains(err.Error(), "no shard selected") {
		t.Errorf("doDescribe with no targets must error, got %v", err)
	}
}
