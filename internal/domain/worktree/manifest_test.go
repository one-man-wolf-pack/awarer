package worktree_test

import (
	"errors"
	"testing"

	"awarer/internal/domain/worktree"
)

// skippedLarge builds a large-file skip for a regular path, used as a manifest
// record fixture distinct from an entry.
func skippedLarge(t *testing.T, path string, size int64) worktree.SkippedInput {
	t.Helper()
	s, err := worktree.NewSkippedInput(
		mustPath(t, path),
		worktree.ReasonLargeFileSkipPolicy,
		size,
		worktree.StatSignature{Mode: 0o644},
		"",
		worktree.SymlinkTarget{},
		worktree.TraversalInfo{},
	)
	if err != nil {
		t.Fatalf("NewSkippedInput(%q): %v", path, err)
	}
	return s
}

// drain pulls every record from a cursor into a slice and returns the cursor's
// terminal error.
func drain(c worktree.ManifestCursor) ([]worktree.ManifestRecord, error) {
	var got []worktree.ManifestRecord
	for c.Next() {
		got = append(got, c.Record())
	}
	return got, c.Err()
}

// TestManifestRecordDiscriminates proves a record wraps exactly one of an entry or
// a skip, keyed by that value's own path.
func TestManifestRecordDiscriminates(t *testing.T) {
	e := regular(t, "a.go", hexA)
	er := worktree.EntryRecord(e)
	if er.IsSkipped() {
		t.Errorf("entry record reports IsSkipped")
	}
	if got, ok := er.Entry(); !ok || !got.Path.Equal(e.Path) {
		t.Errorf("entry record did not yield its entry: ok=%v", ok)
	}
	if _, ok := er.Skipped(); ok {
		t.Errorf("entry record yielded a skip")
	}
	if !er.Path().Equal(e.Path) {
		t.Errorf("entry record path = %q, want %q", er.Path(), e.Path)
	}

	s := skippedLarge(t, "big.bin", 1<<20)
	sr := worktree.SkippedRecord(s)
	if !sr.IsSkipped() {
		t.Errorf("skip record does not report IsSkipped")
	}
	if got, ok := sr.Skipped(); !ok || !got.Path.Equal(s.Path) {
		t.Errorf("skip record did not yield its skip: ok=%v", ok)
	}
	if _, ok := sr.Entry(); ok {
		t.Errorf("skip record yielded an entry")
	}

	var zero worktree.ManifestRecord
	if !zero.IsZero() {
		t.Errorf("zero record does not report IsZero")
	}
	if _, ok := zero.Entry(); ok {
		t.Errorf("zero record yielded an entry")
	}
}

// TestSliceCursorEmpty proves an empty cursor yields nothing and no error.
func TestSliceCursorEmpty(t *testing.T) {
	c := worktree.NewSliceCursor(nil)
	got, err := drain(c)
	if err != nil {
		t.Fatalf("empty cursor Err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty cursor yielded %d records", len(got))
	}
}

// TestSliceCursorSingle proves a one-record cursor yields exactly that record.
func TestSliceCursorSingle(t *testing.T) {
	c := worktree.NewSliceCursor([]worktree.ManifestRecord{worktree.EntryRecord(regular(t, "a.go", hexA))})
	got, err := drain(c)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != 1 || got[0].Path().String() != "a.go" {
		t.Fatalf("unexpected records: %+v", got)
	}
}

// TestOrderedRejectsOutOfOrder proves a descending pair fails loudly with
// ErrOutOfOrder and is sticky.
func TestOrderedRejectsOutOfOrder(t *testing.T) {
	raw := worktree.NewSliceCursor([]worktree.ManifestRecord{
		worktree.EntryRecord(regular(t, "b.go", hexA)),
		worktree.EntryRecord(regular(t, "a.go", hexB)),
	})
	c := worktree.Ordered(raw)
	if !c.Next() {
		t.Fatalf("first Next returned false: %v", c.Err())
	}
	if c.Next() {
		t.Fatalf("out-of-order record was accepted")
	}
	if !errors.Is(c.Err(), worktree.ErrOutOfOrder) {
		t.Fatalf("Err = %v, want ErrOutOfOrder", c.Err())
	}
	// Sticky: further Next stays false and the error is unchanged.
	if c.Next() {
		t.Fatalf("Next returned true after error")
	}
	if !errors.Is(c.Err(), worktree.ErrOutOfOrder) {
		t.Fatalf("Err drifted to %v", c.Err())
	}
}

