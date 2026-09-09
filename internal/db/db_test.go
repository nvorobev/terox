package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/puddle/v2"

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

func TestManagerCloseClosesPoolsAndPreventsRecreation(t *testing.T) {
	m := NewManager()
	shard := cluster.Shard{
		Host: "127.0.0.1", Port: 1, DB: "unused", User: "unused", SSLMode: "disable",
	}

	// MinConns=0, поэтому создание пула не требует живой PostgreSQL.
	p, err := m.pool(context.Background(), shard)
	if err != nil {
		t.Fatalf("create lazy pool: %v", err)
	}
	if len(m.pools) != 1 {
		t.Fatalf("expected one managed pool, got %d", len(m.pools))
	}

	m.Close()
	if !m.closed || len(m.pools) != 0 {
		t.Fatalf("Close must mark manager closed and detach every pool: closed=%v pools=%d", m.closed, len(m.pools))
	}
	if got := p.Stat().TotalConns(); got != 0 {
		t.Fatalf("closed pool still has %d connection(s)", got)
	}
	if _, err := p.Acquire(context.Background()); !errors.Is(err, puddle.ErrClosedPool) {
		t.Fatalf("detached pgx pool was not actually closed: %v", err)
	}
	if _, err := m.pool(context.Background(), shard); !errors.Is(err, errManagerClosed) {
		t.Fatalf("pool must not be recreated after Close; got %v", err)
	}

	// Повторный Close также должен быть безопасен.
	m.Close()
}
