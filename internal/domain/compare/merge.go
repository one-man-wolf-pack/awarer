package compare

import (
	"fmt"

	"awarer/internal/domain/worktree"
)

// ChangeCursor is the streaming result of comparing two states: a pull iterator
// over Changes in canonical order. It is the base contract; the materialized
// ChangeSet (CompareToChangeSet, Compare) is a convenience adapter over it.
//
// The contract mirrors worktree.ManifestCursor: Next advances and reports whether
// a change is available, Change is valid only after Next returned true, Err is
// sticky and tells a clean end of stream from an error, and Close releases the
// underlying manifest cursors and is safe to call more than once.
type ChangeCursor interface {
	Next() bool
	Change() Change
	Err() error
	Close() error
}

// CompareStream compares left (old) against right (new) by merging two canonical
// manifest cursors and returns a ChangeCursor. The base path is a sorted k=2
// merge — no maps — so memory does not scale with manifest size. Path filters are
// pushed into the merge. Rename detection is the one exception: it is a whole-set
// stage that buffers the change set to pair, capped by a bounded presentation policy
// (see renameCursor) — over the limit it skips pairing rather than grow unbounded, so
// memory stays bounded either way; --no-renames skips the stage entirely.
//
// The caller owns left and right and must not consume them elsewhere; the returned
// cursor's Close closes them.
func CompareStream(left, right worktree.ManifestCursor, opts Options) (ChangeCursor, error) {
	return compareStreamLimited(left, right, opts, defaultRenameBufferLimit)
}

// compareStreamLimited is CompareStream parameterized by the rename buffer limit, so a
// test can drive the bounded presentation policy with a small limit. Production callers
// go through CompareStream with the default limit.
func compareStreamLimited(left, right worktree.ManifestCursor, opts Options, limit int) (ChangeCursor, error) {
	if left == nil || right == nil {
		return nil, fmt.Errorf("compare: nil manifest cursor")
	}
	base := &mergeCursor{
		left:    worktree.Ordered(left),
		right:   worktree.Ordered(right),
		filters: opts.PathFilters,
	}
	if opts.DetectRenames {
		return newRenameCursorLimited(base, limit)
	}
	return base, nil
}

// CompareToChangeSet drains a comparison into a materialized ChangeSet. It is the
// named bounded adapter for JSON output, tests, and small callers — its cost is
// the full set of reported changes, made explicit by the name.
func CompareToChangeSet(left, right worktree.ManifestCursor, opts Options) (ChangeSet, error) {
	cur, err := CompareStream(left, right, opts)
	if err != nil {
		return ChangeSet{}, err
	}
	defer func() { _ = cur.Close() }()
	// The rename-detection outcome is fixed once CompareStream returns (the rename stage
	// buffers eagerly), so read it before draining.
	detection := RenameDetectionOf(cur)
	var changes []Change
	for cur.Next() {
		changes = append(changes, cur.Change())
	}
	if err := cur.Err(); err != nil {
		return ChangeSet{}, err
	}
	return ChangeSet{Changes: changes, Summary: summarize(changes), RenameDetection: detection}, nil
}

// mergeCursor merges two canonical manifest cursors into a change stream. It only
// ever holds one record per side, so it adds O(1) memory regardless of manifest
// size. Without rename detection its output is already in final
// PrimaryPath-then-Status order: at any path exactly one of delete/add/present is
// emitted, and the merge advances by ascending path, so emitted changes ascend by
// PrimaryPath with no ties at a path.
type mergeCursor struct {
	left, right worktree.ManifestCursor
	filters     []worktree.RelPath

	started    bool
	lOK, rOK   bool
	lRec, rRec worktree.ManifestRecord

	cur  Change
	err  error
	done bool
}

func (m *mergeCursor) Next() bool {
	if m.done {
		return false
	}
	if !m.started {
		m.started = true
		m.lOK = m.advance(m.left, &m.lRec)
		m.rOK = m.advance(m.right, &m.rRec)
		if m.err != nil {
			m.done = true
			return false
		}
	}

	for {
		if m.err != nil {
			m.done = true
			return false
		}
		if !m.lOK && !m.rOK {
			m.done = true
			return false
		}

		var ch Change
		var changed bool
		switch {
		case !m.rOK || (m.lOK && m.lRec.Path().Less(m.rRec.Path())):
			ch, changed = deletedChange(nodeOf(m.lRec)), true
			m.lOK = m.advance(m.left, &m.lRec)
		case !m.lOK || m.rRec.Path().Less(m.lRec.Path()):
			ch, changed = addedChange(nodeOf(m.rRec)), true
			m.rOK = m.advance(m.right, &m.rRec)
		default: // equal paths: both present
			ch, changed = comparePresent(nodeOf(m.lRec), nodeOf(m.rRec))
			m.lOK = m.advance(m.left, &m.lRec)
			m.rOK = m.advance(m.right, &m.rRec)
		}
		if m.err != nil {
			m.done = true
			return false
		}
		if !changed {
			continue
		}
		if len(m.filters) > 0 && !matchesAny(ch, m.filters) {
			continue
		}
		m.cur = ch
		return true
	}
}

// advance pulls the next record from cur into dst, recording the cursor's terminal
// error (if any) on the merge. It returns whether a record was produced.
func (m *mergeCursor) advance(cur worktree.ManifestCursor, dst *worktree.ManifestRecord) bool {
	if cur.Next() {
		*dst = cur.Record()
		return true
	}
	if err := cur.Err(); err != nil && m.err == nil {
		m.err = err
	}
	return false
}

func (m *mergeCursor) Change() Change { return m.cur }

func (m *mergeCursor) Err() error { return m.err }

func (m *mergeCursor) Close() error {
	err1 := m.left.Close()
	err2 := m.right.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// nodeOf adapts a manifest record to the internal node shape the per-pair
// comparison helpers operate on.
func nodeOf(rec worktree.ManifestRecord) node {
	if e, ok := rec.Entry(); ok {
		return node{entry: e}
	}
	sk, _ := rec.Skipped()
	return node{skipped: true, skip: sk}
}

// sliceChangeCursor serves a precomputed, finally-ordered slice of changes as a
// ChangeCursor. It backs the rename stage, which must materialize to pair and sort.
type sliceChangeCursor struct {
	changes []Change
	i       int
	cur     Change
}

func (c *sliceChangeCursor) Next() bool {
	if c.i >= len(c.changes) {
		return false
	}
	c.cur = c.changes[c.i]
	c.i++
	return true
}

func (c *sliceChangeCursor) Change() Change { return c.cur }
func (c *sliceChangeCursor) Err() error     { return nil }
func (c *sliceChangeCursor) Close() error   { return nil }

// assert the two cursor shapes satisfy ChangeCursor.
var (
	_ ChangeCursor = (*mergeCursor)(nil)
	_ ChangeCursor = (*sliceChangeCursor)(nil)
)
