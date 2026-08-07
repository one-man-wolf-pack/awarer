// Package restore is the pure domain model for evidence-backed worktree restore.
//
// It owns what a restore *means*: the immutable source identity a repair is
// proven from, the normalized selection that bounds it, the individual
// operations that turn the current worktree into what the source proves, the
// closed vocabulary of reasons an operation is not available, and the outcome of
// an apply. It performs no I/O and knows nothing about checkpoints, blobs, the
// scanner, or the filesystem: the application service feeds it facts gathered
// through ports, and the CLI projects the validated result.
//
// The product rule the types encode is: restore only what one immutable awa
// state proves, and mutate only a current path whose planned precondition still
// holds. Every constructor here exists to make a violation of that rule
// unrepresentable rather than to be remembered by a caller — most importantly
// the split between a PlannedOperation (what preview may describe) and a
// StagedOperation (what apply may execute), which is the type-level reason a
// regular file can never be written without verified staged bytes behind it.
package restore

import (
	"fmt"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// SourceKind classifies the immutable evidence a restore is planned from. The
// zero value is not a valid kind, so a source must be built with an explicit one.
type SourceKind int

const (
	// SourceCheckpoint is a stored checkpoint (id, id prefix, latest, or @-N, all
	// resolved to one immutable checkpoint id before planning).
	SourceCheckpoint SourceKind = iota + 1
	// SourceRunObservation is a recorded run's pre/post observed state.
	SourceRunObservation
	// SourceRecoveryObservation is a previous restore's pre-apply recovery
	// observation, addressed as restore:<id>:before. Its recorded selection bounds
	// what it can prove, so a plan built from one is scope-bounded.
	SourceRecoveryObservation
)

// String returns the stable machine token for the kind.
func (k SourceKind) String() string {
	switch k {
	case SourceCheckpoint:
		return "checkpoint"
	case SourceRunObservation:
		return "run-observation"
	case SourceRecoveryObservation:
		return "recovery-observation"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a known source kind.
func (k SourceKind) Valid() bool {
	return k == SourceCheckpoint || k == SourceRunObservation || k == SourceRecoveryObservation
}

// movingRefTokens are the reference spellings that name a *position* rather than
// an immutable state. A resolved source may never still carry one: the whole
// point of resolving once is that the identity a preview prints is the identity a
// later apply acts on, even if another agent records a checkpoint in between.
var movingRefTokens = map[string]bool{"latest": true, "now": true}

// Source is the immutable, already-resolved evidence a restore plans from. It is
// built only through NewSource, which rejects an identity that has not actually
// been resolved — so "a source ref that still means latest after planning" cannot
// be represented. Requested keeps what the user typed, purely so output can show
// "you asked for latest, that is checkpoint <id>".
type Source struct {
	kind SourceKind
	// id is the full immutable identity: a checkpoint id, a run id, or a restore
	// operation id. Never shortened, never a prefix.
	id string
	// canonicalRef is the reference that names exactly this state and nothing else
	// (e.g. "run:<id>:before"). For a checkpoint it is the id itself.
	canonicalRef string
	// requested is the expression the user typed, for display only.
	requested string
}

// NewSource builds a resolved source identity. The id must be a full immutable
// identity, and neither it nor the canonical reference may still be a moving
// token. requested may be anything the user typed (including "latest"): that is
// the display-only record of what was asked for.
func NewSource(kind SourceKind, id, canonicalRef, requested string) (Source, error) {
	if !kind.Valid() {
		return Source{}, fmt.Errorf("invalid restore source kind %d", kind)
	}
	id = strings.TrimSpace(id)
	canonicalRef = strings.TrimSpace(canonicalRef)
	if id == "" {
		return Source{}, fmt.Errorf("restore source requires a full immutable id")
	}
	if canonicalRef == "" {
		return Source{}, fmt.Errorf("restore source requires a canonical reference")
	}
	if movingRefTokens[id] || strings.HasPrefix(id, "@-") {
		return Source{}, fmt.Errorf("restore source id %q is a moving reference, not a resolved identity", id)
	}
	if movingRefTokens[canonicalRef] || strings.HasPrefix(canonicalRef, "@-") {
		return Source{}, fmt.Errorf("restore source reference %q is a moving reference, not a resolved identity", canonicalRef)
	}
	if strings.TrimSpace(requested) == "" {
		return Source{}, fmt.Errorf("restore source requires the requested expression it was resolved from")
	}
	return Source{kind: kind, id: id, canonicalRef: canonicalRef, requested: requested}, nil
}

// Kind returns the source's evidence kind.
func (s Source) Kind() SourceKind { return s.kind }

// ID returns the full immutable identity.
func (s Source) ID() string { return s.id }

// CanonicalRef returns the reference that names exactly this state.
func (s Source) CanonicalRef() string { return s.canonicalRef }

// Requested returns the expression the user typed.
func (s Source) Requested() string { return s.requested }

// IsZero reports whether the source is unset.
func (s Source) IsZero() bool { return s.id == "" }

// Selection is the normalized restore scope: either every path the source
// proves restorable, or an explicit non-empty set of paths. It is built only
// through NewPathSelection/NewAllSelection, so "both all and paths" and "neither"
// are unrepresentable.
type Selection struct {
	all   bool
	paths []worktree.RelPath
}

// NewAllSelection selects all proven restorable awa scope. It is not "every
// filesystem entry under the root": ignored paths are outside awa evidence and
// are never touched.
func NewAllSelection() Selection { return Selection{all: true} }

// NewPathSelection selects one or more explicit project-relative paths. An empty
// set is rejected: an omitted selection must be a usage error at the boundary,
// never a silently whole-project restore. The slice is copied, so a caller cannot
// widen a validated selection afterwards.
func NewPathSelection(paths []worktree.RelPath) (Selection, error) {
	if len(paths) == 0 {
		return Selection{}, fmt.Errorf("a path selection requires at least one path")
	}
	out := make([]worktree.RelPath, 0, len(paths))
	for _, p := range paths {
		if p.IsZero() {
			return Selection{}, fmt.Errorf("a path selection cannot contain an empty path")
		}
		out = append(out, p)
	}
	return Selection{paths: out}, nil
}

// All reports whether the selection covers all proven restorable scope.
func (s Selection) All() bool { return s.all }

// Paths returns the selected paths as a copy, so a consumer that walks them
// cannot mutate the validated selection every other consumer sees. It is empty
// for an --all selection.
func (s Selection) Paths() []worktree.RelPath {
	if len(s.paths) == 0 {
		return nil
	}
	out := make([]worktree.RelPath, len(s.paths))
	copy(out, s.paths)
	return out
}

// IsZero reports whether the selection was never built through a constructor.
// The zero Selection is neither "all" nor a path set, which is exactly the
// invalid state the constructors exist to prevent, so every consumer that
// receives a Selection from outside checks this.
func (s Selection) IsZero() bool { return !s.all && len(s.paths) == 0 }

// Covers reports whether path is inside the selection: any path for --all, and
// otherwise a selected path itself or a descendant of one. It is the single
// definition of "selected", shared by planning, the recovery observation's
// covered scope, and output.
func (s Selection) Covers(path worktree.RelPath) bool {
	if s.all {
		return true
	}
	for _, sel := range s.paths {
		if PathWithin(path, sel) {
			return true
		}
	}
	return false
}

// PathWithin reports whether path is sel itself or a descendant of it. The
// separator check keeps "src/apiv2" from matching a selection of "src/api".
//
// It is exported because Covers answers "is this path selected at all" while a
// caller that must attribute a path back to the operand that selected it needs the
// same rule per operand. Re-deriving containment there would be a second definition
// of "selected": if the two drifted, an operand that did match would be reported as
// outside awa's evidence and the apply would refuse on a path that was fine.
func PathWithin(path, sel worktree.RelPath) bool {
	p, s := path.String(), sel.String()
	if p == s {
		return true
	}
	return strings.HasPrefix(p, s+"/")
}

// String renders the selection for human output.
func (s Selection) String() string {
	if s.all {
		return "all proven scope"
	}
	parts := make([]string, len(s.paths))
	for i, p := range s.paths {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// NodeState is the proved state of one path on one side of the comparison:
// either absent, or present as a regular file, directory, or symlink with the
// identity fields the tree hash folds in. It is used for both the current
// precondition and the desired source state, so the two can be compared with one
// definition of equality rather than two that could drift.
//
// Volatile stat (size, mtime, inode) is deliberately excluded: it is not part of
// worktree identity, so requiring it to match would turn a harmless touch into a
// conflict.
type NodeState struct {
	present bool
	kind    worktree.FileKind
	content hashing.ContentHash
	mode    uint32
	symlink worktree.SymlinkTarget
}

// AbsentNode is the proved absence of a path.
func AbsentNode() NodeState { return NodeState{} }

// RegularNode is a present regular file identified by its content hash and
// permission bits. A zero content hash is rejected: a regular file with no hash
// proves nothing.
func RegularNode(content hashing.ContentHash, mode uint32) (NodeState, error) {
	if content.IsZero() {
		return NodeState{}, fmt.Errorf("a regular node requires a content hash")
	}
	return NodeState{present: true, kind: worktree.KindRegular, content: content, mode: mode & 0o777}, nil
}

// DirNode is a present directory. A directory carries no content identity.
func DirNode() NodeState {
	return NodeState{present: true, kind: worktree.KindDir}
}

// SymlinkNode is a present symbolic link identified by its stored target. The
// target is never resolved: a link is what it points at, not what lives there.
func SymlinkNode(target worktree.SymlinkTarget) (NodeState, error) {
	if target.IsZero() {
		return NodeState{}, fmt.Errorf("a symlink node requires a target")
	}
	return NodeState{present: true, kind: worktree.KindSymlink, symlink: target}, nil
}

// NodeFromEntry projects a scanned/persisted manifest entry into a NodeState. It
// is the one adapter from the worktree model, so planning cannot invent a node
// shape the scanner would never produce. A special file has no NodeState: restore
// refuses unsupported entry kinds rather than pretending to restore them.
func NodeFromEntry(e worktree.Entry) (NodeState, error) {
	switch e.Kind {
	case worktree.KindRegular:
		return RegularNode(e.Content, e.Stat.Mode)
	case worktree.KindDir:
		return DirNode(), nil
	case worktree.KindSymlink:
		return SymlinkNode(e.Symlink)
	default:
		return NodeState{}, fmt.Errorf("entry %q has kind %s, which restore does not support", e.Path, e.Kind)
	}
}

// Present reports whether the path exists in this state.
func (n NodeState) Present() bool { return n.present }

// Kind returns the node's file kind; it is meaningful only when Present.
func (n NodeState) Kind() worktree.FileKind { return n.kind }

// Content returns the regular file's content hash, zero for every other shape.
func (n NodeState) Content() hashing.ContentHash { return n.content }

// Mode returns the regular file's permission bits (0o777), zero for every other
// shape.
func (n NodeState) Mode() uint32 { return n.mode }

// SymlinkTarget returns the link's stored target, zero for every other shape.
func (n NodeState) SymlinkTarget() worktree.SymlinkTarget { return n.symlink }

// Equal reports identity equality, mirroring the fields the tree hash folds into
// an entry's identity: kind plus content hash and permission bits for a regular
// file, or the target for a symlink. Two states that hash equal compare equal
// here, so a restore never plans work the comparison would call "no change".
func (n NodeState) Equal(other NodeState) bool {
	if n.present != other.present {
		return false
	}
	if !n.present {
		return true
	}
	if n.kind != other.kind {
		return false
	}
	switch n.kind {
	case worktree.KindRegular:
		return n.content == other.content && n.mode == other.mode
	case worktree.KindDir:
		return true
	case worktree.KindSymlink:
		return n.symlink == other.symlink
	default:
		return false
	}
}

// Describe renders the node for human output and diagnostics.
func (n NodeState) Describe() string {
	if !n.present {
		return "absent"
	}
	switch n.kind {
	case worktree.KindRegular:
		return fmt.Sprintf("file %04o", n.mode)
	case worktree.KindDir:
		return "dir"
	case worktree.KindSymlink:
		return "symlink -> " + n.symlink.String()
	default:
		return "unknown"
	}
}
