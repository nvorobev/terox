package db

import (
	"strings"
	"testing"

	"terox/internal/cluster"
)

// TestPoolKeyDistinguishesCredentials проверяет, что пул не переиспользуется для
// целей с одинаковыми host/port/db/user, но разными TLS-режимом или паролем.
func TestPoolKeyDistinguishesCredentials(t *testing.T) {
	base := cluster.Shard{Host: "h", Port: 5432, DB: "master", User: "app", Password: "secret", SSLMode: "require"}

	diffSSL := base
	diffSSL.SSLMode = "disable"
	if poolKey(base) == poolKey(diffSSL) {
		t.Error("pool key must differ when sslmode differs")
	}

	diffPass := base
	diffPass.Password = "other"
	if poolKey(base) == poolKey(diffPass) {
		t.Error("pool key must differ when password differs")
	}

	// Пароль в открытом виде не должен попадать в ключ (он хешируется).
	if strings.Contains(poolKey(base), "secret") {
		t.Errorf("pool key leaks the password: %s", poolKey(base))
	}

	// Одинаковые шарды (отдельные значения с теми же полями) делят один ключ, чтобы
	// пул работал.
	same := base
	if poolKey(base) != poolKey(same) {
		t.Error("identical shards must share a pool key")
	}
}

// TestDSNDefaults проверяет sslmode по умолчанию в строке подключения.
func TestDSNDefaults(t *testing.T) {
	s := cluster.Shard{Host: "h", Port: 5432, DB: "master", User: "app"}
	got := dsn(s)
	if !strings.Contains(got, "sslmode=disable") {
		t.Errorf("expected default sslmode in dsn: %s", got)
	}
}
