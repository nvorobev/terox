package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/migration"
)

// Регрессия P1 (аудит 2026-06-24): отказ execWrite ДО выполнения (нетранзакционная
// миграция на prod без migration_timeout) раньше возвращал пустой список без сигнала,
// и staged rollout засчитывал «applied 0, failed 0, pending 0» как успех. Теперь
// отказ помечает шарды этапа pending, ставит раскатку на паузу и возвращает ошибку.

func refusingProdREPL(out *bytes.Buffer) *REPL {
	return &REPL{
		out:         out,
		cfg:         &config.Config{}, // MigrationTimeout = 0 → нет дедлайна на prod
		writeMode:   true,
		service:     "svc",
		storage:     "st",
		targetLabel: "all",
		prod:        true,
		targets:     []cluster.Shard{{Label: "s0", Position: 0}},
		now:         func() string { return "now" },
	}
}

func TestStagedRolloutTreatsExecWriteRefusalAsIncomplete(t *testing.T) {
	var out bytes.Buffer
	r := refusingProdREPL(&out)

	content := "CREATE INDEX CONCURRENTLY idx_t_a ON t(a);"
	plan := migration.Classify(content)
	err := r.runStagedRollout(true, content, "m.sql", plan, migrateOpts{canary: true})
	if err == nil {
		t.Fatalf("rollout refusal must be reported as incomplete (error), got nil; output:\n%s", out.String())
	}
	got := out.String()
	// Отчёт обязан показать pending-шард, а не ложный «pending 0».
	if !strings.Contains(got, "pending 1") {
		t.Errorf("rollout summary must show a pending shard, not a false success; output:\n%s", got)
	}
	if strings.Contains(got, "applied 0, failed 0, pending 0") {
		t.Errorf("refused rollout must NOT report applied 0/failed 0/pending 0; output:\n%s", got)
	}
	// Машиночитаемый отчёт обязан содержать pending-исход (для CI/headless).
	if !strings.Contains(got, `"status": "pending"`) {
		t.Errorf("machine-readable report must contain a pending shard outcome; output:\n%s", got)
	}
}

// Не-staged путь применения тоже не должен молча отдавать success: runMigrationFile
// обязан вернуть ошибку, когда execWrite отказал до выполнения.
func TestRunMigrationFileRejectsNonTransactionalProdWithoutMigrationTimeout(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.sql")
	if err := os.WriteFile(p, []byte("CREATE INDEX CONCURRENTLY idx_t_a ON t(a);"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := refusingProdREPL(&out)

	err := r.runMigrationFile(true, migrateOpts{path: p})
	if err == nil {
		t.Fatalf("non-transactional prod migration without migration_timeout must error, not silently succeed; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "refused: non-transactional migration on PROD has no time bound") {
		t.Errorf("expected the refusal explanation in output; got:\n%s", out.String())
	}
}
