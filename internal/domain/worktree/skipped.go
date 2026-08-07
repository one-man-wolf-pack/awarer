package worktree

import "fmt"

// SkippedReason explains why a node was not hashed. Skipped inputs are always
// explicit: they appear in scan results and contribute to the tree hash, so a
// file flipping from present to skipped changes the tree identity.
type SkippedReason int

const (
	// ReasonSpecialFile: a device, socket, or fifo — never hashable.
	ReasonSpecialFile SkippedReason = iota
	// ReasonLargeFileSkipPolicy: larger than max_file_size under the skip policy.
	ReasonLargeFileSkipPolicy
	// ReasonSymlinkCycle: following symlinks revisited an already-seen target.
	ReasonSymlinkCycle
	// ReasonSymlinkDepthExceeded: symlink traversal exceeded symlink_max_depth.
	ReasonSymlinkDepthExceeded
	// ReasonSymlinkRootEscape: a followed symlink pointed outside the project
	// root and escapes are not allowed.
	ReasonSymlinkRootEscape
	// ReasonReadError: the file could not be read; only recorded when the caller
	// opted into tolerating skipped inputs, and it taints the scan.
	ReasonReadError
)

// ParseSkippedReason resolves a persisted token.
func ParseSkippedReason(s string) (SkippedReason, error) {
	switch s {
	case "special-file":
		return ReasonSpecialFile, nil
	case "large-file-skip":
		return ReasonLargeFileSkipPolicy, nil
	case "symlink-cycle":
		return ReasonSymlinkCycle, nil
	case "symlink-depth-exceeded":
		return ReasonSymlinkDepthExceeded, nil
	case "symlink-root-escape":
		return ReasonSymlinkRootEscape, nil
	case "read-error":
		return ReasonReadError, nil
	default:
		return 0, fmt.Errorf("invalid skipped reason %q", s)
	}
}

func (r SkippedReason) String() string {
	switch r {
	case ReasonSpecialFile:
		return "special-file"
	case ReasonLargeFileSkipPolicy:
		return "large-file-skip"
	case ReasonSymlinkCycle:
		return "symlink-cycle"
	case ReasonSymlinkDepthExceeded:
		return "symlink-depth-exceeded"
	case ReasonSymlinkRootEscape:
		return "symlink-root-escape"
	case ReasonReadError:
		return "read-error"
	default:
		return "unknown"
	}
}

// Valid reports whether r is a known reason.
func (r SkippedReason) Valid() bool {
	return r >= ReasonSpecialFile && r <= ReasonReadError
}

// TaintsScan reports whether a skip for this reason makes the scan incomplete —
// the worktree could not be fully observed. Only a read error qualifies: a
// special file, a large-file skip, or a symlink guard is a deliberate, complete
// decision about an input the scan did see, not a gap in what it could see.
func (r SkippedReason) TaintsScan() bool {
	return r == ReasonReadError
}

// Kind returns the file kind a reason can only apply to, binding the two so the
// domain cannot represent a nonsensical pair such as a regular file skipped for a
// symlink cycle.
func (r SkippedReason) Kind() FileKind {
	switch r {
	case ReasonSpecialFile:
		return KindSpecial
	case ReasonLargeFileSkipPolicy, ReasonReadError:
		return KindRegular
	case ReasonSymlinkCycle, ReasonSymlinkDepthExceeded, ReasonSymlinkRootEscape:
		return KindSymlink
	default:
		return KindSpecial
	}
}

// SkippedInput is a node that was scoped in but not hashed. It carries enough
// metadata to report the skip to a user and to fold a deterministic record into
// the tree hash. Build one with NewSkippedInput, which derives the kind from the
// reason so the two cannot disagree.
type SkippedInput struct {
	Path   RelPath
	Kind   FileKind
	Reason SkippedReason
	Size   int64
	// Stat is the captured signature where available; the zero value when the
	// node could not be stat'd.
	Stat StatSignature
	// OSError is the OS error category for ReasonReadError (for diagnostics),
	// empty otherwise.
	OSError string
	// Symlink is the raw link target for a skipped symlink (cycle, depth, or root
	// escape), and the zero value otherwise. It is part of a symlink's identity,
	// so it is folded into the tree hash: re-pointing a skipped link to a new
	// target changes the tree even when the skip reason is unchanged.
	Symlink SymlinkTarget
	// Traversal records how the node was reached, exactly as for an Entry, so a
	// node skipped under follow_symlinks keeps its provenance instead of leaving
	// it to discipline.
	Traversal TraversalInfo
}