// TestOrderedRejectsDuplicatePath proves the same path twice fails with
// ErrDuplicatePath, including an entry/skip collision.
func TestOrderedRejectsDuplicatePath(t *testing.T) {
	cases := map[string]worktree.ManifestCursor{
		"two entries": worktree.NewSliceCursor([]worktree.ManifestRecord{
			worktree.EntryRecord(regular(t, "a.go", hexA)),
			worktree.EntryRecord(regular(t, "a.go", hexB)),
		}),
		"entry and skip": worktree.NewSliceCursor([]worktree.ManifestRecord{
			worktree.EntryRecord(regular(t, "a.go", hexA)),
			worktree.SkippedRecord(skippedLarge(t, "a.go", 9)),
		}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c := worktree.Ordered(raw)
			_, err := drain(c)
			if !errors.Is(err, worktree.ErrDuplicatePath) {
				t.Fatalf("Err = %v, want ErrDuplicatePath", err)
			}
		})
	}
}

// TestOrderedPassesCanonicalStream proves a well-ordered stream is yielded intact.
func TestOrderedPassesCanonicalStream(t *testing.T) {
	c := worktree.Ordered(worktree.NewSliceCursor([]worktree.ManifestRecord{
		worktree.EntryRecord(regular(t, "a.go", hexA)),
		worktree.EntryRecord(regular(t, "b.go", hexB)),
	}))
	got, err := drain(c)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
}

// closeCounter records how many times Close was called on a wrapped cursor.
type closeCounter struct {
	worktree.ManifestCursor
	closes int
}

func (c *closeCounter) Close() error {
	c.closes++
	return c.ManifestCursor.Close()
}

// TestOrderedCloseIdempotentAndDelegates proves Close is safe to call repeatedly
// and reaches the wrapped cursor.
func TestOrderedCloseIdempotentAndDelegates(t *testing.T) {
	inner := &closeCounter{ManifestCursor: worktree.NewSliceCursor(nil)}
	c := worktree.Ordered(inner)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if inner.closes != 2 {
		t.Fatalf("inner Close called %d times, want 2", inner.closes)
	}
}

// TestSliceCursorClosedStops proves a closed cursor yields nothing further, so
// early termination releases the iterator.
func TestSliceCursorClosedStops(t *testing.T) {
	c := worktree.NewSliceCursor([]worktree.ManifestRecord{
		worktree.EntryRecord(regular(t, "a.go", hexA)),
		worktree.EntryRecord(regular(t, "b.go", hexB)),
	})
	if !c.Next() {
		t.Fatalf("first Next false")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.Next() {
		t.Fatalf("Next returned true after Close")
	}
}

// errCursor yields one record then reports a sticky error, to prove Ordered
// surfaces the inner error on exhaustion.
type errCursor struct {
	yielded bool
	err     error
}

func (e *errCursor) Next() bool {
	if e.yielded {
		return false
	}
	e.yielded = true
	return true
}
func (e *errCursor) Record() worktree.ManifestRecord {
	return worktree.EntryRecord(worktree.Entry{Path: mustPathPkg("a.go")})
}
func (e *errCursor) Err() error   { return e.err }
func (e *errCursor) Close() error { return nil }

func mustPathPkg(s string) worktree.RelPath {
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		panic(err)
	}
	return p
}

// TestOrderedSurfacesInnerError proves an inner cursor's terminal error propagates
// through Ordered.
func TestOrderedSurfacesInnerError(t *testing.T) {
	want := errors.New("boom")
	c := worktree.Ordered(&errCursor{err: want})
	_, err := drain(c)
	if !errors.Is(err, want) {
		t.Fatalf("Err = %v, want %v", err, want)
	}
}
