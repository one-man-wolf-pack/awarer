package restore

import (
	"context"
	"fmt"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// OperationKind names what a restore operation does to one path. The zero value
// is not valid, so an operation must carry a kind derived from its proved
// current and desired states rather than one a caller chose.
type OperationKind int

const (
	// OpCreate materializes a path the source proves and the worktree lacks.
	OpCreate OperationKind = iota + 1
	// OpReplace overwrites a present regular file with the source's bytes.
	OpReplace
	// OpTypeChange replaces a present node with a different kind. It is the most
	// destructive shape: a directory becoming a file may only proceed once every
	// proved child operation has completed and the directory is empty.
	OpTypeChange
	// OpRestoreSymlink creates or replaces a symbolic link from its stored inline
	// target. The target is never followed.
	OpRestoreSymlink
	// OpDeleteFile removes a proved regular file or symlink the source proves
	// absent inside compatible scope.
	OpDeleteFile
	// OpDeleteEmptyDirectory removes a proved directory the source proves absent.
	// It is empty-only and deepest-first; recursive deletion never happens.
	OpDeleteEmptyDirectory
	// OpEqual records that the source and the worktree already agree. It exists so
	// counts can report proven-equal scope rather than leaving it unexplained.
	OpEqual
)

// String returns the stable machine token for the kind.
func (k OperationKind) String() string {
	switch k {
	case OpCreate:
		return "create"
	case OpReplace:
		return "replace"
	case OpTypeChange:
		return "type-change"
	case OpRestoreSymlink:
		return "restore-symlink"
	case OpDeleteFile:
		return "delete-file"
	case OpDeleteEmptyDirectory:
		return "delete-empty-directory"
	case OpEqual:
		return "equal"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a known kind.
func (k OperationKind) Valid() bool { return k >= OpCreate && k <= OpEqual }

// Mutates reports whether the kind changes the worktree. Equal does not, which
// is why an all-equal plan applies as a no-op rather than as work.
func (k OperationKind) Mutates() bool { return k.Valid() && k != OpEqual }

// Availability is whether a planned operation may run. The zero value is not
// valid.
//
// There are exactly two states, and deliberately no third one for a runtime
// conflict. Planning happens before any mutation, so what it can discover is
// whether the evidence supports an operation — not whether the destination will
// still match when the commit reaches it. A destination that moved is found by the
// re-observation and the per-path guard, and it is reported as an apply outcome plus
// bounded per-path Failures. Modelling it here too would give the same fact two
// owners and let a result publish a conflict outcome beside a zero conflict count.
type Availability int

const (
	// Ready: the source proves a desired state, the current precondition is
	// proved, and no boundary refuses the write.
	Ready Availability = iota + 1
	// Blocked: evidence is missing, unusable, or outside a boundary awa may act
	// on. A blocked operation is never executed and never guessed at.
	Blocked
)

// String returns the stable machine token for the availability.
func (a Availability) String() string {
	switch a {
	case Ready:
		return "ready"
	case Blocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// Valid reports whether a is a known availability.
func (a Availability) Valid() bool { return a == Ready || a == Blocked }

// StagedPayload is the capability that proves desired bytes have been read from
// a verified blob and written into awa-owned staging. It is the reason a ready
// regular-file write cannot be represented without verified content: the
// application service can only obtain one from the staging port after a
// same-descriptor integrity check, and NewStagedOperation demands it.
//
// The handle is opaque to the domain: infrastructure decides what it names, and
// the domain only requires that it exist and that its content identity match the
// desired state it will be written for.
type StagedPayload struct {
	content hashing.ContentHash
	size    int64
	handle  string
}

// NewStagedPayload records verified, staged bytes. It is deliberately not
// constructible from a hash alone: a caller must have produced a staging handle,
// which only the staging port does, and only after verifying the bytes.
func NewStagedPayload(content hashing.ContentHash, size int64, handle string) (StagedPayload, error) {
	if content.IsZero() {
		return StagedPayload{}, fmt.Errorf("a staged payload requires a content hash")
	}
	if size < 0 {
		return StagedPayload{}, fmt.Errorf("a staged payload cannot have negative size %d", size)
	}
	if handle == "" {
		return StagedPayload{}, fmt.Errorf("a staged payload requires a staging handle")
	}
	return StagedPayload{content: content, size: size, handle: handle}, nil
}

// Content returns the verified content hash of the staged bytes.
func (p StagedPayload) Content() hashing.ContentHash { return p.content }

// Size returns the staged byte count.
func (p StagedPayload) Size() int64 { return p.size }

// Handle returns the infrastructure-owned staging handle. It is never published
// in output: a staging path is internal state, not user-facing evidence.
func (p StagedPayload) Handle() string { return p.handle }

// IsZero reports whether no payload is staged.
func (p StagedPayload) IsZero() bool { return p.handle == "" }

// PlannedOperation is one path's decision as planning settled it: the derived
// kind, the proved current precondition, the desired source state, whether it may
// run, and — when it may not — the closed reasons why. It is what a preview
// describes. It deliberately carries no content capability: preview reads no
// bytes, so a type that could hold staged content here would misrepresent what
// preview is allowed to do.
type PlannedOperation struct {
	path    worktree.RelPath
	kind    OperationKind
	avail   Availability
	current NodeState
	desired NodeState
	reasons []Reason
}

// NewPlannedOperation builds a ready operation from a proved current
// precondition and a proved desired source state. The kind is derived from the
// two states rather than supplied, so an operation whose kind disagrees with what
// it would actually do cannot exist.
func NewPlannedOperation(path worktree.RelPath, current, desired NodeState) (PlannedOperation, error) {
	kind, err := deriveKind(path, current, desired)
	if err != nil {
		return PlannedOperation{}, err
	}
	return PlannedOperation{path: path, kind: kind, avail: Ready, current: current, desired: desired}, nil
}

// NewBlockedOperation builds the one unavailable shape planning can produce. At
// least one reason is required — a blocked operation with no explanation would be
// exactly the silent refusal restore exists to eliminate — and every reason must
// be one the closed vocabulary allows for a blocked operation, which excludes the
// runtime-only reasons that belong to an apply result.
func NewBlockedOperation(path worktree.RelPath, current, desired NodeState, reasons ...Reason) (PlannedOperation, error) {
	if len(reasons) == 0 {
		return PlannedOperation{}, fmt.Errorf("a blocked operation for %q requires at least one reason", path)
	}
	for _, r := range reasons {
		if !r.Valid() {
			return PlannedOperation{}, fmt.Errorf("invalid restore reason %q", r)
		}
		if !r.allowsAvailability(Blocked) {
			return PlannedOperation{}, fmt.Errorf("reason %q cannot describe a blocked operation", r)
		}
	}
	// A blocked operation still names what it would have done where that is
	// derivable; when the two states are not a legal operation at all (the usual
	// case for an evidence gap) it is reported without a kind claim.
	kind, err := deriveKind(path, current, desired)
	if err != nil {
		kind = 0
	}
	out := make([]Reason, len(reasons))
	copy(out, reasons)
	return PlannedOperation{path: path, kind: kind, avail: Blocked, current: current, desired: desired, reasons: out}, nil
}

// deriveKind settles what an operation does from its two proved states. It is the
// one place the create/replace/type-change/symlink/delete/equal taxonomy is
// decided, so human output, JSON, counts, and the apply executor cannot classify
// the same pair of states differently.
func deriveKind(path worktree.RelPath, current, desired NodeState) (OperationKind, error) {
	if path.IsZero() {
		return 0, fmt.Errorf("a restore operation requires a path")
	}
	if !current.present && !desired.present {
		return 0, fmt.Errorf("restore operation for %q has neither a current nor a desired state", path)
	}
	if current.Equal(desired) {
		return OpEqual, nil
	}
	switch {
	case !desired.present:
		if current.kind == worktree.KindDir {
			return OpDeleteEmptyDirectory, nil
		}
		return OpDeleteFile, nil
	case !current.present:
		if desired.kind == worktree.KindSymlink {
			return OpRestoreSymlink, nil
		}
		return OpCreate, nil
	case current.kind != desired.kind:
		return OpTypeChange, nil
	case desired.kind == worktree.KindSymlink:
		return OpRestoreSymlink, nil
	default:
		return OpReplace, nil
	}
}

// Path returns the project-relative path the operation acts on.
func (o PlannedOperation) Path() worktree.RelPath { return o.path }

// Kind returns the derived operation kind.
func (o PlannedOperation) Kind() OperationKind { return o.kind }

// Availability reports whether the operation may run.
func (o PlannedOperation) Availability() Availability { return o.avail }

// Current returns the proved current precondition.
func (o PlannedOperation) Current() NodeState { return o.current }

// Desired returns the proved desired source state.
func (o PlannedOperation) Desired() NodeState { return o.desired }

// Reasons returns why the operation is unavailable, as a copy so a consumer
// cannot edit a validated decision. It is empty for a ready operation.
func (o PlannedOperation) Reasons() []Reason {
	if len(o.reasons) == 0 {
		return nil
	}
	out := make([]Reason, len(o.reasons))
	copy(out, o.reasons)
	return out
}

// IsZero reports whether the operation was never built through a constructor.
func (o PlannedOperation) IsZero() bool { return o.kind == 0 && o.avail == 0 }

// RequiresContent reports whether executing this operation needs verified source
// bytes: a mutating operation whose desired state is a regular file. A symlink
// restores from its inline target and a delete needs no bytes at all.
func (o PlannedOperation) RequiresContent() bool {
	if !o.kind.Mutates() || !o.desired.present {
		return false
	}
	return o.desired.kind == worktree.KindRegular
}

// StagedOperation is a ready operation that has everything apply needs. It exists
// as a distinct type from PlannedOperation for one reason: an operation that
// requires content cannot become a StagedOperation without a StagedPayload whose
// identity matches the desired state. "Ready regular-file write with no verified
// bytes behind it" is therefore not a state the apply executor can be handed.
type StagedOperation struct {
	planned PlannedOperation
	payload StagedPayload
}

// NewStagedOperation binds a ready operation to its staged content. It rejects an
// unavailable operation, a missing payload for an operation that needs one, a
// payload attached to an operation that needs none, and a payload whose content
// identity is not the desired state's — so staged bytes for one path can never be
// written to another.
func NewStagedOperation(planned PlannedOperation, payload StagedPayload) (StagedOperation, error) {
	if planned.IsZero() {
		return StagedOperation{}, fmt.Errorf("cannot stage an unbuilt operation")
	}
	if planned.avail != Ready {
		return StagedOperation{}, fmt.Errorf("cannot stage %s operation for %q: it is %s", planned.kind, planned.path, planned.avail)
	}
	switch {
	case planned.RequiresContent() && payload.IsZero():
		return StagedOperation{}, fmt.Errorf("%s operation for %q requires verified staged content", planned.kind, planned.path)
	case !planned.RequiresContent() && !payload.IsZero():
		return StagedOperation{}, fmt.Errorf("%s operation for %q takes no content but was given a staged payload", planned.kind, planned.path)
	case planned.RequiresContent() && payload.Content() != planned.desired.Content():
		return StagedOperation{}, fmt.Errorf("staged content %s for %q does not match the desired content %s", payload.Content(), planned.path, planned.desired.Content())
	}
	return StagedOperation{planned: planned, payload: payload}, nil
}

// Planned returns the operation's planning decision.
func (o StagedOperation) Planned() PlannedOperation { return o.planned }

// Payload returns the verified staged content; it is zero for an operation that
// needs none.
func (o StagedOperation) Payload() StagedPayload { return o.payload }

// Path returns the project-relative path the operation acts on. Path, Kind,
// Current, and Desired forward the planning facts the executor acts on, so it
// never has to reach through to the planned operation for them.
func (o StagedOperation) Path() worktree.RelPath { return o.planned.path }

// Kind returns the derived operation kind.
func (o StagedOperation) Kind() OperationKind { return o.planned.kind }

// Current returns the proved current precondition.
func (o StagedOperation) Current() NodeState { return o.planned.current }

// Desired returns the proved desired source state.
func (o StagedOperation) Desired() NodeState { return o.planned.desired }

// IsZero reports whether the staged operation was never built.
func (o StagedOperation) IsZero() bool { return o.planned.IsZero() }

// MutationEffect is what installing one operation actually did to its destination
// before it returned. It is part of the mutation port's contract because "the
// operation completed" and "the worktree changed" are different facts, and only the
// code that performed the write knows which happened.
//
// The case that forces it: installing a link or a directory where something else
// stood has to remove the old node first, so a failure between the removal and the
// installation leaves the destination changed while completing nothing. Counting
// attempts cannot see that. Reported as a conflict — an outcome whose contract says
// nothing was written — it would be a machine-readable lie, and the recovery
// observation describing the removed node might be discarded as unnecessary.
type MutationEffect int

const (
	// EffectNone: the destination is exactly what it was. Every precondition refusal
	// lands here, as does a same-directory temp+rename that failed before the rename.
	EffectNone MutationEffect = iota + 1
	// EffectPartial: the destination changed without reaching its desired state. The
	// worktree is neither what the plan proved nor what it wanted.
	EffectPartial
	// EffectComplete: the destination holds the desired state.
	EffectComplete
)

// String returns the stable token for the effect. It is diagnostic only: the
// published vocabulary a consumer branches on is Outcome plus Reason.
func (e MutationEffect) String() string {
	switch e {
	case EffectNone:
		return "none"
	case EffectPartial:
		return "partial"
	case EffectComplete:
		return "complete"
	default:
		return "unknown"
	}
}

// Valid reports whether e is a known effect. The zero value is not: an applier that
// reported nothing must not be read as "nothing happened".
func (e MutationEffect) Valid() bool { return e >= EffectNone && e <= EffectComplete }

// Changed reports whether the destination differs from what the plan proved. An
// unknown effect counts as changed: if an applier failed to say, the safe reading is
// that the worktree may have moved, because the opposite reading discards the
// evidence needed to undo it.
func (e MutationEffect) Changed() bool { return e != EffectNone }

// MutationResult is everything one install reports back: what it did to the
// destination, and the failure that stopped it if there was one.
//
// It is a single validated value rather than an (effect, error) pair because the pair
// admitted combinations no install can be in — a destination that changed with no
// failure to explain it, and a destination reported untouched by a call that claims to
// have succeeded — and because reading "did the operation complete?" from the error
// alone gets the remaining case wrong. Only three states exist, one constructor each.
type MutationResult struct {
	effect MutationEffect
	err    error
}

// Completed reports that the destination holds the desired state.
func Completed() MutationResult { return MutationResult{effect: EffectComplete} }

// Interrupted reports that the destination changed without reaching the desired
// state: the old node was removed and its replacement could not be put in place. The
// worktree is neither what the plan proved nor what it wanted.
func Interrupted(err error) MutationResult { return MutationResult{effect: EffectPartial, err: err} }

// Untouched reports that the destination is exactly what it was. Every precondition
// refusal lands here, as does a same-directory temp+rename that failed before the
// rename.
func Untouched(err error) MutationResult { return MutationResult{effect: EffectNone, err: err} }

// Effect returns what happened to the destination.
func (r MutationResult) Effect() MutationEffect { return r.effect }

// Err returns the failure that stopped the install, or nil.
func (r MutationResult) Err() error { return r.err }

// Done reports whether the operation reached its desired state. It reads the effect,
// not the error: those are different questions, and the error is the wrong one.
func (r MutationResult) Done() bool { return r.effect == EffectComplete }

// Changed reports whether the destination differs from what the plan proved.
func (r MutationResult) Changed() bool { return r.effect.Changed() }

// Validate rejects the two shapes no install can legitimately report: an unbuilt
// result, and a failure-shaped effect with no failure to explain it. A caller checks
// it once, so a mutation port that mis-reports is a loud internal error rather than a
// silently miscounted commit.
func (r MutationResult) Validate() error {
	if !r.effect.Valid() {
		return fmt.Errorf("a mutation result must state what happened to the destination")
	}
	if r.effect != EffectComplete && r.err == nil {
		return fmt.Errorf("a mutation result reporting %s must carry the failure that stopped it", r.effect)
	}
	return nil
}

// Apply ranks. They are the phases the executor walks in order, and they are the
// whole dependency contract: writes land before anything is torn down, files are
// removed before the directories that held them, and directories are removed
// deepest-first. Exposing them as named constants lets the executor drive a
// stream-first multi-pass commit — one pass per rank, plus one pass per depth
// within the directory rank — without ever sorting a materialized operation set.
const (
	// RankWrite covers create, replace, type-change, and symlink restore.
	RankWrite = 0
	// RankDeleteFile covers proved file and symlink deletions.
	RankDeleteFile = 1
	// RankDeleteDirectory covers proved empty-directory deletions, walked
	// deepest-first.
	RankDeleteDirectory = 2
	// RankReplaceDirectory covers a type change whose CURRENT state is a directory:
	// the destination has to be emptied before anything can take its place, and the
	// entries that empty it are the proved child deletions in the two ranks above.
	// Running it with the other writes — where kind alone would put it — makes the
	// removal fail on a directory its own plan was about to empty.
	//
	// Nesting cannot arise here: two directory-replacing type changes where one path
	// contains the other would require the source to prove a file that holds a file,
	// so this rank needs no depth ordering of its own.
	RankReplaceDirectory = 3
	// rankNone is the rank of a non-mutating (equal) operation, which the executor
	// never walks.
	rankNone = 4
)

// Rank returns the apply phase a planned operation belongs to, or rankNone for a
// non-mutating one. Together with PathDepth it is the whole ordering contract:
// ordinary writes land first, then file deletions, then directory deletions
// deepest-first, and last the type changes that replace a directory — which can
// only run once the deletions above have emptied it.
//
// The rank is derived from the proved states, not from the kind alone. A type
// change is a write when it puts something where a file or a link used to be, and
// a directory replacement when the destination is currently a directory; those are
// opposite ends of the order, and deciding by kind would put both in the first
// phase, where the second one cannot succeed.
//
// The executor filters a spooled stream by rank and depth instead of sorting, so no
// materialized operation set exists to order.
func (o PlannedOperation) Rank() int {
	switch o.kind {
	case OpTypeChange:
		if o.current.present && o.current.kind == worktree.KindDir {
			return RankReplaceDirectory
		}
		return RankWrite
	case OpCreate, OpRestoreSymlink, OpReplace:
		return RankWrite
	case OpDeleteFile:
		return RankDeleteFile
	case OpDeleteEmptyDirectory:
		return RankDeleteDirectory
	default:
		return rankNone
	}
}

// PathDepth returns how many path components the operation's path has. The
// executor uses it to walk directory deletions deepest-first in bounded memory:
// one pass per depth, descending, instead of one sort over every deletion.
func (o PlannedOperation) PathDepth() int { return pathDepth(o.path) }

func pathDepth(p worktree.RelPath) int {
	if p.IsZero() {
		return 0
	}
	return strings.Count(p.String(), "/") + 1
}

// OperationCursor is a pull iterator over planned operations in canonical path
// order. It mirrors the manifest cursor contract: Next advances, Operation is
// valid only after Next returned true, Err is sticky, and Close is idempotent.
//
// It exists so a validated plan can be replayed from awa-owned spill instead of
// held in memory. A restore over a million paths must never require the operation
// set as a slice, which is why no accessor here returns one.
type OperationCursor interface {
	Next() bool
	Operation() PlannedOperation
	Err() error
	Close() error
}

// OperationStream opens fresh cursors over the same spooled operation sequence.
// Apply walks it several times — once per apply rank, and once per directory
// depth within the deletion rank — so re-openability is part of the contract.
type OperationStream interface {
	Open(ctx context.Context) (OperationCursor, error)
}

// There is deliberately no slice-backed OperationStream here, and none for the
// covered scope either. An operation set is exactly the thing that may be as large
// as the worktree, so the only implementation is the application's on-disk spool;
// production scope streams are likewise read from a durable record or derived from
// an operation set, never assembled from a slice a caller happens to hold.
