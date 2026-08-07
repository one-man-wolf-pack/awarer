package gc

// RetentionReason is a stable machine token for why GC made one decision. The set
// is closed: a candidate cannot be built with a reason outside it, so reports never
// carry free-form strings an agent or script cannot rely on. Every reason maps to
// exactly one action (see reasonAction), so a candidate's reason and action can
// never disagree — there is no "delete because it is the latest" or "retain because
// it is unreachable".
type RetentionReason string

const (
	// Delete reasons.
	ReasonCheckpointExpired RetentionReason = "checkpoint-expired"
	ReasonRunExpired        RetentionReason = "run-expired"
	ReasonBlobUnreachable   RetentionReason = "blob-unreachable"
	ReasonTempStale         RetentionReason = "temp-stale"
	// ReasonRestoreExpired marks a restore recovery observation older than the
	// independent gc.keep_restores_for window (or an explicit --older-than override).
	// Reclaiming it removes the ability to undo that restore, which is why the window
	// is its own policy rather than a side effect of run-history retention.
	ReasonRestoreExpired RetentionReason = "restore-expired"

	// Retain reasons.
	ReasonCheckpointLatest       RetentionReason = "checkpoint-latest"
	ReasonCheckpointWithinKeepN  RetentionReason = "checkpoint-within-keep-last"
	ReasonCheckpointTooRecent    RetentionReason = "checkpoint-too-recent"
	ReasonCheckpointNotCommitted RetentionReason = "checkpoint-not-committed"
	ReasonRunTooRecent           RetentionReason = "run-too-recent"
	ReasonRunNotCommitted        RetentionReason = "run-not-committed"
	ReasonBlobReferenced         RetentionReason = "blob-referenced"
	ReasonTempFresh              RetentionReason = "temp-fresh"
	ReasonGitUnavailable         RetentionReason = "git-unavailable"
	// ReasonRestoreTooRecent marks a recovery observation inside its retention
	// window. Git history advancing is never a reason to reclaim one: a commit does
	// not make the pre-restore state of an uncommitted worktree disposable.
	ReasonRestoreTooRecent RetentionReason = "restore-too-recent"

	// Blocked reasons.
	ReasonLockActive        RetentionReason = "lock-active"
	ReasonLockUnknown       RetentionReason = "lock-unknown"
	ReasonCheckpointCorrupt RetentionReason = "checkpoint-corrupt"
	ReasonRunCorrupt        RetentionReason = "run-corrupt"
	// ReasonCheckpointIncompatible and ReasonRunIncompatible mark a record whose
	// declared schema this build cannot read. They block rather than reclaim. The
	// record is intact and its schema is simply unknown here, and an unknown schema is
	// exactly what makes its ownership and reference model unprovable: nothing can
	// demonstrate what such a record points at, so GC must not treat it as disposable.
	// It is retained until the user resets local evidence explicitly.
	//
	// They are distinct from the *-corrupt reasons on purpose. Both block, but they
	// mean different things to whoever reads the report: corruption is damage to
	// investigate, an incompatible schema is intact evidence this binary has no reader
	// for. Collapsing them would tell a user their store is broken when it is not.
	ReasonCheckpointIncompatible RetentionReason = "checkpoint-incompatible"
	ReasonRunIncompatible        RetentionReason = "run-incompatible"
	// ReasonRestoreCorrupt marks a recovery observation GC cannot read. Like every
	// unreadable record it is retained, and it additionally references blobs, so an
	// unreadable one leaves blob reachability incomplete and the sweep must fail
	// closed until doctor or reinitialization resolves it.
	ReasonRestoreCorrupt RetentionReason = "restore-corrupt"

	// Skipped reasons.
	ReasonSubsystemFiltered RetentionReason = "subsystem-filtered"
)

// reasonSpec is everything the domain fixes about one reason: the single action it
// justifies and the candidate kinds it may describe.
type reasonSpec struct {
	reason RetentionReason
	action CandidateAction
	// kinds ties a reason to its subsystem — a lock reason can only attach to a lock,
	// a blob reason only to a blob. The few cross-kind entries (a committed or
	// filtered decision that applies to more than one subsystem) are the exceptions.
	kinds []CandidateKind
}

