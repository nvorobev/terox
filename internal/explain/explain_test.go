package explain

import (
	"strings"
	"testing"
)

// План только с оценками (без полей Actual*): Analyze даёт Analyzed=false
// и Risk="unknown", а не измеренный вердикт "low"/"high".
const estimateOnlyPlan = `[
  {
    "Plan": {
      "Node Type": "Seq Scan",
      "Relation Name": "events",
      "Plan Rows": 77000,
      "Total Cost": 1234.5,
      "Startup Cost": 0.0
    },
    "Planning Time": 0.4
  }
]`

func TestFingerprintGroupsByStructure(t *testing.T) {
	seqScan := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"items","Plan Rows":5,"Total Cost":9}}]`
	idxScan := `[{"Plan":{"Node Type":"Index Scan","Index Name":"items_pkey","Relation Name":"items","Plan Rows":1,"Total Cost":8}}]`
	seqScan2 := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"items","Plan Rows":9999,"Total Cost":1234}}]` // та же структура, другие числа
	fp := func(s string) string {
		root, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return Fingerprint(root)
	}
	if fp(seqScan) != fp(seqScan2) {
		t.Error("same structure with different numbers must share a fingerprint")
	}
	if fp(seqScan) == fp(idxScan) {
		t.Error("Seq Scan vs Index Scan must differ in fingerprint")
	}
	root, _ := Parse(idxScan)
	if got := Shape(root); got != "Index Scan using items_pkey on items" {
		t.Errorf("Shape = %q", got)
	}
}

// TestFingerprintDistinguishesSchema проверяет, что одно имя таблицы в разных
// схемах даёт разные отпечатки (иначе public.users и audit.users неотличимы).
func TestFingerprintDistinguishesSchema(t *testing.T) {
	pub := `[{"Plan":{"Node Type":"Seq Scan","Schema":"public","Relation Name":"users","Plan Rows":5}}]`
	aud := `[{"Plan":{"Node Type":"Seq Scan","Schema":"audit","Relation Name":"users","Plan Rows":5}}]`
	fp := func(s string) string {
		root, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return Fingerprint(root)
	}
	if fp(pub) == fp(aud) {
		t.Error("same relation name in different schemas must have different fingerprints")
	}
}

// TestScanMapCapturesSelfJoin проверяет, что самосоединение (одна таблица,
// два алиаса) записывает оба скана, а не только первый.
func TestScanMapCapturesSelfJoin(t *testing.T) {
	before := `[{"Plan":{"Node Type":"Nested Loop","Plans":[
		{"Node Type":"Seq Scan","Schema":"public","Relation Name":"t","Alias":"a"},
		{"Node Type":"Index Scan","Schema":"public","Relation Name":"t","Alias":"b","Index Name":"t_pkey"}]}}]`
	root, err := Parse(before)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	scanMap(&root.Plan, m)
	if len(m) != 2 {
		t.Errorf("self-join should record 2 scans, got %d: %v", len(m), m)
	}
}

func TestRuleEngineNotEvaluated(t *testing.T) {
	// План только с оценками: правила, которым нужны реальные метрики, попадают
	// в NotEvaluated, чтобы "нет находок" не означало "не проверено".
	a := analyzeJSON(t, estimateOnlyPlan)
	if len(a.NotEvaluated) == 0 {
		t.Error("estimate-only plan should report not-evaluated rules")
	}
	found := false
	for _, ne := range a.NotEvaluated {
		if strings.Contains(ne, "seqscan-high-filter") && strings.Contains(ne, "EXPLAIN ANALYZE") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected seqscan-high-filter not-evaluated (needs ANALYZE); got %v", a.NotEvaluated)
	}
}

