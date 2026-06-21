package repl

import (
	"os"
	"testing"

	"terox/internal/cluster"
	"terox/internal/complete"
	"terox/internal/db"
)

// TestLiveMultiSchemaCompletion строит реальный каталог из тестовой БД с
// несколькими схемами (public/items/billing/analytics) и проверяет автодополнение:
// голые имена только для таблиц search_path, схемы для углубления, drill "schema."
// показывает только таблицы/представления+функции этой схемы (без индексов),
// системный каталог скрыт, встроенные функции доступны только после префикса.
// Требует TEROX_LIVE.
func TestLiveMultiSchemaCompletion(t *testing.T) {
	if os.Getenv("TEROX_LIVE") == "" {
		t.Skip("set TEROX_LIVE=1 to run against the local multi-schema DB")
	}
	mgr := db.NewManager()
	defer mgr.Close()
	host := os.Getenv("TEROX_LIVE_HOST")
	if host == "" {
		host = "localhost"
	}
	targets := []cluster.Shard{{
		Host: host, Port: 55432, DB: "master", User: "postgres",
		Password: "secret", SSLMode: "disable", Label: "t",
	}}
	cat, err := buildCatalog(mgr, targets, 1)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}

	has := func(r complete.Result, name string) bool {
		for _, c := range r.Candidates {
			if c.Display == name {
				return true
			}
		}
		return false
	}
	names := func(r complete.Result) []string {
		out := make([]string, len(r.Candidates))
		for i, c := range r.Candidates {
			out[i] = c.Display
		}
		return out
	}

	// 1) FROM: таблицы public (search_path) + имена схем, но НЕ голые имена
	// других схем и НЕ системный каталог.
	from := complete.Complete("select * from ", cat)
	for _, want := range []string{"orders", "customers", "order_summary", "items", "billing", "analytics"} {
		if !has(from, want) {
			t.Errorf("FROM should offer %q; got %v", want, names(from))
		}
	}
	for _, bad := range []string{"products", "invoices", "daily_stats", "pg_class"} {
		if has(from, bad) {
			t.Errorf("FROM must NOT offer %q (other-schema bare name or system table); got %v", bad, names(from))
		}
	}

	// 2) Ввод имени схемы выдаёт СХЕМУ (для drill "."), а не таблицу.
	it := complete.Complete("select * from items", cat)
	if len(it.Candidates) == 0 || it.Candidates[0].Display != "items" || it.Candidates[0].Kind != complete.KSchema {
		t.Errorf("typing 'items' should lead with the schema; got %v", names(it))
	}

	// 3) drill "items.": таблицы/представления + функции схемы items, по типу
	// затем по имени; БЕЗ индексов/первичных ключей; без утечки pg_catalog.
	drill := complete.Complete("select * from items.", cat)
	for _, want := range []string{"products", "categories", "discounted_price"} {
		if !has(drill, want) {
			t.Errorf("items. drill should list %q; got %v", want, names(drill))
		}
	}
	for _, bad := range []string{"products_pkey", "products_sku_idx", "categories_pkey", "now"} {
		if has(drill, bad) {
			t.Errorf("items. drill must NOT list %q; got %v", bad, names(drill))
		}
	}
	if len(drill.Candidates) == 0 || drill.Candidates[0].Kind != complete.KRelation {
		t.Errorf("items. drill should lead with a relation (table); got %v", names(drill))
	}

	// 4) drill "billing.".
	bill := complete.Complete("select * from billing.", cat)
	for _, want := range []string{"invoices", "payments"} {
		if !has(bill, want) {
			t.Errorf("billing. drill should list %q; got %v", want, names(bill))
		}
	}
	if has(bill, "invoices_pkey") {
		t.Errorf("billing. drill must not list a primary key; got %v", names(bill))
	}

	// 5) drill "analytics." показывает материализованное представление.
	an := complete.Complete("select * from analytics.", cat)
	if !has(an, "daily_stats") {
		t.Errorf("analytics. drill should list the matview daily_stats; got %v", names(an))
	}

	// 6) Встроенные функции в позиции выражения доступны только после префикса.
	if has(complete.Complete("select ", cat), "now") {
		t.Errorf("now() must not dump on an empty Tab in expression position")
	}
	if !has(complete.Complete("select no", cat), "now") {
		t.Errorf("now() should be reachable after typing 'no'")
	}
}
