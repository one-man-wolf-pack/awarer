// Package restore is the application service behind "awa restore".
//
// It orchestrates one use case: prove a desired worktree state from one immutable
// awa state, and — only when explicitly asked — install exactly that state at
// exactly the selected paths, after preserving everything it is about to destroy.
//
// The service owns sequencing and policy; it owns no filesystem, hashing, or
// storage details, which live behind the ports below. Two properties shape almost
// every decision here:
//
//   - Preview is the default and reads no file content. It resolves the source
//     once, streams a canonical inverse comparison, and reports what it found. It
//     never writes, never stages, and never opens a blob's bytes.
//   - Apply is fail-before-mutation. Everything that can refuse — evidence gaps,
//     unstageable payloads, an incomplete recovery observation — refuses before the
//     first worktree write. What cannot be made transactional (a multi-path commit)
//     is reported honestly as partial rather than dressed up as success.
//
// Nothing here holds the operation set in memory. The comparison streams, the plan
// spills to an awa-owned spool, and apply walks that spool once per commit phase.
package restore

import (
	"context"
	"fmt"
	"io"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/blob"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/projfs"
)

// Spool is the awa-owned spill that holds one invocation's validated mutating
// operations. Apply walks it once per commit phase, so it is re-openable; nothing
// materializes it. The restorespool adapter satisfies it.
type Spool interface {
	// Add appends one ready mutating operation in canonical path order.
	Add(op restore.PlannedOperation) error
	// Seal finishes writing; cursors may only be opened after it.
	Seal() error
	// MaxDirectoryDepth reports the deepest directory-deletion path spooled, so the
	// commit can walk directory removals deepest-first in bounded memory.
	MaxDirectoryDepth() int
	// Open returns a fresh cursor in canonical path order.
	Open(ctx context.Context) (restore.OperationCursor, error)
	// Discard removes the spool. It is idempotent and touches only awa's own temp.
	Discard() error
}

// Applier stages verified payloads and installs operations. The worktreemut
// adapter satisfies it. Staging is content-addressed, so Payload can rebuild a
// capability from a spooled operation without a map that would grow with the plan.
type Applier interface {
	// Stage reads source bytes through open, verifies them against content, and
	// records them in awa-owned staging.
	Stage(ctx context.Context, content hashing.ContentHash, open func() (io.ReadCloser, error)) (restore.StagedPayload, error)
	// Payload returns the capability for already-staged content.
	Payload(content hashing.ContentHash) (restore.StagedPayload, error)
	// Staged reports how many payloads were verified into staging.
	Staged() int
	// Apply installs one staged operation, guarding the destination immediately
	// before acting on it. It reports what it did to that destination through one
	// validated value: an error alone cannot distinguish "refused, nothing touched" from
	// "removed the old node and then failed", and those lead to different honest
	// outcomes. There is deliberately no separate error return — the failure travels
	// inside the result, so an implementation cannot describe an effect and a failure
	// that contradict each other.
	Apply(ctx context.Context, op restore.StagedOperation) restore.MutationResult
	// Discard removes the staging area. It is idempotent.
	Discard() error
}

// WorktreeContent opens the current bytes of one observed worktree file. The
// recovery observation needs them: what the commit is about to destroy is exactly
// what would otherwise be unrecoverable.
//
// It is a port rather than a property of the observation for two reasons. Memory: a
// scan that retains a content opener per file costs one closure for every regular
// entry in the project, while a restore reads the bytes of its selection only, so
// asking for openers on demand makes apply's cost proportional to what it will change
// instead of to the worktree. Honesty: an opener retained only for blob-intent entries
// silently ties undo evidence to the project's ordinary storage policy, so a large
// file the scan recorded hash-only could not be preserved even though it reads
// perfectly. Undo evidence may not have that gap.
//
// observed is the stat signature the observation recorded, and the implementation must
// refuse to return bytes for anything that is no longer that exact node — including a
// path reached through a symlinked component.
type WorktreeContent interface {
	Open(path worktree.RelPath, observed worktree.StatSignature) (io.ReadCloser, error)
}

