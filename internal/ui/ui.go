// Package ui — цветовая тема terox: спокойный зелёный на чёрном «матрица»,
// где базы зелёные, опасные режимы красные, таймауты янтарные.
//
// lipgloss сам определяет цветовой профиль терминала: при выводе в пайп стили
// без цвета (чистые логи и тесты), на реальном TTY — с цветом.
package ui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

// Enabled сообщает, является ли stdout интерактивным терминалом. У go-pretty нет
// авто-определения, поэтому раскраска таблиц зависит от этого.
var Enabled = isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())

var (
	// Сервис/хранилище — «базы», зелёный.
	Service = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fd07a")).Bold(true)
	// Целевой шард, мягкий зелёный.
	Shard = lipgloss.NewStyle().Foreground(lipgloss.Color("#6fe39b"))
	// Безопасные состояния (read-only, staging) — спокойный зелёный.
	Safe = lipgloss.NewStyle().Foreground(lipgloss.Color("#6fe39b"))
	// Опасные состояния (режим записи, production) — красный.
	Danger = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5f5f")).Bold(true)
	// statement_timeout — янтарный.
	Timeout = lipgloss.NewStyle().Foreground(lipgloss.Color("#e3b341"))
	// Стрелка приглашения.
	Arrow = lipgloss.NewStyle().Foreground(lipgloss.Color("#3fd07a")).Bold(true)
	// Второстепенное / пунктуация.
	Dim = lipgloss.NewStyle().Foreground(lipgloss.Color("#7d8590"))
)
