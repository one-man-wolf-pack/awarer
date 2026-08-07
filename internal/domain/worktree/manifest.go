package worktree

import (
	"context"
	"errors"
	"fmt"
)

// ManifestRecord is one path-keyed unit of a scanned tree's manifest: either a
// regular Entry or a SkippedInput, never both and never neither. It is the unit a
// ManifestCursor yields and a TreeReducer folds. Its fields are unexported and it is
// built only through EntryRecord or SkippedRecord, so an impossible record (carrying
// an entry and a skip, or carrying nothing) cannot be assembled.
type ManifestRecord struct {
	path    RelPath
	skipped bool
	entry   Entry
	skip    SkippedInput
}

// EntryRecord wraps a scanned entry as a manifest record keyed by its path.
func EntryRecord(e Entry) ManifestRecord {
	return ManifestRecord{path: e.Path, skipped: false, entry: e}
}

// SkippedRecord wraps a skipped input as a manifest record keyed by its path.
func SkippedRecord(s SkippedInput) ManifestRecord {
	return ManifestRecord{path: s.Path, skipped: true, skip: s}
}

// Path returns the project-relative path this record is keyed by. It is the
// entry's or skip's own path, so it can never disagree with the wrapped value.
func (r ManifestRecord) Path() RelPath { return r.path }

// IsZero reports whether r is the zero (unset) record.
func (r ManifestRecord) IsZero() bool { return r.path.IsZero() }

// IsSkipped reports whether the record wraps a skipped input rather than an entry.
func (r ManifestRecord) IsSkipped() bool { return r.skipped }

// Entry returns the wrapped entry and true when this record is an entry; it
// returns the zero entry and false for a skip or the zero record.
func (r ManifestRecord) Entry() (Entry, bool) {
	if r.skipped || r.path.IsZero() {
		return Entry{}, false
	}
	return r.entry, true
}

// Skipped returns the wrapped skipped input and true when this record is a skip;
// it returns the zero skip and false otherwise.
func (r ManifestRecord) Skipped() (SkippedInput, bool) {
	if !r.skipped {
		return SkippedInput{}, false
	}
	return r.skip, true
}

// ManifestCursor is a pull iterator over manifest records in canonical RelPath
// order. It is the stream boundary that replaces handing around a fully
// materialized []Entry/[]SkippedInput pair.
//
// Contract:
//   - Next advances to the next record and reports whether one is available. It
//     returns false at end of stream and on error; the two cases are told apart by
//     Err.
//   - Record is valid only immediately after Next returned true.
//   - Err returns the first error encountered; it is sticky, so once non-nil it
//     stays non-nil and Next keeps returning false. A clean end of stream leaves
//     Err nil.
//   - Close releases any file descriptors or temp resources and is safe to call
//     more than once. Callers should Close even after early termination (breaking
//     out of the loop before the stream is exhausted).
//
// Ordering and duplicate-path rejection are not optional: a cursor either yields
// records in strictly ascending RelPath order or fails loudly through Err. Wrap a
// source cursor with Ordered to obtain that guarantee.
type ManifestCursor interface {
	Next() bool
	Record() ManifestRecord
	Err() error
	Close() error
}

// ManifestStream opens fresh cursors over the same underlying manifest. It is
// re-openable so a state can be compared (one cursor per side) without forcing the
// records into a slice, and so a later pass (verification, diff) can re-walk it.
type ManifestStream interface {
	Open(ctx context.Context) (ManifestCursor, error)
}

// ErrManifestMissing reports that a committed record's manifest payload is absent — its
// owning metadata reads, but the manifest file it promised is gone. It is a distinct,
// payload-level fact from a structurally corrupt manifest (a decode failure, truncation,
// or integrity mismatch), so a consumer can tell an absent payload from corrupt content.
// Stores still tag a missing manifest with their ErrCorruptStore sentinel as well, so
// integrity-oriented callers keep treating a partial store as durable damage.
var ErrManifestMissing = errors.New("manifest payload is missing")

// ErrOutOfOrder reports a manifest record whose path sorts before its predecessor;
// the stream is not in canonical order.
var ErrOutOfOrder = errors.New("manifest records out of canonical order")

// ErrDuplicatePath reports the same path appearing twice in a manifest (including
// an entry and a skip colliding on one path).
var ErrDuplicatePath = errors.New("duplicate path in manifest")

// orderedCursor enforces strictly ascending RelPath order and rejects duplicate
// paths. It only ever compares each record to its immediate predecessor, so it
// adds O(1) memory regardless of stream length — adjacency suffices because the
// order it enforces is total.
type orderedCursor struct {
	inner ManifestCursor
	have  bool
	prev  RelPath
	cur   ManifestRecord
	err   error
	done  bool
}

// Ordered wraps a source cursor so it yields records in canonical order or fails
// loudly. A nil cursor is returned unchanged.
func Ordered(c ManifestCursor) ManifestCursor {
	if c == nil {
		return nil
	}
	if _, ok := c.(*orderedCursor); ok {
		return c
	}
	return &orderedCursor{inner: c}
}

func (o *orderedCursor) Next() bool {
	if o.done {
		return false
	}
	if !o.inner.Next() {
		o.err = o.inner.Err()
		o.done = true
		return false
	}
	rec := o.inner.Record()
	if o.have {
		switch {
		case rec.Path().Equal(o.prev):
			o.err = fmt.Errorf("%w: %q", ErrDuplicatePath, rec.Path())
			o.done = true
			return false
		case rec.Path().Less(o.prev):
			o.err = fmt.Errorf("%w: %q after %q", ErrOutOfOrder, rec.Path(), o.prev)
			o.done = true
			return false
		}
	}
	o.have = true
	o.prev = rec.Path()
	o.cur = rec
	return true
}

func (o *orderedCursor) Record() ManifestRecord { return o.cur }

func (o *orderedCursor) Err() error { return o.err }

func (o *orderedCursor) Close() error { return o.inner.Close() }

// sliceCursor is the named bounded in-memory cursor: it yields records from a
// precomputed slice. It is the primitive fixtures and small in-memory adapters build
// on — not a primary durable contract. It does not enforce ordering on its own; wrap
// it with Ordered for that.
type sliceCursor struct {
	records []ManifestRecord
	i       int
	cur     ManifestRecord
	closed  bool
}

// NewSliceCursor returns a cursor over an in-memory slice of records. The slice is
// not copied; the caller must not mutate it while the cursor is in use.
func NewSliceCursor(records []ManifestRecord) ManifestCursor {
	return &sliceCursor{records: records, i: -1}
}

func (s *sliceCursor) Next() bool {
	if s.closed {
		return false
	}
	s.i++
	if s.i >= len(s.records) {
		return false
	}
	s.cur = s.records[s.i]
	return true
}

func (s *sliceCursor) Record() ManifestRecord { return s.cur }

func (s *sliceCursor) Err() error { return nil }

func (s *sliceCursor) Close() error {
	s.closed = true
	return nil
}
