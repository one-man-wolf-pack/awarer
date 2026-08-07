package provider

import (
	"fmt"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

// Kind distinguishes the four resolvable state identities. run-before and run-after
// are distinct kinds, not a shared kind with a selector, so a run identity can never
// be "both" and a switch over Kind is exhaustive.
type Kind int

const (
	// KindCurrentWorktree is the ephemeral "now" observation of the configured scope.
	// It is comparable but never reconstructable.
	KindCurrentWorktree Kind = iota + 1
	// KindCheckpoint is a stored checkpoint.
	KindCheckpoint
	// KindRunBefore is a recorded run's pre-execution observed state.
	KindRunBefore
	// KindRunAfter is a recorded run's post-execution observed state.
	KindRunAfter
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	return k == KindCurrentWorktree || k == KindCheckpoint || k == KindRunBefore || k == KindRunAfter
}

// String returns the stable machine token for the kind.
func (k Kind) String() string {
	switch k {
	case KindCurrentWorktree:
		return "current-worktree"
	case KindCheckpoint:
		return "checkpoint"
	case KindRunBefore:
		return "run-before"
	case KindRunAfter:
		return "run-after"
	default:
		return "unknown"
	}
}

// ScanIdentity is the scan/observation policy identity needed to interpret a tree
// hash: the scan-config hash, which folds scope, ignore policy, trust mode, and
// large-file policy. It exists only in its complete form — a non-zero scan-config
// hash — because every resolvable state (checkpoint, current worktree, and run
// observation) carries one. NewScanIdentity is the only constructor and rejects an
// incomplete identity, so the zero value is the sole invalid ScanIdentity; Valid()
// guards it. There is no partial or "unknown" policy state.
//
// The hash primitive is not part of the identity: it is fixed product behavior, so
// two scan policies can never differ by it.
type ScanIdentity struct {
	scanConfig hashing.ConfigHash
}

// NewScanIdentity builds a scan identity from a checkpoint header or a
// current-worktree scan. It requires a non-zero scan-config hash — the only form a
// scan identity has — so a constructed identity is always complete.
func NewScanIdentity(scanConfig hashing.ConfigHash) (ScanIdentity, error) {
	if scanConfig.IsZero() {
		return ScanIdentity{}, fmt.Errorf("scan identity requires a scan-config hash")
	}
	return ScanIdentity{scanConfig: scanConfig}, nil
}

// ScanConfig returns the scan-config hash. It is the zero value only for the unset
// zero-value ScanIdentity, which no resolved identity carries.
func (s ScanIdentity) ScanConfig() hashing.ConfigHash { return s.scanConfig }

// Valid reports whether the scan identity is complete: a non-zero scan-config hash.
// Only the zero value is invalid; every constructed identity is valid. The identity
// constructors and provablyCompatible gate on it, so a zero-value operand can never
// enter a resolved identity or claim compatibility.
func (s ScanIdentity) Valid() bool {
	return !s.scanConfig.IsZero()
}

// Identity is the immutable projection of one resolved state. It is built only through
// the kind-specific constructors below, which own the required and forbidden fields for
// each kind, so these states are unrepresentable: an identity with both a checkpoint id
// and a run id; a reconstructable current-worktree; a resolved state without a tree
// hash. Ids are full strings and are never shortened.
type Identity struct {
	kind            Kind
	canonicalRef    string
	checkpointID    checkpoint.CheckpointID
	runID           runcache.RunID
	treeHash        hashing.TreeHash
	scan            ScanIdentity
	observedAt      time.Time
	hasObservedAt   bool
	reconstructable bool
	boundary        EvidenceBoundary
}

// NewCurrentWorktreeIdentity builds the identity of a "now" observation. It sets
// reconstructable=false unconditionally (a live worktree cannot be reconstructed) and
// carries no record id, so a reconstructable-now and a now-with-an-id are both
// unrepresentable. A current worktree is always scanned under the effective config, so it
// requires a valid scan identity. observedAt is the moment the scope was observed.
func NewCurrentWorktreeIdentity(treeHash hashing.TreeHash, scan ScanIdentity, observedAt time.Time, boundary EvidenceBoundary) (Identity, error) {
	if treeHash.IsZero() {
		return Identity{}, fmt.Errorf("current-worktree identity requires a tree hash")
	}
	if !scan.Valid() {
		return Identity{}, fmt.Errorf("current-worktree identity requires a valid scan identity")
	}
	return Identity{
		kind:            KindCurrentWorktree,
		canonicalRef:    "now",
		treeHash:        treeHash,
		scan:            scan,
		observedAt:      observedAt,
		hasObservedAt:   !observedAt.IsZero(),
		reconstructable: false,
		boundary:        boundary,
	}, nil
}

// NewCheckpointIdentity builds the identity of a stored checkpoint. It takes the typed
// checkpoint id (so a malformed or non-id string has no way in — the parse happened at the
// store boundary) and a tree hash, and it derives the canonical reference from the id (a
// checkpoint is referenced by its full id) so a ref that disagrees with the id has no code
// path. A checkpoint records its own scan policy, so it requires a valid scan identity.
// The run id is left zero (so both-ids is unrepresentable) and it is marked
// reconstructable (a checkpoint is reconstructable while its manifest remains
// readable). createdAt is the checkpoint's recorded creation time.
func NewCheckpointIdentity(checkpointID checkpoint.CheckpointID, treeHash hashing.TreeHash, scan ScanIdentity, createdAt time.Time, boundary EvidenceBoundary) (Identity, error) {
	if checkpointID.IsZero() {
		return Identity{}, fmt.Errorf("checkpoint identity requires a checkpoint id")
	}
	if treeHash.IsZero() {
		return Identity{}, fmt.Errorf("checkpoint identity requires a tree hash")
	}
	if !scan.Valid() {
		return Identity{}, fmt.Errorf("checkpoint identity requires a valid scan identity")
	}
	return Identity{
		kind:            KindCheckpoint,
		canonicalRef:    checkpointID.String(),
		checkpointID:    checkpointID,
		treeHash:        treeHash,
		scan:            scan,
		observedAt:      createdAt,
		hasObservedAt:   !createdAt.IsZero(),
		reconstructable: true,
		boundary:        boundary,
	}, nil
}

// NewRunObservationIdentity builds the identity of a recorded run's pre/post observed
// state. before selects the pre-execution observation (KindRunBefore) versus the
// post-execution one (KindRunAfter). It takes the typed run id (so a malformed or non-id
// string has no way in) and a tree hash, and it derives the canonical reference
// (run:<id>:<before|after>) from the id and selector so a mismatched ref has no code path.
// It carries no checkpoint id and is reconstructable while the observation manifest remains
// readable. A recorded run persists the scanner's complete scan_config_hash
// with each observation, so it requires a valid scan identity — a run observation whose
// policy identity is missing is unresolvable at the storage boundary, never a weakened
// partial identity here. observedAt is set only when the run recorded an observation
// time.
func NewRunObservationIdentity(before bool, runID runcache.RunID, treeHash hashing.TreeHash, scan ScanIdentity, observedAt time.Time, hasObservedAt bool, boundary EvidenceBoundary) (Identity, error) {
	if runID.IsZero() {
		return Identity{}, fmt.Errorf("run observation identity requires a run id")
	}
	if treeHash.IsZero() {
		return Identity{}, fmt.Errorf("run observation identity requires a tree hash")
	}
	if !scan.Valid() {
		return Identity{}, fmt.Errorf("run observation identity requires a valid scan identity")
	}
	kind := KindRunAfter
	sel := "after"
	if before {
		kind = KindRunBefore
		sel = "before"
	}
	return Identity{
		kind:            kind,
		canonicalRef:    "run:" + runID.String() + ":" + sel,
		runID:           runID,
		treeHash:        treeHash,
		scan:            scan,
		observedAt:      observedAt,
		hasObservedAt:   hasObservedAt && !observedAt.IsZero(),
		reconstructable: true,
		boundary:        boundary,
	}, nil
}

// Kind returns the resolved state kind.
func (i Identity) Kind() Kind { return i.kind }

// CanonicalRef returns the full canonical reference for the state (never shortened).
func (i Identity) CanonicalRef() string { return i.canonicalRef }

// CheckpointID returns the full checkpoint id as a string, or "" when the state is not a
// checkpoint. The string form is produced only here, at the projection boundary.
func (i Identity) CheckpointID() string { return i.checkpointID.String() }

// RunID returns the full run id as a string, or "" when the state is not a run
// observation. The string form is produced only here, at the projection boundary.
func (i Identity) RunID() string { return i.runID.String() }

// TreeHash returns the state's tree hash.
func (i Identity) TreeHash() hashing.TreeHash { return i.treeHash }

// Scan returns the scan/policy identity needed to interpret the tree hash.
func (i Identity) Scan() ScanIdentity { return i.scan }

// ObservedAt returns the observation/creation time and whether it is known.
func (i Identity) ObservedAt() (time.Time, bool) { return i.observedAt, i.hasObservedAt }

// Comparable reports whether the state can participate in a comparison as an operand.
// Every resolved identity carries a tree hash, so it is always true; whether a *counted*
// comparison is possible is a property of the operand pair, decided by the comparison
// domain, not of a single identity.
func (i Identity) Comparable() bool { return true }

// Reconstructable reports whether the state can be reconstructed from stored evidence
// while its manifest remains readable. It is always false for a current worktree.
func (i Identity) Reconstructable() bool { return i.reconstructable }

// Boundary returns the evidence-boundary facts for the observation.
func (i Identity) Boundary() EvidenceBoundary { return i.boundary }

// sameImmutableRecord reports whether two identities name the exact same immutable
// stored record — the same checkpoint id, or the same run id and the same run
// observation kind (before/before or after/after). A current worktree is never the same
// immutable record as anything, including another current-worktree observation, because
// each "now" is a fresh observation. This is the only basis on which a comparison
// involving an operand of unproven policy compatibility may be declared equal.
func (i Identity) sameImmutableRecord(other Identity) bool {
	switch i.kind {
	case KindCheckpoint:
		return other.kind == KindCheckpoint && !i.checkpointID.IsZero() && i.checkpointID == other.checkpointID
	case KindRunBefore, KindRunAfter:
		return other.kind == i.kind && !i.runID.IsZero() && i.runID == other.runID
	default:
		return false
	}
}
