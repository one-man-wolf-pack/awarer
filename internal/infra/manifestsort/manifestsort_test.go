package manifestsort

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

func testHasher(t *testing.T) hashing.Hasher {
	t.Helper()
	h := blake3hash.New()
	return h
}

// dirEntry builds a directory entry record keyed by path, a cheap record needing no
// content hash — enough to exercise ordering and the merge.
func dirEntry(t *testing.T, path string) worktree.ManifestRecord {
	t.Helper()
	p, err := worktree.ParseRelPath(path)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", path, err)
	}
	e, err := worktree.NewDirEntry(p, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewDirEntry(%q): %v", path, err)
	}
	return worktree.EntryRecord(e)
}

// drain collects a stream's records in order, failing on any cursor error.
func drain(t *testing.T, s worktree.ManifestStream) []string {
	t.Helper()
	cur, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var out []string
	for cur.Next() {
		out = append(out, cur.Record().Path().String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("cursor err: %v", err)
	}
	return out
}

func TestSorterInMemoryOrders(t *testing.T) {
	h := testHasher(t)
	s := New(0, "") // default cap: everything fits in memory
	// Add out of order, including the DFS-vs-canonical case ("a" dir, "a/b", "a.txt").
	for _, p := range []string{"z", "a/b", "a.txt", "a", "m"} {
		if err := s.Add(dirEntry(t, p)); err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
	}
	sorted, err := s.Finish(h)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	defer func() { _ = sorted.Close() }()
	if sorted.Spilled() {
		t.Error("small input must not spill to disk")
	}
	got := drain(t, sorted.Stream())
	want := []string{"a", "a.txt", "a/b", "m", "z"}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want canonical %v", got, want)
		}
	}
	if sorted.Reduction().Count != len(want) {
		t.Errorf("reduction count = %d, want %d", sorted.Reduction().Count, len(want))
	}
}

