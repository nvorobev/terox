package repl

import (
	"strings"
	"testing"
)

func TestListSQLConsts(t *testing.T) {
	if !strings.Contains(listSchemasSQL, "pg_namespace") {
		t.Errorf("listSchemasSQL must query pg_namespace:\n%s", listSchemasSQL)
	}
	if !strings.Contains(listIndexesSQL, "pg_indexes") || !strings.Contains(listIndexesSQL, "pg_size_pretty") {
		t.Errorf("listIndexesSQL must list pg_indexes with size:\n%s", listIndexesSQL)
	}
}

// TestDtDnDiHaveHelpAndCompletion: новые листинги согласованы между справкой и
// автодополнением.
func TestDtDnDiHaveHelpAndCompletion(t *testing.T) {
	inMeta := map[string]bool{}
	for _, c := range metaCommands {
		inMeta[c] = true
	}
	for _, name := range []string{"\\dt", "\\dn", "\\di"} {
		if !inMeta[name] {
			t.Errorf("%s missing from metaCommands", name)
		}
		if lookupHelp(name) == nil {
			t.Errorf("%s missing from helpEntries", name)
		}
	}
}
