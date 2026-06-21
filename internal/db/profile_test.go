package db

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"terox/internal/cluster"
)

func TestDSNConnectionProfile(t *testing.T) {
	s := cluster.Shard{
		Host: "h", Port: 5432, DB: "d", User: "u", Password: "p", SSLMode: "verify-full",
		SSLRootCert: "/ca.crt", SSLCert: "/c.crt", SSLKey: "/c.key",
		ConnectTimeout: 5 * time.Second,
	}
	u, err := url.Parse(dsn(s))
	if err != nil {
		t.Fatalf("dsn not a valid URL: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"sslmode":         "verify-full",
		"sslrootcert":     "/ca.crt",
		"sslcert":         "/c.crt",
		"sslkey":          "/c.key",
		"connect_timeout": "5",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("dsn %s = %q, want %q", k, got, v)
		}
	}
}

func TestDSNPassFile(t *testing.T) {
	s := cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u", PassFile: "/home/u/.pgpass"}
	u, err := url.Parse(dsn(s))
	if err != nil {
		t.Fatalf("dsn not a valid URL: %v", err)
	}
	if got := u.Query().Get("passfile"); got != "/home/u/.pgpass" {
		t.Errorf("dsn passfile = %q, want /home/u/.pgpass", got)
	}
	// passfile входит в ключ пула: его смена инвалидирует пул.
	a := cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u", PassFile: "/a"}
	b := cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u", PassFile: "/b"}
	if poolKey(a) == poolKey(b) {
		t.Error("poolKey must differ when passfile changes")
	}
}

func TestRedactDSN(t *testing.T) {
	s := cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u", Password: "s3cr3t"}
	red := RedactDSN(dsn(s))
	if strings.Contains(red, "s3cr3t") {
		t.Errorf("RedactDSN leaked the password: %s", red)
	}
	if !strings.Contains(red, "xxxxx") {
		t.Errorf("RedactDSN should mask the password: %s", red)
	}
	// Должен остаться валидным URL c хостом/базой.
	if u, err := url.Parse(red); err != nil || u.Host != "h:5432" {
		t.Errorf("redacted DSN malformed: %s (%v)", red, err)
	}
	// Строка без пароля возвращается без изменений по смыслу (тоже валидный URL).
	noPw := RedactDSN("postgresql://u@h:5432/d?sslmode=disable")
	if strings.Contains(noPw, "xxxxx") {
		t.Errorf("RedactDSN should not inject a mask when there is no password: %s", noPw)
	}
}

func TestDSNOmitsEmptyProfileParams(t *testing.T) {
	// Без профиля в DSN только sslmode — никаких пустых sslrootcert и т.п.
	u, _ := url.Parse(dsn(cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u"}))
	q := u.Query()
	for _, k := range []string{"sslrootcert", "sslcert", "connect_timeout", "passfile"} {
		if _, ok := q[k]; ok {
			t.Errorf("dsn should omit empty %q, got %q", k, q.Get(k))
		}
	}
}

func TestDSNConnectTimeoutSubSecond(t *testing.T) {
	// Субсекундный таймаут округляется вверх до 1 секунды (libpq — целые секунды).
	u, _ := url.Parse(dsn(cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u", ConnectTimeout: 500 * time.Millisecond}))
	if got := u.Query().Get("connect_timeout"); got != "1" {
		t.Errorf("sub-second connect_timeout = %q, want 1", got)
	}
}

func TestPoolKeyDistinctOnProfile(t *testing.T) {
	base := cluster.Shard{Host: "h", Port: 5432, DB: "d", User: "u", Password: "p", SSLMode: "verify-full"}
	withCA := base
	withCA.SSLRootCert = "/ca.crt"
	if poolKey(base) == poolKey(withCA) {
		t.Error("poolKey must differ when sslrootcert changes (pool must be invalidated)")
	}
	withCert := base
	withCert.SSLCert = "/c.crt"
	if poolKey(base) == poolKey(withCert) {
		t.Error("poolKey must differ when sslcert changes")
	}
}
