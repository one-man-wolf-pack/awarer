package runcache_test

import (
	"testing"

	"awarer/internal/domain/runcache"
)

// TestOverBudgetReportCarriesTheFact proves a well-formed over-budget fact is stored
// and readable, and that it is mutually exclusive with the largest-root fact.
func TestOverBudgetReportCarriesTheFact(t *testing.T) {
	r := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000})
	fact, ok := r.OverBudget()
	if !ok {
		t.Fatal("expected an over-budget fact")
	}
	if fact.Path != "target" || fact.Limit != 100_000 {
		t.Errorf("over-budget fact = %+v, want {target 100000}", fact)
	}
	if _, ok := r.LargestRoot(); ok {
		t.Error("an over-budget report must not also carry a largest-root fact")
	}
}

// TestLargestRootReportCarriesTheFact is the mirror for the success-path fact.
func TestLargestRootReportCarriesTheFact(t *testing.T) {
	r := runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: 4200})
	fact, ok := r.LargestRoot()
	if !ok {
		t.Fatal("expected a largest-root fact")
	}
	if fact.Path != "node_modules" || fact.Entries != 4200 {
		t.Errorf("largest-root fact = %+v, want {node_modules 4200}", fact)
	}
	if _, ok := r.OverBudget(); ok {
		t.Error("a largest-root report must not also carry an over-budget fact")
	}
}

// TestDegenerateFactsCollapseToEmpty proves the diagnostic side channel is fail-soft:
// a fact that names nothing worth a hint (empty path, non-positive limit/count)
// collapses to the empty report instead of being stored, so a stored fact is always
// meaningful and a bad hint is dropped rather than emitted or turned into an error.
func TestDegenerateFactsCollapseToEmpty(t *testing.T) {
	degenerate := []runcache.EffectReport{
		runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "", Limit: 100_000}),
		runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 0}),
		runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: -1}),
		runcache.LargestRootReport(runcache.LargestRootFact{Path: "", Entries: 4200}),
		runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: 0}),
		runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: -1}),
	}
	for i, r := range degenerate {
		if _, ok := r.OverBudget(); ok {
			t.Errorf("case %d: degenerate fact must not be stored as over-budget", i)
		}
		if _, ok := r.LargestRoot(); ok {
			t.Errorf("case %d: degenerate fact must not be stored as largest-root", i)
		}
	}
}

// TestWithDurationLeavesFactsUntouched proves duration stamping is orthogonal to the
// evidence fact — the two-phase build (producer sets the fact, clock owner stamps the
// duration) composes without disturbing the fact.
func TestWithDurationLeavesFactsUntouched(t *testing.T) {
	r := runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: 4200}).WithDuration(6800)
	if r.DurationMs() != 6800 {
		t.Errorf("DurationMs = %d, want 6800", r.DurationMs())
	}
	fact, ok := r.LargestRoot()
	if !ok || fact.Entries != 4200 {
		t.Errorf("largest-root fact lost after WithDuration: ok=%v fact=%+v", ok, fact)
	}
}

// TestWithDurationClampsNegativeToZero proves the diagnostic stays fail-soft: a
// negative measured duration (a meaningless latency reading) clamps to zero rather
// than being stored, so no downstream consumer sees a negative wall-clock time.
func TestWithDurationClampsNegativeToZero(t *testing.T) {
	r := runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: 4200}).WithDuration(-5)
	if r.DurationMs() != 0 {
		t.Errorf("DurationMs = %d, want 0 (negative clamps to zero)", r.DurationMs())
	}
}
