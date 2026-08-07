package provider_test

import (
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
)

// Mutation proofs (verified by temporarily breaking the production guard, observing the
// named test go red, then restoring):
//   - drop the treeHash.IsZero() check in each constructor -> the "zero tree hash"
//     subtests below pass a zero hash and would no longer see an error (red).
//   - hard-code reconstructable=true in NewCurrentWorktreeIdentity ->
//     TestCurrentWorktreeIsNeverReconstructable goes red (oracle asserts a literal
//     false, independent of the field the producer sets).
//   - swap KindRunBefore/KindRunAfter selection in NewRunObservationIdentity ->
//     TestRunObservationKindFollowsSelector goes red.
//   - hard-code canonicalRef to a literal (e.g. "x") in a constructor -> the CanonicalRef
//     derivation asserts in TestCheckpointIdentity / TestRunObservationKindFollowsSelector
//     go red (the ref is derived from the id/selector, not passed in).
//   - drop the !scan.Valid() check in NewCheckpointIdentity / NewCurrentWorktreeIdentity ->
//     the requires-known-scan subtests go red.
//   - relax sameImmutableRecord to compare tree hashes instead of ids ->
//     TestClassifyDistinctRecordsNeedProof goes red.
//
// The constructors take typed checkpoint.CheckpointID / runcache.RunID, so a malformed or
// non-id string is unrepresentable at the call site — there is no in-domain string guard
// left to mutate; the id parsers own that (their own package tests).

func TestCurrentWorktreeIdentity(t *testing.T) {
	th := treeHash(t, "a")
	now := time.Unix(1700000000, 0).UTC()
	id, err := provider.NewCurrentWorktreeIdentity(th, validScan(t, "b"), now, boundary(t, 2))
	if err != nil {
		t.Fatalf("NewCurrentWorktreeIdentity: %v", err)
	}
	if id.Kind() != provider.KindCurrentWorktree {
		t.Errorf("kind = %v, want current-worktree", id.Kind())
	}
	if id.CanonicalRef() != "now" {
		t.Errorf("canonicalRef = %q, want now", id.CanonicalRef())
	}
	if id.CheckpointID() != "" || id.RunID() != "" {
		t.Errorf("current worktree must carry no record id, got checkpoint=%q run=%q", id.CheckpointID(), id.RunID())
	}
	if got, ok := id.ObservedAt(); !ok || !got.Equal(now) {
		t.Errorf("ObservedAt = (%v,%v), want (%v,true)", got, ok, now)
	}
	if id.Boundary().SkippedInputs() != 2 {
		t.Errorf("SkippedInputs = %d, want 2", id.Boundary().SkippedInputs())
	}
}

func TestCurrentWorktreeIsNeverReconstructable(t *testing.T) {
	th := treeHash(t, "a")
	id, err := provider.NewCurrentWorktreeIdentity(th, validScan(t, "b"), time.Now().UTC(), boundary(t, 0))
	if err != nil {
		t.Fatalf("NewCurrentWorktreeIdentity: %v", err)
	}
	// Oracle: a live worktree is by definition not reconstructable; assert the literal.
	if id.Reconstructable() {
		t.Error("current worktree must never be reconstructable")
	}
	if !id.Comparable() {
		t.Error("current worktree must be comparable")
	}
}

func TestCurrentWorktreeIdentityRejectsInvalidInputs(t *testing.T) {
	th := treeHash(t, "a")
	if _, err := provider.NewCurrentWorktreeIdentity(hashing.TreeHash{}, validScan(t, "b"), time.Now(), boundary(t, 0)); err == nil {
		t.Error("expected error for zero tree hash")
	}
	// A current worktree is scanned under the effective config, so an unknown (zero-value)
	// scan identity — with no scope/policy — is not enough to interpret its tree hash.
	if _, err := provider.NewCurrentWorktreeIdentity(th, provider.ScanIdentity{}, time.Now(), boundary(t, 0)); err == nil {
		t.Error("expected error for an unknown scan identity")
	}
}

func TestCheckpointIdentity(t *testing.T) {
	th := treeHash(t, "c")
	created := time.Unix(1699999999, 0).UTC()
	cid := cpID(t, "cpfull")
	id, err := provider.NewCheckpointIdentity(cid, th, validScan(t, "d"), created, boundary(t, 0))
	if err != nil {
		t.Fatalf("NewCheckpointIdentity: %v", err)
	}
	if id.Kind() != provider.KindCheckpoint {
		t.Errorf("kind = %v, want checkpoint", id.Kind())
	}
	if id.CheckpointID() != cid.String() {
		t.Errorf("CheckpointID = %q, want %q", id.CheckpointID(), cid.String())
	}
	// The canonical ref is derived from the id, never passed in, so it cannot disagree.
	if id.CanonicalRef() != cid.String() {
		t.Errorf("CanonicalRef = %q, want the full checkpoint id %q", id.CanonicalRef(), cid.String())
	}
	if id.RunID() != "" {
		t.Errorf("checkpoint identity must carry no run id, got %q", id.RunID())
	}
	if !id.Reconstructable() {
		t.Error("checkpoint must be reconstructable")
	}
}

