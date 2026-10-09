package worktree

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"awarer/internal/domain/hashing"
)

type CoverageLimit uint8

const (
	// LimitNode leaves only the named node unobserved. The traversal saw it as a
	// non-directory without following it, so nothing could exist below it.
	LimitNode CoverageLimit = iota + 1
	LimitSubtree
)

func (l CoverageLimit) String() string {
	switch l {
	case LimitNode:
		return "node"
	case LimitSubtree:
		return "subtree"
	default:
		return ""
	}
}

func ParseCoverageLimit(s string) (CoverageLimit, error) {
	switch s {
	case "node":
		return LimitNode, nil
	case "subtree":
		return LimitSubtree, nil
	default:
		return 0, fmt.Errorf("unknown coverage limit %q", s)
	}
}

func (l CoverageLimit) Valid() bool { return l == LimitNode || l == LimitSubtree }

type FrontierRecord struct {
	Path  RelPath
	Limit CoverageLimit
}

func (r FrontierRecord) Validate() error {
	if r.Path.IsZero() {
		return fmt.Errorf("frontier record has no path")
	}
	if !r.Limit.Valid() {
		return fmt.Errorf("frontier record %q has no coverage limit", r.Path)
	}
	return nil
}

type FrontierCursor interface {
	Next() bool
	Record() FrontierRecord
	Err() error
	Close() error
}

type FrontierStream interface {
	Open(ctx context.Context) (FrontierCursor, error)
}

var ErrFrontierOutOfOrder = errors.New("frontier records out of canonical order")

type orderedFrontier struct {
	inner FrontierCursor
	have  bool
	prev  RelPath
	cur   FrontierRecord
	err   error
	done  bool
}

func OrderedFrontier(c FrontierCursor) FrontierCursor {
	if c == nil {
		return nil
	}
	if _, ok := c.(*orderedFrontier); ok {
		return c
	}
	return &orderedFrontier{inner: c}
}

func (o *orderedFrontier) Next() bool {
	if o.done {
		return false
	}
	if !o.inner.Next() {
		o.err = o.inner.Err()
		o.done = true
		return false
	}
	rec := o.inner.Record()
	if err := rec.Validate(); err != nil {
		o.err, o.done = err, true
		return false
	}
	if o.have && !o.prev.Less(rec.Path) {
		o.err = fmt.Errorf("%w: %q after %q", ErrFrontierOutOfOrder, rec.Path, o.prev)
		o.done = true
		return false
	}
	o.have, o.prev, o.cur = true, rec.Path, rec
	return true
}

func (o *orderedFrontier) Record() FrontierRecord { return o.cur }
func (o *orderedFrontier) Err() error             { return o.err }
func (o *orderedFrontier) Close() error           { return o.inner.Close() }

type frontierSlice struct {
	records []FrontierRecord
	i       int
}

func NewFrontierSliceCursor(records []FrontierRecord) FrontierCursor {
	return &frontierSlice{records: records, i: -1}
}

func (s *frontierSlice) Next() bool {
	if s.i+1 >= len(s.records) {
		s.i = len(s.records)
		return false
	}
	s.i++
	return true
}

func (s *frontierSlice) Record() FrontierRecord { return s.records[s.i] }
func (s *frontierSlice) Err() error             { return nil }
func (s *frontierSlice) Close() error           { return nil }

// frontierDigestVersion prefixes the frontier integrity encoding. The digest
// protects a persisted frontier payload; it is not an observation identity.
const frontierDigestVersion byte = 0x01

type FrontierDigest struct {
	Hash  hashing.TreeHash
	Count int
}

type FrontierReducer struct {
	w        interface{ Write([]byte) (int, error) }
	finalize func() hashing.TreeHash
	have     bool
	prev     RelPath
	count    int
	scratch  []byte
}

