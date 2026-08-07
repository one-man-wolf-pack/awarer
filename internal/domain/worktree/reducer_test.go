package worktree_test

import (
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

func skippedRead(t *testing.T, path string) worktree.SkippedInput {
	t.Helper()
	s, err := worktree.NewSkippedInput(
		mustPath(t, path),
		worktree.ReasonReadError,
		0,
		worktree.StatSignature{Mode: 0o600},
		"permission-denied",
		worktree.SymlinkTarget{},
		worktree.TraversalInfo{},
	)
	if err != nil {
		t.Fatalf("NewSkippedInput(read-error %q): %v", path, err)
	}
	return s
}

func symlink(t *testing.T, path, target string) worktree.Entry {
	t.Helper()
	e, err := worktree.NewSymlinkEntry(mustPath(t, path), mustTarget(t, target), worktree.StatSignature{Mode: 0o777}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewSymlinkEntry: %v", err)
	}
	return e
}

func dir(t *testing.T, path string) worktree.Entry {
	t.Helper()
	e, err := worktree.NewDirEntry(mustPath(t, path), worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewDirEntry: %v", err)
	}
	return e
}

// mixedManifest is a representative manifest covering every kind plus skips, in a
// deliberately unsorted order to prove the reducer/merge sort it canonically.
func mixedManifest(t *testing.T) ([]worktree.Entry, []worktree.SkippedInput) {
	t.Helper()
	entries := []worktree.Entry{
		regular(t, "src/b.go", hexB),
		dir(t, "src"),
		regular(t, "a.go", hexA),
		symlink(t, "link", "a.go"),
	}
	skips := []worktree.SkippedInput{
		skippedLarge(t, "big.bin", 1<<30),
	}
	return entries, skips
}

// TestReducerGoldenHash pins the canonical tree encoding to a known digest. The
// reducer is the only producer of a tree hash, so this golden value is the
// independent oracle for the byte layout: any change to it (field order,
// length-prefixing, version byte) must be a deliberate treeHashVersion bump that
// updates this constant, never an accident.
func TestReducerGoldenHash(t *testing.T) {
	const golden = "blake3:9ce5962a740104374de443ca27114f85e6fc4302c50c1c58459de87161ea9986"
	h := fakeHasher{}
	entries, skips := mixedManifest(t)
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(entries, skips))
	if err != nil {
		t.Fatalf("ReduceCursor: %v", err)
	}
	if red.Hash.String() != golden {
		t.Fatalf("canonical tree encoding changed: got %q, want %q\n"+
			"if this is intentional, bump treeHashVersion and update this golden", red.Hash, golden)
	}
}

// TestReducerStats proves the folded stats match the manifest's actual composition
// and stay consistent with the hash (same record sequence).
func TestReducerStats(t *testing.T) {
	h := fakeHasher{}
	entries, skips := mixedManifest(t)
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(entries, skips))
	if err != nil {
		t.Fatalf("ReduceCursor: %v", err)
	}
	got := red.Stats
	want := worktree.ReducedStats{
		Files:      2, // a.go, src/b.go
		Dirs:       1, // src
		Symlinks:   1, // link
		Blobs:      2, // both regulars are StorageBlob
		HashOnly:   0,
		Skipped:    1, // big.bin
		TotalBytes: 0, // regular fixtures carry zero size
	}
	if got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	if red.Count != 5 {
		t.Fatalf("count = %d, want 5", red.Count)
	}
	if red.Tainted {
		t.Fatalf("large-file skip must not taint the scan")
	}
}

// TestReducerTaintFromReadError proves a read-error skip taints the reduction,
// while a deliberate skip does not.
func TestReducerTaintFromReadError(t *testing.T) {
	h := fakeHasher{}
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(
		[]worktree.Entry{regular(t, "a.go", hexA)},
		[]worktree.SkippedInput{skippedRead(t, "secret")},
	))
	if err != nil {
		t.Fatalf("ReduceCursor: %v", err)
	}
	if !red.Tainted {
		t.Fatalf("read-error skip must taint the reduction")
	}
}

// TestReducerRejectsDuplicate proves a duplicate path fails loudly, including an
// entry/skip collision on one path.
func TestReducerRejectsDuplicate(t *testing.T) {
	h := fakeHasher{}
	_, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(
		[]worktree.Entry{regular(t, "a.go", hexA)},
		[]worktree.SkippedInput{skippedLarge(t, "a.go", 3)},
	))
	if err == nil {
		t.Fatalf("reducer accepted entry/skip collision on one path")
	}
}

// TestReduceCursorRejectsNil proves a nil cursor is a loud error, not a panic.
func TestReduceCursorRejectsNil(t *testing.T) {
	if _, err := worktree.ReduceCursor(fakeHasher{}, nil); err == nil {
		t.Fatalf("ReduceCursor(nil) returned no error")
	}
}

// TestReducerRejectsNilHasher proves the other half of the wiring guard: a reducer
// built without a hasher is refused up front rather than panicking on the first Add,
// so a miswired caller fails where it is wired, not deep inside a fold.
func TestReducerRejectsNilHasher(t *testing.T) {
	if _, err := worktree.NewTreeReducer(nil); err == nil {
		t.Errorf("NewTreeReducer(nil) returned no error")
	}
	if _, err := worktree.ReduceCursor(nil, worktree.NewSliceCursor(nil)); err == nil {
		t.Errorf("ReduceCursor with a nil hasher returned no error")
	}
}

// TestReducerRejectsOutOfOrder proves the reducer re-guards canonical order even
// when fed an unsorted (un-Ordered) cursor, so a buggy upstream cannot produce a
// wrong hash silently.
func TestReducerRejectsOutOfOrder(t *testing.T) {
	h := fakeHasher{}
	raw := worktree.NewSliceCursor([]worktree.ManifestRecord{
		worktree.EntryRecord(regular(t, "b.go", hexA)),
		worktree.EntryRecord(regular(t, "a.go", hexB)),
	})
	if _, err := worktree.ReduceCursor(h, raw); err == nil {
		t.Fatalf("reducer accepted out-of-order records")
	}
}