func TestRuleEngineStructuredFinding(t *testing.T) {
	root, _ := Parse(samplePlan)
	a := AnalyzeVersion(root, 150000)
	var seq *Finding
	for i := range a.Findings {
		if a.Findings[i].RuleID == "seqscan-high-filter" {
			seq = &a.Findings[i]
		}
	}
	if seq == nil {
		t.Fatalf("expected a seqscan-high-filter finding; got %+v", a.Findings)
	}
	if seq.Severity != Critical {
		t.Errorf("severity = %q, want CRITICAL (it dominates time)", seq.Severity)
	}
	if seq.Confidence <= 0 || seq.Hypothesis == "" || len(seq.Evidence) == 0 || len(seq.Actions) == 0 {
		t.Errorf("finding lacks structured fields: %+v", seq)
	}
}

func TestRuleEngineVersionGate(t *testing.T) {
	// hashagg-spill требует PG13+. На PG12 правило попадает в NotEvaluated.
	plan := `[{"Plan":{"Node Type":"Aggregate","Strategy":"Hashed","Actual Total Time":900,"Actual Rows":1000,"Actual Loops":1,"Plan Rows":1000,"Disk Usage":524288},"Execution Time":900}]`
	root, _ := Parse(plan)
	a := AnalyzeVersion(root, 120000) // PG12
	if hasIssue(a, "Hash Aggregate spilled") {
		t.Error("hashagg-spill must not fire on PG12")
	}
	gated := false
	for _, ne := range a.NotEvaluated {
		if strings.Contains(ne, "hashagg-spill") && strings.Contains(ne, "PostgreSQL 13") {
			gated = true
		}
	}
	if !gated {
		t.Errorf("expected hashagg-spill version-gated; got %v", a.NotEvaluated)
	}
	// На PG14 правило срабатывает.
	if a14 := AnalyzeVersion(root, 140000); !hasIssue(a14, "Hash Aggregate spilled") {
		t.Error("hashagg-spill should fire on PG14")
	}
}

func TestRuleEngineIOTiming(t *testing.T) {
	plan := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Actual Total Time":1000,"Actual Rows":10,"Actual Loops":1,"Plan Rows":10,"I/O Read Time":800},"Execution Time":1000}]`
	a := analyzeJSON(t, plan)
	if !hasIssue(a, "reading from disk") {
		t.Errorf("expected io-time-dominant finding; got %+v", a.Findings)
	}
}

func TestEstimateOnlyRiskUnknown(t *testing.T) {
	root, err := Parse(estimateOnlyPlan)
	if err != nil {
		t.Fatal(err)
	}
	a := Analyze(root)
	if a.Analyzed {
		t.Error("estimate-only plan must not be marked Analyzed")
	}
	if a.Risk != "unknown" {
		t.Errorf("estimate-only Risk = %q, want unknown", a.Risk)
	}
}

// Показательный план EXPLAIN ANALYZE: Sort со сбросом на диск над Seq Scan по
// events, который читает всю таблицу и фильтрует, с большим промахом в оценке
// числа строк.
const samplePlan = `[
  {
    "Plan": {
      "Node Type": "Sort",
      "Actual Total Time": 4800.0,
      "Actual Rows": 2104,
      "Actual Loops": 1,
      "Plan Rows": 2104,
      "Sort Method": "external merge",
      "Sort Space Used": 1258291,
      "Sort Space Type": "Disk",
      "Plans": [
        {
          "Node Type": "Seq Scan",
          "Relation Name": "events",
          "Actual Total Time": 3900.0,
          "Actual Rows": 2104,
          "Actual Loops": 1,
          "Plan Rows": 77000,
          "Rows Removed by Filter": 48208339,
          "Filter": "(event_type = 'purchase')"
        }
      ]
    },
    "Planning Time": 12.0,
    "Execution Time": 4820.0
  }
]`

