package repl

import (
	"os"
	"strings"
	"testing"
	"time"

	"terox/internal/cluster"
	"terox/internal/config"
	"terox/internal/db"
)

// TestLivePkeyFind проверяет сокращение \find/\count "table.pkey=value" на
// реальной песочной БД: автодополнение подставляет первичный ключ после второй
// точки, а команда отклоняет колонку, не являющуюся pkey. Включается TEROX_LIVE=1.
func TestLivePkeyFind(t *testing.T) {
	if os.Getenv("TEROX_LIVE") != "1" {
		t.Skip("set TEROX_LIVE=1 to run against the local DB")
	}
	shards, err := cluster.Expand(&config.Storage{
		HostTemplate: "127.0.0.1", DBTemplate: "shard_{p}", Port: 6432,
		User: "ro_user", Password: "ropass", Count: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr := db.NewManager()
	defer mgr.Close()
	r := &REPL{mgr: mgr, cfg: &config.Config{}, shards: shards, targets: shards[:1], out: os.Stdout}
	c := newCompleter(r)

	r.kickCatalog()
	deadline := time.Now().Add(15 * time.Second)
	for r.completeCatalog() == nil {
		if time.Now().After(deadline) {
			t.Fatal("catalog did not load within 15s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Синхронно прогреваем кэш первичных ключей (автодополнение грузит его в фоне).
	if pk := r.primaryKeyColumns("items", "items"); len(pk) != 1 || pk[0] != "item_id" {
		t.Fatalf("primary key of items.items = %v, want [item_id]", pk)
	}

	// Автодополнение: "\find items.items." подставляет колонку pkey.
	subs, _ := c.suggestions(`\find items.items.`, len(`\find items.items.`))
	found := false
	for _, s := range subs {
		if s == "item_id" {
			found = true
		}
	}
	if !found {
		t.Errorf(`\find items.items. should offer pkey 'item_id'; got %v`, subs)
	}

	// Сокращение по pkey принимается (раскрывается в таблицу + условие).
	got, err := r.resolvePkeyShorthand([]string{"items.items.item_id=100"})
	if err != nil {
		t.Errorf("pkey search should be allowed: %v", err)
	} else if len(got) != 2 || got[0] != "items.items" || got[1] != "item_id=100" {
		t.Errorf("rewrite = %v, want [items.items item_id=100]", got)
	}

	// Поиск по колонке, не являющейся pkey, отклоняется.
	if _, err := r.resolvePkeyShorthand([]string{"items.items.name=foo"}); err == nil {
		t.Errorf("search by non-pkey 'name' must be refused")
	} else if !strings.Contains(err.Error(), "item_id") {
		t.Errorf("refusal should name the real pkey; got %v", err)
	}
}
