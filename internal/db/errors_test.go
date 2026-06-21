package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestSQLState(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", Message: "duplicate key", Severity: "ERROR"}
	if got := SQLState(pgErr); got != "23505" {
		t.Errorf("SQLState(pgErr) = %q, want 23505", got)
	}
	// Обёрнутая ошибка тоже распознаётся.
	if got := SQLState(fmt.Errorf("exec: %w", pgErr)); got != "23505" {
		t.Errorf("SQLState(wrapped) = %q, want 23505", got)
	}
	// Не-pg ошибка -> пусто.
	if got := SQLState(errors.New("connection refused")); got != "" {
		t.Errorf("SQLState(plain) = %q, want empty", got)
	}
	if got := SQLState(nil); got != "" {
		t.Errorf("SQLState(nil) = %q, want empty", got)
	}
}

func TestClassifyError(t *testing.T) {
	if ClassifyError(nil) != nil {
		t.Error("ClassifyError(nil) must be nil")
	}
	server := ClassifyError(&pgconn.PgError{Code: "42501", Message: "permission denied", Severity: "ERROR"})
	if server.Kind != "server" || server.SQLState != "42501" || server.Message != "permission denied" {
		t.Errorf("server error misclassified: %+v", server)
	}
	canceled := ClassifyError(&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"})
	if canceled.Kind != "canceled" {
		t.Errorf("57014 should map to canceled, got %q", canceled.Kind)
	}
	tmo := ClassifyError(fmt.Errorf("ctx: %w", context.DeadlineExceeded))
	if tmo.Kind != "timeout" {
		t.Errorf("DeadlineExceeded should map to timeout, got %q", tmo.Kind)
	}
	cli := ClassifyError(errors.New("dial tcp: connection refused"))
	if cli.Kind != "client" || cli.SQLState != "" {
		t.Errorf("plain error should be client/no-sqlstate, got %+v", cli)
	}
}
