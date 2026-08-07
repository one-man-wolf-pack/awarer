package compare

// renameCursor is the one comparison stage that buffers its input. It wraps the base
// merge cursor and pairs a unique deleted+added record of identical content (or symlink
// target) into a single Renamed change. Because the pairing is global and
// order-independent — a delete at "z/old" can pair with an add at "a/new" — it cannot
// emit in final order until it has seen every candidate, so it drains the base cursor,
// pairs, sorts, and then serves the result.
//
// Memory — the bounded presentation policy. Whole-set pairing means this stage retains
// the change set (the changes slice holds every reported Change; the candidate maps in
// detectRenames are the smaller term). That buffer is capped: newRenameCursorLimited
// buffers at most `limit` changes, and a comparison that exceeds the limit does not try
// to be a hero — it stops pairing and serves the changes as plain add/delete/modify (see
// renameSkippedCursor), so memory stays bounded by the limit and the diff stays full and
// honest, just without rename sugar. The skip is reported, not silent, via
// RenameDetection. A comparison under the limit (the realistic case) gets full pairing;
// --no-renames skips this stage entirely and streams the base cursor directly. A future
// fully-incremental implementation could spill candidates to a content-keyed temporary
// index and re-merge, keeping pairing above the limit without changing this cursor's
// contract (cursor in, cursor out).
type renameCursor struct {
	*sliceChangeCursor
	detection RenameDetection
}

func (c *renameCursor) RenameDetection() RenameDetection { return c.detection }

// RenameReasonLimitExceeded is the RenameDetection.Reason recorded when the change
// set was larger than the rename buffer limit, so rename pairing was not applied and
// the changes are presented as plain add/delete/modify.
const RenameReasonLimitExceeded = "limit-exceeded"

// defaultRenameBufferLimit caps how many changes the rename stage buffers to attempt
// whole-set pairing. It is chosen far above any realistic review diff (a diff a human
// reads is tens to a few thousand changes), so ordinary use never reaches it; it is
// the ceiling that keeps a pathological drift (for example a first comparison of a
// huge tree against an empty baseline) from buffering the whole change set in memory.
const defaultRenameBufferLimit = 100_000

// RenameDetection is the outcome of the rename-detection presentation policy for one
// comparison. Attempted is true when rename detection was requested (not --no-renames);
// Applied is true when it actually ran. When a comparison's change set exceeds the
// buffer limit, Attempted is true but Applied is false with Reason
// RenameReasonLimitExceeded: pairing was not applied and the changes are presented as
// plain add/delete/modify — a full, honest diff without rename sugar. This is a bounded
// presentation policy, not a failure: the comparison succeeds, memory stays bounded, and
// the report is complete, just without rename pairing.
type RenameDetection struct {
	Attempted bool
	Applied   bool
	Reason    string
	Limit     int
}

// RenameDetectionReporter is implemented by a change cursor that can report its
// rename-detection outcome. The rename-detecting cursors implement it; the plain merge
// cursor (rename detection not requested) does not.
type RenameDetectionReporter interface {
	RenameDetection() RenameDetection
}

// RenameDetectionOf reports the rename-detection outcome behind a change cursor. A
// cursor that does not implement RenameDetectionReporter had no rename detection
// requested, so the zero (not-attempted) outcome is returned.
func RenameDetectionOf(c ChangeCursor) RenameDetection {
	if r, ok := c.(RenameDetectionReporter); ok {
		return r.RenameDetection()
	}
	return RenameDetection{}
}

// newRenameCursorLimited attempts whole-set rename pairing over base, buffering at most
// limit changes. Within the limit it drains base, pairs, sorts, and serves the result
// (Applied). When the change that would exceed the limit arrives it does not try to be a
// hero: it stops buffering — the prefix stays exactly limit — and returns a pass-through
// cursor that serves the buffered prefix (already in canonical order), then that one
// tipping change, then the rest of base unchanged, so the diff stays full and honest as
// plain add/delete/modify while the buffer stays bounded by the limit
// (RenameReasonLimitExceeded). Ownership of base transfers to the returned cursor on the
// over-limit path (its Close closes base); on the applied and error paths base is closed
// here.
func newRenameCursorLimited(base ChangeCursor, limit int) (ChangeCursor, error) {
	var buffered []Change
	for base.Next() {
		if len(buffered) == limit {
			// Adding this change would exceed the limit. Hold it as the overflow sentinel
			// rather than grow the buffer, so the buffered prefix never exceeds limit.
			return &renameSkippedCursor{
				buffered:  buffered,
				overflow:  base.Change(),
				base:      base,
				detection: RenameDetection{Attempted: true, Applied: false, Reason: RenameReasonLimitExceeded, Limit: limit},
			}, nil
		}
		buffered = append(buffered, base.Change())
	}
	if err := base.Err(); err != nil {
		_ = base.Close()
		return nil, err
	}
	_ = base.Close()
	buffered = detectRenames(buffered)
	sortChanges(buffered)
	return &renameCursor{
		sliceChangeCursor: &sliceChangeCursor{changes: buffered},
		detection:         RenameDetection{Attempted: true, Applied: true, Limit: limit},
	}, nil
}

// renameSkippedCursor serves a buffered prefix of changes, then the single change that
// tipped the buffer over the limit, then the remaining base cursor — all unpaired. It
// backs the bounded presentation policy when a change set exceeds the rename buffer
// limit: because the base merge cursor already yields changes in canonical order and no
// pairing reorders them, concatenating the buffered prefix, the tipping change, and the
// streamed remainder stays in canonical order — the same order --no-renames produces. The
// buffered prefix holds exactly limit changes; the tipping change is one held value, so
// memory stays bounded by the limit.
type renameSkippedCursor struct {
	buffered     []Change
	i            int
	overflow     Change
	overflowDone bool
	base         ChangeCursor
	cur          Change
	detection    RenameDetection
}

func (c *renameSkippedCursor) Next() bool {
	if !c.overflowDone && c.i == len(c.buffered) {
		c.overflowDone = true
		c.cur = c.overflow
		return true
	}
	if c.i < len(c.buffered) {
		c.cur = c.buffered[c.i]
		c.i++
		return true
	}
	if c.base.Next() {
		c.cur = c.base.Change()
		return true
	}
	return false
}

func (c *renameSkippedCursor) Change() Change                   { return c.cur }
func (c *renameSkippedCursor) Err() error                       { return c.base.Err() }
func (c *renameSkippedCursor) Close() error                     { return c.base.Close() }
func (c *renameSkippedCursor) RenameDetection() RenameDetection { return c.detection }