func NewFrontierReducer(h hashing.Hasher) (*FrontierReducer, error) {
	if h == nil {
		return nil, fmt.Errorf("frontier: hasher must not be nil")
	}
	r := &FrontierReducer{}
	if tw, ok := h.(treeDigestHasher); ok {
		r.w, r.finalize = tw.NewTreeWriter()
	} else {
		buf := &strings.Builder{}
		r.w = buf
		r.finalize = func() hashing.TreeHash { return h.HashBytes([]byte(buf.String())) }
	}
	_, _ = r.w.Write([]byte{frontierDigestVersion})
	return r, nil
}

func (r *FrontierReducer) Add(rec FrontierRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	if r.have && !r.prev.Less(rec.Path) {
		return fmt.Errorf("%w: %q after %q", ErrFrontierOutOfOrder, rec.Path, r.prev)
	}
	r.scratch = r.scratch[:0]
	r.scratch = binary.BigEndian.AppendUint64(r.scratch, uint64(len(rec.Path.String())))
	r.scratch = append(r.scratch, rec.Path.String()...)
	r.scratch = append(r.scratch, byte(rec.Limit))
	_, _ = r.w.Write(r.scratch)
	r.have, r.prev = true, rec.Path
	r.count++
	return nil
}

func (r *FrontierReducer) Finish() FrontierDigest {
	return FrontierDigest{Hash: r.finalize(), Count: r.count}
}

func ReduceFrontier(h hashing.Hasher, c FrontierCursor) (FrontierDigest, error) {
	if c == nil {
		return FrontierDigest{}, fmt.Errorf("frontier: nil cursor")
	}
	defer func() { _ = c.Close() }()
	r, err := NewFrontierReducer(h)
	if err != nil {
		return FrontierDigest{}, err
	}
	for c.Next() {
		if err := r.Add(c.Record()); err != nil {
			return FrontierDigest{}, err
		}
	}
	if err := c.Err(); err != nil {
		return FrontierDigest{}, err
	}
	return r.Finish(), nil
}

type CoverageKind uint8

const (
	// CoverageUnknown is a predecessor observation published without coverage
	// evidence: its nonappearance proves nothing.
	CoverageUnknown CoverageKind = iota
	CoverageRecorded
	// CoverageScoped is a recovery observation whose covered paths absent from its
	// manifest were independently proved physically absent before publication.
	CoverageScoped
)

func (k CoverageKind) String() string {
	switch k {
	case CoverageRecorded, CoverageScoped:
		return "recorded"
	default:
		return "unknown"
	}
}

func (k CoverageKind) Known() bool { return k != CoverageUnknown }

type Coverage struct {
	kind     CoverageKind
	frontier FrontierStream
	digest   FrontierDigest
}

func UnknownCoverage() Coverage { return Coverage{kind: CoverageUnknown} }

func ScopedCoverage() Coverage { return Coverage{kind: CoverageScoped} }

func RecordedCoverage(f FrontierStream, d FrontierDigest) (Coverage, error) {
	if f == nil {
		return Coverage{}, fmt.Errorf("recorded coverage requires a frontier stream")
	}
	if d.Hash.IsZero() || d.Count < 0 {
		return Coverage{}, fmt.Errorf("recorded coverage requires a frontier digest")
	}
	return Coverage{kind: CoverageRecorded, frontier: f, digest: d}, nil
}

func (c Coverage) Kind() CoverageKind { return c.kind }

func (c Coverage) Frontier() (FrontierStream, FrontierDigest, bool) {
	if c.kind != CoverageRecorded {
		return nil, FrontierDigest{}, false
	}
	return c.frontier, c.digest, true
}

type AbsenceVerdict uint8

const (
	AbsenceProved AbsenceVerdict = iota + 1
	AbsenceExcluded
	AbsenceSkippedBoundary
	AbsenceUnknown
)

type AbsenceTracker struct {
	kind      CoverageKind
	front     FrontierCursor
	pending   FrontierRecord
	havePend  bool
	exhausted bool
	// open holds every boundary read so far that is a string prefix of the last
	// queried path. In byte order a boundary b <= p can relate to p or any later
	// path only while b is a string prefix of p, so this never grows beyond the
	// depth of one path however many boundaries the observation has.
	open []boundary
	last RelPath
	err  error
}

type boundary struct {
	path    string
	subtree bool
	skipped bool
}

