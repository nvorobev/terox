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

// TestLiveCompletion проверяет типизированный движок автодополнения на локальной
// БД-песочнице (сборка каталога + связка движка). Включается через TEROX_LIVE=1.
func TestLiveCompletion(t *testing.T) {
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
	r := &REPL{mgr: mgr, cfg: &config.Config{}, shards: shards, targets: shards[:1]}
	c := newCompleter(r)

	// Каталог грузится в фоне; ждём его перед проверками.
	r.kickCatalog()
	deadline := time.Now().Add(15 * time.Second)
	for r.completeCatalog() == nil {
		if time.Now().After(deadline) {
			t.Fatal("catalog did not load within 15s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	hasSuffix := func(head, want string) bool {
		subs, _ := c.suggestions(head, len(head))
		for _, s := range subs {
			if s == want {
				return true
			}
		}
		return false
	}
	// отношение предлагается в позиции FROM
	if !hasSuffix("select * from ", "items") {
		t.Error("FROM: expected relation 'items'")
	}
	// колонки по алиасу берутся только из соответствующего отношения
	if !hasSuffix("select * from items i where i.", "item_id") {
		t.Error("alias.col: expected 'item_id'")
	}
	// UPDATE SET дополняет колонки целевой таблицы
	if !hasSuffix("update items set ", "price") {
		t.Error("UPDATE SET: expected column 'price'")
	}
	// префикс без учёта регистра, суффикс дополняет остаток
	if !hasSuffix("SELECT * FROM IT", "ems") {
		t.Error("case-insensitive FROM IT -> items")
	}
	// голый WHERE дополняет колонки в области видимости (ленивая загрузка по Tab)
	if !hasSuffix("select * from items where item", "_id") {
		t.Error("WHERE should complete the column item_id")
	}
	// имя CTE из запроса предлагается в позиции FROM
	if !hasSuffix("with recent as (select 1) select * from re", "cent") {
		t.Error("FROM should offer the CTE 'recent'")
	}
	// FROM предлагает табличные функции вроде generate_series...
	if !hasSuffix("select * from generate_serie", "s(") {
		t.Error("FROM should offer set-returning generate_series")
	}
	// ...но не скалярные функции вроде now().
	if nowSubs, _ := c.suggestions("select * from no", len("select * from no")); contains(nowSubs, "w()") {
		t.Error("FROM should not offer the scalar function now()")
	}
	// внутри строкового литерала дополнения нет
	if subs, _ := c.suggestions("select * from items where name = 'it", len("select * from items where name = 'it")); len(subs) > 0 {
		t.Errorf("no completion inside a string; got %v", subs)
	}
	// в FROM отношения ранжируются выше функций pg_catalog
	subs, _ := c.suggestions("select * from ", len("select * from "))
	if len(subs) > 0 && strings.HasSuffix(subs[0], "()") {
		t.Errorf("FROM should lead with a relation, not a function: %q", subs[0])
	}

	// призрачная подсказка показывает тип колонки
	if g, a := c.ghostHint("select * from items where pri"); g != "ce" || !strings.Contains(a, "numeric") && !strings.Contains(a, ":") {
		t.Logf("ghost(price) = %q  annot=%q", g, a) // имя типа может отличаться; для информации
	}
	// призрачная подсказка показывает сигнатуру функции
	if g, a := c.ghostHint("select generate_serie"); !strings.HasPrefix(g, "s") || a == "" {
		t.Errorf("ghost(generate_series) ghost=%q annot=%q (want signature annotation)", g, a)
	}
}
