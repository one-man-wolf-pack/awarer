// Package restorespool holds one restore invocation's validated operations in
// awa-owned temporary spill.
//
// A restore over a large worktree must never require its operation set as a
// slice: planning streams a comparison, and apply walks the result several times
// (once per apply rank, then once per directory depth within the deletion rank).
// This package is the deterministic JSONL spill that makes those repeat walks
// possible without holding the operations in memory.
//
// The spool is scratch, never authority. It lives under `.awa/store/tmp`, which
// gc and doctor already classify by age, so an abandoned spool from a crashed
// restore is reclaimed through the existing temp ownership rules rather than by
// any new sweeping logic. Nothing ever reads a spool from a previous process:
// each invocation writes and reads its own, and a rerun re-plans from current
// reality.
package restorespool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
)

const (
	// opsName is the single spill file. One file keeps the format trivial; the
	// executor's rank and depth passes filter it while streaming rather than
	// needing a file per phase.
	opsName = "ops.jsonl"

	// dirPrefix names a spool directory under the store temp area. The prefix
	// makes an abandoned spool self-describing to a human reading `awa gc
	// --dry-run` output.
	dirPrefix = "restore-"

	// maxLineBytes bounds one spooled record. Every field is a path, a hash, or a
	// small token, so a longer line means a corrupt or truncated spill rather than
	// a reason to grow a buffer without limit.
	maxLineBytes = 1 << 20

	// cancelCheckInterval is how often a long spool read checks cancellation.
	cancelCheckInterval = 512
)

// Spool is one invocation's operation spill. Add operations in canonical path
// order, Seal it, then open as many cursors as the apply passes need. It is not
// safe for concurrent use.
type Spool struct {
	root   string
	relDir string
	absOps string

	f      *os.File
	bw     *bufio.Writer
	enc    *json.Encoder
	sealed bool
	count  int
	prev   worktree.RelPath
	// maxDirDepth is the deepest directory-deletion path added, so the executor
	// knows how many descending depth passes to make without measuring the set.
	maxDirDepth int
}

// Open creates a fresh spool for one restore operation under the project's store
// temp directory. A spool for an id that already has one is a wiring error — each
// invocation mints its own id — so it fails rather than appending to foreign
// state.
func Open(layout paths.Layout, id restore.OperationID) (*Spool, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("restorespool: a spool requires an operation id")
	}
	tmpRel, err := fsx.RelUnder(layout.Root(), layout.TmpDir())
	if err != nil {
		return nil, fmt.Errorf("restorespool: store temp dir is not under the project root: %w", err)
	}
	relDir := tmpRel + "/" + dirPrefix + id.String()
	dir, err := fsx.MkdirAllNoFollow(layout.Root(), relDir, paths.DirPerm)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()

	// The spill is created THROUGH the directory descriptor opened above and then
	// linked to its readable name through that same descriptor. Both halves matter:
	// anchoring is what a joined path cannot give — a symlink swapped into an
	// ancestor between the mkdir and the create would otherwise redirect the write —
	// and linkat fails with EEXIST rather than adopting an existing file, so a
	// leftover spill from a crashed run is never silently extended. (An id is minted
	// per invocation, so a collision here is a wiring error, not a normal race.)
	f, tmpName, err := fsx.CreateTempAt(dir, ".ops-")
	if err != nil {
		return nil, fmt.Errorf("restorespool: creating spool for %s: %w", id.Short(), err)
	}
	if err := fsx.LinkAt(dir, tmpName, dir, opsName); err != nil {
		_ = f.Close()
		_ = fsx.RemoveAt(dir, tmpName)
		return nil, fmt.Errorf("restorespool: claiming spool for %s: %w", id.Short(), err)
	}
	// The name is claimed and the descriptor stays open, so the anonymous temp name
	// has done its job and is removed; the spill lives on under opsName.
	if err := fsx.RemoveAt(dir, tmpName); err != nil {
		_ = f.Close()
		return nil, err
	}
	bw := bufio.NewWriter(f)
	return &Spool{
		root:   layout.Root(),
		relDir: relDir,
		absOps: filepath.Join(layout.Root(), filepath.FromSlash(relDir), opsName),
		f:      f,
		bw:     bw,
		enc:    json.NewEncoder(bw),
	}, nil
}