// reasonCatalog is the canonical enumeration of the closed vocabulary and the ONE
// source of truth about it: Valid, Action, allowsKind, and RetentionReasons all read
// it, so membership, the action a reason justifies, the kinds it may describe, and the
// published order cannot disagree. Making an invalid decision unrepresentable is one
// table's job here rather than three parallel ones.
//
// The order is semantic and deliberate, not alphabetical: it groups by action —
// delete, retain, blocked, skipped — because that is the order a gc report reads in
// and the order the documentation that must cover this vocabulary presents it in.
// Consumers may rely on it being stable.
var reasonCatalog = []reasonSpec{
	// Deleted: proved disposable.
	{ReasonCheckpointExpired, ActionDelete, []CandidateKind{KindCheckpoint}},
	{ReasonRunExpired, ActionDelete, []CandidateKind{KindRun}},
	{ReasonBlobUnreachable, ActionDelete, []CandidateKind{KindBlob}},
	{ReasonTempStale, ActionDelete, []CandidateKind{KindTemp}},
	{ReasonRestoreExpired, ActionDelete, []CandidateKind{KindRestore}},

	// Retained: a retention rule protects it.
	{ReasonCheckpointLatest, ActionRetain, []CandidateKind{KindCheckpoint}},
	{ReasonCheckpointWithinKeepN, ActionRetain, []CandidateKind{KindCheckpoint}},
	{ReasonCheckpointTooRecent, ActionRetain, []CandidateKind{KindCheckpoint}},
	{ReasonCheckpointNotCommitted, ActionRetain, []CandidateKind{KindCheckpoint}},
	{ReasonRunTooRecent, ActionRetain, []CandidateKind{KindRun}},
	{ReasonRunNotCommitted, ActionRetain, []CandidateKind{KindRun}},
	{ReasonBlobReferenced, ActionRetain, []CandidateKind{KindBlob}},
	{ReasonTempFresh, ActionRetain, []CandidateKind{KindTemp}},
	{ReasonRestoreTooRecent, ActionRetain, []CandidateKind{KindRestore}},
	{ReasonGitUnavailable, ActionRetain, []CandidateKind{KindCheckpoint, KindRun, KindTemp, KindRestore}},

	// Blocked: an obstruction stands in the way, so nothing is removed.
	{ReasonLockActive, ActionBlocked, []CandidateKind{KindLock}},
	{ReasonLockUnknown, ActionBlocked, []CandidateKind{KindLock}},
	{ReasonCheckpointCorrupt, ActionBlocked, []CandidateKind{KindCheckpoint}},
	{ReasonCheckpointIncompatible, ActionBlocked, []CandidateKind{KindCheckpoint}},
	{ReasonRunCorrupt, ActionBlocked, []CandidateKind{KindRun}},
	{ReasonRunIncompatible, ActionBlocked, []CandidateKind{KindRun}},
	{ReasonRestoreCorrupt, ActionBlocked, []CandidateKind{KindRestore}},

	// Skipped: a subsystem filter excluded an otherwise-deletable item.
	{ReasonSubsystemFiltered, ActionSkipped, []CandidateKind{KindCheckpoint, KindRun, KindBlob, KindTemp, KindRestore}},
}

// RetentionReasons returns the closed reason vocabulary in canonical order. The result
// is a copy: the catalog is package state, and a consumer that walks it must not be
// able to reorder or truncate what every other consumer sees.
func RetentionReasons() []RetentionReason {
	out := make([]RetentionReason, 0, len(reasonCatalog))
	for _, s := range reasonCatalog {
		out = append(out, s.reason)
	}
	return out
}

// reasonAction and reasonKinds are the lookup views of reasonCatalog, built once so
// the hot paths stay map lookups without becoming second hand-maintained lists.
var reasonAction, reasonKinds = func() (map[RetentionReason]CandidateAction, map[RetentionReason]map[CandidateKind]bool) {
	actions := make(map[RetentionReason]CandidateAction, len(reasonCatalog))
	kinds := make(map[RetentionReason]map[CandidateKind]bool, len(reasonCatalog))
	for _, s := range reasonCatalog {
		actions[s.reason] = s.action
		set := make(map[CandidateKind]bool, len(s.kinds))
		for _, k := range s.kinds {
			set[k] = true
		}
		kinds[s.reason] = set
	}
	return actions, kinds
}()

// Valid reports whether r is a known retention reason.
func (r RetentionReason) Valid() bool {
	_, ok := reasonAction[r]
	return ok
}

// allowsKind reports whether r may describe a candidate of kind k.
func (r RetentionReason) allowsKind(k CandidateKind) bool {
	return reasonKinds[r][k]
}

func (r RetentionReason) String() string { return string(r) }

// Action returns the single action this reason justifies. It is only meaningful for
// a valid reason; an unknown reason returns the zero action (which is not Valid).
func (r RetentionReason) Action() CandidateAction { return reasonAction[r] }

// needsUserAction reports whether r marks durable state GC cannot resolve on its own
// — damage it must not delete, or a schema it cannot read — which blocks the sweep
// and maps to the state-action-required exit code. Both need a person: one to repair
// or discard damage, the other to reset evidence this build has no reader for.
func (r RetentionReason) needsUserAction() bool {
	return r == ReasonCheckpointCorrupt || r == ReasonRunCorrupt || r == ReasonRestoreCorrupt ||
		r == ReasonCheckpointIncompatible || r == ReasonRunIncompatible
}

// Incompatible reports whether r marks an intact record in a schema this build cannot
// read, as opposed to damage. A presentation layer uses it to name the obstruction
// honestly instead of calling every blocked plan corruption.
func (r RetentionReason) Incompatible() bool {
	return r == ReasonCheckpointIncompatible || r == ReasonRunIncompatible
}

// isLock reports whether r marks a lock obstruction that blocks destructive GC but
// is not corruption (exit 0, no deletion).
func (r RetentionReason) isLock() bool {
	return r == ReasonLockActive || r == ReasonLockUnknown
}
