package repl

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
)

// TestInvalidIndexDropStmt проверяет, что DROP-команда строится через QuoteIdent:
// простые имена остаются без кавычек, а спецсимволы/верхний регистр/кавычки
// экранируются — защита от инъекции из имён объектов в БД.
func TestInvalidIndexDropStmt(t *testing.T) {
	cases := []struct {
		name   string
		ix     invalidIndex
		want   string
		labelW string
	}{
		{
			name:   "simple lowercase",
			ix:     invalidIndex{schema: "public", index: "items_idx", table: "items"},
			want:   `DROP INDEX CONCURRENTLY IF EXISTS public.items_idx`,
			labelW: "public.items_idx on items",
		},
		{
			name:   "mixed case quoted",
			ix:     invalidIndex{schema: "App", index: "Items_IDX", table: "Items"},
			want:   `DROP INDEX CONCURRENTLY IF EXISTS "App"."Items_IDX"`,
			labelW: "App.Items_IDX on Items",
		},
		{
			name:   "reserved word and special chars",
			ix:     invalidIndex{schema: "select", index: "idx-with-dash", table: "t"},
			want:   `DROP INDEX CONCURRENTLY IF EXISTS "select"."idx-with-dash"`,
			labelW: "select.idx-with-dash on t",
		},
		{
			name:   "embedded double quote is doubled",
			ix:     invalidIndex{schema: "public", index: `ev"il`, table: "t"},
			want:   `DROP INDEX CONCURRENTLY IF EXISTS public."ev""il"`,
			labelW: `public.ev"il on t`,
		},
		{
			name:   "injection attempt is neutralized",
			ix:     invalidIndex{schema: "public", index: `x"; DROP TABLE users; --`, table: "t"},
			want:   `DROP INDEX CONCURRENTLY IF EXISTS public."x""; DROP TABLE users; --"`,
			labelW: `public.x"; DROP TABLE users; -- on t`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ix.dropStmt(); got != tc.want {
				t.Errorf("dropStmt()=%q, want %q", got, tc.want)
			}
			if got := tc.ix.label(); got != tc.labelW {
				t.Errorf("label()=%q, want %q", got, tc.labelW)
			}
		})
	}
}

// TestDoHealFlagParsing проверяет разбор аргументов \heal: только --apply допустим,
// прочее — ошибка с подсказкой usage; без целей — "no shard selected".
func TestDoHealFlagParsing(t *testing.T) {
	t.Run("unknown flag", func(t *testing.T) {
		var buf bytes.Buffer
		r := &REPL{out: &buf, cfg: &config.Config{}, targets: []cluster.Shard{{Label: "s0"}}}
		err := r.doHeal([]string{"--nope"})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("expected usage error, got %v", err)
		}
	})
	t.Run("no shard selected", func(t *testing.T) {
		var buf bytes.Buffer
		r := &REPL{out: &buf, cfg: &config.Config{}}
		err := r.doHeal(nil)
		if err == nil || !strings.Contains(err.Error(), "no shard selected") {
			t.Fatalf("expected no-shard error, got %v", err)
		}
	})
}

// TestHealApplyRequiresWriteMode проверяет инвариант безопасности: \heal --apply
// без write-режима отказывает с подсказкой \write on и НЕ обращается к БД (mgr nil
// здесь — любой коннект упал бы паникой).
func TestHealApplyRequiresWriteMode(t *testing.T) {
	var buf bytes.Buffer
	r := &REPL{out: &buf, cfg: &config.Config{}, targets: []cluster.Shard{{Label: "s0"}}, writeMode: false}
	err := r.doHeal([]string{"--apply"})
	if err == nil {
		t.Fatal("expected error when --apply without write mode")
	}
	if !strings.Contains(err.Error(), "write mode") || !strings.Contains(err.Error(), "\\write on") {
		t.Errorf("error %q should mention write mode and \\write on", err.Error())
	}
}

// TestHasCreateIndexConcurrently проверяет локальный детектор CREATE INDEX
// CONCURRENTLY, который включает напоминание про \heal после сбоя.
func TestHasCreateIndexConcurrently(t *testing.T) {
	yes := [][]string{
		{"CREATE INDEX CONCURRENTLY idx ON t (a)"},
		{"create unique index concurrently idx on t (a)"},
		{"SELECT 1", "CREATE  INDEX\n CONCURRENTLY idx ON t (a)"},
	}
	for _, stmts := range yes {
		if !hasCreateIndexConcurrently(stmts) {
			t.Errorf("expected CREATE INDEX CONCURRENTLY detected in %v", stmts)
		}
	}
	no := [][]string{
		{"CREATE INDEX idx ON t (a)"},
		{"VACUUM ANALYZE t"},
		{"DROP INDEX CONCURRENTLY idx"},
		nil,
	}
	for _, stmts := range no {
		if hasCreateIndexConcurrently(stmts) {
			t.Errorf("did not expect CREATE INDEX CONCURRENTLY in %v", stmts)
		}
	}
}

// TestAnyExecError проверяет агрегатор ошибок по шардам.
func TestAnyExecError(t *testing.T) {
	if anyExecError(nil) {
		t.Error("nil results should report no error")
	}
	ok := []db.ExecResult{{Shard: cluster.Shard{Label: "a"}}, {Shard: cluster.Shard{Label: "b"}}}
	if anyExecError(ok) {
		t.Error("all-ok results should report no error")
	}
	mixed := []db.ExecResult{{Shard: cluster.Shard{Label: "a"}}, {Shard: cluster.Shard{Label: "b"}, Err: errors.New("boom")}}
	if !anyExecError(mixed) {
		t.Error("one failed shard should report an error")
	}
}
