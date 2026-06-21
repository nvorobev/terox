package config

import "testing"

func cfgWith(mutate func(*Storage)) *Config {
	st := &Storage{HostTemplate: "127.0.0.1", DBTemplate: "db", Port: 5432, User: "u", Password: "p", Count: 1}
	if mutate != nil {
		mutate(st)
	}
	return &Config{Services: map[string]*Service{"svc": {Storages: map[string]*Storage{"sto": st}}}}
}

// TestFindingsMatchValidate гарантирует, что строковый Validate() остаётся точной
// проекцией структурированного Findings() (тот же набор и порядок сообщений) — это
// контракт обратной совместимости.
func TestFindingsMatchValidate(t *testing.T) {
	c := cfgWith(func(s *Storage) { s.SSLMode = "verify-full"; s.Port = 0 })
	fs := c.Findings()
	errs, warns := c.Validate()
	wantErrs, wantWarns := splitFindings(fs)
	if len(errs) != len(wantErrs) || len(warns) != len(wantWarns) {
		t.Fatalf("Validate (%d errs, %d warns) != splitFindings (%d errs, %d warns)", len(errs), len(warns), len(wantErrs), len(wantWarns))
	}
	for i := range errs {
		if errs[i] != wantErrs[i] {
			t.Errorf("err[%d]: %q != %q", i, errs[i], wantErrs[i])
		}
	}
}

// TestEveryFindingHasCode — ни одна находка не должна остаться без стабильного кода
// (код — контракт для --allow-warning и CI).
func TestEveryFindingHasCode(t *testing.T) {
	for _, c := range []*Config{
		cfgWith(func(s *Storage) { s.SSLMode = "verify-full" }),
		cfgWith(func(s *Storage) { s.Port = 99999 }),
		{Services: map[string]*Service{}},
	} {
		for _, f := range c.Findings() {
			if f.Code == "" {
				t.Errorf("finding without code: %q", f.Message)
			}
		}
	}
}

func TestSeverityString(t *testing.T) {
	if SeverityError.String() != "error" || SeverityWarning.String() != "warning" {
		t.Errorf("severity strings wrong: %q %q", SeverityError, SeverityWarning)
	}
}
