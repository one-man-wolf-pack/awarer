package gc

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseEnumsRoundTrip(t *testing.T) {
	for _, k := range []CandidateKind{KindCheckpoint, KindRun, KindBlob, KindTemp} {
		got, err := ParseCandidateKind(k.String())
		if err != nil || got != k {
			t.Errorf("ParseCandidateKind(%s) = %v, %v", k, got, err)
		}
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	for _, a := range []CandidateAction{ActionDelete, ActionRetain, ActionBlocked, ActionSkipped} {
		got, err := ParseCandidateAction(a.String())
		if err != nil || got != a {
			t.Errorf("ParseCandidateAction(%s) = %v, %v", a, got, err)
		}
	}
	for _, f := range []SubsystemFilter{FilterAll, FilterRunsOnly, FilterCheckpointsOnly, FilterBlobsOnly} {
		got, err := ParseSubsystemFilter(f.String())
		if err != nil || got != f {
			t.Errorf("ParseSubsystemFilter(%s) = %v, %v", f, got, err)
		}
	}
}

func TestParseEnumsRejectUnknown(t *testing.T) {
	if _, err := ParseCandidateKind("widget"); err == nil {
		t.Error("ParseCandidateKind(widget) should fail")
	}
	if _, err := ParseCandidateAction("purge"); err == nil {
		t.Error("ParseCandidateAction(purge) should fail")
	}
	if _, err := ParseSubsystemFilter("everything"); err == nil {
		t.Error("ParseSubsystemFilter(everything) should fail")
	}
	if CandidateKind(0).Valid() || CandidateKind(99).Valid() {
		t.Error("out-of-range kinds must be invalid")
	}
}

func TestReasonActionConsistency(t *testing.T) {
	// Every reason maps to a valid action, and that action round-trips.
	for r, a := range reasonAction {
		if !r.Valid() {
			t.Errorf("reason %q in map but not Valid", r)
		}
		if r.Action() != a {
			t.Errorf("reason %q Action() = %v, want %v", r, r.Action(), a)
		}
		if !a.Valid() {
			t.Errorf("reason %q maps to invalid action", r)
		}
	}
	if RetentionReason("invented").Valid() {
		t.Error("unknown reason must be invalid")
	}
	// Every known reason must also declare at least one allowed kind, all valid.
	for r := range reasonAction {
		kinds := reasonKinds[r]
		if len(kinds) == 0 {
			t.Errorf("reason %q declares no allowed kinds", r)
		}
		for k := range kinds {
			if !k.Valid() {
				t.Errorf("reason %q allows invalid kind %v", r, k)
			}
		}
	}
}

func TestNewCandidateRejectsReasonKindMismatch(t *testing.T) {
	// A lock reason cannot describe a blob, a checkpoint reason cannot describe a run.
	if _, err := NewCandidate(KindBlob, ReasonLockActive, "b1", ".awa/store/blob/b1", "x", 0); err == nil {
		t.Error("blob with a lock reason should be rejected")
	}
	if _, err := NewCandidate(KindRun, ReasonCheckpointExpired, "r1", ".awa/runs/r1", "x", 0); err == nil {
		t.Error("run with a checkpoint reason should be rejected")
	}
	// A cross-kind reason is accepted for each of its declared kinds.
	if _, err := NewCandidate(KindCheckpoint, ReasonGitUnavailable, "s1", ".awa/checkpoints/s1", "x", 0); err != nil {
		t.Errorf("git-unavailable should describe a checkpoint: %v", err)
	}
	if _, err := NewCandidate(KindRun, ReasonGitUnavailable, "r1", ".awa/runs/r1", "x", 0); err != nil {
		t.Errorf("git-unavailable should describe a run: %v", err)
	}
}

func TestFilterAllows(t *testing.T) {
	// A full GC allows every kind; a *-only filter allows exactly its subsystem and
	// excludes temp (temp is collected only by a full GC).
	if !FilterAll.Allows(KindTemp) || !FilterAll.Allows(KindBlob) || !FilterAll.Allows(KindCheckpoint) || !FilterAll.Allows(KindRun) {
		t.Error("FilterAll should allow every kind")
	}
	for _, f := range []SubsystemFilter{FilterRunsOnly, FilterCheckpointsOnly, FilterBlobsOnly} {
		if f.Allows(KindTemp) {
			t.Errorf("%s must not allow temp deletion", f)
		}
	}
	if FilterCheckpointsOnly.Allows(KindBlob) {
		t.Error("checkpoints-only must not allow blob deletion")
	}
	if !FilterBlobsOnly.Allows(KindBlob) {
		t.Error("blobs-only must allow blob deletion")
	}
	if FilterBlobsOnly.Allows(KindCheckpoint) {
		t.Error("blobs-only must not allow checkpoint deletion")
	}
	if !FilterRunsOnly.Allows(KindRun) {
		t.Error("runs-only must allow run deletion")
	}
}

func TestNewCandidateValidation(t *testing.T) {
	// Valid delete candidate.
	c, err := NewCandidate(KindCheckpoint, ReasonCheckpointExpired, "018f", ".awa/checkpoints/018f", "expired", 4096)
	if err != nil {
		t.Fatalf("valid candidate rejected: %v", err)
	}
	if c.Action() != ActionDelete {
		t.Errorf("action = %v, want delete", c.Action())
	}

	// Missing id for a non-temp kind.
	if _, err := NewCandidate(KindBlob, ReasonBlobUnreachable, "", ".awa/store/blob/ab", "x", 1); err == nil {
		t.Error("blob without id should be rejected")
	}
	// Temp needs no id.
	if _, err := NewCandidate(KindTemp, ReasonTempStale, "", ".awa/store/tmp/blob-x", "stale", 0); err != nil {
		t.Errorf("temp without id should be accepted: %v", err)
	}
	// Empty path / message rejected.
	if _, err := NewCandidate(KindRun, ReasonRunExpired, "r1", "", "x", 0); err == nil {
		t.Error("empty path should be rejected")
	}
	if _, err := NewCandidate(KindRun, ReasonRunExpired, "r1", ".awa/runs/r1", "", 0); err == nil {
		t.Error("empty message should be rejected")
	}
	// Negative bytes rejected.
	if _, err := NewCandidate(KindRun, ReasonRunExpired, "r1", ".awa/runs/r1", "x", -1); err == nil {
		t.Error("negative bytes should be rejected")
	}
	// Unknown reason rejected.
	if _, err := NewCandidate(KindRun, RetentionReason("nope"), "r1", ".awa/runs/r1", "x", 0); err == nil {
		t.Error("unknown reason should be rejected")
	}
}

func TestPlanSummaryDerivation(t *testing.T) {
	mk := func(k CandidateKind, r RetentionReason, id string, bytes int64) GCCandidate {
		c, err := NewCandidate(k, r, id, ".awa/"+id, "msg", bytes)
		if err != nil {
			t.Fatalf("NewCandidate: %v", err)
		}
		return c
	}
	cands := []GCCandidate{
		mk(KindCheckpoint, ReasonCheckpointExpired, "s1", 100),
		mk(KindBlob, ReasonBlobUnreachable, "b1", 200),
		mk(KindCheckpoint, ReasonCheckpointLatest, "s2", 0),
		mk(KindBlob, ReasonBlobReferenced, "b2", 0),
		mk(KindLock, ReasonLockActive, "", 0),
	}
	plan := NewPlan(false, cands, nil)
	s := plan.Summary()
	if s.PlannedDelete != 2 || s.BytesPlanned != 300 {
		t.Errorf("planned/bytes = %d/%d, want 2/300", s.PlannedDelete, s.BytesPlanned)
	}
	if s.Retained != 2 {
		t.Errorf("retained = %d, want 2", s.Retained)
	}
	if s.Blocked != 1 {
		t.Errorf("blocked = %d, want 1", s.Blocked)
	}
	if len(plan.Deletions()) != 2 {
		t.Errorf("deletions = %d, want 2", len(plan.Deletions()))
	}
	if !plan.LockBlocked() || plan.StateActionBlocked() {
		t.Errorf("expected lock-blocked, not corruption-blocked")
	}
	if !plan.Blocked() {
		t.Error("plan should be blocked")
	}
}

func TestPlanStateActionBlocked(t *testing.T) {
	c, err := NewCandidate(KindCheckpoint, ReasonCheckpointCorrupt, "s1", ".awa/checkpoints/s1", "corrupt", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan := NewPlan(false, []GCCandidate{c}, nil)
	if !plan.StateActionBlocked() {
		t.Error("expected corruption-blocked")
	}
	if plan.LockBlocked() {
		t.Error("did not expect lock-blocked")
	}
}

func TestPlanBlobFootprint(t *testing.T) {
	// A plan built without a blob pass carries no footprint.
	plan := NewPlan(true, nil, nil)
	if _, ok := plan.BlobFootprint(); ok {
		t.Error("a plan without a blob pass reports a footprint")
	}

	// WithBlobFootprint attaches an immutable copy without mutating the original plan.
	fp := BlobFootprint{Count: 3, Bytes: 4096, Complete: true}
	withFP := plan.WithBlobFootprint(fp)
	if _, ok := plan.BlobFootprint(); ok {
		t.Error("WithBlobFootprint mutated the original plan")
	}
	got, ok := withFP.BlobFootprint()
	if !ok {
		t.Fatal("BlobFootprint absent after WithBlobFootprint")
	}
	if got != fp {
		t.Errorf("footprint = %+v, want %+v", got, fp)
	}
}

func TestNewGCRequestValidation(t *testing.T) {
	if _, err := NewGCRequest(false, FilterAll, KeepLastDefault, 0, false); err != nil {
		t.Errorf("default request rejected: %v", err)
	}
	if _, err := NewGCRequest(false, FilterAll, -5, 0, false); err == nil {
		t.Error("negative keep-last should be rejected")
	}
	if _, err := NewGCRequest(false, FilterAll, 0, -time.Second, false); err == nil {
		t.Error("negative older-than should be rejected")
	}
	if _, err := NewGCRequest(false, SubsystemFilter(0), 0, 0, false); err == nil {
		t.Error("invalid filter should be rejected")
	}
}

func TestGCRequestKeepLast(t *testing.T) {
	r, _ := NewGCRequest(false, FilterAll, KeepLastDefault, 0, false)
	if got := r.KeepLast(50); got != 50 {
		t.Errorf("KeepLast(50) with default = %d, want 50", got)
	}
	r2, _ := NewGCRequest(false, FilterAll, 7, 0, false)
	if got := r2.KeepLast(50); got != 7 {
		t.Errorf("KeepLast(50) with explicit 7 = %d, want 7", got)
	}
}

func TestGCResultExecSummary(t *testing.T) {
	c, _ := NewCandidate(KindBlob, ReasonBlobUnreachable, "b1", ".awa/store/blob/b1", "x", 200)
	c2, _ := NewCandidate(KindBlob, ReasonBlobUnreachable, "b2", ".awa/store/blob/b2", "x", 50)
	plan := NewPlan(false, []GCCandidate{c, c2}, nil)
	res := NewResult(plan, []DeletionResult{
		NewDeletionResult(c, 200, nil),
		NewDeletionResult(c2, 0, errors.New("boom")),
	}, nil)
	s := res.ExecSummary()
	if s.Deleted != 1 || s.Failed != 1 || s.BytesDeleted != 200 {
		t.Errorf("exec summary = %+v, want deleted 1 failed 1 bytes 200", s)
	}
}

func TestNewLockSuppressedResult(t *testing.T) {
	del, _ := NewCandidate(KindBlob, ReasonBlobUnreachable, "b1", ".awa/store/blob/b1", "x", 200)

	// A real, unblocked plan yields a suppressed result with no deletions.
	res := NewLockSuppressedResult(NewPlan(false, []GCCandidate{del}, nil), []string{"writer appeared; retaining state"})
	if !res.LockSuppressed() {
		t.Fatal("LockSuppressed should be true")
	}
	if len(res.Deletions()) != 0 {
		t.Fatalf("a suppressed result deletes nothing, got %d", len(res.Deletions()))
	}

	// A dry-run plan and a blocked plan are programmer errors: suppression is
	// meaningful only for a real run that took the lease and then withheld deletion.
	assertPanics(t, "dry-run", func() {
		NewLockSuppressedResult(NewPlan(true, []GCCandidate{del}, nil), nil)
	})
	lock, _ := NewCandidate(KindLock, ReasonLockActive, "", ".awa/locks/x.lock", "active", 0)
	assertPanics(t, "blocked", func() {
		NewLockSuppressedResult(NewPlan(false, []GCCandidate{lock}, nil), nil)
	})
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	fn()
}

// TestRetentionReasonsIsTheClosedVocabulary proves the published enumeration and the
// lookup views are one catalog rather than three lists that happen to agree: every
// enumerated reason is Valid, carries a valid action, and admits at least one kind;
// none is empty or duplicated; and the count is pinned so adding a reason is a
// deliberate edit that every consumer walking the vocabulary — notably the
// documentation coverage matrix — sees.
func TestRetentionReasonsIsTheClosedVocabulary(t *testing.T) {
	reasons := RetentionReasons()
	if len(reasons) != 23 {
		t.Errorf("RetentionReasons() has %d entries, want 23 — update the documentation coverage matrix too", len(reasons))
	}
	seen := map[RetentionReason]bool{}
	for _, r := range reasons {
		switch {
		case r == "":
			t.Error("RetentionReasons() contains an empty reason")
			continue
		case seen[r]:
			t.Errorf("RetentionReasons() lists %q twice", r)
		case !r.Valid():
			t.Errorf("enumerated reason %q is not Valid; membership and enumeration disagree", r)
		}
		seen[r] = true
		if a := r.Action(); a.String() == "unknown" {
			t.Errorf("reason %q justifies no valid action", r)
		}
	}
	// The "every reason declares at least one valid kind" half is not repeated here:
	// TestReasonActionConsistency already asserts it, and also that each declared kind
	// is itself valid.
}

// candidateKinds is every kind a candidate can have, in declaration order.
var candidateKinds = []CandidateKind{KindCheckpoint, KindRun, KindBlob, KindTemp, KindLock, KindRestore}

// TestSingleKindReasonNamesItsSubsystem pins the naming rule the reason catalog's own
// comment states: a reason tied to one subsystem says which one in its token, and the
// only reasons admitting several kinds are the deliberate cross-kind decisions.
//
// This is the invariant, not a copy of the table. Restating every reason-to-kind pair
// would recreate the parallel list the catalog exists to remove; what is worth pinning
// is that the token and the kind cannot disagree — a "blob-referenced" filed under
// KindTemp would make NewCandidate reject the very candidate it is meant to describe,
// and nothing else in the suite would notice.
func TestSingleKindReasonNamesItsSubsystem(t *testing.T) {
	crossKind := 0
	for _, r := range RetentionReasons() {
		var kinds []CandidateKind
		for _, k := range candidateKinds {
			if r.allowsKind(k) {
				kinds = append(kinds, k)
			}
		}
		if len(kinds) != 1 {
			crossKind++
			continue
		}
		own := kinds[0]
		if !strings.Contains(r.String(), own.String()) {
			t.Errorf("reason %q admits only %s candidates but does not name that subsystem", r, own)
		}
		for _, k := range candidateKinds {
			if k != own && strings.Contains(r.String(), k.String()) {
				t.Errorf("reason %q names the %s subsystem but is filed under %s", r, k, own)
			}
		}
	}
	// Cross-kind reasons are the documented exception, so their number is worth
	// pinning: a mapping slip that widened a subsystem reason to several kinds would
	// otherwise slip past the naming rule above by being skipped.
	if crossKind != 2 {
		t.Errorf("%d reasons admit several kinds, want 2 (git-unavailable, subsystem-filtered)", crossKind)
	}
}

// TestRetentionReasonsAreGroupedByAction proves the canonical order really is the
// action grouping it claims to be: all deletes, then all retains, then all blocked,
// then skipped, with no interleaving. The documentation presents the vocabulary in
// exactly these groups, so a reordering that broke the grouping would silently make
// that presentation wrong.
func TestRetentionReasonsAreGroupedByAction(t *testing.T) {
	want := []CandidateAction{ActionDelete, ActionRetain, ActionBlocked, ActionSkipped}
	group := 0
	for _, r := range RetentionReasons() {
		for group < len(want) && r.Action() != want[group] {
			group++
		}
		if group >= len(want) {
			t.Fatalf("reason %q (action %s) appears out of the delete/retain/blocked/skipped order",
				r, r.Action())
		}
	}
}

// TestRetentionReasonsIsCopySafe proves a consumer cannot reorder or truncate the
// catalog every other consumer reads.
func TestRetentionReasonsIsCopySafe(t *testing.T) {
	first := RetentionReasons()
	original := first[0]
	first[0] = "mutated"
	if got := RetentionReasons()[0]; got != original {
		t.Errorf("RetentionReasons() shares its backing array: first entry is now %q", got)
	}
}
