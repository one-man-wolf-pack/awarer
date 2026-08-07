package compare_test

import (
	"errors"
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

func reg(t *testing.T, path, hex string) worktree.Entry {
	t.Helper()
	return regular(t, path, hex, worktree.StorageBlob)
}

// streamChanges drains a ChangeCursor into a slice of changes.
func streamChanges(t *testing.T, cur compare.ChangeCursor) []compare.Change {
	t.Helper()
	var got []compare.Change
	for cur.Next() {
		got = append(got, cur.Change())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("cursor err: %v", err)
	}
	return got
}

// TestCompareStreamYieldsCanonicalSequence pins what the streaming cursor emits for a
// mixed comparison: one modify, one add, one delete, one unchanged path, in ascending
// primary-path order. The expected sequence is written out rather than derived from a
// second comparison, so the check has an oracle independent of the code it covers.
func TestCompareStreamYieldsCanonicalSequence(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{
		reg(t, "a.go", hexA), reg(t, "b.go", hexA), reg(t, "d.go", hexA),
	}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{
		reg(t, "a.go", hexB), reg(t, "c.go", hexA), reg(t, "d.go", hexA),
	}, nil)

	cur, err := compare.CompareStream(left, right, compare.Options{})
	if err != nil {
		t.Fatalf("CompareStream: %v", err)
	}
	defer func() { _ = cur.Close() }()
	got := streamChanges(t, cur)

	want := []struct {
		path   string
		status compare.Status
	}{
		{"a.go", compare.Modified},
		{"b.go", compare.Deleted},
		{"c.go", compare.Added},
	}
	if len(got) != len(want) {
		t.Fatalf("stream produced %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].PrimaryPath().String() != w.path || got[i].Status != w.status {
			t.Fatalf("change %d = %s/%s, want %s/%s", i,
				got[i].PrimaryPath(), got[i].Status, w.path, w.status)
		}
	}
}

// TestCompareStreamOneSideExhausted covers a left-only (all deletes) and a
// right-only (all adds) comparison, where one cursor ends immediately.
func TestCompareStreamOneSideExhausted(t *testing.T) {
	populated := []worktree.Entry{reg(t, "a.go", hexA), reg(t, "b.go", hexA)}

	dels, err := compare.CompareToChangeSet(
		scantest.CanonicalCursor(populated, nil), scantest.CanonicalCursor(nil, nil), compare.Options{})
	if err != nil {
		t.Fatalf("deletes: %v", err)
	}
	if dels.Summary.Deleted != 2 || dels.Summary.Total() != 2 {
		t.Fatalf("want 2 deletes, got %+v", dels.Summary)
	}

	adds, err := compare.CompareToChangeSet(
		scantest.CanonicalCursor(nil, nil), scantest.CanonicalCursor(populated, nil), compare.Options{})
	if err != nil {
		t.Fatalf("adds: %v", err)
	}
	if adds.Summary.Added != 2 || adds.Summary.Total() != 2 {
		t.Fatalf("want 2 adds, got %+v", adds.Summary)
	}
}

// outOfOrderCursor yields two descending paths to prove the merge surfaces a
// non-canonical input loudly instead of mis-merging.
type outOfOrderCursor struct {
	recs []worktree.ManifestRecord
	i    int
}

func (c *outOfOrderCursor) Next() bool {
	c.i++
	return c.i <= len(c.recs)
}
func (c *outOfOrderCursor) Record() worktree.ManifestRecord { return c.recs[c.i-1] }
func (c *outOfOrderCursor) Err() error                      { return nil }
func (c *outOfOrderCursor) Close() error                    { return nil }

func TestCompareStreamRejectsOutOfOrder(t *testing.T) {
	bad := &outOfOrderCursor{recs: []worktree.ManifestRecord{
		worktree.EntryRecord(reg(t, "b.go", hexA)),
		worktree.EntryRecord(reg(t, "a.go", hexA)),
	}}
	cur, err := compare.CompareStream(bad, scantest.CanonicalCursor(nil, nil), compare.Options{})
	if err != nil {
		t.Fatalf("CompareStream: %v", err)
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
	}
	if !errors.Is(cur.Err(), worktree.ErrOutOfOrder) {
		t.Fatalf("merge over out-of-order input err = %v, want ErrOutOfOrder", cur.Err())
	}
}

// TestCompareStreamRejectsNil proves a nil input cursor is a loud error, not a
// panic when the merge first pulls it.
func TestCompareStreamRejectsNil(t *testing.T) {
	if _, err := compare.CompareStream(nil, scantest.CanonicalCursor(nil, nil), compare.Options{}); err == nil {
		t.Fatalf("CompareStream(nil, ...) returned no error")
	}
	if _, err := compare.CompareStream(scantest.CanonicalCursor(nil, nil), nil, compare.Options{}); err == nil {
		t.Fatalf("CompareStream(..., nil) returned no error")
	}
}

// TestCompareStreamPathFilterPushedIn proves path filters narrow the streamed
// changes while preserving order.
func TestCompareStreamPathFilterPushedIn(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{reg(t, "keep/a.go", hexA), reg(t, "drop/b.go", hexA)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{reg(t, "keep/a.go", hexB), reg(t, "drop/b.go", hexB)}, nil)
	cs, err := compare.CompareToChangeSet(left, right, compare.Options{PathFilters: []worktree.RelPath{relPath(t, "keep")}})
	if err != nil {
		t.Fatalf("CompareToChangeSet: %v", err)
	}
	if len(cs.Changes) != 1 || cs.Changes[0].PrimaryPath().String() != "keep/a.go" {
		t.Fatalf("path filter result = %+v", cs.Changes)
	}
}