func NewAbsenceTracker(ctx context.Context, c Coverage) (*AbsenceTracker, error) {
	t := &AbsenceTracker{kind: c.kind}
	if c.kind == CoverageRecorded {
		cur, err := c.frontier.Open(ctx)
		if err != nil {
			return nil, err
		}
		t.front = OrderedFrontier(cur)
	}
	return t, nil
}

func (t *AbsenceTracker) Note(rec ManifestRecord) error {
	if !rec.IsSkipped() || t.kind != CoverageRecorded {
		return nil
	}
	if err := t.advance(rec.Path()); err != nil {
		return err
	}
	sk, _ := rec.Skipped()
	// A skipped regular or special node was seen as a non-directory, so nothing
	// below it existed; a skipped symlink may have led anywhere.
	t.open = append(t.open, boundary{path: rec.Path().String(), subtree: sk.Kind == KindSymlink || sk.Kind == KindDir, skipped: true})
	return nil
}

func (t *AbsenceTracker) Absent(p RelPath) (AbsenceVerdict, error) {
	switch t.kind {
	case CoverageUnknown:
		return AbsenceUnknown, nil
	case CoverageScoped:
		return AbsenceProved, nil
	}
	if err := t.advance(p); err != nil {
		return 0, err
	}
	return t.hit(p.String()), nil
}

// Reachable answers whether a create at p would stay inside observed evidence: unlike
// Absent, a node-only boundary on an ancestor also blocks, because the create would
// have to replace that unobserved node.
func (t *AbsenceTracker) Reachable(p RelPath) (AbsenceVerdict, error) {
	if t.kind != CoverageRecorded {
		return t.Absent(p)
	}
	if err := t.advance(p); err != nil {
		return 0, err
	}
	return t.match(p.String(), true), nil
}

func (t *AbsenceTracker) hit(p string) AbsenceVerdict { return t.match(p, false) }

func (t *AbsenceTracker) match(p string, nodesEnclose bool) AbsenceVerdict {
	verdict := AbsenceProved
	for _, b := range t.open {
		exact := b.path == p
		below := (b.subtree || nodesEnclose) && len(p) > len(b.path) && p[len(b.path)] == '/'
		if !exact && !below {
			continue
		}
		if b.skipped {
			return AbsenceSkippedBoundary
		}
		verdict = AbsenceExcluded
	}
	return verdict
}

func (t *AbsenceTracker) advance(p RelPath) error {
	if t.err != nil {
		return t.err
	}
	if !t.last.IsZero() && p.Less(t.last) {
		t.err = fmt.Errorf("absence query %q after %q is out of canonical order", p, t.last)
		return t.err
	}
	t.last = p
	ps := p.String()
	kept := t.open[:0]
	for _, b := range t.open {
		if strings.HasPrefix(ps, b.path) {
			kept = append(kept, b)
		}
	}
	t.open = kept
	for {
		if !t.havePend {
			if t.exhausted || t.front == nil {
				return nil
			}
			if !t.front.Next() {
				t.exhausted = true
				if err := t.front.Err(); err != nil {
					t.err = err
					return err
				}
				return nil
			}
			t.pending, t.havePend = t.front.Record(), true
		}
		if p.Less(t.pending.Path) {
			return nil
		}
		t.havePend = false
		if strings.HasPrefix(ps, t.pending.Path.String()) {
			t.open = append(t.open, boundary{path: t.pending.Path.String(), subtree: t.pending.Limit == LimitSubtree})
		}
	}
}

// Finish drains the rest of the frontier so its stream verifies its declared count
// and digest. A consumer calls it before acting on any verdict: answers read only a
// prefix, and a tampered record there is caught only at the end of the stream.
func (t *AbsenceTracker) Finish() error {
	if t.err != nil || t.front == nil {
		return t.err
	}
	for t.front.Next() {
	}
	t.exhausted, t.havePend = true, false
	if err := t.front.Err(); err != nil {
		t.err = err
	}
	return t.err
}

func (t *AbsenceTracker) Close() error {
	if t.front == nil {
		return nil
	}
	return t.front.Close()
}
