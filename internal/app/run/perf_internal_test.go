package run

import (
	"testing"

	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
)

func TestEffectDiagnosticsOverBudgetIsDurationIndependent(t *testing.T) {
	// A budget overrun fails the observation closed and is always worth naming, even
	// when the walk-until-fail was fast, because it defeated reuse and cost the budget.
	// well below the interactive threshold — a budget overrun is named regardless.
	report := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000}).WithDuration(5)
	diags := effectDiagnostics(report)
	if len(diags) != 1 {
		t.Fatalf("len(diags) = %d, want 1", len(diags))
	}
	d := diags[0]
	if d.Cause() != perfdiag.CauseLargeEffectRoot {
		t.Errorf("cause = %s, want large-effect-root", d.Cause())
	}
	th, ok := d.Evidence().Threshold()
	if !ok || th != 100_000 {
		t.Errorf("threshold evidence = (%d, %v), want (100000, true)", th, ok)
	}
	if _, ok := d.Evidence().ExactCount(); ok {
		t.Error("a fail-closed over-budget root must not carry an exact count")
	}
	if h, ok := d.Hint(); !ok || h.Kind() != perfdiag.LargeEffectRootHintKind {
		t.Errorf("expected a record-mode hint, got ok=%v", ok)
	}
}

func TestEffectDiagnosticsLargeRootNeedsBothLargeAndSlow(t *testing.T) {
	big := perfdiag.LargeRootEntriesThreshold + 1
	small := perfdiag.LargeRootEntriesThreshold - 1
	slow := perfdiag.InteractiveThresholdMs
	fast := perfdiag.InteractiveThresholdMs - 1

	cases := []struct {
		name    string
		report  runcache.EffectReport
		wantLen int
	}{
		{"large-and-slow", runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: big}).WithDuration(slow), 1},
		{"large-but-fast", runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: big}).WithDuration(fast), 0},
		{"slow-but-small", runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: small}).WithDuration(slow), 0},
		{"empty", runcache.EffectReport{}.WithDuration(slow), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := effectDiagnostics(tc.report)
			if len(diags) != tc.wantLen {
				t.Fatalf("len(diags) = %d, want %d", len(diags), tc.wantLen)
			}
			if tc.wantLen == 1 {
				if c, ok := diags[0].Evidence().ExactCount(); !ok || c != big {
					t.Errorf("exact count = (%d, %v), want (%d, true)", c, ok, big)
				}
			}
		})
	}
}

func TestEffectDiagnosticsYieldsAtMostOne(t *testing.T) {
	// One report yields at most one diagnostic; an empty report yields none.
	report := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000}).WithDuration(6800)
	if diags := effectDiagnostics(report); len(diags) != 1 {
		t.Fatalf("len(diags) = %d, want 1", len(diags))
	}
	if diags := effectDiagnostics(runcache.EffectReport{}.WithDuration(6800)); len(diags) != 0 {
		t.Fatalf("empty report len(diags) = %d, want 0", len(diags))
	}
}

func TestInputDiagnosticThresholdBoundary(t *testing.T) {
	// The boundary is inclusive: the threshold names the duration at which a scan is
	// worth explaining, so the scan that reaches it is explained.
	for _, c := range []struct {
		name string
		ms   int64
		want bool
	}{
		{"one millisecond short stays silent", perfdiag.InteractiveThresholdMs - 1, false},
		{"exactly at the threshold explains itself", perfdiag.InteractiveThresholdMs, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, ok := inputDiagnostic(newInputReport(c.ms, 1200))
			if ok != c.want {
				t.Fatalf("inputDiagnostic(%dms) ok = %v, want %v", c.ms, ok, c.want)
			}
			if !ok {
				return
			}
			if d.Cause() != perfdiag.CauseFullInputRehash {
				t.Errorf("cause = %s, want full-input-rehash", d.Cause())
			}
			if d.Stage() != perfdiag.StageRunInputObservation {
				t.Errorf("stage = %s, want run.input-observation", d.Stage())
			}
			if d.DurationMs() != c.ms {
				t.Errorf("duration = %d, want %d", d.DurationMs(), c.ms)
			}
			count, hasCount := d.Evidence().ExactCount()
			if !hasCount || count != 1200 {
				t.Errorf("evidence count = (%d, %v), want (1200, true)", count, hasCount)
			}
			if p := d.Evidence().Path(); p != "." {
				t.Errorf("evidence path = %q, want the project root", p)
			}
			h, hasHint := d.Hint()
			if !hasHint || h.Kind() != perfdiag.ReviewRunScopeHintKind {
				t.Errorf("hint = (%v, %v), want a review-run-scope hint", h.Kind(), hasHint)
			}
		})
	}
}

