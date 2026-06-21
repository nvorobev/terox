package catalog

import "testing"

func TestStatusString(t *testing.T) {
	cases := map[Status]string{
		StatusPending:   "pending",
		StatusLoaded:    "loaded",
		StatusPartial:   "partial",
		StatusForbidden: "forbidden",
		StatusTimeout:   "timeout",
		StatusFailed:    "failed",
		Status(99):      "pending", // неизвестное значение → pending
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", int(st), got, want)
		}
	}
}

func TestLoadStateCoverage(t *testing.T) {
	if got := (LoadState{ShardsOK: 17, ShardsN: 32}).Coverage(); got != "17/32" {
		t.Errorf("Coverage() = %q, want %q", got, "17/32")
	}
	if got := (LoadState{ShardsOK: 0, ShardsN: 0}).Coverage(); got != "" {
		t.Errorf("Coverage() with ShardsN=0 = %q, want empty", got)
	}
	if got := (LoadState{ShardsOK: 3, ShardsN: 3}).Coverage(); got != "3/3" {
		t.Errorf("Coverage() = %q, want %q", got, "3/3")
	}
}

func TestLoadStateDegraded(t *testing.T) {
	cases := map[Status]bool{
		StatusPending:   false,
		StatusLoaded:    false,
		StatusPartial:   true,
		StatusForbidden: true,
		StatusTimeout:   true,
		StatusFailed:    true,
	}
	for st, want := range cases {
		if got := (LoadState{Status: st}).Degraded(); got != want {
			t.Errorf("LoadState{Status: %s}.Degraded() = %v, want %v", st, got, want)
		}
	}
}
