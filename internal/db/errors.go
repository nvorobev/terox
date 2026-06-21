package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// SQLState возвращает SQLSTATE-код PostgreSQL (5 символов, напр. "23505"), если
// err (или любая обёрнутая в неё) — ошибка сервера; иначе "". Позволяет коду
// отличать SQLSTATE от человекочитаемого сообщения (Release 2: структурированные
// ошибки).
func SQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ErrorInfo — структурированная ошибка для машиночитаемого вывода (headless JSON,
// per-shard статус): SQLSTATE отдельно от сообщения, плюс грубая категория.
type ErrorInfo struct {
	SQLState string `json:"sqlstate,omitempty"`
	Message  string `json:"message"`
	Severity string `json:"severity,omitempty"`
	// Kind — грубая категория: "server" (ошибка PostgreSQL с SQLSTATE),
	// "timeout" (клиентский дедлайн), "canceled" (отмена/Ctrl-C или серверное
	// 57014) или "client" (всё остальное — сеть/TLS/драйвер).
	Kind string `json:"kind,omitempty"`
}

// ClassifyError раскладывает err на структурированную ErrorInfo (nil для nil err).
// Чистая функция: SQLSTATE из *pgconn.PgError, иначе категория по context-ошибке.
func ClassifyError(err error) *ErrorInfo {
	if err == nil {
		return nil
	}
	info := &ErrorInfo{Message: err.Error(), Kind: "client"}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		info.SQLState = pgErr.Code
		info.Message = pgErr.Message
		info.Severity = pgErr.Severity
		info.Kind = "server"
		// 57014 query_canceled НАМЕРЕННО относим к "canceled". Сервер отдаёт один и
		// тот же код и при истечении statement_timeout, и при явной отмене запроса
		// (наш Ctrl-C через отмену ctx заставляет pgx послать cancel request, на
		// который сервер отвечает тем же 57014). Надёжно различить серверный таймаут
		// и пользовательскую отмену по одному PgError нельзя (текст сообщения
		// зависит от версии/локали, а ctx сюда не передаётся), поэтому остаёмся
		// консервативными: "canceled" покрывает оба случая. Не дробим без сигнала,
		// который можно отличить детерминированно.
		if pgErr.Code == "57014" {
			info.Kind = "canceled"
		}
		return info
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		info.Kind = "timeout"
	case errors.Is(err, context.Canceled):
		info.Kind = "canceled"
	}
	return info
}
