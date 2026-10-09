package fsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSizeTreeWithinUnlimitedMatchesSizeTreeAt: zero limits are no limits, so the
// bounded walk sums exactly what the unbounded one does, including hard-linked paths
// once each (the metric is per enumerated path, not per inode).
func TestSizeTreeWithinUnlimitedMatchesSizeTreeAt(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "awa", "store", "checkpoints", "c1", "meta.json"), 300)
	writeSized(t, filepath.Join(parent, "awa", "store", "checkpoints", "c1", "manifest.jsonl"), 4000)
	writeSized(t, filepath.Join(parent, "awa", "store", "blobs", "ab", "cdef"), 5000)
	writeSized(t, filepath.Join(parent, "awa", "store", "tmp", "partial"), 70)
	writeSized(t, filepath.Join(parent, "awa", "config.toml"), 8)
	writeSized(t, filepath.Join(parent, "awa", "index", "awa.sqlite"), 900)
	if err := os.Link(filepath.Join(parent, "awa", "store", "blobs", "ab", "cdef"), filepath.Join(parent, "awa", "store", "tmp", "linked")); err != nil {
		t.Logf("hard links unavailable here (%v); the per-path rule is not exercised", err)
	}
	want := int64(300 + 4000 + 5000 + 70 + 8 + 900)
	if _, err := os.Lstat(filepath.Join(parent, "awa", "store", "tmp", "linked")); err == nil {
		want += 5000
	}

	got, err := SizeTreeWithin(context.Background(), openParent(t, parent), "awa", TreeLimits{})
	if err != nil {
		t.Fatalf("SizeTreeWithin: %v", err)
	}
	if got.Bytes != want || !got.Complete() {
		t.Fatalf("SizeTreeWithin = %+v, want %d complete", got, want)
	}
	plain, err := SizeTreeAt(context.Background(), openParent(t, parent), "awa")
	if err != nil || plain != (TreeSize{Bytes: want, Complete: true}) {
		t.Fatalf("SizeTreeAt = %+v, %v; want %d complete", plain, err, want)
	}
}

// TestSizeTreeWithinEntryBudgetStopsWithALowerBound: a wide tree larger than the
// budget stops after exactly the budgeted examinations, whatever the listing order,
// and says so instead of claiming the established bytes are the whole tree.
func TestSizeTreeWithinEntryBudgetStopsWithALowerBound(t *testing.T) {
	parent := t.TempDir()
	for i := range 50 {
		writeSized(t, filepath.Join(parent, "wide", fmt.Sprintf("f%02d", i)), 1)
	}
	got, err := SizeTreeWithin(context.Background(), openParent(t, parent), "wide", TreeLimits{MaxEntries: 10})
	if err != nil {
		t.Fatalf("SizeTreeWithin: %v", err)
	}
	// The root is the first examined entry; nine files follow before the budget ends.
	if want := (BoundedTreeSize{Bytes: 9, Entries: 10, EntryBudgetReached: true}); got != want {
		t.Fatalf("SizeTreeWithin = %+v, want %+v", got, want)
	}
	if got.Complete() {
		t.Error("a budget-stopped walk must not be complete")
	}

	// A budget the tree fits in leaves the measurement exact.
	got, err = SizeTreeWithin(context.Background(), openParent(t, parent), "wide", TreeLimits{MaxEntries: 51})
	if err != nil || got != (BoundedTreeSize{Bytes: 50, Entries: 51}) || !got.Complete() {
		t.Fatalf("SizeTreeWithin(fits) = %+v, %v; want 50 bytes complete", got, err)
	}
}

// TestSizeTreeWithinBudgetSpansSubtrees: the budget is one walk-wide count, so it
// ends the walk from inside a nested directory rather than per directory.
func TestSizeTreeWithinBudgetSpansSubtrees(t *testing.T) {
	parent := t.TempDir()
	for d := range 5 {
		for f := range 5 {
			writeSized(t, filepath.Join(parent, "tree", fmt.Sprintf("d%d", d), fmt.Sprintf("f%d", f)), 1)
		}
	}
	got, err := SizeTreeWithin(context.Background(), openParent(t, parent), "tree", TreeLimits{MaxEntries: 12})
	if err != nil {
		t.Fatalf("SizeTreeWithin: %v", err)
	}
	if got.Entries != 12 || !got.EntryBudgetReached || got.Bytes >= 25 {
		t.Fatalf("SizeTreeWithin = %+v, want 12 entries, budget reached, partial bytes", got)
	}
}

