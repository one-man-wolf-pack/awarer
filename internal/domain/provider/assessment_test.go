package provider_test

import (
	"testing"
	"time"

	"awarer/internal/domain/provider"
)

// Mutation proofs:
//   - drop the inputRef=="" check in Resolved/Unavailable -> the "empty input ref"
//     subtests would no longer see an error (red).
//   - drop the reason.Valid() check in Unavailable -> TestUnavailableRejectsInvalidReason
//     goes red.
//   - in Resolved, also store the reason (or in Unavailable, also store an identity) ->
//     TestResolvedCarriesOnlyIdentity / TestUnavailableCarriesOnlyReason go red because
//     the excluded accessor would start returning ok=true.

func checkpointIdentity(t *testing.T) provider.Identity {
	t.Helper()
	id, err := provider.NewCheckpointIdentity(cpID(t, "cp"), treeHash(t, "a"), validScan(t, "b"), time.Now().UTC(), boundary(t, 0))
	if err != nil {
		t.Fatalf("NewCheckpointIdentity: %v", err)
	}
	return id
}

func TestResolvedCarriesOnlyIdentity(t *testing.T) {
	a, err := provider.Resolved("latest", checkpointIdentity(t))
	if err != nil {
		t.Fatalf("Resolved: %v", err)
	}
	if a.InputRef() != "latest" {
		t.Errorf("InputRef = %q, want latest", a.InputRef())
	}
	if a.Outcome() != provider.OutcomeResolved {
		t.Errorf("Outcome = %v, want resolved", a.Outcome())
	}
	if _, ok := a.Identity(); !ok {
		t.Error("resolved assessment must carry an identity")
	}
	if _, ok := a.Reason(); ok {
		t.Error("resolved assessment must not carry a reason")
	}
}

func TestUnavailableCarriesOnlyReason(t *testing.T) {
	a, err := provider.Unavailable("bogus", provider.ReasonNotFound)
	if err != nil {
		t.Fatalf("Unavailable: %v", err)
	}
	if a.Outcome() != provider.OutcomeUnavailable {
		t.Errorf("Outcome = %v, want unavailable", a.Outcome())
	}
	r, ok := a.Reason()
	if !ok || r != provider.ReasonNotFound {
		t.Errorf("Reason = (%v,%v), want (not-found,true)", r, ok)
	}
	if _, ok := a.Identity(); ok {
		t.Error("unavailable assessment must not carry an identity")
	}
}

func TestResolvedRejectsEmptyInputRef(t *testing.T) {
	if _, err := provider.Resolved("", checkpointIdentity(t)); err == nil {
		t.Error("expected error for empty input ref")
	}
}

func TestResolvedRejectsInvalidIdentity(t *testing.T) {
	if _, err := provider.Resolved("latest", provider.Identity{}); err == nil {
		t.Error("expected error for zero-value identity")
	}
}

func TestUnavailableRejectsEmptyInputRef(t *testing.T) {
	if _, err := provider.Unavailable("", provider.ReasonNotFound); err == nil {
		t.Error("expected error for empty input ref")
	}
}

func TestUnavailableRejectsInvalidReason(t *testing.T) {
	if _, err := provider.Unavailable("latest", provider.Reason("not-a-real-reason")); err == nil {
		t.Error("expected error for invalid reason")
	}
	if _, err := provider.Unavailable("latest", provider.Reason("")); err == nil {
		t.Error("expected error for empty reason")
	}
}

func TestOutcomeString(t *testing.T) {
	if provider.OutcomeResolved.String() != "resolved" || provider.OutcomeUnavailable.String() != "unavailable" {
		t.Error("outcome tokens drifted")
	}
}
