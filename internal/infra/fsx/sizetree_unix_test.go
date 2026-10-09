//go:build darwin || linux || freebsd

package fsx

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// These cases rest on Unix permission bits and special nodes; the portable contract
// (sizes, absence, symlinks, cancellation) is proved in sizetree_test.go.

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
}

// TestSizeTreeAtReadsMetadataOnly: a file no one may open is still measured, which is
// only possible if the walk never opens payloads.
func TestSizeTreeAtReadsMetadataOnly(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "rec", "sealed")
	writeSized(t, path, 123)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	got, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec")
	if err != nil || got != (TreeSize{Bytes: 123, Complete: true}) {
		t.Fatalf("SizeTreeAt = %+v, %v; want 123 complete", got, err)
	}
}

// TestSizeTreeAtUnreadableSubdirIsPartial: a descendant that cannot be listed leaves
// the established lengths as a lower bound and does not stop the rest of the walk.
func TestSizeTreeAtUnreadableSubdirIsPartial(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "rec", "a-before"), 7)
	writeSized(t, filepath.Join(parent, "rec", "locked", "hidden"), 1000)
	writeSized(t, filepath.Join(parent, "rec", "z-after"), 11)
	locked := filepath.Join(parent, "rec", "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec")
	if err != nil || got != (TreeSize{Bytes: 18, Complete: false}) {
		t.Fatalf("SizeTreeAt = %+v, %v; want 18 partial", got, err)
	}
}

// TestSizeTreeAtUnopenableRootIsAnError: when the tree itself cannot be listed nothing
// was established, which is distinct from both a known zero and a partial sum.
func TestSizeTreeAtUnopenableRootIsAnError(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "rec", "a"), 7)
	rec := filepath.Join(parent, "rec")
	if err := os.Chmod(rec, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rec, 0o755) })
	if got, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec"); err == nil {
		t.Fatalf("SizeTreeAt(unopenable) = %+v; want an error", got)
	}
}

// TestSizeTreeAtSpecialNodesContributeNothing: a FIFO has no logical file length.
func TestSizeTreeAtSpecialNodesContributeNothing(t *testing.T) {
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "rec", "a"), 9)
	if err := syscall.Mkfifo(filepath.Join(parent, "rec", "fifo"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	got, err := SizeTreeAt(context.Background(), openParent(t, parent), "rec")
	if err != nil || got != (TreeSize{Bytes: 9, Complete: true}) {
		t.Fatalf("SizeTreeAt = %+v, %v; want 9 complete", got, err)
	}
}

// TestSizeTreeWithinUnixBoundaries: the bounded walk keeps the unbounded walk's
// metadata-only and special-node rules, and an unreadable directory is a typed
// incompleteness distinct from the work limits.
func TestSizeTreeWithinUnixBoundaries(t *testing.T) {
	skipIfRoot(t)
	parent := t.TempDir()
	writeSized(t, filepath.Join(parent, "awa", "sealed"), 123)
	if err := os.Chmod(filepath.Join(parent, "awa", "sealed"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(parent, "awa", "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(parent, "awa", "locked", "hidden"), 1000)
	locked := filepath.Join(parent, "awa", "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got, err := SizeTreeWithin(context.Background(), openParent(t, parent), "awa", TreeLimits{MaxEntries: 100, MaxDepth: 8})
	if err != nil {
		t.Fatalf("SizeTreeWithin: %v", err)
	}
	if got.Bytes != 123 || !got.Unreadable || got.EntryBudgetReached || got.DepthLimitReached {
		t.Fatalf("SizeTreeWithin = %+v, want 123 bytes with only the unreadable flag", got)
	}
}
