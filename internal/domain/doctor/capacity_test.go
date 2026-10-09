package doctor

import (
	"slices"
	"testing"
)

// TestStoreCapacityStatesStayDistinct pins the four measurement states: a known zero
// is exact, partial carries its gaps in canonical order, unmeasured and disabled have
// no observed sum, and only an enabled advisory has a threshold.
func TestStoreCapacityStatesStayDistinct(t *testing.T) {
	zero, err := MeasuredStoreCapacity(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok := zero.ObservedBytes(); !ok || b != 0 || zero.Measurement() != CapacityExact || len(zero.Gaps()) != 0 {
		t.Errorf("known zero = %v %d %v %v, want exact 0 without gaps", zero.Measurement(), b, ok, zero.Gaps())
	}

	partial, err := MeasuredStoreCapacity(40, 100, GapUnreadableEntries, GapWorkBudget, GapWorkBudget)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Measurement() != CapacityPartial || !slices.Equal(partial.Gaps(), []CapacityGap{GapWorkBudget, GapUnreadableEntries}) {
		t.Errorf("partial = %v %v, want partial with canonical, deduplicated gaps", partial.Measurement(), partial.Gaps())
	}

	unmeasured, err := UnmeasuredStoreCapacity(100)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := unmeasured.ObservedBytes(); ok || unmeasured.Measurement() != CapacityUnmeasured ||
		!slices.Equal(unmeasured.Gaps(), []CapacityGap{GapUnavailable}) {
		t.Errorf("unmeasured = %v %v, want no sum and the unavailable gap", unmeasured.Measurement(), unmeasured.Gaps())
	}
	if th, ok := unmeasured.Threshold(); !ok || th != 100 {
		t.Errorf("unmeasured threshold = %d %v, want 100", th, ok)
	}

	off := DisabledStoreCapacity()
	if _, ok := off.ObservedBytes(); ok {
		t.Error("disabled must carry no observed sum")
	}
	if _, ok := off.Threshold(); ok || off.Measurement() != CapacityDisabled || off.AtOrAboveThreshold() {
		t.Error("disabled must carry no threshold and never cross it")
	}

	for name, bad := range map[string]func() error{
		"zero threshold":       func() error { _, err := MeasuredStoreCapacity(1, 0); return err },
		"negative bytes":       func() error { _, err := MeasuredStoreCapacity(-1, 1); return err },
		"unavailable measured": func() error { _, err := MeasuredStoreCapacity(1, 1, GapUnavailable); return err },
		"unknown gap":          func() error { _, err := MeasuredStoreCapacity(1, 1, "guess"); return err },
		"unmeasured no limit":  func() error { _, err := UnmeasuredStoreCapacity(0); return err },
	} {
		if bad() == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

// TestStoreCapacityThresholdCrossing: reaching the threshold counts, an observed
// partial sum that reaches it is enough, and one below it establishes nothing.
func TestStoreCapacityThresholdCrossing(t *testing.T) {
	cases := []struct {
		observed int64
		gaps     []CapacityGap
		want     bool
	}{
		{99, nil, false},
		{100, nil, true},
		{101, nil, true},
		{100, []CapacityGap{GapWorkBudget}, true},
		{99, []CapacityGap{GapWorkBudget}, false},
	}
	for _, c := range cases {
		m, err := MeasuredStoreCapacity(c.observed, 100, c.gaps...)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.AtOrAboveThreshold(); got != c.want {
			t.Errorf("observed %d gaps %v: AtOrAboveThreshold = %v, want %v", c.observed, c.gaps, got, c.want)
		}
	}
	if u, _ := UnmeasuredStoreCapacity(1); u.AtOrAboveThreshold() {
		t.Error("an unmeasured store never crosses the threshold")
	}
}