// LockAcquirer takes one lock and returns its release function. Restore takes two:
// an exclusive restore lock that serializes restore against restore, and a writer
// presence lock that keeps a concurrent collector from reclaiming the blobs this
// operation is about to read. The lockfile-backed adapters at the composition root
// satisfy it.
type LockAcquirer interface {
	Acquire() (release func() error, err error)
}

// Deps are the service's ports.
type Deps struct {
	// Resolver resolves the source reference once, to one immutable identity.
	Resolver *state.Resolver
	// Scanner produces the current observation. Restore takes it twice on an
	// apply: once to plan, once to revalidate immediately before committing.
	Scanner *scanner.Service
	// Blobs is restore's private content capability. It is deliberately not the
	// public metadata-only provider: restore streams a blob into staging, so a
	// large file is recovered without ever fitting in memory, and neighbouring
	// read-only tools never receive this port.
	Blobs blob.Store
	// Recovery publishes and reads pre-restore recovery observations.
	Recovery restore.RecoveryRepository
	// Current opens the current bytes of an observed worktree file, for the recovery
	// observation to capture before anything is overwritten.
	Current WorktreeContent
	// Spools and Appliers mint this invocation's spill and staging.
	Spools   func(id restore.OperationID) (Spool, error)
	Appliers func(id restore.OperationID) (Applier, error)
	// Exclusive serializes restore against restore. Presence keeps a concurrent
	// collector from reclaiming referenced blobs mid-operation. Both are optional:
	// a nil acquirer skips that lock, which only tests and callers managing
	// exclusion themselves do.
	Exclusive LockAcquirer
	Presence  LockAcquirer
	// Now, Rand, and Version stamp the recovery observation. Now and Rand are
	// injected so tests are deterministic.
	Now     func() time.Time
	Rand    io.Reader
	Version string
}

// Service plans and applies restores. Construct it with New.
type Service struct {
	deps Deps
}

// New builds the service, failing loudly on a missing required port rather than
// deferring a nil dereference to the first restore.
func New(d Deps) *Service {
	switch {
	case d.Resolver == nil:
		panic("restore.New: state resolver must not be nil")
	case d.Scanner == nil:
		panic("restore.New: scanner must not be nil")
	case d.Blobs == nil:
		panic("restore.New: blob store must not be nil")
	case d.Recovery == nil:
		panic("restore.New: recovery repository must not be nil")
	case d.Current == nil:
		panic("restore.New: current-content reader must not be nil")
	case d.Spools == nil:
		panic("restore.New: spool factory must not be nil")
	case d.Appliers == nil:
		panic("restore.New: applier factory must not be nil")
	case d.Now == nil:
		panic("restore.New: clock must not be nil")
	case d.Rand == nil:
		panic("restore.New: randomness source must not be nil")
	}
	return &Service{deps: d}
}

// Request is one restore invocation.
type Request struct {
	Project projfs.Project
	// Config is the already-effective current config (trust override applied). It
	// is loaded because applying to the current worktree requires a
	// policy-consistent destination observation.
	Config domainconfig.Config
	// Ref is the parsed source reference. The CLI has already rejected "now" and
	// ranges; the service rejects them again rather than trusting that.
	Ref state.Ref
	// Selection is the normalized scope: explicit paths, or all proven scope.
	Selection restore.Selection
	// Apply requests mutation. Without it the invocation is preview-only and is
	// provably side-effect free.
	Apply bool
}

// Result is what one invocation produced: the validated domain result plus the
// few presentation facts that are not domain concerns (the source's own message
// and observation time), so the CLI can name the source the way a human expects
// without re-resolving it.
type Result struct {
	Result restore.ApplyResult
	// SourceMessage is the checkpoint message when the source is a checkpoint.
	SourceMessage string
	// SourceTime is when the source state was observed, and HasSourceTime whether
	// that is known (a run observation records none on this surface).
	SourceTime    time.Time
	HasSourceTime bool
	// RecoveryRef is the `restore:<id>:before` reference of the observation this
	// invocation published, empty when it published none.
	RecoveryRef string
	// Diagnostic is the failure behind a reported stop, in whatever words the operating
	// system or the failing adapter used. It exists because a stop that leaves the
	// worktree changed cannot be returned as an error — the result is the only report of
	// what landed — and because a reason token plus a path does not say what went wrong.
	// It is human-facing detail, bounded in length, and never part of the typed contract:
	// a machine consumer branches on the outcome and the closed reason tokens beside it.
	Diagnostic string
}