func TestAnalyzeSamplePlan(t *testing.T) {
	root, err := Parse(samplePlan)
	if err != nil {
		t.Fatal(err)
	}
	a := Analyze(root)

	if !a.Analyzed {
		t.Error("expected analyzed plan")
	}
	if a.Risk != "high" {
		t.Errorf("risk = %q, want high", a.Risk)
	}
	if !strings.Contains(a.MainProblem, "events") {
		t.Errorf("main problem = %q, want it to mention events", a.MainProblem)
	}

	var hasCritical, hasSort, hasMisestimate bool
	for _, fd := range a.Findings {
		if fd.Severity == Critical {
			hasCritical = true
		}
		if strings.Contains(fd.Title, "Sort spilled") {
			hasSort = true
		}
		if strings.Contains(fd.Title, "estimated rows by") {
			hasMisestimate = true
		}
	}
	if !hasCritical {
		t.Error("expected a CRITICAL issue for the seq-scan filter")
	}
	if !hasSort {
		t.Error("expected a sort-spilled-to-disk issue")
	}
	if !hasMisestimate {
		t.Error("expected a row misestimate issue")
	}
}

// hasIssue сообщает, содержит ли заголовок какой-либо находки substr.
func hasIssue(a *Analysis, substr string) bool {
	for _, fd := range a.Findings {
		if strings.Contains(fd.Title, substr) {
			return true
		}
	}
	return false
}

func analyzeJSON(t *testing.T, j string) *Analysis {
	t.Helper()
	root, err := Parse(j)
	if err != nil {
		t.Fatal(err)
	}
	return Analyze(root)
}

func TestDetectHashAggSpill(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Aggregate","Strategy":"Hashed","Actual Total Time":900,"Actual Rows":1000,"Actual Loops":1,"Plan Rows":1000,"Disk Usage":524288,"Plans":[]},"Execution Time":900}]`)
	if !hasIssue(a, "Hash Aggregate spilled to disk") {
		t.Errorf("expected hash-agg spill issue: %+v", a.Findings)
	}
}

func TestDetectHeapFetches(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Index Only Scan","Index Name":"idx_x","Relation Name":"t","Actual Total Time":500,"Actual Rows":50000,"Actual Loops":1,"Plan Rows":50000,"Heap Fetches":48000},"Execution Time":500}]`)
	if !hasIssue(a, "heap fetches") {
		t.Errorf("expected heap-fetch issue: %+v", a.Findings)
	}
}

func TestDetectLateJoinFilter(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Nested Loop","Actual Total Time":1000,"Actual Rows":2000,"Actual Loops":1,"Plan Rows":2000,"Rows Removed by Join Filter":18000000,"Join Filter":"(a.x = b.y)","Plans":[]},"Execution Time":1000}]`)
	if !hasIssue(a, "join filter") {
		t.Errorf("expected late-join-filter issue: %+v", a.Findings)
	}
}

func TestDetectRootJITAndTriggers(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Actual Total Time":10,"Actual Rows":5,"Actual Loops":1,"Plan Rows":5},"Execution Time":10,"JIT":{"Timing":{"Total":40}},"Triggers":[{"Trigger Name":"fk","Time":5,"Calls":100}]}]`)
	if !hasIssue(a, "JIT spent") {
		t.Errorf("expected JIT issue: %+v", a.Findings)
	}
	if !hasIssue(a, "Triggers added") {
		t.Errorf("expected trigger issue: %+v", a.Findings)
	}
}

func TestDetectExpressionCast(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Actual Total Time":10,"Actual Rows":5,"Actual Loops":1,"Plan Rows":5,"Filter":"(lower(email) = 'a@b.c'::text)"},"Execution Time":10}]`)
	if !hasIssue(a, "expression/type-cast mismatch") {
		t.Errorf("expected expression/cast hint: %+v", a.Findings)
	}
}

