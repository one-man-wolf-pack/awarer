package doctor

import (
	"slices"
	"testing"
)

// mustRoot unwraps a root constructor the test expects to accept its arguments.
func mustRoot(r WatchedRoot, err error) WatchedRoot {
	if err != nil {
		panic(err)
	}
	return r
}

// TestWatchedRootStates: only coherent root states construct, and a known zero is an
// exact measurement while an unmeasured root has no figure at all.
func TestWatchedRootStates(t *testing.T) {
	empty := mustRoot(MeasuredWatchedRoot("build", 0))
	if b, ok := empty.ObservedBytes(); !ok || b != 0 || empty.Measurement() != CapacityExact {
		t.Errorf("empty root = %v %d %v, want exact 0", empty.Measurement(), b, ok)
	}
	partial := mustRoot(MeasuredWatchedRoot("dist", 9, GapUnreadableEntries, GapWorkBudget))
	if partial.Measurement() != CapacityPartial || !slices.Equal(partial.Gaps(), []CapacityGap{GapWorkBudget, GapUnreadableEntries}) {
		t.Errorf("partial root = %v %v, want partial with canonical gaps", partial.Measurement(), partial.Gaps())
	}
	unmeasured := mustRoot(UnmeasuredWatchedRoot("out", GapAggregateBudget))
	if _, ok := unmeasured.ObservedBytes(); ok || unmeasured.Measurement() != CapacityUnmeasured {
		t.Errorf("unmeasured root carries a figure: %v", unmeasured.Measurement())
	}
	for name, err := range map[string]error{
		"unavailable gap on a measured root": func() error { _, err := MeasuredWatchedRoot("a", 1, GapUnavailable); return err }(),
		"negative bytes":                     func() error { _, err := MeasuredWatchedRoot("a", -1); return err }(),
		"empty path":                         func() error { _, err := MeasuredWatchedRoot("", 1); return err }(),
		"work budget as unmeasured reason":   func() error { _, err := UnmeasuredWatchedRoot("a", GapWorkBudget); return err }(),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := MeasuredStoreCapacity(1, 1, GapAggregateBudget); err == nil {
		t.Error("the store measurement accepted aggregate-budget, which only the audit spends")
	}
}

// TestWatchedRootTallyBoundsDetail: the tally keeps at most the detail limit, largest
// first and unmeasured last, while counts, sums, and the threshold count cover every
// added root and the omitted count is exact.
func TestWatchedRootTallyBoundsDetail(t *testing.T) {
	tally, err := NewWatchedRootTally([]string{"build"}, 50, 2)
	if err != nil {
		t.Fatal(err)
	}
	tally.Add(mustRoot(UnmeasuredWatchedRoot("u", GapUnavailable)))
	tally.Add(mustRoot(MeasuredWatchedRoot("small", 10)))
	tally.Add(mustRoot(MeasuredWatchedRoot("big", 70)))
	tally.Add(mustRoot(MeasuredWatchedRoot("lower-bound", 60, GapWorkBudget)))
	tally.Add(mustRoot(MeasuredWatchedRoot("under", 40, GapWorkBudget)))
	a := tally.Audit()

	var paths []string
	for _, r := range a.Detail() {
		paths = append(paths, r.Path())
	}
	if !slices.Equal(paths, []string{"big", "lower-bound"}) || a.Omitted() != 3 || a.Found() != 5 {
		t.Fatalf("detail = %v omitted %d found %d, want [big lower-bound], 3, 5", paths, a.Omitted(), a.Found())
	}
	if exact, partial, unmeasured := a.Counts(); exact != 2 || partial != 2 || unmeasured != 1 {
		t.Errorf("counts = %d/%d/%d, want 2/2/1", exact, partial, unmeasured)
	}
	if a.ObservedBytes() != 180 || a.ObservedBytesComplete() {
		t.Errorf("bytes = %d complete %v, want a 180-byte lower bound", a.ObservedBytes(), a.ObservedBytesComplete())
	}
	if a.Reaching() != 2 {
		t.Errorf("reaching = %d, want 2 (the exact 70 and the lower bound 60; not the partial 40)", a.Reaching())
	}
	if total, known := a.Total(); !known || total != 5 {
		t.Errorf("total = %d known %v, want 5 from a complete search", total, known)
	}
	if err := tally.SearchIncomplete(GapWorkBudget); err != nil {
		t.Fatal(err)
	}
	if err := tally.SearchIncomplete(GapDepthLimit); err == nil {
		t.Error("depth-limit is not a search gap")
	}
	if _, known := tally.Audit().Total(); known {
		t.Error("an incomplete search must not know its total")
	}
	dup, err := NewWatchedRootTally([]string{"build", "dist", "build"}, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := dup.Audit().Selectors(); !slices.Equal(got, []string{"build", "dist"}) {
		t.Errorf("selectors = %v, want a repeated selector listed once", got)
	}
	if (WatchedRootAudit{}).Requested() {
		t.Error("the zero audit must be unrequested")
	}
}
