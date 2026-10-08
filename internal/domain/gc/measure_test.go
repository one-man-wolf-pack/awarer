package gc

import (
	"errors"
	"testing"
)

func mustCandidate(t *testing.T, kind CandidateKind, reason RetentionReason, id string, size ByteMeasure) GCCandidate {
	t.Helper()
	c, err := NewCandidate(kind, reason, id, ".awa/"+id, "msg", size)
	if err != nil {
		t.Fatalf("NewCandidate: %v", err)
	}
	return c
}

func TestByteMeasureKeepsZeroUnmeasuredAndPartialDistinct(t *testing.T) {
	for _, tc := range []struct {
		m     ByteMeasure
		token string
		exact bool
	}{
		{ExactBytes(0), "exact", true},
		{UnmeasuredBytes(), "unmeasured", false},
		{PartialBytes(0), "partial", false},
	} {
		if got := tc.m.Measurement().String(); got != tc.token || tc.m.Exact() != tc.exact || tc.m.Bytes() != 0 {
			t.Errorf("%+v: token %q exact %v, want %q %v", tc.m, got, tc.m.Exact(), tc.token, tc.exact)
		}
	}
	if _, err := NewCandidate(KindRun, ReasonRunExpired, "r1", ".awa/r1", "x", ByteMeasure{state: Measurement(9)}); err == nil {
		t.Error("an undefined measurement must be rejected")
	}
}

// TestPlanCompletenessFoldsOnlySelectedBoundaries: retained, skipped, and blocked
// candidates are unmeasured by design and never make the selected total a lower
// bound; a partial or unmeasured delete candidate does, and contributes only what it
// established.
func TestPlanCompletenessFoldsOnlySelectedBoundaries(t *testing.T) {
	exactOnly := NewPlan(true, []GCCandidate{
		mustCandidate(t, KindCheckpoint, ReasonCheckpointExpired, "s1", ExactBytes(100)),
		mustCandidate(t, KindTemp, ReasonTempStale, "", ExactBytes(0)),
		mustCandidate(t, KindCheckpoint, ReasonCheckpointLatest, "s2", UnmeasuredBytes()),
		mustCandidate(t, KindRun, ReasonSubsystemFiltered, "r1", UnmeasuredBytes()),
	}, nil)
	if s := exactOnly.Summary(); s.BytesPlanned != 100 || !s.BytesPlannedComplete() {
		t.Fatalf("summary = %+v, want exact 100", s)
	}

	lowerBound := NewPlan(true, []GCCandidate{
		mustCandidate(t, KindCheckpoint, ReasonCheckpointExpired, "s1", ExactBytes(100)),
		mustCandidate(t, KindRun, ReasonRunExpired, "r1", PartialBytes(40)),
		mustCandidate(t, KindRestore, ReasonRestoreExpired, "o1", UnmeasuredBytes()),
	}, nil)
	s := lowerBound.Summary()
	if s.BytesPlanned != 140 || s.PlannedNotFullyMeasured != 2 || s.BytesPlannedComplete() {
		t.Fatalf("summary = %+v, want lower bound 140 with 2 incomplete", s)
	}
}

// TestExecutionCreditsOnlySuccessfulMeasuredDeletions: a success credits its planned
// measure (partial stays partial), a failure credits zero and makes the total a lower
// bound however many bytes it planned, and the plan's own completeness is not used.
func TestExecutionCreditsOnlySuccessfulMeasuredDeletions(t *testing.T) {
	exact := mustCandidate(t, KindCheckpoint, ReasonCheckpointExpired, "s1", ExactBytes(100))
	partial := mustCandidate(t, KindRun, ReasonRunExpired, "r1", PartialBytes(40))
	failing := mustCandidate(t, KindRun, ReasonRunExpired, "r2", ExactBytes(5000))
	plan := NewPlan(false, []GCCandidate{exact, partial, failing}, nil)

	all := NewResult(plan, []DeletionResult{NewDeletionResult(exact, nil)}, nil).ExecSummary()
	if all.BytesDeleted != 100 || !all.BytesDeletedComplete() {
		t.Fatalf("exact-only execution = %+v, want exact 100", all)
	}

	mixed := NewResult(plan, []DeletionResult{
		NewDeletionResult(exact, nil),
		NewDeletionResult(partial, nil),
		NewDeletionResult(failing, errors.New("sync failed after unlink")),
	}, nil).ExecSummary()
	if mixed.BytesDeleted != 140 || mixed.Failed != 1 || mixed.DeletedNotFullyMeasured != 1 || mixed.BytesDeletedComplete() {
		t.Fatalf("mixed execution = %+v, want 140 lower bound, 1 failed, 1 not fully measured", mixed)
	}
	if c := NewDeletionResult(failing, errors.New("x")).Credited(); c.Bytes() != 0 || c.Exact() {
		t.Fatalf("a failed deletion credited %+v, want unmeasured zero", c)
	}
}

// TestNoExecutionClaimsNoBytes: a dry run or a suppressed run attempted nothing, so
// it reports no deleted bytes whatever the plan selected.
func TestNoExecutionClaimsNoBytes(t *testing.T) {
	c := mustCandidate(t, KindBlob, ReasonBlobUnreachable, "b1", ExactBytes(200))
	for name, res := range map[string]GCResult{
		"dry-run":    NewResult(NewPlan(true, []GCCandidate{c}, nil), nil, nil),
		"suppressed": NewLockSuppressedResult(NewPlan(false, []GCCandidate{c}, nil), nil),
	} {
		if s := res.ExecSummary(); s.BytesDeleted != 0 || s.Deleted != 0 {
			t.Errorf("%s: exec summary = %+v, want no deleted bytes", name, s)
		}
	}
}
