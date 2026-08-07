package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/infra/sqliteindex"
)

// indexFile returns the worktree index database path under a project root.
func indexFile(root string) string {
	return filepath.Join(root, ".awa", "indexes", sqliteindex.FileName)
}

// TestChangesDoesNotWriteWorktreeIndex proves the comparison commands are read-only:
// with the index absent, "changes" falls back to re-hashing, still reports the
// modification correctly, and never recreates the index as a persist side-effect. A
// comparison compares state; it must not warm durable state.
func TestChangesDoesNotWriteWorktreeIndex(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n")

	// The checkpoint above warmed a mutable index; remove it so the read-only path
	// runs with no index at all (the unavailable-index fallback).
	if err := os.RemoveAll(filepath.Join(root, ".awa", "indexes")); err != nil {
		t.Fatalf("removing index dir: %v", err)
	}

	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "M calc/calc.go") {
		t.Errorf("stdout = %q, want M calc/calc.go (re-hash fallback must still detect the change)", stdout)
	}
	if _, err := os.Stat(indexFile(root)); !os.IsNotExist(err) {
		t.Errorf("changes recreated the worktree index (%v); a read-only comparison must not persist", err)
	}
}

// TestChangesLeavesCorruptIndexUntouched proves a corrupt read-only index degrades to
// a re-hash miss — the comparison still succeeds — and is never rebuilt or rewritten
// by the read-only open.
func TestChangesLeavesCorruptIndexUntouched(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")

	// Replace the warmed index (and drop any WAL/SHM sidecars) with garbage so the
	// read-only open must classify it as corrupt and fall back.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(indexFile(root) + suffix)
	}
	const garbage = "not a sqlite database"
	if err := os.WriteFile(indexFile(root), []byte(garbage), 0o600); err != nil {
		t.Fatalf("corrupting index: %v", err)
	}

	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "M calc/calc.go") {
		t.Errorf("stdout = %q, want M calc/calc.go (corrupt-index fallback must still detect the change)", stdout)
	}
	got, err := os.ReadFile(indexFile(root))
	if err != nil {
		t.Fatalf("reading index after changes: %v", err)
	}
	if string(got) != garbage {
		t.Errorf("read-only comparison rewrote a corrupt index file: %q", got)
	}
}
