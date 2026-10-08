package fsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSized writes a file of exactly n bytes, so the expected totals below are the
// lengths the fixture chose rather than anything the walk under test reports.
func writeSized(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", n)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openParent(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestSizeTreeAtSumsNestedRegularFiles is the portable contract: every regular file
// beneath the named tree contributes its length, at any depth, and nothing outside
// the tree does.
func TestSizeTreeAtSumsNestedRegularFiles(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "rec", "meta.json"), 300)
	writeSized(t, filepath.Join(parent, "rec", "manifest.jsonl"), 4000)
	writeSized(t, filepath.Join(parent, "rec", "nested", "deeper", "extra"), 17)
	writeSized(t, filepath.Join(parent, "rec", "empty"), 0)
	if err := os.MkdirAll(filepath.Join(parent, "rec", "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(parent, "sibling"), 99999)

	got, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec")
	if err != nil {
		t.Fatalf("SizeTreeAt: %v", err)
	}
	if want := (TreeSize{Bytes: 4317, Complete: true}); got != want {
		t.Fatalf("SizeTreeAt = %+v, want %+v", got, want)
	}
}

func TestSizeTreeAtMeasuresARegularLeaf(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "leaf"), 42)
	got, err := SizeTreeAt(context.Background(), openParent(t, parent), "leaf")
	if err != nil || got != (TreeSize{Bytes: 42, Complete: true}) {
		t.Fatalf("SizeTreeAt(leaf) = %+v, %v; want 42 complete", got, err)
	}
}

// TestSizeTreeAtAbsentIsKnownZero: a tree that is not there has no files, which is an
// exact zero rather than an unknown.
func TestSizeTreeAtAbsentIsKnownZero(t *testing.T) {
	got, err := SizeTreeAt(context.Background(), openParent(t, t.TempDir()), "missing")
	if err != nil || got != (TreeSize{Complete: true}) {
		t.Fatalf("SizeTreeAt(missing) = %+v, %v; want exact zero", got, err)
	}
}

// TestSizeTreeAtDoesNotCountSymlinkTargets: a symlink inside the tree is never
// followed, whether it names a large file or a directory outside the tree.
func TestSizeTreeAtDoesNotCountSymlinkTargets(t *testing.T) {
	parent := t.TempDir()
	outside := t.TempDir()
	writeSized(t, filepath.Join(outside, "big"), 1<<16)
	writeSized(t, filepath.Join(outside, "dir", "inner"), 1<<12)
	writeSized(t, filepath.Join(parent, "rec", "own"), 5)
	requireSymlinks(t, filepath.Join(outside, "big"), filepath.Join(parent, "rec", "to-file"))
	requireSymlinks(t, filepath.Join(outside, "dir"), filepath.Join(parent, "rec", "to-dir"))

	got, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec")
	if err != nil || got != (TreeSize{Bytes: 5, Complete: true}) {
		t.Fatalf("SizeTreeAt = %+v, %v; want only the 5 owned bytes", got, err)
	}
	// A tree that is itself a symlink measures the link, not its target.
	requireSymlinks(t, filepath.Join(outside, "dir"), filepath.Join(parent, "linked"))
	got, err = SizeTreeAt(context.Background(), openParent(t, parent), "linked")
	if err != nil || got != (TreeSize{Complete: true}) {
		t.Fatalf("SizeTreeAt(symlink) = %+v, %v; want exact zero", got, err)
	}
}

// TestSizeTreeAtPropagatesCancellation: a cancelled walk is an error, never a
// successful partial measurement.
func TestSizeTreeAtPropagatesCancellation(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "rec", "a"), 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := SizeTreeAt(ctx, openParent(t, parent), "rec"); !errors.Is(err, context.Canceled) {
		t.Fatalf("SizeTreeAt(cancelled) = %+v, %v; want context.Canceled", got, err)
	}
}

// cancelAfterCtx reports cancellation once Err has been consulted n times, so a test
// can cancel a walk part-way without a hook in the code under test.
type cancelAfterCtx struct {
	context.Context
	n int
}

func (c *cancelAfterCtx) Err() error {
	if c.n <= 0 {
		return context.Canceled
	}
	c.n--
	return nil
}

// TestSizeTreeAtPropagatesMidWalkCancellation: cancellation observed inside a nested
// directory unwinds the whole walk as an error rather than returning the lengths
// summed so far.
func TestSizeTreeAtPropagatesMidWalkCancellation(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"a", "b", "c", "sub/d", "sub/e", "sub/deeper/f"} {
		writeSized(t, filepath.Join(parent, "rec", filepath.FromSlash(name)), 10)
	}
	for n := 1; n <= 6; n++ {
		ctx := &cancelAfterCtx{Context: context.Background(), n: n}
		got, err := SizeTreeAt(ctx, openParent(t, parent), "rec")
		if !errors.Is(err, context.Canceled) || got != (TreeSize{}) {
			t.Fatalf("cancelled after %d checks: SizeTreeAt = %+v, %v; want context.Canceled and no size", n, got, err)
		}
	}
}

func TestSizeTreeAtRejectsAPathAsName(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "rec", "a"), 10)
	if _, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec/a"); err == nil {
		t.Fatal("a multi-component name must be refused, not walked")
	}
}
