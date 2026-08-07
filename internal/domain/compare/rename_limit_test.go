package compare_test

import (
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// renameFixture builds a comparison whose change set is a rename (old.go -> new.go,
// content hexA) plus two plain modifies (m1.go, m2.go). Under rename detection the
// delete+add pair into one Renamed; without it (or over the buffer limit) they stay a
// separate add and delete. Four raw changes lets a small limit exercise the over-limit
// policy. Each call returns fresh canonical cursors, since a cursor is consumed once.
func renameFixture(t *testing.T) (left, right worktree.ManifestCursor) {
	t.Helper()
	left = scantest.CanonicalCursor([]worktree.Entry{
		regular(t, "m1.go", hexB, worktree.StorageBlob),
		regular(t, "m2.go", hexC, worktree.StorageBlob),
		regular(t, "old.go", hexA, worktree.StorageBlob),
	}, nil)
	right = scantest.CanonicalCursor([]worktree.Entry{
		regular(t, "m1.go", hexC, worktree.StorageBlob),
		regular(t, "m2.go", hexB, worktree.StorageBlob),
		regular(t, "new.go", hexA, worktree.StorageBlob),
	}, nil)
	return left, right
}

func summarize(changes []compare.Change) compare.Summary {
	var s compare.Summary
	for _, c := range changes {
		s.Add(c)
	}
	return s
}

// TestRenameDetectionAppliedUnderLimit proves that when the change set fits the buffer
// limit rename pairing runs and the outcome reports applied.
func TestRenameDetectionAppliedUnderLimit(t *testing.T) {
	left, right := renameFixture(t)
	cur, err := compare.CompareStreamLimited(left, right, compare.Options{DetectRenames: true}, 100)
	if err != nil {
		t.Fatalf("CompareStreamLimited: %v", err)
	}
	defer func() { _ = cur.Close() }()

	rd := compare.RenameDetectionOf(cur)
	if !rd.Attempted || !rd.Applied || rd.Reason != "" {
		t.Fatalf("under-limit detection = %+v, want attempted+applied with no reason", rd)
	}
	got := streamChanges(t, cur)
	sum := summarize(got)
	if sum.Renamed != 1 || sum.Modified != 2 || sum.Added != 0 || sum.Deleted != 0 {
		t.Fatalf("under-limit summary = %+v, want 1 rename + 2 modify", sum)
	}
}

// TestRenameDetectionSkippedOverLimit proves the bounded presentation policy: when the
// change set exceeds the buffer limit, pairing is not applied, the would-be rename is
// shown as a plain add + delete, every change is still present in canonical order, and
// the outcome honestly reports the skip. This is the no-OOM, no-lie behavior.
func TestRenameDetectionSkippedOverLimit(t *testing.T) {
	left, right := renameFixture(t) // 4 raw changes
	const limit = 2
	cur, err := compare.CompareStreamLimited(left, right, compare.Options{DetectRenames: true}, limit)
	if err != nil {
		t.Fatalf("CompareStreamLimited: %v", err)
	}
	defer func() { _ = cur.Close() }()

	rd := compare.RenameDetectionOf(cur)
	if !rd.Attempted || rd.Applied {
		t.Fatalf("over-limit detection = %+v, want attempted but not applied", rd)
	}
	if rd.Reason != compare.RenameReasonLimitExceeded || rd.Limit != limit {
		t.Fatalf("over-limit detection = %+v, want reason %q limit %d", rd, compare.RenameReasonLimitExceeded, limit)
	}

	got := streamChanges(t, cur)
	sum := summarize(got)
	// No rename sugar: the pair shows as one add + one delete, the modifies untouched.
	if sum.Renamed != 0 || sum.Added != 1 || sum.Deleted != 1 || sum.Modified != 2 {
		t.Fatalf("over-limit summary = %+v, want 1 add + 1 delete + 2 modify, no rename", sum)
	}
	// Nothing is dropped and order stays canonical (primary path ascending).
	wantPaths := []string{"m1.go", "m2.go", "new.go", "old.go"}
	if len(got) != len(wantPaths) {
		t.Fatalf("over-limit produced %d changes, want %d", len(got), len(wantPaths))
	}
	for i, p := range wantPaths {
		if got[i].PrimaryPath().String() != p {
			t.Fatalf("change %d primary path = %q, want %q (order/completeness broken)", i, got[i].PrimaryPath(), p)
		}
	}
}

// TestRenameSkippedTightLimitLosesNothing exercises the overflow-sentinel path at its
// tightest: with limit 1 the buffered prefix holds exactly one change and every other
// change (the tipping one plus the rest of the base stream) must still be served, in
// canonical order. A bug in the sentinel handling would drop or reorder a change.
func TestRenameSkippedTightLimitLosesNothing(t *testing.T) {
	left, right := renameFixture(t) // 4 raw changes: m1.go, m2.go, new.go(add), old.go(del)
	cur, err := compare.CompareStreamLimited(left, right, compare.Options{DetectRenames: true}, 1)
	if err != nil {
		t.Fatalf("CompareStreamLimited: %v", err)
	}
	defer func() { _ = cur.Close() }()

	got := streamChanges(t, cur)
	wantPaths := []string{"m1.go", "m2.go", "new.go", "old.go"}
	if len(got) != len(wantPaths) {
		t.Fatalf("limit=1 produced %d changes, want %d (overflow dropped?)", len(got), len(wantPaths))
	}
	for i, p := range wantPaths {
		if got[i].PrimaryPath().String() != p {
			t.Fatalf("change %d primary path = %q, want %q", i, got[i].PrimaryPath(), p)
		}
	}
	if sum := summarize(got); sum.Renamed != 0 {
		t.Fatalf("limit=1 must not pair, got %+v", sum)
	}
}

// TestRenameDetectionNotAttempted proves a --no-renames comparison reports the
// not-attempted outcome (the plain merge cursor implements no reporter).
func TestRenameDetectionNotAttempted(t *testing.T) {
	left, right := renameFixture(t)
	cur, err := compare.CompareStream(left, right, compare.Options{DetectRenames: false})
	if err != nil {
		t.Fatalf("CompareStream: %v", err)
	}
	defer func() { _ = cur.Close() }()
	if rd := compare.RenameDetectionOf(cur); rd.Attempted || rd.Applied || rd.Reason != "" {
		t.Fatalf("no-renames detection = %+v, want the zero not-attempted outcome", rd)
	}
}

// TestChangeSetCarriesRenameDetection proves the materialized adapter (used by JSON)
// records the rename-detection outcome.
func TestChangeSetCarriesRenameDetection(t *testing.T) {
	left, right := renameFixture(t)
	cs, err := compare.CompareToChangeSet(left, right, compare.Options{DetectRenames: true})
	if err != nil {
		t.Fatalf("CompareToChangeSet: %v", err)
	}
	if !cs.RenameDetection.Attempted || !cs.RenameDetection.Applied {
		t.Fatalf("ChangeSet detection = %+v, want attempted+applied", cs.RenameDetection)
	}
	if cs.RenameDetection.Limit != compare.DefaultRenameBufferLimit {
		t.Fatalf("ChangeSet detection limit = %d, want default %d", cs.RenameDetection.Limit, compare.DefaultRenameBufferLimit)
	}
}
