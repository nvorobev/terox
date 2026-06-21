package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func storageWith(mut func(*Storage)) *Config {
	st := &Storage{HostTemplate: "h", Port: 5432, User: "u", Count: 1, Password: "x"}
	mut(st)
	return &Config{Services: map[string]*Service{"s": {Storages: map[string]*Storage{"t": st}}}}
}

func hasErr(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

func TestValidateConnectionProfile(t *testing.T) {
	// Отрицательный connect_timeout -> ошибка.
	errs, _ := storageWith(func(s *Storage) { s.ConnectTimeout = Duration(-time.Second) }).Validate()
	if !hasErr(errs, "connect_timeout") {
		t.Errorf("expected connect_timeout error, got %v", errs)
	}
}

func TestValidateVerifyFullWithoutRootCert(t *testing.T) {
	// verify-full без sslrootcert -> предупреждение (не ошибка).
	_, warns := storageWith(func(s *Storage) { s.SSLMode = "verify-full" }).Validate()
	if !hasErr(warns, "sslrootcert") {
		t.Errorf("expected verify-full-without-CA warning, got %v", warns)
	}
	// С sslrootcert, указывающим на СУЩЕСТВУЮЩИЙ файл -> предупреждения нет.
	ca := tempFile(t, "ca.crt")
	_, warns = storageWith(func(s *Storage) { s.SSLMode = "verify-full"; s.SSLRootCert = ca }).Validate()
	if hasErr(warns, "sslrootcert") {
		t.Errorf("readable sslrootcert still warned: %v", warns)
	}
}

// tempFile создаёт читаемый файл во временном каталоге теста и возвращает путь.
func tempFile(t *testing.T, name string) string {
	t.Helper()
	p := t.TempDir() + "/" + name
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidatePasswordPlaceholderWithPassfile(t *testing.T) {
	pgpass := tempFile(t, ".pgpass")
	// Пустой password без passfile -> предупреждение о пустом пароле.
	_, warns := storageWith(func(s *Storage) { s.Password = "" }).Validate()
	if !hasErr(warns, "placeholder/empty password") {
		t.Errorf("empty password without passfile should warn, got %v", warns)
	}
	// Пустой password + ЗАДАННЫЙ passfile -> о пароле НЕ предупреждаем (libpq берёт
	// секрет из .pgpass — штатный безопасный путь).
	_, warns = storageWith(func(s *Storage) { s.Password = ""; s.PassFile = pgpass }).Validate()
	if hasErr(warns, "placeholder/empty password") {
		t.Errorf("empty password with passfile should NOT warn about password, got %v", warns)
	}
	// 'changeme' предупреждаем даже с passfile: явный пароль перекрывает passfile.
	_, warns = storageWith(func(s *Storage) { s.Password = "changeme"; s.PassFile = pgpass }).Validate()
	if !hasErr(warns, "placeholder/empty password") {
		t.Errorf("'changeme' should warn even with passfile, got %v", warns)
	}
}

func TestValidateProfileFileExistence(t *testing.T) {
	// Несуществующий sslrootcert -> предупреждение «не читается».
	_, warns := storageWith(func(s *Storage) { s.SSLRootCert = "/nope/ca.crt" }).Validate()
	if !hasErr(warns, "is not readable") {
		t.Errorf("missing sslrootcert should warn, got %v", warns)
	}
	// passfile тоже проверяется.
	_, warns = storageWith(func(s *Storage) { s.PassFile = "/nope/.pgpass" }).Validate()
	if !hasErr(warns, "passfile") || !hasErr(warns, "is not readable") {
		t.Errorf("missing passfile should warn, got %v", warns)
	}
	// Существующие файлы -> без предупреждений о читаемости.
	cert, key := tempFile(t, "c.crt"), tempFile(t, "c.key")
	_, warns = storageWith(func(s *Storage) { s.SSLCert = cert; s.SSLKey = key }).Validate()
	if hasErr(warns, "is not readable") {
		t.Errorf("readable cert/key still warned: %v", warns)
	}
}

// TestValidateTildePathWithoutHome: если ~ нельзя развернуть (HOME не задан),
// проверка существования пропускается, а не даёт путающее «not readable».
func TestValidateTildePathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	// На некоторых платформах HOME="" не делает UserHomeDir ошибкой; тогда тест
	// просто проверяет, что путаного предупреждения нет в любом случае.
	_, warns := storageWith(func(s *Storage) { s.SSLRootCert = "~/.postgresql/root.crt" }).Validate()
	if hasErr(warns, "is not readable") {
		t.Errorf("unexpandable ~ path should not warn 'not readable': %v", warns)
	}
}

func TestValidatePasswordEnv(t *testing.T) {
	// password_env с заданной непустой переменной: открытый password не нужен,
	// предупреждения placeholder/empty нет.
	t.Setenv("TEROX_CFG_PW", "secret")
	_, warns := storageWith(func(s *Storage) { s.Password = ""; s.PasswordEnv = "TEROX_CFG_PW" }).Validate()
	if hasErr(warns, "placeholder/empty password") {
		t.Errorf("password_env set: should not warn placeholder/empty, got %v", warns)
	}
	if hasErr(warns, "empty/unset") {
		t.Errorf("password_env set+non-empty: should not warn empty/unset, got %v", warns)
	}
	// password_env, указывающий на пустую/незаданную переменную: предупреждение о
	// пустом пароле, даже если открытый password непуст.
	_, warns = storageWith(func(s *Storage) { s.Password = "secret"; s.PasswordEnv = "TEROX_CFG_PW_MISSING" }).Validate()
	if !hasErr(warns, "empty/unset") {
		t.Errorf("password_env unset: expected empty/unset warning, got %v", warns)
	}
}

func TestValidateClientCertPair(t *testing.T) {
	// Только sslcert без sslkey -> ОШИБКА о паре (mTLS требует оба).
	cert := tempFile(t, "c.crt")
	errs, _ := storageWith(func(s *Storage) { s.SSLCert = cert }).Validate()
	if !hasErr(errs, "sslcert and sslkey must be set together") {
		t.Errorf("expected cert/key pairing ERROR, got %v", errs)
	}
	// Только sslkey без sslcert -> тоже ошибка.
	key := tempFile(t, "c.key")
	errs, _ = storageWith(func(s *Storage) { s.SSLKey = key }).Validate()
	if !hasErr(errs, "sslcert and sslkey must be set together") {
		t.Errorf("expected cert/key pairing ERROR for lone sslkey, got %v", errs)
	}
	// Оба заданы (и существуют) -> ошибки пары нет.
	errs, _ = storageWith(func(s *Storage) { s.SSLCert = cert; s.SSLKey = key }).Validate()
	if hasErr(errs, "sslcert and sslkey must be set together") {
		t.Errorf("both cert+key set but still errored: %v", errs)
	}
}
