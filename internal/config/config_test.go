package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWriteApproveEnabled(t *testing.T) {
	// По умолчанию (не задано) подтверждение записи включено.
	if !(&Config{}).WriteApproveEnabled() {
		t.Error("write approve must default to enabled")
	}
	off := false
	if (&Config{WriteApprove: &off}).WriteApproveEnabled() {
		t.Error("explicit write_approve: false must disable confirmation")
	}
	on := true
	if !(&Config{WriteApprove: &on}).WriteApproveEnabled() {
		t.Error("explicit write_approve: true must enable confirmation")
	}
}

func TestNoDefaultMigrationRole(t *testing.T) {
	// Роль для записи задаётся строго на уровне storage: без неё `set role` не
	// вызывается. Per-storage migration_role парсится и сохраняется.
	p := writeTemp(t, `
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 1
        migration_role: writer_fa
`)
	loaded, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Services["s"].Storages["st"].MigrationRole; got != "writer_fa" {
		t.Errorf("per-storage migration_role = %q, want writer_fa", got)
	}
}

func TestMaxRowsZeroMeansUnlimited(t *testing.T) {
	// Явный max_rows: 0 проходит через Defaults() как «без лимита» (0), а не 1000.
	p := writeTemp(t, `
max_rows: 0
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 2
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MaxRowsValue(); got != 0 {
		t.Errorf("max_rows: 0 should resolve to 0 (unlimited), got %d", got)
	}

	// Отсутствующий max_rows по умолчанию равен 1000.
	p2 := writeTemp(t, `
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 2
`)
	c2, err := Load(p2)
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.MaxRowsValue(); got != 1000 {
		t.Errorf("absent max_rows should default to 1000, got %d", got)
	}
}

func TestStrictYAMLRejectsUnknownKey(t *testing.T) {
	// Ключ с опечаткой (ssl_mode вместо sslmode) даёт жёсткую ошибку, а не
	// молча игнорируется.
	p := writeTemp(t, `
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        ssl_mode: require
        count: 2
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected a parse error for the unknown key ssl_mode")
	}
	if !strings.Contains(err.Error(), "ssl_mode") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestNilServiceDoesNotPanic(t *testing.T) {
	// `services:\n  foo:` даёт nil *Service. Validate/IsEmpty/StorageNames
	// не должны на нём паниковать.
	p := writeTemp(t, "services:\n  foo:\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.IsEmpty() {
		t.Error("a service with a nil/empty storages map is empty")
	}
	if names := c.StorageNames("foo"); len(names) != 0 {
		t.Errorf("nil service has no storages, got %v", names)
	}
	_, warns := c.Validate() // не должно паниковать
	if len(warns) == 0 {
		t.Error("expected a warning about the empty service")
	}
}

func TestValidateNumericRanges(t *testing.T) {
	neg := -5
	c := &Config{
		MaxRows:           &neg,
		FanoutConcurrency: -1,
		Services: map[string]*Service{
			"s": {Storages: map[string]*Storage{
				"st": {HostTemplate: "db{p1}.x", Port: 6432, User: "u", Password: "p", Count: -3},
			}},
		},
	}
	errs, _ := c.Validate()
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"max_rows", "fanout_concurrency", "count"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a range error mentioning %q; got:\n%s", want, joined)
		}
	}
}

