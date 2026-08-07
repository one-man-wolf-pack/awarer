package run

import (
	"testing"

	"awarer/internal/domain/runcache"
)

// TestEffectDiagnosisForIsGatedAndSourcedFromTheReport pins the one rule that turns a
// mismatch reason plus an already-produced observation report into the review-facing
// diagnosis. It covers the over-budget root — a watched directory too large to observe
// with fidelity — which is the honest root source on the fail-closed path and is not
// reproducible through the filesystem without building a tree past the entry budget.
func TestEffectDiagnosisForIsGatedAndSourcedFromTheReport(t *testing.T) {
	largest := runcache.LargestRootReport(runcache.LargestRootFact{Path: "target", Entries: 1200})
	overBudget := runcache.OverBudgetReport(runcache.OverBudgetFact{Path: "node_modules", Limit: 500})

	for _, tc := range []struct {
		name     string
		reason   runcache.MismatchReason
		report   runcache.EffectReport
		wantDiag bool
		wantRoot string
	}{
		{"differs with a fully observed root", runcache.ReasonEffectStateDiffers, largest, true, "target"},
		{"differs with an over-budget root", runcache.ReasonEffectStateDiffers, overBudget, true, "node_modules"},
		{"differs with no named root", runcache.ReasonEffectStateDiffers, runcache.EffectReport{}, true, ""},
		{"unavailable with an over-budget root", runcache.ReasonEffectStateUnavailable, overBudget, true, "node_modules"},
		{"unavailable with no named root", runcache.ReasonEffectStateUnavailable, runcache.EffectReport{}, true, ""},
		{"input tree differs", runcache.ReasonInputTreeDiffers, largest, false, ""},
		{"record only", runcache.ReasonRecordOnly, largest, false, ""},
		{"mutated state", runcache.ReasonMutatedState, largest, false, ""},
		{"expired", runcache.ReasonExpired, largest, false, ""},
		{"no reason at all", "", largest, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := effectDiagnosisFor(tc.reason, tc.report)
			if !tc.wantDiag {
				if got != nil {
					t.Fatalf("reason %q produced a diagnosis %+v, want none", tc.reason, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("reason %q produced no diagnosis", tc.reason)
			}
			if got.Reason != tc.reason {
				t.Errorf("diagnosis reason = %q, want %q", got.Reason, tc.reason)
			}
			if got.Root != tc.wantRoot {
				t.Errorf("diagnosis root = %q, want %q", got.Root, tc.wantRoot)
			}
		})
	}
}

// TestAttachEffectPerformsNoObservation pins that enriching a candidate is a pure
// projection of a value the caller already holds: the method reads the report handed to
// it and nothing else, so it cannot be the place a hidden scan appears. A candidate
// whose reason is not an effect one keeps an absent diagnosis even when the report names
// a root.
func TestAttachEffectPerformsNoObservation(t *testing.T) {
	report := runcache.LargestRootReport(runcache.LargestRootFact{Path: "dist", Entries: 42})

	effect := RunReuseCandidate{Reason: runcache.ReasonEffectStateDiffers}
	effect.attachEffect(report, reasonFromComparison)
	if effect.Effect == nil || effect.Effect.Root != "dist" {
		t.Fatalf("effect candidate diagnosis = %+v, want root %q", effect.Effect, "dist")
	}

	other := RunReuseCandidate{Reason: runcache.ReasonConfigMismatch}
	other.attachEffect(report, reasonFromComparison)
	if other.Effect != nil {
		t.Fatalf("non-effect candidate diagnosis = %+v, want none", other.Effect)
	}
}

// TestAttachEffectDropsRootForAStoredVerdict pins the provenance rule at its one owner:
// the same reason and the same report yield a root under a comparison-derived reason and
// no root under a stored historical verdict, while the diagnosis itself — reason, and so
// the sample fact and typed actions projected from it — survives both ways.
func TestAttachEffectDropsRootForAStoredVerdict(t *testing.T) {
	report := runcache.LargestRootReport(runcache.LargestRootFact{Path: "node_modules", Entries: 900})

	for _, reason := range []runcache.MismatchReason{
		runcache.ReasonEffectStateDiffers,
		runcache.ReasonEffectStateUnavailable,
	} {
		compared := RunReuseCandidate{Reason: reason}
		compared.attachEffect(report, reasonFromComparison)
		if compared.Effect == nil || compared.Effect.Root != "node_modules" {
			t.Errorf("%s comparison-derived: diagnosis = %+v, want root %q", reason, compared.Effect, "node_modules")
		}

		stored := RunReuseCandidate{Reason: reason}
		stored.attachEffect(report, reasonFromStoredVerdict)
		if stored.Effect == nil {
			t.Fatalf("%s stored verdict: diagnosis dropped entirely, want it kept without a root", reason)
		}
		if stored.Effect.Reason != reason {
			t.Errorf("%s stored verdict: diagnosis reason = %q, want the candidate's own", reason, stored.Effect.Reason)
		}
		if stored.Effect.Root != "" {
			t.Errorf("%s stored verdict: root = %q, want it omitted — today's dominant root is not evidence about a past run", reason, stored.Effect.Root)
		}
	}
}