// TestSizeTreeWithinDepthLimitSkipsDeeperLevels: directories past MaxDepth are not
// opened, which bounds the descriptors held at once; their contents are reported as
// unmeasured rather than as empty.
func TestSizeTreeWithinDepthLimitSkipsDeeperLevels(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "deep")
	for level := range 10 {
		writeSized(t, filepath.Join(dir, "f"), 1)
		dir = filepath.Join(dir, fmt.Sprintf("d%d", level))
	}
	got, err := SizeTreeWithin(context.Background(), openParent(t, parent), "deep", TreeLimits{MaxDepth: 3})
	if err != nil {
		t.Fatalf("SizeTreeWithin: %v", err)
	}
	if got.Bytes != 3 || !got.DepthLimitReached || got.EntryBudgetReached || got.Unreadable {
		t.Fatalf("SizeTreeWithin = %+v, want 3 bytes from three opened levels, depth limit only", got)
	}

	got, err = SizeTreeWithin(context.Background(), openParent(t, parent), "deep", TreeLimits{MaxDepth: 32})
	if err != nil || got.Bytes != 10 || !got.Complete() {
		t.Fatalf("SizeTreeWithin(deep enough) = %+v, %v; want 10 bytes complete", got, err)
	}
}

// TestSizeTreeWithinDoesNotFollowSymlinks: a descendant symlink to a large file or an
// outside directory contributes nothing.
func TestSizeTreeWithinDoesNotFollowSymlinks(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	writeSized(t, filepath.Join(outside, "big"), 1<<16)
	writeSized(t, filepath.Join(parent, "awa", "own"), 5)
	if err := os.Symlink(filepath.Join(outside, "big"), filepath.Join(parent, "awa", "to-file")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "awa", "to-dir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := SizeTreeWithin(context.Background(), openParent(t, parent), "awa", TreeLimits{MaxEntries: 100, MaxDepth: 8})
	if err != nil || got.Bytes != 5 || !got.Complete() {
		t.Fatalf("SizeTreeWithin = %+v, %v; want 5 complete", got, err)
	}
}

// TestSizeTreeWithinCancellationIsAnError: a cancelled walk is never a partial size.
func TestSizeTreeWithinCancellationIsAnError(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "awa", "f"), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SizeTreeWithin(ctx, openParent(t, parent), "awa", TreeLimits{MaxEntries: 100}); err == nil {
		t.Fatal("a cancelled walk must return an error")
	}
}

// TestSizeDirWithinLimits: the open directory is the first level and not an examined
// entry, so a budget of n stats exactly n children, and the depth limit counts from it.
func TestSizeDirWithinLimits(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "out", "a"), 3)
	writeSized(t, filepath.Join(parent, "out", "sub", "deep"), 5)
	open := func() *os.File {
		d, err := os.Open(filepath.Join(parent, "out"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}

	full, err := SizeDirWithin(context.Background(), open(), TreeLimits{MaxEntries: 3}, nil)
	if err != nil || full.Bytes != 8 || full.Entries != 3 || !full.Complete() {
		t.Fatalf("SizeDirWithin(3 entries) = %+v, %v; want 8 complete over 3 entries", full, err)
	}
	short, err := SizeDirWithin(context.Background(), open(), TreeLimits{MaxEntries: 2}, nil)
	if err != nil || !short.EntryBudgetReached || short.Entries != 2 {
		t.Fatalf("SizeDirWithin(2 entries) = %+v, %v; want the budget reached after 2", short, err)
	}
	shallow, err := SizeDirWithin(context.Background(), open(), TreeLimits{MaxDepth: 1}, nil)
	if err != nil || shallow.Bytes != 3 || !shallow.DepthLimitReached {
		t.Fatalf("SizeDirWithin(depth 1) = %+v, %v; want 3 with the depth limit reached", shallow, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SizeDirWithin(ctx, open(), TreeLimits{}, nil); err == nil {
		t.Fatal("SizeDirWithin(cancelled) succeeded; cancellation must be an error")
	}
}

// TestSizeDirWithinSkipsNamedSubtrees: a skipped name is left out at every depth —
// neither its bytes nor its entries count — while sibling content is still measured.
func TestSizeDirWithinSkipsNamedSubtrees(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "out", "visible"), 1)
	writeSized(t, filepath.Join(parent, "out", ".git", "object"), 100)
	writeSized(t, filepath.Join(parent, "out", "sub", ".awa", "blob"), 200)
	writeSized(t, filepath.Join(parent, "out", "sub", "kept"), 4)
	d, err := os.Open(filepath.Join(parent, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	got, err := SizeDirWithin(context.Background(), d, TreeLimits{}, map[string]bool{".git": true, ".awa": true})
	if err != nil || got.Bytes != 5 || got.Entries != 3 || !got.Complete() {
		t.Fatalf("SizeDirWithin = %+v, %v; want 5 bytes over 3 entries, complete", got, err)
	}
}
