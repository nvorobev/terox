package cluster

import (
	"testing"

	"terox/internal/config"
)

func TestExpandRealExamples(t *testing.T) {
	cases := []struct {
		name      string
		st        config.Storage
		wantFirst string // хост первого шарда
		wantLast  string // хост последнего шарда
		firstDB   string
		lastDB    string
	}{
		{
			name: "sharded 128 master",
			st: config.Storage{
				HostTemplate: "pgbouncer-dev-pg-pr-item-sharded-rs-rs{p1:03}.db.avito-sd",
				DBTemplate:   "master", Port: 6404, Count: 128,
			},
			wantFirst: "pgbouncer-dev-pg-pr-item-sharded-rs-rs001.db.avito-sd",
			wantLast:  "pgbouncer-dev-pg-pr-item-sharded-rs-rs128.db.avito-sd",
			firstDB:   "master", lastDB: "master",
		},
		{
			name: "uz 32 internal",
			st: config.Storage{
				HostTemplate: "pgbouncer-dev-pg-pr-item-uz-195d-rs-rs{p1:03}.db.uz.internal",
				DBTemplate:   "master", Port: 6404, Count: 32,
			},
			wantFirst: "pgbouncer-dev-pg-pr-item-uz-195d-rs-rs001.db.uz.internal",
			wantLast:  "pgbouncer-dev-pg-pr-item-uz-195d-rs-rs032.db.uz.internal",
			firstDB:   "master", lastDB: "master",
		},
		{
			name: "legacy host 1-based, db 0-based",
			st: config.Storage{
				HostTemplate: "pgbouncer-dev-item-rs-rs{p1:02}.db.avito-sd",
				DBTemplate:   "shard_{p}", Port: 6404, Count: 32,
			},
			wantFirst: "pgbouncer-dev-item-rs-rs01.db.avito-sd",
			wantLast:  "pgbouncer-dev-item-rs-rs32.db.avito-sd",
			firstDB:   "shard_0", lastDB: "shard_31",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shards, err := Expand(&tc.st)
			if err != nil {
				t.Fatal(err)
			}
			if len(shards) != tc.st.Count {
				t.Fatalf("count = %d, want %d", len(shards), tc.st.Count)
			}
			if shards[0].Host != tc.wantFirst {
				t.Errorf("first host = %q, want %q", shards[0].Host, tc.wantFirst)
			}
			if shards[len(shards)-1].Host != tc.wantLast {
				t.Errorf("last host = %q, want %q", shards[len(shards)-1].Host, tc.wantLast)
			}
			if shards[0].DB != tc.firstDB {
				t.Errorf("first db = %q, want %q", shards[0].DB, tc.firstDB)
			}
			if shards[len(shards)-1].DB != tc.lastDB {
				t.Errorf("last db = %q, want %q", shards[len(shards)-1].DB, tc.lastDB)
			}
		})
	}
}

func TestDeriveHostTemplate(t *testing.T) {
	cases := []struct {
		first, last  string
		wantTemplate string
		wantCount    int
		wantWidth    int
	}{
		{
			first:        "pgbouncer-dev-pg-pr-item-sharded-rs-rs001.db.avito-sd",
			last:         "pgbouncer-dev-pg-pr-item-sharded-rs-rs128.db.avito-sd",
			wantTemplate: "pgbouncer-dev-pg-pr-item-sharded-rs-rs{p1:03}.db.avito-sd",
			wantCount:    128, wantWidth: 3,
		},
		{
			first:        "pgbouncer-dev-item-rs-rs01.db.avito-sd",
			last:         "pgbouncer-dev-item-rs-rs32.db.avito-sd",
			wantTemplate: "pgbouncer-dev-item-rs-rs{p1:02}.db.avito-sd",
			wantCount:    32, wantWidth: 2,
		},
		{
			first:        "pg-billing-main.db.avito-sd",
			last:         "pg-billing-main.db.avito-sd",
			wantTemplate: "pg-billing-main.db.avito-sd",
			wantCount:    1, wantWidth: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.first, func(t *testing.T) {
			d, err := DeriveHostTemplate(tc.first, tc.last)
			if err != nil {
				t.Fatal(err)
			}
			if d.HostTemplate != tc.wantTemplate {
				t.Errorf("template = %q, want %q", d.HostTemplate, tc.wantTemplate)
			}
			if d.Count != tc.wantCount {
				t.Errorf("count = %d, want %d", d.Count, tc.wantCount)
			}
			if d.Width != tc.wantWidth {
				t.Errorf("width = %d, want %d", d.Width, tc.wantWidth)
			}
		})
	}
}

// TestDeriveRoundTrip проверяет, что выведенный шаблон разворачивается обратно
// в исходные первый/последний хосты (один числовой сегмент, нумерация с 1).
func TestDeriveRoundTrip(t *testing.T) {
	first := "pgbouncer-dev-item-cold-rs-rs001.db.avito-sd"
	last := "pgbouncer-dev-item-cold-rs-rs128.db.avito-sd"
	d, err := DeriveHostTemplate(first, last)
	if err != nil {
		t.Fatal(err)
	}
	shards, err := Expand(&config.Storage{HostTemplate: d.HostTemplate, Count: d.Count, Port: 6404})
	if err != nil {
		t.Fatal(err)
	}
	if shards[0].Host != first {
		t.Errorf("first = %q, want %q", shards[0].Host, first)
	}
	if shards[len(shards)-1].Host != last {
		t.Errorf("last = %q, want %q", shards[len(shards)-1].Host, last)
	}
}

