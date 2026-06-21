package migration

import (
	"reflect"
	"testing"
)

func TestPlanRolloutCanaryBatches(t *testing.T) {
	shards := []string{"s0", "s1", "s2", "s3", "s4"}
	p := PlanRollout(shards, nil, false, true, 2)
	want := [][]string{{"s0"}, {"s1", "s2"}, {"s3", "s4"}}
	if !reflect.DeepEqual(p.Stages, want) {
		t.Errorf("stages = %v, want %v", p.Stages, want)
	}
	if len(p.Pending) != 5 || len(p.Skipped) != 0 {
		t.Errorf("pending/skipped = %d/%d, want 5/0", len(p.Pending), len(p.Skipped))
	}
}

func TestPlanRolloutResumeSkipsApplied(t *testing.T) {
	shards := []string{"s0", "s1", "s2", "s3"}
	applied := map[string]bool{"s0": true, "s1": true}
	p := PlanRollout(shards, applied, true, false, 0)
	if !reflect.DeepEqual(p.Pending, []string{"s2", "s3"}) {
		t.Errorf("pending = %v, want [s2 s3]", p.Pending)
	}
	if !reflect.DeepEqual(p.Skipped, []string{"s0", "s1"}) {
		t.Errorf("skipped = %v, want [s0 s1]", p.Skipped)
	}
	// batchSize 0 → один этап со всеми оставшимися.
	if !reflect.DeepEqual(p.Stages, [][]string{{"s2", "s3"}}) {
		t.Errorf("stages = %v, want [[s2 s3]]", p.Stages)
	}
}

func TestPlanRolloutResumeDisabledIgnoresApplied(t *testing.T) {
	shards := []string{"s0", "s1"}
	applied := map[string]bool{"s0": true}
	p := PlanRollout(shards, applied, false, false, 0)
	if len(p.Pending) != 2 || len(p.Skipped) != 0 {
		t.Errorf("without resume all shards are pending, got pending=%d skipped=%d", len(p.Pending), len(p.Skipped))
	}
}

func TestPlanRolloutCanaryOnly(t *testing.T) {
	p := PlanRollout([]string{"s0", "s1", "s2"}, nil, false, true, 0)
	want := [][]string{{"s0"}, {"s1", "s2"}}
	if !reflect.DeepEqual(p.Stages, want) {
		t.Errorf("canary-only stages = %v, want %v", p.Stages, want)
	}
}

func TestRolloutReportSummary(t *testing.T) {
	rep := RolloutReport{Shards: []ShardOutcome{
		{Status: "applied"}, {Status: "verified"}, {Status: "failed"}, {Status: "pending"}, {Status: "skipped"},
	}}
	a, f, p, s := rep.Summary()
	if a != 2 || f != 1 || p != 1 || s != 1 {
		t.Errorf("summary = applied %d failed %d pending %d skipped %d, want 2/1/1/1", a, f, p, s)
	}
}

func TestAppliedSetFromShards(t *testing.T) {
	set := AppliedSetFromShards(map[string]string{"b": "t1", "a": "t2"})
	if !set["a"] || !set["b"] {
		t.Error("applied set should contain a and b")
	}
	if len(set) != 2 {
		t.Errorf("applied set size = %d, want 2", len(set))
	}
}