func TestValidateTimeoutErrors(t *testing.T) {
	base := func(stmt, lock string) *Config {
		return &Config{
			StatementTimeout: stmt, LockTimeout: lock,
			Services: map[string]*Service{"s": {Storages: map[string]*Storage{
				"st": {HostTemplate: "db{p1}.x", Port: 6432, User: "u", Password: "p", Count: 2},
			}}},
		}
	}
	// Некорректная длительность — блокирующая ошибка (ломает SET LOCAL в каждом чтении).
	errs, _ := base("5 bananas", "").Validate()
	if !strings.Contains(strings.Join(errs, "\n"), "statement_timeout") {
		t.Errorf("invalid statement_timeout must be an error; got %v", errs)
	}
	errs2, _ := base("5s", "nope").Validate()
	if !strings.Contains(strings.Join(errs2, "\n"), "lock_timeout") {
		t.Errorf("invalid lock_timeout must be an error; got %v", errs2)
	}
	// Значение меньше миллисекунды — только предупреждение (таймаут отключён, запросы идут).
	errs3, warns3 := base("500us", "").Validate()
	if strings.Contains(strings.Join(errs3, "\n"), "statement_timeout") {
		t.Errorf("sub-ms statement_timeout should not be a hard error; got %v", errs3)
	}
	if !strings.Contains(strings.Join(warns3, "\n"), "statement_timeout") {
		t.Errorf("sub-ms statement_timeout should warn; got %v", warns3)
	}
	// Корректное значение проходит без ошибок и предупреждений.
	errs4, _ := base("500ms", "2s").Validate()
	if strings.Contains(strings.Join(errs4, "\n"), "timeout") {
		t.Errorf("valid timeouts should not error; got %v", errs4)
	}
}

func TestNestedUnknownFieldsRejected(t *testing.T) {
	// Проверяет, что KnownFields действует на вложенные структуры Storage.
	p := writeTemp(t, `
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 2
        ssl_mode: require
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error for unknown nested field ssl_mode")
	}
	if !strings.Contains(err.Error(), "ssl_mode") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestRootLevelUnknownFieldsRejected(t *testing.T) {
	// Проверяет, что неизвестные поля верхнего уровня отвергаются.
	p := writeTemp(t, `
unknown_root_field: value
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 2
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error for unknown root field")
	}
	if !strings.Contains(err.Error(), "unknown_root_field") {
		t.Errorf("error should name the offending key, got: %v", err)
	}
}

func TestYAMLFieldTag(t *testing.T) {
	// Проверяет, что поля с yaml:"-" игнорируются.
	p := writeTemp(t, `
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 2
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.path == "" {
		t.Fatalf("path field should be set by Load(), got empty")
	}
	// Поле path берётся не из YAML, а устанавливается в Load().
	if !strings.Contains(c.path, "config.yaml") {
		t.Fatalf("path field should be set to the file path, got: %q", c.path)
	}
}

func TestInteractiveModeDefaults(t *testing.T) {
	on, off := true, false
	// Дефолты при отсутствии ключей.
	c := &Config{}
	if !c.TimingEnabled() {
		t.Error("timing must default to enabled")
	}
	if c.ImpactEnabled() {
		t.Error("impact must default to disabled")
	}
	if !c.SuggestEnabled() {
		t.Error("suggest must default to enabled")
	}
	if c.ExpandedDefault() {
		t.Error("expanded must default to disabled")
	}
	// Явные значения уважаются в обе стороны.
	if (&Config{Timing: &off}).TimingEnabled() {
		t.Error("timing: false must disable")
	}
	if !(&Config{Impact: &on}).ImpactEnabled() {
		t.Error("impact: true must enable")
	}
	if (&Config{Suggest: &off}).SuggestEnabled() {
		t.Error("suggest: false must disable")
	}
	if !(&Config{Expanded: &on}).ExpandedDefault() {
		t.Error("expanded: true must enable")
	}
}

func TestDurationUnmarshal(t *testing.T) {
	p := writeTemp(t, `
query_timeout: 30s
migration_timeout: 60s
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 1
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Проверяет, что кастомный разбор Duration сработал.
	if c.QueryTimeout == 0 {
		t.Fatal("query_timeout should have been unmarshalled")
	}
	if c.MigrationTimeout == 0 {
		t.Fatal("migration_timeout should have been unmarshalled")
	}
}

func TestNoFalseRangeErrors(t *testing.T) {
	p := writeTemp(t, `
max_rows: 1000
fanout_concurrency: 16
query_timeout: 30s
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 1
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	errs, _ := c.Validate()
	if len(errs) > 0 {
		t.Fatalf("normal config should not have errors, got: %v", errs)
	}
}

func TestNegativeRanges(t *testing.T) {
	p := writeTemp(t, `
max_rows: -1
fanout_concurrency: -1
query_timeout: -1s
migration_timeout: -1s
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 1
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error during load: %v", err)
	}

	errs, _ := c.Validate()
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"max_rows", "fanout_concurrency", "query_timeout", "migration_timeout"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected a range error mentioning %q; got:\n%s", want, joined)
		}
	}
}

