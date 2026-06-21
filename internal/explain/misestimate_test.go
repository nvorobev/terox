package explain

import "testing"

func fp(v float64) *float64 { return &v }

func TestTopMisestimates(t *testing.T) {
	root := &Root{Plan: Node{
		NodeType: "Hash Join", PlanRows: 10, ActualRows: fp(10), ActualLoops: fp(1),
		Plans: []Node{
			{NodeType: "Seq Scan", RelationName: "users", PlanRows: 1, ActualRows: fp(1000), ActualLoops: fp(1)},     // ~1000× off
			{NodeType: "Index Scan", RelationName: "orders", PlanRows: 500, ActualRows: fp(480), ActualLoops: fp(1)}, // ~1× ok
		},
	}}
	mis := TopMisestimates(root, 5)
	if len(mis) != 1 {
		t.Fatalf("expected 1 node over the 10× threshold, got %d: %+v", len(mis), mis)
	}
	if mis[0].Relation != "users" || mis[0].Ratio < 900 {
		t.Errorf("worst offender should be users ~1000×, got %+v", mis[0])
	}
}

func TestTopMisestimatesLoopsNormalized(t *testing.T) {
	// Узел с loops: фактические строки = ActualRows × Loops.
	root := &Root{Plan: Node{
		NodeType: "Nested Loop", PlanRows: 1, ActualRows: fp(2), ActualLoops: fp(100), // факт = 200, est 1 → 200×
	}}
	mis := TopMisestimates(root, 5)
	if len(mis) != 1 || mis[0].Actual != 200 {
		t.Fatalf("loops should multiply actual rows to 200, got %+v", mis)
	}
}

func TestTopMisestimatesEstimateOnlySkipped(t *testing.T) {
	// Без ActualRows (estimate-only план) — ничего не возвращаем.
	root := &Root{Plan: Node{NodeType: "Seq Scan", PlanRows: 1}}
	if mis := TopMisestimates(root, 5); len(mis) != 0 {
		t.Errorf("estimate-only plan should yield no misestimates, got %+v", mis)
	}
}
