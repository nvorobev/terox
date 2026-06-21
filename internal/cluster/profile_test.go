package cluster

import (
	"testing"
	"time"

	"terox/internal/config"
)

func TestExpandPasswordEnv(t *testing.T) {
	t.Setenv("TEROX_TEST_PW", "s3cret")
	// password_env имеет приоритет над открытым password.
	st := &config.Storage{HostTemplate: "h", Port: 5432, User: "u", Count: 1,
		Password: "plain", PasswordEnv: "TEROX_TEST_PW"}
	sh, err := Expand(st)
	if err != nil {
		t.Fatal(err)
	}
	if sh[0].Password != "s3cret" {
		t.Errorf("password from env = %q, want s3cret", sh[0].Password)
	}

	// Незаданная переменная окружения -> пустой пароль (не падаем, не берём plain).
	st2 := &config.Storage{HostTemplate: "h", Port: 5432, User: "u", Count: 1,
		Password: "plain", PasswordEnv: "TEROX_TEST_PW_MISSING"}
	sh2, _ := Expand(st2)
	if sh2[0].Password != "" {
		t.Errorf("missing env -> %q, want empty", sh2[0].Password)
	}

	// Без password_env берётся открытый password.
	st3 := &config.Storage{HostTemplate: "h", Port: 5432, User: "u", Count: 1, Password: "plain"}
	sh3, _ := Expand(st3)
	if sh3[0].Password != "plain" {
		t.Errorf("no env -> %q, want plain", sh3[0].Password)
	}
}

func TestExpandRoundsConnectTimeout(t *testing.T) {
	// Субсекундное -> 1с; дробное -> усечение до целых секунд (libpq — секунды).
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{500 * time.Millisecond, time.Second},
		{5500 * time.Millisecond, 5 * time.Second},
		{0, 0},
	}
	for _, c := range cases {
		st := &config.Storage{HostTemplate: "h", Port: 5432, User: "u", Count: 1, ConnectTimeout: config.Duration(c.in)}
		sh, _ := Expand(st)
		if sh[0].ConnectTimeout != c.want {
			t.Errorf("connect_timeout %v -> %v, want %v", c.in, sh[0].ConnectTimeout, c.want)
		}
	}
}

func TestExpandPropagatesProfile(t *testing.T) {
	st := &config.Storage{HostTemplate: "h", Port: 5432, User: "u", Count: 1,
		SSLMode: "verify-full", SSLRootCert: "/ca.crt", SSLCert: "/c.crt", SSLKey: "/c.key",
		ConnectTimeout: config.Duration(5 * time.Second), PassFile: "/home/u/.pgpass"}
	sh, err := Expand(st)
	if err != nil {
		t.Fatal(err)
	}
	s := sh[0]
	if s.SSLRootCert != "/ca.crt" || s.SSLCert != "/c.crt" || s.SSLKey != "/c.key" ||
		s.ConnectTimeout != 5*time.Second || s.PassFile != "/home/u/.pgpass" {
		t.Errorf("profile not propagated: %+v", s)
	}
}
