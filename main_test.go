package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terox/internal/config"
	"terox/internal/preflight"
	"terox/internal/repl"
)

// writeTempConfig пишет body в файл 0600 во временном каталоге, возвращает путь.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// errorConfig: блокирующая ошибка (отрицательный max_rows), но в остальном непустой
// разбираемый конфиг — отклоняет его preflight, а не YAML-декодер.
const errorConfig = `services:
  svc:
    storages:
      sto:
        host_template: 10.0.0.1
        port: 5432
        user: u
        count: 1
max_rows: -5
`

// warningConfig: только предупреждения (заглушка-пароль), без ошибок.
const warningConfig = `services:
  svc:
    storages:
      sto:
        host_template: 10.0.0.1
        port: 5432
        user: u
        password: changeme
        count: 1
`

func loadCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	cfg, err := config.Load(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// TestGateBlocksAllEntrypointsEqually: один и тот же плохой конфиг одинаково
// блокирует интерактивный REPL (allowEmpty=true) и query/plan (allowEmpty=false),
// а пустой конфиг в интерактиве по-прежнему открывает мастер.
func TestGateBlocksAllEntrypointsEqually(t *testing.T) {
	bad := loadCfg(t, errorConfig)
	if err := gateConfig(bad, false, false); err == nil {
		t.Error("query/plan path: bad config must be rejected")
	}
	if err := gateConfig(bad, true, false); err == nil {
		t.Error("interactive path: a non-empty bad config must be rejected too")
	}

	empty := loadCfg(t, "services: {}\n")
	if err := gateConfig(empty, true, false); err != nil {
		t.Errorf("interactive path: empty config should open the wizard, got %v", err)
	}
	if err := gateConfig(empty, false, false); err == nil {
		t.Error("query/plan path: empty config (no services) must be rejected")
	}
}

// TestGateStrictTurnsWarningsIntoErrors проверяет --strict.
func TestGateStrictTurnsWarningsIntoErrors(t *testing.T) {
	cfg := loadCfg(t, warningConfig)
	if err := gateConfig(cfg, false, false); err != nil {
		t.Errorf("warnings alone should not block without --strict, got %v", err)
	}
	if err := gateConfig(cfg, false, true); err == nil {
		t.Error("--strict must turn warnings into a blocking error")
	}
}

// TestMisplacedFlag: известный флаг, поставленный после SQL, должен ловиться, а
// SQL-токены с ведущим дефисом (-1, оператор -, комментарий --text) — нет.
func TestMisplacedFlag(t *testing.T) {
	names := []string{"t", "format", "order-by", "mode", "strict", "allow-warning"}
	hits := []struct {
		args []string
		want string
	}{
		{[]string{"select", "1", "--format", "json"}, "--format"},
		{[]string{"select", "1", "--mode=quorum"}, "--mode=quorum"},
		{[]string{"select", "1", "-t", "x"}, "-t"},
		{[]string{"select", "1", "--allow-warning", "C"}, "--allow-warning"},
	}
	for _, c := range hits {
		if got := misplacedFlag(c.args, names...); got != c.want {
			t.Errorf("misplacedFlag(%v) = %q, want %q", c.args, got, c.want)
		}
	}
	noHits := [][]string{
		{"select", "-1"},
		{"select", "a", "-", "b"},
		{"select", "1", "--", "comment"},
		{"select", "1", "--comment"},
		{"where", "x", "<", "-5"},
		{"select", "*", "from", "t"},
	}
	for _, args := range noHits {
		if got := misplacedFlag(args, names...); got != "" {
			t.Errorf("misplacedFlag(%v) false positive: %q", args, got)
		}
	}
}

// TestQueryRejectsFlagAfterSQL: `terox query -t T <sql> --format json` должен дать
// понятную ошибку про порядок флагов, а не молча свернуть флаг в текст запроса.
func TestQueryRejectsFlagAfterSQL(t *testing.T) {
	path := writeTempConfig(t, warningConfig)
	err := run([]string{"-c", path, "query", "-t", "svc/sto", "select 1", "--format", "json"})
	if err == nil || !strings.Contains(err.Error(), "must come before the SQL") {
		t.Errorf("expected misplaced-flag error, got %v", err)
	}
}

// `terox query` с плохим конфигом обязан упасть на preflight и потому не дойти
// до repl.Query / подключения к БД. Возвращается ошибка проверки.
func TestRunQueryBlocksOnBadConfigBeforeConnecting(t *testing.T) {
	path := writeTempConfig(t, errorConfig)
	for _, sub := range []string{"query", "plan"} {
		args := []string{"-c", path, sub, "-t", "svc/sto", "select 1"}
		err := run(args)
		if err == nil {
			t.Fatalf("%s: expected preflight rejection, got nil", sub)
		}
		if !strings.Contains(err.Error(), "config error") && !strings.Contains(err.Error(), "refusing to start") {
			t.Errorf("%s: expected a preflight error, got %v", sub, err)
		}
	}
}

// TestStrictAfterSubcommand: --strict принимается ПОСЛЕ подкоманды query/plan
// (не только перед ней) — должен дойти до preflight и блокировать по
// предупреждениям, а не падать с "flag provided but not defined".
func TestStrictAfterSubcommand(t *testing.T) {
	path := writeTempConfig(t, warningConfig)
	for _, sub := range []string{"query", "plan"} {
		args := []string{"-c", path, sub, "--strict", "-t", "svc/sto", "select 1"}
		err := run(args)
		if err == nil {
			t.Fatalf("%s --strict: expected a strict preflight rejection, got nil", sub)
		}
		if strings.Contains(err.Error(), "not defined") {
			t.Errorf("%s --strict: flag was not accepted after the subcommand: %v", sub, err)
		}
		if !strings.Contains(err.Error(), "warning") && !strings.Contains(err.Error(), "config") {
			t.Errorf("%s --strict: expected a config/warning gate error, got %v", sub, err)
		}
	}
}

// TestAllowWarningAcknowledgesStrictWarning: --allow-warning CODE снимает с
// проверки --strict именно это предупреждение (и только его).
func TestAllowWarningAcknowledgesStrictWarning(t *testing.T) {
	cfg := loadCfg(t, warningConfig) // единственное предупреждение: password-placeholder
	if err := gateConfigOpts(cfg, preflight.Options{Strict: true}); err == nil {
		t.Error("strict should block on the placeholder-password warning")
	}
	if err := gateConfigOpts(cfg, preflight.Options{Strict: true, AllowWarning: map[string]bool{"password-placeholder": true}}); err != nil {
		t.Errorf("acknowledging the warning code should let strict pass, got %v", err)
	}
	// Другой код предупреждение НЕ снимает.
	if err := gateConfigOpts(cfg, preflight.Options{Strict: true, AllowWarning: map[string]bool{"config-perms": true}}); err == nil {
		t.Error("an unrelated allow-warning code must not suppress the real warning")
	}
}

// TestPartialErrorMapsToExitCode: repl.PartialError ловится через errors.As
// (чтобы main() мог отдать частичный успех по шардам кодом выхода 2).
func TestPartialErrorMapsToExitCode(t *testing.T) {
	err := error(&repl.PartialError{Failed: 1, Total: 2})
	var pe *repl.PartialError
	if !errors.As(err, &pe) {
		t.Error("PartialError must be detectable via errors.As for exit code 2")
	}
	// Обычная ошибка совпадать НЕ должна.
	var pe2 *repl.PartialError
	if errors.As(errors.New("boom"), &pe2) {
		t.Error("a plain error must not be a PartialError")
	}
}

// TestVersionAndHelpNeedNoConfig: version и help проходят даже когда файл
// конфига отсутствует.
func TestVersionAndHelpNeedNoConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	cases := [][]string{
		{"version"},
		{"--version"},
		{"-c", missing, "help"},
		// version после глобальных флагов (раньше падало "unknown command version").
		{"-c", missing, "version"},
	}
	for _, args := range cases {
		if err := run(args); err != nil {
			t.Errorf("run(%v) should succeed without a config, got %v", args, err)
		}
	}
}