// TestSorterSpillMatchesInMemory proves the external (spill+merge) path produces the
// exact same canonical order and tree reduction as the in-memory path — the parity
// that keeps the tree hash independent of whether a scan spilled.
func TestSorterSpillMatchesInMemory(t *testing.T) {
	h := testHasher(t)
	paths := []string{"z", "a/b", "a.txt", "a", "m", "b", "b/c", "b/a", "q", "d/e/f", "d", "d/e"}

	// In-memory reference.
	ref := New(0, "")
	for _, p := range paths {
		if err := ref.Add(dirEntry(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	refSorted, err := ref.Finish(h)
	if err != nil {
		t.Fatalf("ref Finish: %v", err)
	}
	defer func() { _ = refSorted.Close() }()
	refOrder := drain(t, refSorted.Stream())

	// Spill path: a tiny buffer forces several runs.
	spill := New(3, "")
	for _, p := range paths {
		if err := spill.Add(dirEntry(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	spillSorted, err := spill.Finish(h)
	if err != nil {
		t.Fatalf("spill Finish: %v", err)
	}
	defer func() { _ = spillSorted.Close() }()

	if !spillSorted.Spilled() {
		t.Fatal("a tiny buffer over many records must spill")
	}
	spillOrder := drain(t, spillSorted.Stream())

	if len(refOrder) != len(spillOrder) {
		t.Fatalf("spill order len %d != in-memory %d", len(spillOrder), len(refOrder))
	}
	for i := range refOrder {
		if refOrder[i] != spillOrder[i] {
			t.Fatalf("spill order %v != in-memory %v", spillOrder, refOrder)
		}
	}
	if refSorted.Reduction().Hash != spillSorted.Reduction().Hash {
		t.Errorf("spill tree hash %s != in-memory %s", spillSorted.Reduction().Hash, refSorted.Reduction().Hash)
	}
	if refSorted.Reduction().Count != spillSorted.Reduction().Count {
		t.Errorf("spill count %d != in-memory %d", spillSorted.Reduction().Count, refSorted.Reduction().Count)
	}
}

// TestSorterStreamReopenable proves the result stream can be opened more than once
// (comparison opens one cursor per side; checkpoint re-walks for materialization).
func TestSorterStreamReopenable(t *testing.T) {
	h := testHasher(t)
	s := New(2, "") // force spill
	for _, p := range []string{"c", "a", "b", "d", "e"} {
		if err := s.Add(dirEntry(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	sorted, err := s.Finish(h)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	defer func() { _ = sorted.Close() }()
	first := drain(t, sorted.Stream())
	second := drain(t, sorted.Stream())
	if len(first) != 5 || len(second) != 5 {
		t.Fatalf("reopen drained %d then %d, want 5 each", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("reopen order differs: %v vs %v", first, second)
		}
	}
}

// TestSorterSpillsUnderProvidedRoot proves scan spill stays inside the awa-owned
// spill root (e.g. .awa/store/tmp) rather than the world-readable OS temp dir.
func TestSorterSpillsUnderProvidedRoot(t *testing.T) {
	h := testHasher(t)
	root := filepath.Join(t.TempDir(), "state", "store", "tmp")
	s := New(2, root) // force spill under root (which does not exist yet)
	for _, p := range []string{"c", "a", "b", "d"} {
		if err := s.Add(dirEntry(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	sorted, err := s.Finish(h)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	defer func() { _ = sorted.Close() }()
	if !sorted.Spilled() {
		t.Fatal("expected the sort to spill")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read spill root: %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("no spill dir created under the provided root %s", root)
	}
	// The merged manifest file left in the spill dir must be owner-private, so worktree
	// manifest data does not rely on the directory mode alone.
	if runtime.GOOS != "windows" {
		manifest := filepath.Join(root, entries[0].Name(), "manifest.jsonl")
		info, err := os.Stat(manifest)
		if err != nil {
			t.Fatalf("stat merged manifest: %v", err)
		}
		if got := info.Mode().Perm(); got != paths.FilePerm {
			t.Errorf("spill manifest mode = %o, want %o", got, paths.FilePerm)
		}
	}
}

// TestSorterCloseRemovesTempFiles proves the spill path cleans up its temp directory.
func TestSorterCloseRemovesTempFiles(t *testing.T) {
	h := testHasher(t)
	s := New(2, "")
	for _, p := range []string{"c", "a", "b", "d"} {
		if err := s.Add(dirEntry(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	sorted, err := s.Finish(h)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if !sorted.Spilled() {
		t.Fatal("expected spill")
	}
	if _, err := os.Stat(sorted.dir); err != nil {
		t.Fatalf("temp dir should exist before Close: %v", err)
	}
	if err := sorted.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sorted.dir); !os.IsNotExist(err) {
		t.Errorf("temp dir should be removed after Close, stat err = %v", err)
	}
	// Close is idempotent.
	if err := sorted.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSorterRejectsDuplicatePath proves a duplicate path fails loudly on drain (the
// Ordered guard), so a buggy caller cannot corrupt a tree hash.
func TestSorterRejectsDuplicatePath(t *testing.T) {
	h := testHasher(t)
	s := New(0, "")
	if err := s.Add(dirEntry(t, "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(dirEntry(t, "a")); err != nil {
		t.Fatal(err)
	}
	// Finish reduces over the sorted slice, where Ordered rejects the duplicate.
	if _, err := s.Finish(h); err == nil {
		t.Fatal("expected a duplicate-path error from Finish")
	}
}

func TestSorterEmpty(t *testing.T) {
	h := testHasher(t)
	sorted, err := New(0, "").Finish(h)
	if err != nil {
		t.Fatalf("Finish empty: %v", err)
	}
	defer func() { _ = sorted.Close() }()
	if got := drain(t, sorted.Stream()); len(got) != 0 {
		t.Errorf("empty sorter drained %v, want none", got)
	}
	if sorted.Reduction().Count != 0 {
		t.Errorf("empty count = %d, want 0", sorted.Reduction().Count)
	}
}