// Add appends one validated mutating operation. It accepts only ready mutating
// work: a blocked operation aborts the whole apply and an equal operation is not
// work, so neither belongs in the replay set. Paths must arrive in strictly
// ascending canonical order — planning streams a canonical merge, so a violation
// is a planner bug, and catching it here keeps the executor's rank and depth
// passes from silently depending on an order they were never given.
func (s *Spool) Add(op restore.PlannedOperation) error {
	if s.sealed {
		return fmt.Errorf("restorespool: cannot add to a sealed spool")
	}
	if op.Availability() != restore.Ready {
		return fmt.Errorf("restorespool: refusing to spool a %s operation for %q", op.Availability(), op.Path())
	}
	if !op.Kind().Mutates() {
		return fmt.Errorf("restorespool: refusing to spool a non-mutating %s operation for %q", op.Kind(), op.Path())
	}
	if s.count > 0 && !s.prev.Less(op.Path()) {
		return fmt.Errorf("restorespool: operations must arrive in strictly ascending canonical order, got %q after %q", op.Path(), s.prev)
	}
	doc, err := encodeOperation(op)
	if err != nil {
		return err
	}
	if err := s.enc.Encode(doc); err != nil {
		return fmt.Errorf("restorespool: writing operation for %q: %w", op.Path(), err)
	}
	s.prev = op.Path()
	s.count++
	if op.Rank() == restore.RankDeleteDirectory && op.PathDepth() > s.maxDirDepth {
		s.maxDirDepth = op.PathDepth()
	}
	return nil
}

// Seal flushes and closes the writer. It must be called before any cursor is
// opened, so a cursor can never read a partially flushed spill.
func (s *Spool) Seal() error {
	if s.sealed {
		return nil
	}
	if err := s.bw.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	if err := s.f.Sync(); err != nil {
		_ = s.f.Close()
		return err
	}
	if err := s.f.Close(); err != nil {
		return err
	}
	s.sealed = true
	return nil
}

// Count returns how many mutating operations were spooled.
func (s *Spool) Count() int { return s.count }

// MaxDirectoryDepth returns the deepest directory-deletion path depth spooled, or
// zero when none were. The executor walks directory deletions with one pass per
// depth, descending, so this bounded integer is all it needs to be deepest-first
// without sorting.
func (s *Spool) MaxDirectoryDepth() int { return s.maxDirDepth }