func TestInputReportClampsABackwardsClock(t *testing.T) {
	// A clock that went backwards is a broken measurement, not a negative duration to
	// propagate into a diagnostic that would then render as "took -3s".
	if got := newInputReport(-3000, 10).durationMs; got != 0 {
		t.Errorf("durationMs = %d, want 0", got)
	}
}

func TestRunPerformanceBoundsAllCausesTogether(t *testing.T) {
	// Three separate causes are available at once — two watched roots and the input
	// rehash — and the invocation still gets two notes, ranked by what actually cost
	// the most. Adding a cause must not widen what a command prints; the new note has
	// to compete with the existing ones rather than sit beside them.
	slow := perfdiag.InteractiveThresholdMs + 500
	first := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000}).WithDuration(slow)
	second := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "dist", Limit: 100_000}).WithDuration(slow + 1)

	diags := runPerformance([]inputReport{newInputReport(slow+2, 900)}, first, second)
	if len(diags) != maxPerfDiagnostics {
		t.Fatalf("len(diags) = %d, want the per-invocation bound of %d", len(diags), maxPerfDiagnostics)
	}
	// Slowest first, so the input rehash leads and one effect note is dropped.
	if diags[0].Cause() != perfdiag.CauseFullInputRehash {
		t.Errorf("first note cause = %s, want the slowest (full-input-rehash)", diags[0].Cause())
	}
}

func TestRunPerformanceIsSilentForAFastScan(t *testing.T) {
	if diags := runPerformance([]inputReport{newInputReport(perfdiag.InteractiveThresholdMs-1, 50_000)}); len(diags) != 0 {
		t.Errorf("a fast scan produced %d notes, want silence whatever the file count", len(diags))
	}
}

func TestRunPerformanceKeepsTheSlowerOfTwoInputPasses(t *testing.T) {
	// A miss measures the same stage twice. Both crossing the threshold must still
	// produce one note — the stage was slow once, not twice — and it must carry the
	// worse pass, which is usually the one after the command, since a command that
	// generates files leaves the second scan more to read.
	fast := perfdiag.InteractiveThresholdMs
	slow := perfdiag.InteractiveThresholdMs * 3

	diags := runPerformance([]inputReport{newInputReport(fast, 100), newInputReport(slow, 900)})
	if len(diags) != 1 {
		t.Fatalf("len(diags) = %d, want 1: two passes of one stage are one note", len(diags))
	}
	if diags[0].DurationMs() != slow {
		t.Errorf("duration = %d, want the slower pass %d", diags[0].DurationMs(), slow)
	}
	if count, _ := diags[0].Evidence().ExactCount(); count != 900 {
		t.Errorf("file count = %d, want %d — the evidence must belong to the pass it reports", count, 900)
	}
}

func TestRunPerformanceNeverEvictsASlowInputScan(t *testing.T) {
	// Two watched roots, both slower than the input scan, and only two seats. Ranking
	// the union purely by duration would drop the input note — and it is the one a user
	// cannot otherwise explain, since a slow content rehash leaves no trace in the
	// command's own output while a huge target/ announces itself.
	input := perfdiag.InteractiveThresholdMs
	first := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000}).WithDuration(input * 10)
	second := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "dist", Limit: 100_000}).WithDuration(input * 20)

	diags := runPerformance([]inputReport{newInputReport(input, 700)}, first, second)
	if len(diags) != maxPerfDiagnostics {
		t.Fatalf("len(diags) = %d, want %d", len(diags), maxPerfDiagnostics)
	}
	var sawInput bool
	for _, d := range diags {
		if d.Cause() == perfdiag.CauseFullInputRehash {
			sawInput = true
		}
	}
	if !sawInput {
		t.Error("the input note was evicted by slower effect notes; a scan past the threshold must always be exposed")
	}
	// The seat is reserved, not promoted: presentation stays slowest-first.
	if diags[0].DurationMs() < diags[len(diags)-1].DurationMs() {
		t.Errorf("notes are not ordered slowest-first: %d then %d", diags[0].DurationMs(), diags[len(diags)-1].DurationMs())
	}
}

func TestRunPerformanceReservesNothingForAQuietScan(t *testing.T) {
	// A scan below the threshold must not hold a seat open, or a surface with two real
	// effect notes would print one.
	fast := perfdiag.InteractiveThresholdMs - 1
	slow := perfdiag.InteractiveThresholdMs * 2
	first := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "target", Limit: 100_000}).WithDuration(slow)
	second := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "dist", Limit: 100_000}).WithDuration(slow)

	diags := runPerformance([]inputReport{newInputReport(fast, 700)}, first, second)
	if len(diags) != maxPerfDiagnostics {
		t.Fatalf("len(diags) = %d, want the full %d effect notes", len(diags), maxPerfDiagnostics)
	}
	for _, d := range diags {
		if d.Cause() == perfdiag.CauseFullInputRehash {
			t.Error("a scan below the threshold must produce no note at all")
		}
	}
}
