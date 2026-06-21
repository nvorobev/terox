package advisor

import (
	"strings"
	"testing"

	"terox/internal/explain"
)

// propByKind возвращает первое предложение заданного вида.
func propByKind(props []Proposal, kind string) *Proposal {
	for i := range props {
		if props[i].Kind == kind {
			return &props[i]
		}
	}
	return nil
}

func colsCSV(p *Proposal) string {
	if p == nil {
		return "<nil>"
	}
	return strings.Join(p.Cols, ",")
}

func TestCollectProposalsFilterComposite(t *testing.T) {
	// Равенство ведёт, диапазон — в хвост: составной (status, created_at).
	root := explain.Node{
		NodeType:     "Seq Scan",
		RelationName: "orders",
		Filter:       "((status = 'new'::text) AND (created_at > '2024-01-01'::date))",
	}
	props := CollectProposals(&root)
	p := propByKind(props, "filter")
	if p == nil || colsCSV(p) != "status,created_at" {
		t.Fatalf("filter proposal cols = %q, want status,created_at", colsCSV(p))
	}
	if p.Confidence != "medium" {
		t.Errorf("composite filter confidence = %q, want medium", p.Confidence)
	}
}

func TestCollectProposalsJoin(t *testing.T) {
	// Hash Join, обе стороны — Seq Scan: предложения по обоим join-столбцам.
	root := explain.Node{
		NodeType: "Hash Join",
		HashCond: "(o.user_id = u.id)",
		Plans: []explain.Node{
			{NodeType: "Seq Scan", RelationName: "orders", Alias: "o"},
			{NodeType: "Hash", Plans: []explain.Node{
				{NodeType: "Seq Scan", RelationName: "users", Alias: "u"},
			}},
		},
	}
	props := CollectProposals(&root)
	var gotOrders, gotUsers bool
	for _, p := range props {
		if p.Kind != "join" {
			continue
		}
		if p.Table == "orders" && colsCSV(&p) == "user_id" {
			gotOrders = true
		}
		if p.Table == "users" && colsCSV(&p) == "id" {
			gotUsers = true
		}
	}
	if !gotOrders || !gotUsers {
		t.Fatalf("expected join proposals on orders(user_id) and users(id), got %+v", props)
	}
}

func TestCollectProposalsJoinSkipsIndexScannedSide(t *testing.T) {
	// users читается Index Scan — индекс уже есть, join-предложение для него не нужно.
	root := explain.Node{
		NodeType: "Hash Join",
		HashCond: "(o.user_id = u.id)",
		Plans: []explain.Node{
			{NodeType: "Seq Scan", RelationName: "orders", Alias: "o"},
			{NodeType: "Index Scan", RelationName: "users", Alias: "u", IndexName: "users_pkey"},
		},
	}
	props := CollectProposals(&root)
	for _, p := range props {
		if p.Kind == "join" && p.Table == "users" {
			t.Errorf("should not propose a join index on the index-scanned users side: %+v", p)
		}
	}
}

func TestCollectProposalsSortAndGroup(t *testing.T) {
	sortRoot := explain.Node{
		NodeType: "Sort",
		SortKey:  []string{"u.created_at", "u.id DESC"},
		Plans: []explain.Node{
			{NodeType: "Seq Scan", RelationName: "users", Alias: "u"},
		},
	}
	if p := propByKind(CollectProposals(&sortRoot), "sort"); p == nil || colsCSV(p) != "created_at,id" {
		t.Fatalf("sort proposal cols = %q, want created_at,id", colsCSV(p))
	}

	groupRoot := explain.Node{
		NodeType: "GroupAggregate",
		GroupKey: []string{"u.country", "u.status"},
		Plans: []explain.Node{
			{NodeType: "Seq Scan", RelationName: "users", Alias: "u"},
		},
	}
	if p := propByKind(CollectProposals(&groupRoot), "group"); p == nil || colsCSV(p) != "country,status" {
		t.Fatalf("group proposal cols = %q, want country,status", colsCSV(p))
	}
}

func TestSingleRelKeyColsRejectsExpressionsAndMixed(t *testing.T) {
	aliases := map[string]*scanRel{
		"u": {schema: "public", table: "users", seq: true},
		"o": {schema: "public", table: "orders", seq: true},
	}
	// Выражение в ключе сортировки → отказ.
	if rel, _ := singleRelKeyCols(aliases, []string{"lower((u.name)::text)"}); rel != nil {
		t.Error("expression sort key should not yield an index proposal")
	}
	// Ключи из разных таблиц → отказ (общий индекс не подходит).
	if rel, _ := singleRelKeyCols(aliases, []string{"u.created_at", "o.id"}); rel != nil {
		t.Error("keys spanning two tables should not yield a single index proposal")
	}
	// Один и тот же стол — ок.
	if rel, cols := singleRelKeyCols(aliases, []string{"u.a", "u.b"}); rel == nil || strings.Join(cols, ",") != "a,b" {
		t.Errorf("single-table keys should resolve, got rel=%v cols=%v", rel, cols)
	}
}

func TestCollectScanAliasesAmbiguous(t *testing.T) {
	// Один и тот же псевдоним у двух разных таблиц → неразрешимо (nil).
	root := explain.Node{
		NodeType: "Hash Join",
		Plans: []explain.Node{
			{NodeType: "Seq Scan", RelationName: "a", Alias: "t"},
			{NodeType: "Seq Scan", RelationName: "b", Alias: "t"},
		},
	}
	aliases := map[string]*scanRel{}
	collectScanAliases(&root, aliases)
	if aliases["t"] != nil {
		t.Errorf("ambiguous alias t should map to nil, got %+v", aliases["t"])
	}
	if _, ok := resolveSeqRel(aliases, "t"); ok {
		t.Error("ambiguous alias must not resolve")
	}
}

func TestCondColumns(t *testing.T) {
	got := condColumns("((o.user_id)::bigint = u.id)")
	if len(got) != 2 || got[0] != (qualCol{"o", "user_id"}) || got[1] != (qualCol{"u", "id"}) {
		t.Fatalf("condColumns = %+v", got)
	}
}

func TestProposalIndexDDLIfExists(t *testing.T) {
	p := Proposal{Schema: "public", Table: "users", Cols: []string{"email", "status"}, Kind: "filter"}
	suggest, rollback := p.IndexDDL()
	if !strings.Contains(suggest, "idx_users_email_status") || !strings.Contains(suggest, "(email, status)") {
		t.Errorf("suggest DDL wrong: %s", suggest)
	}
	if !strings.Contains(rollback, "IF EXISTS") {
		t.Errorf("rollback must use IF EXISTS: %s", rollback)
	}
	if !strings.Contains(rollback, "CONCURRENTLY") {
		t.Errorf("rollback must be CONCURRENTLY: %s", rollback)
	}
}

func TestCollectProposalsDedup(t *testing.T) {
	// Один и тот же фильтр по двум одинаковым сканам схлопывается в одно предложение.
	root := explain.Node{
		NodeType: "Append",
		Plans: []explain.Node{
			{NodeType: "Seq Scan", RelationName: "t", Filter: "(x = 1)"},
			{NodeType: "Seq Scan", RelationName: "t", Filter: "(x = 1)"},
		},
	}
	n := 0
	for _, p := range CollectProposals(&root) {
		if p.Kind == "filter" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected 1 deduped filter proposal, got %d", n)
	}
}
