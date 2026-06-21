package cluster

import (
	"testing"

	"terox/internal/config"
)

func mkShards(n int) []Shard {
	s, _ := Expand(&config.Storage{
		HostTemplate: "pg-rs{p1:03}.db", DBTemplate: "master", Count: n, Port: 5432,
	})
	return s
}

func TestParseSelector(t *testing.T) {
	shards := mkShards(16)

	cases := []struct {
		sel       string
		wantCount int
		wantLabel string
	}{
		{"", 16, "all"},
		{"all", 16, "all"},
		{"rs005", 1, "rs005"},
		{"0", 1, "0"},
		{"0,1,2", 3, "0-2"},
		{"0,1,3..7,10", 8, "0-1,3-7,10"},
		{"5..5", 1, "5"},
		{"10,2,2,3", 3, "2-3,10"},
	}
	for _, tc := range cases {
		t.Run(tc.sel, func(t *testing.T) {
			got, label, err := ParseSelector(shards, tc.sel)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantCount {
				t.Errorf("count = %d, want %d", len(got), tc.wantCount)
			}
			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

func TestParseSelectorNumericLabel(t *testing.T) {
	// Хост вида "pg-rs-001" (дефис перед числом) даёт чисто числовые
	// метки "001","002",.... Ввод метки "001" выбирает этот шард
	// (позиция 0), а не индекс 1.
	shards, _ := Expand(&config.Storage{
		HostTemplate: "pg-rs-{p1:03}.db", DBTemplate: "master", Count: 4, Port: 5432,
	})
	if shards[0].Label != "001" {
		t.Fatalf("precondition: label[0]=%q want 001", shards[0].Label)
	}
	got, label, err := ParseSelector(shards, "001")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Position != 0 || label != "001" {
		t.Errorf("ParseSelector(\"001\") = pos %v label %q, want pos [0] label 001", posList(got), label)
	}
}

func posList(s []Shard) []int {
	out := make([]int, len(s))
	for i, sh := range s {
		out[i] = sh.Position
	}
	return out
}

func TestParseSelectorErrors(t *testing.T) {
	shards := mkShards(8)
	for _, sel := range []string{"99", "3..1", "0..100", "abc,1"} {
		if _, _, err := ParseSelector(shards, sel); err == nil {
			t.Errorf("expected error for %q", sel)
		}
	}
}
