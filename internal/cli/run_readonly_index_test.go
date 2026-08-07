package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/sqliteindex"
)

// lazyReadOnlyIndex exposes only the read facet: the write path is gone at compile
// time, not rejected at runtime, so a read-only caller cannot begin a scan.
var _ worktree.IndexReader = (*lazyReadOnlyIndex)(nil)

// TestLazyReadOnlyIndexMissingDirFailsSoft proves the read-only acceleration wrapper
// degrades a missing index to a quiet miss (so the scanner re-hashes) rather than
// surfacing an error.
func TestLazyReadOnlyIndexMissingDirFailsSoft(t *testing.T) {
	li := &lazyReadOnlyIndex{dir: filepath.Join(t.TempDir(), "indexes")}
	defer li.Close()

	entry, ok, err := li.Lookup(context.Background(), mustRel(t, "a.go"))
	if ok || err != nil {
		t.Errorf("missing-index Lookup: ok=%v err=%v entry=%+v, want a quiet miss", ok, err, entry)
	}
}

// TestLazyReadOnlyIndexCorruptFileFailsSoft proves an unreadable/corrupt index file is
// swallowed into permanent-miss mode rather than surfacing an error to the command.
func TestLazyReadOnlyIndexCorruptFileFailsSoft(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sqliteindex.FileName), []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	li := &lazyReadOnlyIndex{dir: dir}
	defer li.Close()

	if _, ok, err := li.Lookup(context.Background(), mustRel(t, "a.go")); ok || err != nil {
		t.Errorf("corrupt-index Lookup: ok=%v err=%v, want a quiet miss", ok, err)
	}
	// A corrupt read-only open must never rebuild or otherwise rewrite the file.
	got, err := os.ReadFile(filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "not a sqlite database" {
		t.Errorf("read-only open rewrote a corrupt index file: %q", got)
	}
}