func TestCheckpointIdentityRejectsInvalidInputs(t *testing.T) {
	th := treeHash(t, "c")
	scan := validScan(t, "d")
	valid := cpID(t, "cp")
	cases := map[string]struct {
		id   checkpoint.CheckpointID
		th   hashing.TreeHash
		scan provider.ScanIdentity
	}{
		"zero id":      {checkpoint.CheckpointID{}, th, scan},
		"zero hash":    {valid, hashing.TreeHash{}, scan},
		"unknown scan": {valid, th, provider.ScanIdentity{}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := provider.NewCheckpointIdentity(c.id, c.th, c.scan, time.Now(), boundary(t, 0)); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestRunObservationKindFollowsSelector(t *testing.T) {
	th := treeHash(t, "e")
	scan := validScan(t, "f")
	rid := runID(t, "runid1")
	before, err := provider.NewRunObservationIdentity(true, rid, th, scan, time.Time{}, false, boundary(t, 0))
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	after, err := provider.NewRunObservationIdentity(false, rid, th, scan, time.Time{}, false, boundary(t, 0))
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before.Kind() != provider.KindRunBefore {
		t.Errorf("before kind = %v, want run-before", before.Kind())
	}
	if after.Kind() != provider.KindRunAfter {
		t.Errorf("after kind = %v, want run-after", after.Kind())
	}
	// Canonical refs are derived from id+selector, so before/after cannot be mislabelled.
	if want := "run:" + rid.String() + ":before"; before.CanonicalRef() != want {
		t.Errorf("before CanonicalRef = %q, want %q", before.CanonicalRef(), want)
	}
	if want := "run:" + rid.String() + ":after"; after.CanonicalRef() != want {
		t.Errorf("after CanonicalRef = %q, want %q", after.CanonicalRef(), want)
	}
	if before.CheckpointID() != "" || after.CheckpointID() != "" {
		t.Error("run observation must carry no checkpoint id")
	}
	if before.RunID() != rid.String() {
		t.Errorf("RunID = %q, want %q", before.RunID(), rid.String())
	}
	// No recorded observation time -> ObservedAt reports unknown.
	if _, ok := before.ObservedAt(); ok {
		t.Error("run observation with no time must report ObservedAt unknown")
	}
}

func TestRunObservationIdentityRejectsInvalidInputs(t *testing.T) {
	th := treeHash(t, "e")
	scan := validScan(t, "f")
	rid := runID(t, "run")
	if _, err := provider.NewRunObservationIdentity(true, runcache.RunID{}, th, scan, time.Time{}, false, boundary(t, 0)); err == nil {
		t.Error("expected error for a zero run id")
	}
	if _, err := provider.NewRunObservationIdentity(true, rid, hashing.TreeHash{}, scan, time.Time{}, false, boundary(t, 0)); err == nil {
		t.Error("expected error for zero hash")
	}
	// A run observation persists a complete scan_config_hash, so an unknown
	// (zero-value) scan identity is rejected rather than accepted as a weakened identity.
	if _, err := provider.NewRunObservationIdentity(true, rid, th, provider.ScanIdentity{}, time.Time{}, false, boundary(t, 0)); err == nil {
		t.Error("expected error for an unknown scan identity")
	}
}

func TestScanIdentityConstructors(t *testing.T) {
	if _, err := provider.NewScanIdentity(hashing.ConfigHash{}); err == nil {
		t.Error("scan identity must reject a zero config hash")
	}
	valid := validScan(t, "a")
	if !valid.Valid() {
		t.Error("NewScanIdentity must produce a valid identity")
	}
	// The zero-value scan identity is the only invalid identity; no constructor produces
	// one, and it can never prove policy compatibility.
	if (provider.ScanIdentity{}).Valid() {
		t.Error("zero-value scan identity must not be valid")
	}
}

func TestKindStringAndValid(t *testing.T) {
	for k, want := range map[provider.Kind]string{
		provider.KindCurrentWorktree: "current-worktree",
		provider.KindCheckpoint:      "checkpoint",
		provider.KindRunBefore:       "run-before",
		provider.KindRunAfter:        "run-after",
	} {
		if k.String() != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, k.String(), want)
		}
		if !k.Valid() {
			t.Errorf("Kind %v must be valid", k)
		}
	}
	if provider.Kind(0).Valid() || provider.Kind(99).Valid() {
		t.Error("unknown kinds must be invalid")
	}
}
