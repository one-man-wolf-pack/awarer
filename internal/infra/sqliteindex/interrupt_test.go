package sqliteindex_test

import (
	"context"
	"testing"
	"time"

	"awarer/internal/infra/sqliteindex"
)

// TestScanInterruptedThenReRunCommits proves the index transaction is atomic across
// an interruption: an aborted scan leaves no completed partial state, and a fresh
// scan afterward commits and is visible — recovery needs no manual cleanup. The
// existing TestRollbackLeavesNoState and TestInterruptedScanClearedOnReopen cover
// the no-partial-state half; this pins the recovery half end to end.
func TestScanInterruptedThenReRunCommits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	// An interrupted scan: rows upserted, never committed.
	tx, err := idx.BeginScan(ctx, meta(t, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if _, ok, err := idx.Lookup(ctx, relPath(t, "a.go")); ok || err != nil {
		t.Fatalf("interrupted scan left visible state: ok=%v err=%v", ok, err)
	}
	_ = idx.Close()

	// A fresh process (reopen) re-runs the scan to completion and recovers.
	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer idx2.Close()

	tx2, err := idx2.BeginScan(ctx, meta(t, 2000))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx, treeHash(t, "tree"), time.Unix(2, 0)); err != nil {
		t.Fatalf("re-run Commit: %v", err)
	}
	if _, ok, err := idx2.Lookup(ctx, relPath(t, "a.go")); !ok || err != nil {
		t.Fatalf("entry not visible after recovery: ok=%v err=%v", ok, err)
	}
}
