package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terox/internal/config"
)

// previewCfg — минимальный конфиг с одним сервисом/хранилищем на несколько шардов.
func previewCfg() *config.Config {
	return &config.Config{
		Services: map[string]*config.Service{
			"shop": {
				Storages: map[string]*config.Storage{
					"sharded": {
						HostTemplate: "rs{p1:3}",
						DBTemplate:   "shop",
						Port:         5432,
						User:         "app",
						Count:        3,
					},
				},
			},
		},
	}
}

func writeTempSQL(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.sql")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMigratePreviewPayloadAndPlan(t *testing.T) {
	path := writeTempSQL(t, "UPDATE items SET active = true WHERE id = 1;")
	var out bytes.Buffer
	err := MigratePreview(previewCfg(), "shop/sharded", path, MigrateOptions{Canary: true}, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "exact exec terox would send") {
		t.Errorf("preview should print the exact payload; got:\n%s", s)
	}
	if !strings.Contains(strings.ToLower(s), "begin;") || !strings.Contains(strings.ToLower(s), "commit;") {
		t.Errorf("transactional wrapper expected in payload; got:\n%s", s)
	}
	if !strings.Contains(s, "rollout plan:") {
		t.Errorf("preview should print the rollout plan; got:\n%s", s)
	}
	// canary → первый этап — один шард.
	if !strings.Contains(s, "stage 1 (canary): rs001") {
		t.Errorf("canary first stage should be a single shard; got:\n%s", s)
	}
}

func TestMigratePreviewRefusesTxControl(t *testing.T) {
	path := writeTempSQL(t, "BEGIN; UPDATE items SET active = true WHERE id = 1; COMMIT;")
	var out bytes.Buffer
	err := MigratePreview(previewCfg(), "shop/sharded", path, MigrateOptions{}, &out)
	if err == nil || !strings.Contains(err.Error(), "BEGIN/COMMIT") {
		t.Errorf("tx-control file must be refused; got err=%v", err)
	}
}

func TestMigratePreviewRefusesSessionState(t *testing.T) {
	path := writeTempSQL(t, "SET search_path = private; UPDATE items SET x = 1 WHERE id = 1;")
	// session-scoped SET отклоняется всегда: terox ходит через собственный pgxpool
	// (transaction pooling), поэтому session-state протекло бы внутри его пула.
	cfg := previewCfg()
	var out bytes.Buffer
	if err := MigratePreview(cfg, "shop/sharded", path, MigrateOptions{}, &out); err == nil {
		t.Error("session-scoped SET must be refused")
	}
}

func TestMigratePreviewUnknownTarget(t *testing.T) {
	path := writeTempSQL(t, "select 1;")
	var out bytes.Buffer
	if err := MigratePreview(previewCfg(), "shop/nope", path, MigrateOptions{}, &out); err == nil {
		t.Errorf("unknown storage must error")
	}
}