// Run executes the use case: preview by default, mutate only with Apply.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	if req.Selection.IsZero() {
		return Result{}, fmt.Errorf("restore: a normalized selection is required")
	}
	if err := rejectMutableSource(req.Ref); err != nil {
		return Result{}, err
	}
	if !req.Apply {
		return s.preview(ctx, req)
	}
	return s.apply(ctx, req)
}

// rejectMutableSource re-checks the source-reference vocabulary the CLI already
// validated. The rule — restore reads immutable evidence and writes the current
// worktree — is a product invariant, so the service does not rely on a caller
// having enforced it.
func rejectMutableSource(ref state.Ref) error {
	if ref.Kind == state.RefNow {
		return fmt.Errorf("restore: %q is not a restore source: the current worktree is the destination, not the evidence", ref.Display)
	}
	return nil
}

// preview plans without mutating anything. It takes no lock, opens no blob bytes,
// creates no operation spool, stages no payload, publishes nothing durable, and
// touches no worktree path: the plan it produces is bounded counts plus a bounded
// sample, so there is no operation set to spill.
//
// One qualifier, because the stronger claim would be false: the current-state
// observation is the scanner's, and a scan whose record count exceeds the sorter's
// in-memory buffer spills through awa's temp area like every other read-only
// comparison. So "a preview needs nothing writable" holds for an observation that
// fits that buffer, not for an arbitrarily large one. What holds unconditionally is
// that a preview adds no restore state of its own, and therefore cannot leave any
// behind however it is interrupted.
func (s *Service) preview(ctx context.Context, req Request) (Result, error) {
	sess, err := s.begin(ctx, req, false)
	if err != nil {
		return Result{}, err
	}
	defer sess.close()

	plan, err := sess.plan(ctx)
	if err != nil {
		return Result{}, err
	}
	res, err := restore.PreviewResult(plan)
	if err != nil {
		return Result{}, err
	}
	return sess.result(res), nil
}

// sourceKindOf maps a resolved state to the domain's source kind, refusing any
// state that is not immutable evidence.
func sourceKindOf(rs *state.ResolvedState) (restore.SourceKind, error) {
	switch rs.Kind {
	case state.KindCheckpoint:
		return restore.SourceCheckpoint, nil
	case state.KindRunObservation:
		return restore.SourceRunObservation, nil
	case state.KindRestoreObservation:
		return restore.SourceRecoveryObservation, nil
	default:
		return 0, fmt.Errorf("restore: %q is not immutable evidence", rs.CanonicalRef)
	}
}

// sourceOf builds the domain source identity from a resolved state. The full
// immutable id is what output publishes and what a later apply acts on, so it is
// taken from the resolved identity rather than from what the user typed.
func sourceOf(rs *state.ResolvedState) (restore.Source, error) {
	kind, err := sourceKindOf(rs)
	if err != nil {
		return restore.Source{}, err
	}
	id := rs.CanonicalRef
	switch kind {
	case restore.SourceCheckpoint:
		cid, ok := rs.CheckpointID()
		if !ok {
			return restore.Source{}, fmt.Errorf("restore: resolved checkpoint state without an id")
		}
		id = cid.String()
	case restore.SourceRunObservation:
		runID, sel, _, _, ok := rs.RunObservation()
		if !ok {
			return restore.Source{}, fmt.Errorf("restore: resolved run state without an observation id")
		}
		id = runID
		return restore.NewSource(kind, id, "run:"+runID+":"+sel.String(), rs.RequestedRef)
	case restore.SourceRecoveryObservation:
		rec, _, ok := rs.RecoveryObservation()
		if !ok {
			return restore.Source{}, fmt.Errorf("restore: resolved recovery state without a record")
		}
		return restore.NewSource(kind, rec.ID().String(), rec.BeforeRef(), rs.RequestedRef)
	}
	return restore.NewSource(kind, id, id, rs.RequestedRef)
}