// Validate checks that a skipped input represents a real skip: it has a path, a
// known kind and reason, a reason that matches the kind, and a non-negative size
// (a negative size would wrap to a huge value in the canonical encoding). Like
// Entry.Validate, it lets the persistence boundary re-check the invariant rather
// than trust the caller.
func (s SkippedInput) Validate() error {
	if s.Path.IsZero() {
		return fmt.Errorf("skipped input has no path")
	}
	if !s.Stat.Omitted.Valid() {
		return fmt.Errorf("skipped input %q has unknown omitted stat fields", s.Path)
	}
	if !s.Kind.Valid() {
		return fmt.Errorf("skipped input %q has invalid kind %v", s.Path, s.Kind)
	}
	if !s.Reason.Valid() {
		return fmt.Errorf("skipped input %q has invalid reason %v", s.Path, s.Reason)
	}
	if s.Reason.Kind() != s.Kind {
		return fmt.Errorf("skipped input %q: reason %v does not apply to kind %v", s.Path, s.Reason, s.Kind)
	}
	if s.Size < 0 {
		return fmt.Errorf("skipped input %q has negative size %d", s.Path, s.Size)
	}
	// The OS error category is the diagnostic for a read error and applies to
	// nothing else, so the reason and the category cannot disagree.
	if s.Reason == ReasonReadError && s.OSError == "" {
		return fmt.Errorf("skipped input %q: read-error requires an OS error category", s.Path)
	}
	if s.Reason != ReasonReadError && s.OSError != "" {
		return fmt.Errorf("skipped input %q: OS error category %q applies only to read-error", s.Path, s.OSError)
	}
	// A skipped symlink is still identified by its target, so the target must be
	// present for symlink skips and absent for everything else.
	if s.Kind == KindSymlink && s.Symlink.IsZero() {
		return fmt.Errorf("skipped symlink %q has no target", s.Path)
	}
	if s.Kind != KindSymlink && !s.Symlink.IsZero() {
		return fmt.Errorf("skipped input %q: a target applies only to symlinks", s.Path)
	}
	if err := s.Traversal.Validate(); err != nil {
		return fmt.Errorf("skipped input %q: %w", s.Path, err)
	}
	return nil
}

// SkippedSample is a bounded diagnostic projection of one skipped input: just the
// path and reason, enough to explain a skip without retaining the full record.
type SkippedSample struct {
	Path   string
	Reason string
}

// SkippedFacts is a bounded summary of a scan's skipped inputs: the total count,
// whether any skip taints the scan (a read error), and up to a caller-chosen
// number of samples. It lets a consumer (the run cache) reason about skipped
// inputs without materializing every SkippedInput.
type SkippedFacts struct {
	Count   int
	Tainted bool
	Samples []SkippedSample
}

// NewSkippedInput builds a skipped input. The kind is derived from the reason, so
// a regular file can never be recorded as skipped for a symlink cycle, and the
// result is validated so an invalid reason, negative size, mismatched OS error
// category, or missing/misplaced symlink target is rejected at construction. The
// symlink target must be set for symlink skip reasons and zero otherwise.
func NewSkippedInput(path RelPath, reason SkippedReason, size int64, stat StatSignature, osErr string, symlink SymlinkTarget, traversal TraversalInfo) (SkippedInput, error) {
	s := SkippedInput{
		Path:      path,
		Kind:      reason.Kind(),
		Reason:    reason,
		Size:      size,
		Stat:      stat,
		OSError:   osErr,
		Symlink:   symlink,
		Traversal: traversal,
	}
	if err := s.Validate(); err != nil {
		return SkippedInput{}, err
	}
	return s, nil
}
