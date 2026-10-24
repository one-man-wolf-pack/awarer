package effectfs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func fill(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		mustWrite(t, filepath.Join(dir, "f"+strconv.Itoa(i)), "x")
	}
}

func TestWaivedRootIsNeitherDiscoveredNorRead(t *testing.T) {
	root := t.TempDir()
	fill(t, filepath.Join(root, "target", "debug"), 40)
	mustWrite(t, filepath.Join(root, "crates", "x", "target", "a"), "x")
	if runtime.GOOS != "windows" {
		locked := filepath.Join(root, "target", "debug")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	}

	w := newWalkerLimited(t, 8, 100, 100_000)
	if _, err := w.Discover(root, []string{"target"}, nil); err == nil {
		t.Fatal("the unwaived fixture must fail closed, or this test proves nothing")
	}
	got, err := w.Discover(root, []string{"target"}, []string{"target"})
	if err != nil {
		t.Fatalf("Discover with target waived: %v", err)
	}
	idx := byPath(got)
	if _, ok := idx["target"]; ok {
		t.Error("the waived root was observed")
	}
	if _, ok := idx["crates/x/target"]; !ok || len(got) != 1 {
		t.Errorf("roots = %v, want only the nested crates/x/target", idx)
	}
}

func TestWaivedDirectoryInsideAWatchedRootIsSkipped(t *testing.T) {
	root := t.TempDir()
	fill(t, filepath.Join(root, "build", "gen"), 40)
	mustWrite(t, filepath.Join(root, "build", "report.txt"), "x")

	w := newWalkerLimited(t, 8, 100, 100_000)
	if _, err := w.Discover(root, []string{"build"}, nil); err == nil {
		t.Fatal("the unwaived fixture must fail closed, or this test proves nothing")
	}
	before, err := w.Discover(root, []string{"build"}, []string{"build/gen"})
	if err != nil {
		t.Fatalf("Discover with build/gen waived: %v", err)
	}
	if len(before) != 1 || before[0].Path != "build" || before[0].Entries != 1 {
		t.Fatalf("roots = %+v, want build observed with only its unwaived entry", before)
	}

	if err := os.RemoveAll(filepath.Join(root, "build", "gen")); err != nil {
		t.Fatal(err)
	}
	afterRemoval, err := w.Discover(root, []string{"build"}, []string{"build/gen"})
	if err != nil {
		t.Fatal(err)
	}
	if afterRemoval[0].SubHash != before[0].SubHash {
		t.Error("removing only the waived directory changed the enclosing root's signature")
	}

	mustWrite(t, filepath.Join(root, "build", "report.txt"), "changed")
	if err := os.Chtimes(filepath.Join(root, "build", "report.txt"), timeAt(1), timeAt(1)); err != nil {
		t.Fatal(err)
	}
	afterSibling, err := w.Discover(root, []string{"build"}, []string{"build/gen"})
	if err != nil {
		t.Fatal(err)
	}
	if afterSibling[0].SubHash == before[0].SubHash {
		t.Error("a change beside the waived directory went unobserved")
	}
}

func TestWaiverIsExactNotABasename(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "dist", "a"), "x")
	mustWrite(t, filepath.Join(root, "web", "dist", "b"), "x")
	got, err := newWalker(t).Discover(root, []string{"dist"}, []string{"dist"})
	if err != nil {
		t.Fatal(err)
	}
	if idx := byPath(got); len(idx) != 1 || idx["web/dist"].Path == "" {
		t.Fatalf("roots = %v, want only web/dist", idx)
	}
}

func timeAt(sec int64) time.Time { return time.Unix(sec, 0) }

// Creating or removing build/middle/cache rewrites middle's own size and mtime. The
// mtime is set explicitly so the witness does not depend on timestamp granularity.
func TestWaiverBelowANestedDirectoryLeavesItsAncestorsAggregateOut(t *testing.T) {
	root := t.TempDir()
	middle := filepath.Join(root, "build", "middle")
	fill(t, filepath.Join(middle, "cache"), 3)
	mustWrite(t, filepath.Join(middle, "keep.txt"), "x")
	waived := []string{"build/middle/cache"}
	w := newWalker(t)
	sig := func(t *testing.T, waived []string) string {
		t.Helper()
		got, err := w.Discover(root, []string{"build"}, waived)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("roots = %+v, want build", got)
		}
		return got[0].SubHash.String()
	}
	touch := func(t *testing.T, sec int64) {
		t.Helper()
		if err := os.Chtimes(middle, timeAt(sec), timeAt(sec)); err != nil {
			t.Fatal(err)
		}
	}

	touch(t, 1)
	before := sig(t, waived)
	ordinary := sig(t, nil)
	touch(t, 2)
	if sig(t, nil) == ordinary {
		t.Fatal("without a waiver a directory's mtime must stay observed, or this test proves nothing")
	}

	if err := os.RemoveAll(filepath.Join(middle, "cache")); err != nil {
		t.Fatal(err)
	}
	touch(t, 3)
	if sig(t, waived) != before {
		t.Error("removing the waived directory changed the root through its parent's metadata")
	}
	fill(t, filepath.Join(middle, "cache"), 5)
	touch(t, 4)
	if sig(t, waived) != before {
		t.Error("recreating the waived directory changed the root through its parent's metadata")
	}

	mustWrite(t, filepath.Join(middle, "added.txt"), "x")
	if sig(t, waived) == before {
		t.Error("a new entry beside the waived directory went unobserved")
	}
	if err := os.Remove(filepath.Join(middle, "added.txt")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(middle, "keep.txt"), "changed")
	if sig(t, waived) == before {
		t.Error("a changed entry beside the waived directory went unobserved")
	}
	mustWrite(t, filepath.Join(middle, "keep.txt"), "x")
	if err := os.Chtimes(filepath.Join(middle, "keep.txt"), timeAt(9), timeAt(9)); err != nil {
		t.Fatal(err)
	}
	restored := sig(t, waived)

	if runtime.GOOS != "windows" {
		if err := os.Chmod(middle, 0o700); err != nil {
			t.Fatal(err)
		}
		if sig(t, waived) == restored {
			t.Error("an ancestor's permission change went unobserved")
		}
		if err := os.Chmod(middle, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.RemoveAll(middle); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, middle, "now a file")
	if sig(t, waived) == restored {
		t.Error("an ancestor replaced by a file went unobserved")
	}
}