func TestMaxRowsPointerAllowsZero(t *testing.T) {
	// Проверяет, что явный max_rows: 0 отличается от отсутствующего.
	p := writeTemp(t, `
max_rows: 0
services:
  s:
    storages:
      st:
        host_template: db{p1}.example
        port: 6432
        user: u
        password: secret
        count: 1
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.MaxRowsValue() != 0 {
		t.Fatalf("explicit max_rows: 0 should mean unlimited (0), got %d", c.MaxRowsValue())
	}

	// Проверяет, что Validate() не отвергает 0.
	errs, _ := c.Validate()
	if len(errs) > 0 {
		t.Fatalf("max_rows: 0 should not trigger validation errors, got: %v", errs)
	}
}

func TestConcurrency(t *testing.T) {
	// parallel (по умолчанию): все n сразу, без потолка.
	c := &Config{FanoutMode: "parallel"}
	if got := c.Concurrency(32); got != 32 {
		t.Errorf("parallel/32 = %d, want 32 (no limit)", got)
	}
	if got := c.Concurrency(200); got != 200 {
		t.Errorf("parallel/200 = %d, want 200 (no limit)", got)
	}
	// parallel с необязательным ограничением.
	c = &Config{FanoutMode: "parallel", FanoutConcurrency: 8}
	if got := c.Concurrency(32); got != 8 {
		t.Errorf("cap 8 over 32 shards = %d, want 8", got)
	}
	if got := c.Concurrency(4); got != 4 {
		t.Errorf("cap 8 over 4 shards = %d, want 4", got)
	}
	// sequential: по одному за раз.
	c = &Config{FanoutMode: "sequential"}
	if got := c.Concurrency(32); got != 1 {
		t.Errorf("sequential = %d, want 1", got)
	}
	// пустой режим означает parallel (без потолка).
	c = &Config{}
	if got := c.Concurrency(10); got != 10 {
		t.Errorf("default = %d, want 10 (parallel)", got)
	}
}

func TestValidateFanoutMode(t *testing.T) {
	c := &Config{Services: map[string]*Service{"s": {Storages: map[string]*Storage{"t": {HostTemplate: "h", Port: 5432}}}}, FanoutMode: "bogus"}
	errs, _ := c.Validate()
	found := false
	for _, e := range errs {
		if strings.Contains(e, "fanout_mode") {
			found = true
		}
	}
	if !found {
		t.Errorf("bogus fanout_mode should be a validation error; got %v", errs)
	}
}

func TestSavePreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	original := `# top comment
write_mode_default: false   # stay read-only
editor: tea                 # input editor
services:
  item:                     # a service
    storages:
      main:
        host_template: h    # the host
        port: 5432
        user: app
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Editor = "readline" // имитация \editor readline → Save
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	s := string(out)
	for _, want := range []string{"# top comment", "# stay read-only", "# input editor", "# a service", "# the host"} {
		if !strings.Contains(s, want) {
			t.Errorf("Save dropped comment %q:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "editor: readline") {
		t.Errorf("Save did not apply the new value:\n%s", s)
	}
}

func TestNilStorageReportsErrorNotPanic(t *testing.T) {
	// `storage_name:` без тела декодируется в nil *Storage. Validate сообщает
	// об ошибке вместо разыменования.
	p := writeTemp(t, "services:\n  item:\n    storages:\n      main:\n")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	errs, _ := c.Validate() // не должно паниковать
	if joined := strings.Join(errs, "\n"); !strings.Contains(joined, "empty storage definition") {
		t.Errorf("expected an empty-storage error; got:\n%s", joined)
	}
}

func TestValidateProdInsecureSSL(t *testing.T) {
	mk := func(prod bool, sslmode, host string, allow bool) *Config {
		return &Config{AllowInsecureProd: allow, Services: map[string]*Service{
			"s": {Storages: map[string]*Storage{
				"st": {HostTemplate: host, Port: 6432, User: "u", Password: "p", Count: 1, Prod: prod, SSLMode: sslmode},
			}},
		}}
	}
	// Удалённый prod + незашифрованный sslmode → блокирующая ошибка.
	for _, m := range []string{"", "disable", "allow", "prefer"} {
		errs, _ := mk(true, m, "db{p1}.x", false).Validate()
		if !strings.Contains(strings.Join(errs, "\n"), "unencrypted") {
			t.Errorf("remote prod sslmode=%q should ERROR about unencrypted traffic; got %v", m, errs)
		}
	}
	// allow_insecure_prod: true понижает ошибку до предупреждения.
	errs, warns := mk(true, "disable", "db{p1}.x", true).Validate()
	if strings.Contains(strings.Join(errs, "\n"), "unencrypted") {
		t.Errorf("allow_insecure_prod should not error; got %v", errs)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "unencrypted") {
		t.Errorf("allow_insecure_prod should still warn; got %v", warns)
	}
	// Loopback prod + незашифрованный → допустимо (ни ошибки, ни предупреждения): MITM невозможен.
	errs, warns = mk(true, "disable", "127.0.0.1", false).Validate()
	if strings.Contains(strings.Join(errs, "\n")+strings.Join(warns, "\n"), "unencrypted") {
		t.Errorf("loopback prod sslmode=disable should be tolerated; errs=%v warns=%v", errs, warns)
	}
	// prod + require → предупреждение о MITM (шифрование есть, но сервер не проверяется).
	if _, warns := mk(true, "require", "db{p1}.x", false).Validate(); !strings.Contains(strings.Join(warns, "\n"), "MITM") {
		t.Errorf("prod sslmode=require should warn about MITM; got %v", warns)
	}
	// prod + verify-full → нет ошибок и предупреждений по TLS.
	errs, warns = mk(true, "verify-full", "db{p1}.x", false).Validate()
	if strings.Contains(strings.Join(errs, "\n")+strings.Join(warns, "\n"), "unencrypted") ||
		strings.Contains(strings.Join(warns, "\n"), "MITM") {
		t.Errorf("prod sslmode=verify-full must not warn; errs=%v warns=%v", errs, warns)
	}
	// не-prod без шифрования → нет ошибок и предупреждений по TLS (не докучаем dev-окружениям).
	errs, warns = mk(false, "disable", "db{p1}.x", false).Validate()
	if strings.Contains(strings.Join(errs, "\n")+strings.Join(warns, "\n"), "unencrypted") {
		t.Errorf("non-prod sslmode=disable should not warn or error; errs=%v warns=%v", errs, warns)
	}
}

func TestValidateWarnsOnWorldReadableConfig(t *testing.T) {
	p := writeTemp(t, "services:\n  s:\n    storages:\n      st:\n        host_template: db{p1}.x\n        port: 6432\n        user: u\n        password: secret\n        count: 1\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, warns := c.Validate()
	if !strings.Contains(strings.Join(warns, "\n"), "0600") {
		t.Errorf("expected a file-permission warning; got %v", warns)
	}
}

func TestSaveWritesMode0600(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	// Создаём заранее с широкими правами, чтобы убедиться, что Save сужает режим итогового файла.
	if err := os.WriteFile(path, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config saved with mode %#o, want 0600", perm)
	}
	// В каталоге не осталось временных файлов.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestProbeConcurrencyAlwaysParallel(t *testing.T) {
	// Внутренние пробы игнорируют режим sequential (иначе проверка здоровья по
	// множеству шардов застопорит UI).
	c := &Config{FanoutMode: "sequential"}
	if got := c.ProbeConcurrency(32); got != 32 {
		t.Errorf("ProbeConcurrency must stay parallel in sequential mode; got %d, want 32", got)
	}
	// Но явное ограничение всё равно учитывается.
	c = &Config{FanoutMode: "sequential", FanoutConcurrency: 8}
	if got := c.ProbeConcurrency(32); got != 8 {
		t.Errorf("ProbeConcurrency with cap 8 = %d, want 8", got)
	}
}
