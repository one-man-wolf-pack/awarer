package run

import (
	"bytes"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
)

// mutation builds a mutation status for a test, failing on an invalid outcome.
func mutation(t *testing.T, o runcache.MutationOutcome) runcache.MutationStatus {
	t.Helper()
	m, err := runcache.NewMutationStatus(o)
	if err != nil {
		t.Fatalf("NewMutationStatus(%v): %v", o, err)
	}
	return m
}

// nonReusable builds a non-reusable reuse state for a test.
func nonReusable(t *testing.T, r runcache.MismatchReason) runcache.ReuseState {
	t.Helper()
	st, err := runcache.NonReusableRun(r)
	if err != nil {
		t.Fatalf("NonReusableRun(%v): %v", r, err)
	}
	return st
}

func TestNewRunExecutionSummaryValidStates(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(2 * time.Second)
	base := func() Result {
		return Result{
			Execution: Execution{StartedAt: start, FinishedAt: end, Mutation: mutation(t, runcache.MutationUnchanged)},
		}
	}

	t.Run("stored reusable miss", func(t *testing.T) {
		res := base()
		res.Status = runcache.CacheMiss
		res.Written = true
		res.Recorded = true
		res.Reuse = runcache.ReusableCacheEntry()
		sum, err := NewRunExecutionSummary(res)
		if err != nil {
			t.Fatalf("NewRunExecutionSummary: %v", err)
		}
		if sum.State != RunStateStored {
			t.Errorf("state = %v, want stored", sum.State)
		}
		if !sum.Reusable || sum.Duration != 2*time.Second {
			t.Errorf("reusable=%v duration=%v, want true / 2s", sum.Reusable, sum.Duration)
		}
	})

	t.Run("mutating record-only miss exposes pre/post compare", func(t *testing.T) {
		res := base()
		res.Status = runcache.CacheMiss
		res.Recorded = true
		res.Reuse = nonReusable(t, runcache.ReasonMutatedState)
		res.Execution.Mutation = mutation(t, runcache.MutationChanged)
		res.RunID = mustRunID(t)
		sum, err := NewRunExecutionSummary(res)
		if err != nil {
			t.Fatalf("NewRunExecutionSummary: %v", err)
		}
		if sum.State != RunStateRecordOnly {
			t.Errorf("state = %v, want record-only", sum.State)
		}
		if sum.Reusable {
			t.Error("a mutating run must not be reusable")
		}
		if !sum.Mutated || !sum.PrePostCompareAvailable() {
			t.Errorf("mutated=%v prePost=%v, want true/true", sum.Mutated, sum.PrePostCompareAvailable())
		}
	})

	t.Run("hit", func(t *testing.T) {
		res := base()
		res.Status = runcache.CacheHit
		res.Recorded = true
		res.Reuse = runcache.ReusableCacheEntry()
		res.Execution.Mutation = mutation(t, runcache.MutationUnobserved)
		sum, err := NewRunExecutionSummary(res)
		if err != nil {
			t.Fatalf("NewRunExecutionSummary: %v", err)
		}
		if sum.State != RunStateHit {
			t.Errorf("state = %v, want hit", sum.State)
		}
		if sum.PrePostCompareAvailable() {
			t.Error("a replayed hit observed nothing, so it has no pre/post compare")
		}
	})
}

func TestNewRunExecutionSummaryRejectsIllegalStates(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(time.Second)

	t.Run("hit that did not record", func(t *testing.T) {
		res := Result{
			Status:    runcache.CacheHit,
			Recorded:  false,
			Reuse:     runcache.ReusableCacheEntry(),
			Execution: Execution{StartedAt: start, FinishedAt: end, Mutation: mutation(t, runcache.MutationUnobserved)},
		}
		if _, err := NewRunExecutionSummary(res); err == nil {
			t.Error("a hit that did not record must be rejected")
		}
	})

	t.Run("reusable run that mutated state", func(t *testing.T) {
		res := Result{
			Status:    runcache.CacheMiss,
			Written:   true,
			Recorded:  true,
			Reuse:     runcache.ReusableCacheEntry(),
			Execution: Execution{StartedAt: start, FinishedAt: end, Mutation: mutation(t, runcache.MutationChanged)},
		}
		if _, err := NewRunExecutionSummary(res); err == nil {
			t.Error("a reusable run that mutated state must be rejected")
		}
	})

	t.Run("record-only run reported reusable", func(t *testing.T) {
		res := Result{
			Status:    runcache.CacheMiss,
			Written:   false,
			Recorded:  true,
			Reuse:     runcache.ReusableCacheEntry(),
			Execution: Execution{StartedAt: start, FinishedAt: end, Mutation: mutation(t, runcache.MutationUnchanged)},
		}
		if _, err := NewRunExecutionSummary(res); err == nil {
			t.Error("a record-only (unwritten) run reported reusable must be rejected")
		}
	})
}

// mustRunID returns a valid run id for a test.
func mustRunID(t *testing.T) runcache.RunID {
	t.Helper()
	return mustRunIDAt(t, time.Unix(1_700_000_000, 0))
}

// mustRunIDAt returns a valid run id stamped at a given time, so tests can order two
// ids deterministically (run ids are time-prefixed).
func mustRunIDAt(t *testing.T, at time.Time) runcache.RunID {
	t.Helper()
	id, err := runcache.NewRunID(at.UnixNano(), bytes.NewReader([]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
	}))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	return id
}

func TestSortCandidatesPrefersReusableOriginOverHistory(t *testing.T) {
	older := mustRunIDAt(t, time.Unix(1_700_000_000, 0))
	newer := mustRunIDAt(t, time.Unix(1_700_000_100, 0))

	// An older run that was a real cache entry and drifted on inputs.
	reusable := RunReuseCandidate{
		Entry:      runcache.RunEntry{ID: older, Reuse: runcache.ReusableCacheEntry()},
		Comparison: runcache.KeyComparison{Differences: []runcache.Difference{{Code: runcache.DiffInputTreeChanged}}},
		Reason:     runcache.ReasonInputTreeDiffers,
	}
	// A newer run policy kept out of the cache from the start (record-only history).
	recordState, err := runcache.RecordOnly(runcache.ReasonRecordOnly)
	if err != nil {
		t.Fatalf("RecordOnly: %v", err)
	}
	history := RunReuseCandidate{
		Entry:  runcache.RunEntry{ID: newer, Reuse: recordState},
		Reason: runcache.ReasonRecordOnly,
	}

	// Feed them newest-first to prove the bucket, not recency, wins.
	cands := []RunReuseCandidate{history, reusable}
	sortCandidates(cands)
	if cands[0].Reason != runcache.ReasonInputTreeDiffers {
		t.Errorf("first candidate reason = %q, want the reusable-origin drift to outrank newer record-only history", cands[0].Reason)
	}
}