// Discard removes the spool and everything in it. It is idempotent and only ever
// touches awa's own temp directory — never a worktree path.
func (s *Spool) Discard() error {
	if !s.sealed && s.f != nil {
		_ = s.f.Close()
		s.sealed = true
	}
	tmpDir, err := fsx.OpenDirNoFollow(s.root, parentRel(s.relDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = tmpDir.Close() }()
	if err := fsx.RemoveTreeAt(tmpDir, baseRel(s.relDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func parentRel(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return rel
}

func baseRel(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

var _ restore.OperationStream = (*Spool)(nil)

// Open returns a fresh cursor over the spooled operations in canonical path
// order. The spool must be sealed first.
func (s *Spool) Open(ctx context.Context) (restore.OperationCursor, error) {
	if !s.sealed {
		return nil, fmt.Errorf("restorespool: cannot read an unsealed spool")
	}
	rel, err := fsx.RelUnder(s.root, s.absOps)
	if err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollowAt(s.root, rel)
	if err != nil {
		return nil, fmt.Errorf("restorespool: opening spool: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("restorespool: spool is not a regular file (%s)", info.Mode().Type())
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	return &cursor{ctx: ctx, f: f, sc: sc, expected: s.count}, nil
}

// cursor decodes one operation per line. It rebuilds every operation through the
// domain constructor, so a spilled record can never re-enter as a shape the
// domain would refuse, and it checks the record count at clean EOF so a truncated
// spill is never read as a shorter plan.
type cursor struct {
	ctx      context.Context
	f        *os.File
	sc       *bufio.Scanner
	expected int

	cur   restore.PlannedOperation
	line  int
	count int
	err   error
	done  bool
}

func (c *cursor) Next() bool {
	if c.done {
		return false
	}
	if c.count%cancelCheckInterval == 0 {
		if err := c.ctx.Err(); err != nil {
			c.err, c.done = err, true
			return false
		}
	}
	if !c.sc.Scan() {
		c.done = true
		if err := c.sc.Err(); err != nil {
			c.err = err
			return false
		}
		if c.count != c.expected {
			c.err = fmt.Errorf("restorespool: spool holds %d operation(s), %d were written", c.count, c.expected)
		}
		return false
	}
	c.line++
	op, err := decodeOperation(c.sc.Bytes())
	if err != nil {
		c.err, c.done = fmt.Errorf("restorespool: spool line %d: %w", c.line, err), true
		return false
	}
	c.cur = op
	c.count++
	return true
}

func (c *cursor) Operation() restore.PlannedOperation { return c.cur }
func (c *cursor) Err() error                          { return c.err }
func (c *cursor) Close() error                        { return c.f.Close() }

// --- codec ----------------------------------------------------------------

type opDoc struct {
	Path    string  `json:"path"`
	Kind    string  `json:"kind"`
	Current nodeDoc `json:"current"`
	Desired nodeDoc `json:"desired"`
}

type nodeDoc struct {
	Present bool   `json:"present"`
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	Symlink string `json:"symlink,omitempty"`
}

func encodeOperation(op restore.PlannedOperation) (opDoc, error) {
	cur, err := encodeNode(op.Current())
	if err != nil {
		return opDoc{}, err
	}
	des, err := encodeNode(op.Desired())
	if err != nil {
		return opDoc{}, err
	}
	return opDoc{Path: op.Path().String(), Kind: op.Kind().String(), Current: cur, Desired: des}, nil
}

func encodeNode(n restore.NodeState) (nodeDoc, error) {
	if !n.Present() {
		return nodeDoc{}, nil
	}
	switch n.Kind() {
	case worktree.KindRegular:
		return nodeDoc{Present: true, Kind: n.Kind().String(), Content: n.Content().String(), Mode: n.Mode()}, nil
	case worktree.KindDir:
		return nodeDoc{Present: true, Kind: n.Kind().String()}, nil
	case worktree.KindSymlink:
		return nodeDoc{Present: true, Kind: n.Kind().String(), Symlink: n.SymlinkTarget().String()}, nil
	default:
		return nodeDoc{}, fmt.Errorf("cannot spool a node of kind %s", n.Kind())
	}
}

// decodeOperation parses one spooled line strictly and rebuilds the operation
// through the domain constructor. The recorded kind is compared against the kind
// the constructor derives, so a spill whose states and label disagree fails
// rather than executing as something other than what was planned.
func decodeOperation(data []byte) (restore.PlannedOperation, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var doc opDoc
	if err := dec.Decode(&doc); err != nil {
		return restore.PlannedOperation{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return restore.PlannedOperation{}, fmt.Errorf("unexpected trailing data")
	}
	path, err := worktree.ParseRelPath(doc.Path)
	if err != nil {
		return restore.PlannedOperation{}, err
	}
	current, err := decodeNode(doc.Current)
	if err != nil {
		return restore.PlannedOperation{}, err
	}
	desired, err := decodeNode(doc.Desired)
	if err != nil {
		return restore.PlannedOperation{}, err
	}
	op, err := restore.NewPlannedOperation(path, current, desired)
	if err != nil {
		return restore.PlannedOperation{}, err
	}
	if op.Kind().String() != doc.Kind {
		return restore.PlannedOperation{}, fmt.Errorf("spooled operation for %q is labelled %q but its states describe %q", path, doc.Kind, op.Kind())
	}
	return op, nil
}

func decodeNode(doc nodeDoc) (restore.NodeState, error) {
	if !doc.Present {
		if doc.Kind != "" || doc.Content != "" || doc.Mode != 0 || doc.Symlink != "" {
			return restore.NodeState{}, fmt.Errorf("an absent node carries state")
		}
		return restore.AbsentNode(), nil
	}
	kind, err := worktree.ParseFileKind(doc.Kind)
	if err != nil {
		return restore.NodeState{}, err
	}
	switch kind {
	case worktree.KindRegular:
		content, err := hashing.ParseContentHash(doc.Content)
		if err != nil {
			return restore.NodeState{}, err
		}
		return restore.RegularNode(content, doc.Mode)
	case worktree.KindDir:
		return restore.DirNode(), nil
	case worktree.KindSymlink:
		target, err := worktree.NewSymlinkTarget(doc.Symlink)
		if err != nil {
			return restore.NodeState{}, err
		}
		return restore.SymlinkNode(target)
	default:
		return restore.NodeState{}, fmt.Errorf("cannot restore a node of kind %s", kind)
	}
}
