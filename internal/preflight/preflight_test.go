package preflight

import (
	"strings"
	"testing"

	"terox/internal/config"
)

// baseCfg — минимальный валидный конфиг с одним хранилищем; mutate настраивает его.
func baseCfg(mutate func(*config.Storage)) *config.Config {
	st := &config.Storage{HostTemplate: "127.0.0.1", DBTemplate: "db", Port: 5432, User: "u", Password: "p", Count: 1}
	if mutate != nil {
		mutate(st)
	}
	return &config.Config{
		Services: map[string]*config.Service{
			"svc": {Storages: map[string]*config.Storage{"sto": st}},
		},
	}
}

// findCode сообщает, есть ли среди находок код code.
func findCode(fs []config.Finding, code string) *config.Finding {
	for i := range fs {
		if fs[i].Code == code {
			return &fs[i]
		}
	}
	return nil
}

func TestRunEmitsStableCodes(t *testing.T) {
	// verify-full без rootcert → предупреждение ssl-rootcert-missing с path svc/sto.
	cfg := baseCfg(func(s *config.Storage) { s.SSLMode = "verify-full" })
	fs := Run(cfg)
	f := findCode(fs, "ssl-rootcert-missing")
	if f == nil {
		t.Fatalf("expected ssl-rootcert-missing finding; got %+v", fs)
	}
	if f.Severity != config.SeverityWarning {
		t.Errorf("ssl-rootcert-missing should be a warning, got %v", f.Severity)
	}
	if f.Path != "svc/sto" {
		t.Errorf("expected path svc/sto, got %q", f.Path)
	}
}

func TestRunPortErrorIsBlocking(t *testing.T) {
	cfg := baseCfg(func(s *config.Storage) { s.Port = 0 })
	fs := Run(cfg)
	f := findCode(fs, "port-invalid")
	if f == nil || !f.IsError() {
		t.Fatalf("expected blocking port-invalid finding; got %+v", fs)
	}
}

func TestRunIncludesStorageExpandFinding(t *testing.T) {
	// count>1 без плейсхолдера → cluster.Expand даёт дублирующиеся шарды → ошибка
	// storage-expand (это и есть та проверка, ради которой preflight зовёт cluster).
	cfg := baseCfg(func(s *config.Storage) { s.Count = 3 })
	fs := Run(cfg)
	if findCode(fs, "storage-expand") == nil {
		t.Fatalf("expected storage-expand finding for count>1 without placeholder; got %+v", fs)
	}
	// И находка валидации count-no-placeholder тоже должна быть.
	if findCode(fs, "count-no-placeholder") == nil {
		t.Errorf("expected count-no-placeholder warning too; got %+v", fs)
	}
}

func TestPartitionMovesAllowedWarningToOverrides(t *testing.T) {
	cfg := baseCfg(func(s *config.Storage) { s.SSLMode = "verify-full" })
	fs := Run(cfg)
	errs, warns, overrides := Partition(fs, map[string]bool{"ssl-rootcert-missing": true})
	if len(errs) != 0 {
		t.Errorf("expected no errors, got %d", len(errs))
	}
	for _, w := range warns {
		if w.Code == "ssl-rootcert-missing" {
			t.Errorf("ssl-rootcert-missing should have been moved to overrides, still in warnings")
		}
	}
	if findCode(overrides, "ssl-rootcert-missing") == nil {
		t.Errorf("expected ssl-rootcert-missing in overrides, got %+v", overrides)
	}
}

func TestPartitionCannotSuppressErrors(t *testing.T) {
	cfg := baseCfg(func(s *config.Storage) { s.Port = -1 })
	fs := Run(cfg)
	// Попытка подавить код ошибки не должна её убрать.
	errs, _, _ := Partition(fs, map[string]bool{"port-invalid": true})
	if findCode(errs, "port-invalid") == nil {
		t.Errorf("errors must not be suppressible via allow-list; got %+v", errs)
	}
}

func TestGateBlocksOnError(t *testing.T) {
	cfg := baseCfg(func(s *config.Storage) { s.Port = 0 })
	if err := Gate(cfg, Run(cfg), Options{}); err == nil {
		t.Errorf("Gate should block on a config error")
	}
}

func TestGateStrictBlocksOnWarningUnlessAllowed(t *testing.T) {
	cfg := baseCfg(func(s *config.Storage) { s.SSLMode = "verify-full" })
	fs := Run(cfg)
	if err := Gate(cfg, fs, Options{Strict: true}); err == nil {
		t.Errorf("strict Gate should block on a warning")
	}
	if err := Gate(cfg, fs, Options{Strict: true, AllowWarning: map[string]bool{"ssl-rootcert-missing": true}}); err != nil {
		t.Errorf("allow-warning should let strict Gate pass, got %v", err)
	}
}

func TestGateAllowEmptyBypassesEmptyConfig(t *testing.T) {
	empty := &config.Config{Services: map[string]*config.Service{}}
	fs := Run(empty)
	if err := Gate(empty, fs, Options{AllowEmpty: true}); err != nil {
		t.Errorf("allowEmpty should let an empty config pass, got %v", err)
	}
	if err := Gate(empty, fs, Options{}); err == nil {
		t.Errorf("a non-allowEmpty empty config should be blocked (no-services error)")
	}
}

func TestUnknownAllowCodes(t *testing.T) {
	unknown := UnknownAllowCodes(map[string]bool{"ssl-rootcert-missing": true, "made-up": true})
	if len(unknown) != 1 || unknown[0] != "made-up" {
		t.Errorf("expected only made-up to be unknown, got %v", unknown)
	}
}

func TestParseAllowWarning(t *testing.T) {
	got := ParseAllowWarning([]string{"a", "b,c", " d "})
	for _, code := range []string{"a", "b", "c", "d"} {
		if !got[code] {
			t.Errorf("expected %q in parsed set %v", code, got)
		}
	}
}

func TestAllCodesCoverActualFindings(t *testing.T) {
	// Каждый код, который реально выдаёт Findings/Run на репрезентативных конфигах,
	// обязан присутствовать в AllCodes (иначе --allow-warning «не знает» его).
	known := AllCodes()
	cfgs := []*config.Config{
		baseCfg(func(s *config.Storage) { s.SSLMode = "verify-full" }),
		baseCfg(func(s *config.Storage) { s.Port = 0 }),
		baseCfg(func(s *config.Storage) { s.Count = 3 }),
		{Services: map[string]*config.Service{}},
	}
	for _, cfg := range cfgs {
		for _, f := range Run(cfg) {
			if !known[f.Code] {
				t.Errorf("finding code %q (%s) is missing from AllCodes()", f.Code, strings.TrimSpace(f.Message))
			}
		}
	}
}

func TestAllCodesNoOrphansForRemovedFields(t *testing.T) {
	// Поля channel_binding/target_session_attrs/pool_mode удалены из конфига,
	// поэтому их коды никогда не выдаются — они не должны числиться в реестре,
	// иначе --allow-warning молча примет устаревший код вместо ошибки.
	known := AllCodes()
	for _, code := range []string{"channel-binding-invalid", "target-session-attrs-invalid", "pool-mode-invalid"} {
		if known[code] {
			t.Errorf("устаревший код %q не должен быть в AllCodes()", code)
		}
	}
}