func TestSummaryAggregates(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Actual Total Time":100,"Actual Rows":10,"Actual Loops":1,"Plan Rows":10,"Rows Removed by Filter":90,"Shared Read Blocks":131072,"Temp Written Blocks":65536},"Execution Time":100}]`)
	if a.RowsProcessed != 100 { // 10 оставлено + 90 отброшено
		t.Errorf("rows processed = %.0f, want 100", a.RowsProcessed)
	}
	if a.DiskReadMB < 1000 { // 131072 блоков * 8KB = 1 ГБ
		t.Errorf("disk read MB = %.0f, want ~1024", a.DiskReadMB)
	}
}

// BUFFERS накапливаются вверх по дереву: сводка равна значению корня,
// а не сумме корня и детей (иначе двойной учёт).
func TestSummaryNoDoubleCount(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Sort","Actual Total Time":50,"Actual Rows":1,"Actual Loops":1,"Plan Rows":1,"Temp Written Blocks":171,"Plans":[
		{"Node Type":"Function Scan","Actual Total Time":10,"Actual Rows":1,"Actual Loops":1,"Plan Rows":1,"Temp Written Blocks":171}]},"Execution Time":50}]`)
	wantMB := 171.0 * 8 / 1024
	if a.TempMB < wantMB-0.01 || a.TempMB > wantMB+0.01 {
		t.Errorf("temp MB = %.4f, want %.4f (root only, not doubled)", a.TempMB, wantMB)
	}
}

// Heap Fetches — суммарный итог, а не на цикл: не умножаем на число циклов.
func TestHeapFetchesNotMultiplied(t *testing.T) {
	a := analyzeJSON(t, `[{"Plan":{"Node Type":"Index Only Scan","Index Name":"i","Relation Name":"t","Actual Total Time":5,"Actual Rows":1,"Actual Loops":10,"Plan Rows":1,"Heap Fetches":5000},"Execution Time":50}]`)
	for _, fd := range a.Findings {
		if strings.Contains(fd.Title, "heap fetches") {
			joined := fd.Title + " " + strings.Join(fd.Evidence, " ")
			if !strings.Contains(joined, "5000") {
				t.Errorf("heap fetches should report 5000 (not 50000): %q", joined)
			}
		}
	}
}

func TestComparePlans(t *testing.T) {
	before, err := Parse(samplePlan)
	if err != nil {
		t.Fatal(err)
	}
	// "after": быстрый Index Scan по events, без сброса на диск и отбрасывания фильтром.
	const after = `[{"Plan":{"Node Type":"Index Scan","Index Name":"events_type_idx","Relation Name":"events","Actual Total Time":12.0,"Actual Rows":2104,"Actual Loops":1,"Plan Rows":2100},"Planning Time":1.0,"Execution Time":15.0}]`
	aft, err := Parse(after)
	if err != nil {
		t.Fatal(err)
	}
	c := Compare(before, aft)

	foundAccess := false
	for _, ch := range c.AccessChanges {
		if strings.Contains(ch, "events:") && strings.Contains(ch, "Seq Scan") && strings.Contains(ch, "Index Scan") {
			foundAccess = true
		}
	}
	if !foundAccess {
		t.Errorf("expected events Seq Scan → Index Scan, got %v", c.AccessChanges)
	}
	if len(c.Resolved) == 0 {
		t.Error("expected resolved issues (seq scan / sort spill)")
	}
	if len(c.Introduced) != 0 {
		t.Errorf("expected no introduced issues, got %v", c.Introduced)
	}
}

func TestAnalyzeEstimateOnly(t *testing.T) {
	// Обычный EXPLAIN (без ANALYZE) — нет реальных таймингов.
	const est = `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"t","Plan Rows":100,"Total Cost":1234.5},"Planning Time":1.0}]`
	root, err := Parse(est)
	if err != nil {
		t.Fatal(err)
	}
	a := Analyze(root)
	if a.Analyzed {
		t.Error("estimate-only plan should not be marked analyzed")
	}
	if !strings.Contains(a.MainProblem, "t") {
		t.Errorf("main problem = %q", a.MainProblem)
	}
}
