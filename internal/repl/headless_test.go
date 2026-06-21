package repl

import (
	"bytes"
	"strings"
	"testing"

	"terox/internal/config"
)

// TestQueryRejectsUnknownFormat проверяет, что неизвестный --format даёт явную
// ошибку, а не молчаливо переходит к табличному выводу. Проверка идёт до доступа к БД.
func TestQueryRejectsUnknownFormat(t *testing.T) {
	cfg := &config.Config{}
	var buf bytes.Buffer
	err := Query(cfg, "svc/st", "select 1", QueryOptions{Format: "xml"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "unknown --format") {
		t.Fatalf("expected unknown-format error, got %v", err)
	}
	// Запрос на запись отклоняется независимо от формата.
	if err := Query(cfg, "svc/st", "delete from t", QueryOptions{Format: "json"}, &buf); err == nil ||
		!strings.Contains(err.Error(), "read-only") {
		t.Fatalf("expected read-only rejection, got %v", err)
	}
}

// TestQueryRejectsEmptyStorage проверяет, что выбор пустого storage возвращает
// понятную ошибку, а не вызывает панику на nil *Storage.
func TestQueryRejectsEmptyStorage(t *testing.T) {
	cfg := &config.Config{Services: map[string]*config.Service{
		"svc": {Storages: map[string]*config.Storage{"st": nil}},
	}}
	var buf bytes.Buffer
	err := Query(cfg, "svc/st", "select 1", QueryOptions{Format: "json"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "empty storage") {
		t.Fatalf("expected empty-storage error, got %v", err)
	}
}

func TestParseOrderBy(t *testing.T) {
	cases := []struct {
		in   string
		col  string
		desc bool
	}{
		{"", "", false},
		{"id", "id", false},
		{"id:asc", "id", false},
		{"id:desc", "id", true},
		{"created_at:DESC", "created_at", true},
		{" name ", "name", false},
		{"weird:name", "weird:name", false}, // суффикс не asc/desc -> часть имени
	}
	for _, c := range cases {
		col, desc := parseOrderBy(c.in)
		if col != c.col || desc != c.desc {
			t.Errorf("parseOrderBy(%q) = (%q,%v), want (%q,%v)", c.in, col, desc, c.col, c.desc)
		}
	}
}