func TestExpandRejectsDuplicateTargets(t *testing.T) {
	// count>1 без {p}/{p1} в host или db схлопывает все шарды в одну
	// физическую цель — fan-out запись попадёт в одну БД N раз.
	_, err := Expand(&config.Storage{
		HostTemplate: "db.example", DBTemplate: "master", Port: 6432,
		User: "u", Count: 4,
	})
	if err == nil {
		t.Fatal("expected an error for count=4 with no placeholder (duplicate targets)")
	}

	// Плейсхолдер {p}/{p1} делает каждый шард уникальным → ошибки нет.
	if _, err := Expand(&config.Storage{
		HostTemplate: "db{p1}.example", DBTemplate: "master", Port: 6432,
		User: "u", Count: 4,
	}); err != nil {
		t.Fatalf("placeholdered host should expand cleanly: %v", err)
	}
	// Своя db на каждый шард тоже различает цели на одном хосте.
	if _, err := Expand(&config.Storage{
		HostTemplate: "db.example", DBTemplate: "shard_{p}", Port: 6432,
		User: "u", Count: 4,
	}); err != nil {
		t.Fatalf("placeholdered db should expand cleanly: %v", err)
	}
	// count=1, одна база — это нормально.
	if _, err := Expand(&config.Storage{
		HostTemplate: "db.example", DBTemplate: "master", Port: 6432,
		User: "u", Count: 1,
	}); err != nil {
		t.Fatalf("single database should expand cleanly: %v", err)
	}
}

// TestExpandDisambiguatesCollidingLabels проверяет, что когда hostLabel
// (сегмент после последнего "-" до первой ".") совпадает у разных физических
// шардов, Expand делает метки различимыми. Шаблон "pg-{p}-rs.db" даёт хосты
// pg-0-rs.db, pg-1-rs.db, ... — у всех суффикс хоста "rs", т.е. без фикса
// метка "rs" коллизировала бы, и выбор/адресация по метке молча ушли бы в
// первый шард.
func TestExpandDisambiguatesCollidingLabels(t *testing.T) {
	shards, err := Expand(&config.Storage{
		HostTemplate: "pg-{p}-rs.db", DBTemplate: "master", Port: 6432, Count: 3,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("count = %d, want 3", len(shards))
	}

	// Метки уникальны и детерминированы (суффикс "#<позиция>").
	seen := map[string]int{}
	for _, s := range shards {
		seen[s.Label]++
	}
	for label, n := range seen {
		if n != 1 {
			t.Errorf("label %q appears %d times, want unique", label, n)
		}
	}
	for _, want := range []string{"rs#0", "rs#1", "rs#2"} {
		if seen[want] != 1 {
			t.Errorf("missing disambiguated label %q (got %v)", want, seen)
		}
	}

	// ParseSelector по каждой метке выбирает ровно один соответствующий шард,
	// а не первый совпавший — это и есть устранённый риск адресации записи.
	for _, s := range shards {
		got, label, err := ParseSelector(shards, s.Label)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", s.Label, err)
		}
		if len(got) != 1 {
			t.Fatalf("ParseSelector(%q) selected %d shards, want 1", s.Label, len(got))
		}
		if got[0].Position != s.Position || got[0].Host != s.Host {
			t.Errorf("ParseSelector(%q) -> pos %d host %q, want pos %d host %q",
				s.Label, got[0].Position, got[0].Host, s.Position, s.Host)
		}
		if label != s.Label {
			t.Errorf("ParseSelector(%q) label = %q, want %q", s.Label, label, s.Label)
		}
	}
}

// TestExpandKeepsUniqueLabels проверяет, что неконфликтный случай не меняется:
// при уже уникальных метках суффикс "#<p>" не добавляется.
func TestExpandKeepsUniqueLabels(t *testing.T) {
	shards, err := Expand(&config.Storage{
		HostTemplate: "pg-rs{p1:03}.db", DBTemplate: "master", Port: 6432, Count: 4,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{"rs001", "rs002", "rs003", "rs004"}
	for i, w := range want {
		if shards[i].Label != w {
			t.Errorf("label[%d] = %q, want %q (unique labels must stay unchanged)", i, shards[i].Label, w)
		}
	}
}

func TestShardLabelDB(t *testing.T) {
	cases := []struct {
		label, db, want string
	}{
		{"rs002", "shard_1", "rs002/shard_1"}, // шардинг по хостам → показываем оба
		{"shard_0", "shard_0", "shard_0"},     // шардинг по db (label==db) → без дублей
		{"rs002", "", "rs002"},                // нет db → только label
		{"rs002", "RS002", "rs002"},           // равны без учёта регистра → без дублей
	}
	for _, c := range cases {
		if got := (Shard{Label: c.label, DB: c.db}).LabelDB(); got != c.want {
			t.Errorf("LabelDB(label=%q db=%q) = %q, want %q", c.label, c.db, got, c.want)
		}
	}
}
