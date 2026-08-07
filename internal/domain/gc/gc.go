// Package gc defines the domain model for "awa gc": the closed vocabulary of
// candidate kinds, actions, and retention reasons, the immutable GCCandidate that
// records one decision, the GCPlan produced before any mutation, and the GCResult
// that aggregates execution.
//
// The package is pure: it performs no I/O and knows nothing about the filesystem,
// checkpoints, blobs, runs, or git. Its job is to make an invalid decision
// unrepresentable — a candidate cannot carry an unknown kind/action/reason, a
// reason cannot disagree with its action, and a plan's summary is derived from its
// candidates rather than set by hand. The infrastructure and application layers
// produce these values; the CLI renders them.
//
// GC is conservative: it deletes only what it can prove disposable. That posture is
// encoded here as the action taxonomy — delete, retain, blocked, skipped — and the
// reason set that justifies each, including the corruption and lock reasons that
// block destructive deletion entirely.
package gc

import "fmt"

// CandidateKind names the subsystem an item belongs to. The zero value is not a
// valid kind, so a candidate must be built with an explicit one.
type CandidateKind int

const (
	// KindCheckpoint is a committed checkpoint (metadata + manifest).
	KindCheckpoint CandidateKind = iota + 1
	// KindRun is a run-cache entry (metadata, payloads, key pointer).
	KindRun
	// KindBlob is a content-addressed blob in the store.
	KindBlob
	// KindTemp is a temp artifact under a known .awa temp location.
	KindTemp
	// KindLock is a lock record under .awa/locks. A lock is never deleted by GC; it
	// appears only as a blocked candidate explaining why destructive GC is held off.
	KindLock
	// KindRestore is a restore recovery observation: the immutable pre-restore state
	// an applied restore can be undone from. It has its own retention window
	// (gc.keep_restores_for) and, unlike a run, it references blobs — so a retained
	// one is a blob-reachability root and an unreadable one blocks the blob sweep.
	KindRestore
)

// ParseCandidateKind resolves a token to a CandidateKind, rejecting unknown values.
func ParseCandidateKind(s string) (CandidateKind, error) {
	switch s {
	case "checkpoint":
		return KindCheckpoint, nil
	case "run":
		return KindRun, nil
	case "blob":
		return KindBlob, nil
	case "temp":
		return KindTemp, nil
	case "lock":
		return KindLock, nil
	case "restore":
		return KindRestore, nil
	default:
		return 0, fmt.Errorf("invalid candidate kind %q: want checkpoint, run, blob, temp, lock, or restore", s)
	}
}

func (k CandidateKind) String() string {
	switch k {
	case KindCheckpoint:
		return "checkpoint"
	case KindRun:
		return "run"
	case KindBlob:
		return "blob"
	case KindTemp:
		return "temp"
	case KindLock:
		return "lock"
	case KindRestore:
		return "restore"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a known kind. Go lets any int be cast to the type, so
// the domain checks validity rather than trusting construction.
func (k CandidateKind) Valid() bool {
	return k == KindCheckpoint || k == KindRun || k == KindBlob || k == KindTemp || k == KindLock || k == KindRestore
}

// CandidateAction is what GC decided to do with a candidate. The zero value is not
// valid.
type CandidateAction int

const (
	// ActionDelete: GC proved the item disposable and (outside dry-run) removes it.
	ActionDelete CandidateAction = iota + 1
	// ActionRetain: GC kept the item because a retention rule protects it.
	ActionRetain
	// ActionBlocked: GC could not act safely — a lock or corruption stands in the
	// way — so the item is kept and the obstruction is reported.
	ActionBlocked
	// ActionSkipped: a subsystem filter excluded an otherwise-deletable item.
	ActionSkipped
)

// ParseCandidateAction resolves a token to a CandidateAction, rejecting unknowns.
func ParseCandidateAction(s string) (CandidateAction, error) {
	switch s {
	case "delete":
		return ActionDelete, nil
	case "retain":
		return ActionRetain, nil
	case "blocked":
		return ActionBlocked, nil
	case "skipped":
		return ActionSkipped, nil
	default:
		return 0, fmt.Errorf("invalid candidate action %q: want delete, retain, blocked, or skipped", s)
	}
}

func (a CandidateAction) String() string {
	switch a {
	case ActionDelete:
		return "delete"
	case ActionRetain:
		return "retain"
	case ActionBlocked:
		return "blocked"
	case ActionSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Valid reports whether a is a known action.
func (a CandidateAction) Valid() bool {
	return a == ActionDelete || a == ActionRetain || a == ActionBlocked || a == ActionSkipped
}

// SubsystemFilter restricts which subsystem GC may delete from. The zero value is
// not valid; the default is FilterAll.
type SubsystemFilter int

const (
	// FilterAll considers checkpoints, runs, blobs, and temp.
	FilterAll SubsystemFilter = iota + 1
	// FilterRunsOnly restricts deletion to run entries.
	FilterRunsOnly
	// FilterCheckpointsOnly restricts deletion to checkpoints (never blobs).
	FilterCheckpointsOnly
	// FilterBlobsOnly restricts deletion to unreachable blobs.
	FilterBlobsOnly
)

// ParseSubsystemFilter resolves a token to a SubsystemFilter, rejecting unknowns.
func ParseSubsystemFilter(s string) (SubsystemFilter, error) {
	switch s {
	case "all":
		return FilterAll, nil
	case "runs-only":
		return FilterRunsOnly, nil
	case "checkpoints-only":
		return FilterCheckpointsOnly, nil
	case "blobs-only":
		return FilterBlobsOnly, nil
	default:
		return 0, fmt.Errorf("invalid subsystem filter %q", s)
	}
}

func (f SubsystemFilter) String() string {
	switch f {
	case FilterAll:
		return "all"
	case FilterRunsOnly:
		return "runs-only"
	case FilterCheckpointsOnly:
		return "checkpoints-only"
	case FilterBlobsOnly:
		return "blobs-only"
	default:
		return "unknown"
	}
}

// Valid reports whether f is a known filter.
func (f SubsystemFilter) Valid() bool {
	return f == FilterAll || f == FilterRunsOnly || f == FilterCheckpointsOnly || f == FilterBlobsOnly
}

// Allows reports whether this filter permits deleting the given kind. A *-only
// filter restricts deletion to exactly its subsystem: blobs are never deleted under
// checkpoints-only (a later full GC collects them), and temp artifacts are deleted
// only by a full GC, not by any *-only filter — "restrict deletion to the selected
// subsystem" means temp is out of scope unless everything is.
func (f SubsystemFilter) Allows(k CandidateKind) bool {
	switch f {
	case FilterAll:
		return true
	case FilterRunsOnly:
		return k == KindRun
	case FilterCheckpointsOnly:
		return k == KindCheckpoint
	case FilterBlobsOnly:
		return k == KindBlob
	default:
		return false
	}
}