// currentScanOptions are the observation policy restore takes the current worktree
// under. Preview and apply use exactly the same ones, so a preview predicts what an
// apply will find rather than describing a laxer observation — and so an apply is
// never observing under a policy a user could not have previewed.
//
// AllowSkippedInputs keeps an unreadable input from aborting the whole invocation:
// it becomes a per-path evidence gap that blocks only the operations it intersects.
// FailOnObservationChange is the opposite posture for a *moving* input: a file that
// changed while being observed means the snapshot is not a consistent point in
// time, and a destructive operation planned from an inconsistent snapshot is not
// evidence at all, so the observation is refused outright.
//
// Content-source retention is deliberately left at none. Retaining any would make the
// observation hold a closure per blob-intent regular entry — a cost scaling with the
// project rather than with the selection — and it would tie undo evidence to the
// storage policy, since it retains nothing for an entry the scan recorded hash-only.
// Apply reads the bytes it must preserve through the WorktreeContent port instead.
func currentScanOptions() scanner.Options {
	return scanner.Options{
		AllowSkippedInputs:      true,
		ReadOnly:                true,
		FailOnObservationChange: true,
	}
}

// observeCurrent takes one current-worktree observation under the restore policy.
func (s *Service) observeCurrent(ctx context.Context, req Request) (scanner.Result, error) {
	return s.deps.Scanner.Scan(ctx, req.Project, req.Config, req.Config.HistoryScanScope(), currentScanOptions())
}

// compatibility compares a source's persisted scan identity with the current
// effective one and reports what that permits.
//
// A differing scan-config means the two observations had different boundaries:
// positive evidence (this path held these bytes) still holds, but *absence* does
// not — a path missing from the source may simply have been out of the old scope —
// so deletions lose their justification while creates and replaces keep theirs.
type compatibility struct {
	policyMatches bool
}

func (c compatibility) deletionsProven() bool { return c.policyMatches }

// compatibilityOf compares the source's persisted scan identity with the identity
// of the current observation this invocation just took. It reads the observation
// directly rather than resolving "now" a second time: a second scan would be a
// second, different snapshot, and comparing the source against a snapshot the plan
// was not built from would make the compatibility verdict describe something other
// than the plan.
func compatibilityOf(source *state.ResolvedState, current scanner.Result) compatibility {
	return compatibility{policyMatches: source.ScanIdentity() == current.Meta().ConfigHash}
}

// observedNode is a manifest record projected into planning terms: the proved node
// state, plus how the observation reached it.
type observedNode struct {
	state restore.NodeState
	// traversal is the record's full provenance: whether it was reached by following
	// symlinks, through which link, and after how many hops. All of it matters, because
	// all of it is part of the project's canonical worktree identity — the tree hash
	// folds provenance exactly so that the same bytes reached two different ways are not
	// the same observed state. Restore's own comparison must not be coarser than that,
	// or it would report "already matches" for a shape the source never proved.
	//
	// It travels beside the state rather than short-circuiting the projection, because
	// whether the boundary matters depends on the comparison: a path that already matches
	// its source needs no mutation at all, and refusing it would turn a settled path into
	// an evidence gap.
	traversal worktree.TraversalInfo
}

// followed reports whether the record was reached through a symlink, so the path it is
// filed under is not one awa may write through.
func (n observedNode) followed() bool { return n.traversal.Followed }

// entryNode projects a manifest record into a proved node state, reporting the
// closed reason when the record describes something restore cannot act on.
func entryNode(rec worktree.ManifestRecord) (observedNode, restore.Reason, bool) {
	if rec.IsZero() {
		return observedNode{state: restore.AbsentNode()}, "", true
	}
	if rec.IsSkipped() {
		return observedNode{}, restore.ReasonSkippedBoundary, false
	}
	e, _ := rec.Entry()
	n, err := restore.NodeFromEntry(e)
	if err != nil {
		return observedNode{}, restore.ReasonUnsupportedEntryKind, false
	}
	return observedNode{state: n, traversal: e.Traversal}, "", true
}
