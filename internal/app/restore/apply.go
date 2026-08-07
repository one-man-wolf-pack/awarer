package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"awarer/internal/domain/blob"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
)

// apply runs the mutating half of the use case. Its shape is the product rule:
// everything that can refuse refuses before the first worktree write, and the one
// thing that cannot be made transactional — a commit spanning several paths — is
// reported honestly rather than smoothed over.
//
// The order below is the contract, not an implementation detail:
//
//  1. take the locks (restore-vs-restore, and the writer presence that keeps a
//     collector from reclaiming the blobs this operation is about to read);
//  2. resolve the source once and observe the current worktree;
//  3. stream and validate the complete plan into the spool;
//  4. refuse now if the plan is incomplete — a blocked operation means the
//     evidence does not support the whole selection;
//  5. verify and stage every required payload under awa-owned temporary state;
//  6. publish the immutable pre-restore recovery observation, capturing verified
//     bytes for everything the commit may destroy, and refuse if that is not
//     complete;
//  7. re-observe the selected current state and require exact equality with the
//     planned preconditions;
//  8. commit in dependency-safe order with a per-path guard at each destination,
//     which re-proves shape AND content identity at the descriptor it writes
//     through — the re-observation in step 7 cannot see a write that lands after
//     it.
func (s *Service) apply(ctx context.Context, req Request) (Result, error) {
	// Restore-vs-restore serializes on an exclusive lock: two concurrent restores
	// would each plan against a worktree the other is changing. Presence protection
	// then stands the operation down for an active collector and, once held, keeps
	// gc from reclaiming a blob this restore is about to read.
	if s.deps.Exclusive != nil {
		release, err := s.deps.Exclusive.Acquire()
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = release() }()
	}
	if s.deps.Presence != nil {
		release, err := s.deps.Presence.Acquire()
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = release() }()
	}

	sess, err := s.begin(ctx, req, true)
	if err != nil {
		return Result{}, err
	}
	defer sess.close()

	plan, err := sess.plan(ctx)
	if err != nil {
		return Result{}, err
	}

	// An incomplete plan is a refusal, not a partial apply: awa restores what one
	// state proves, and a blocked operation means it does not prove the selection.
	if !plan.Complete() {
		return sess.refusedByPlan(plan)
	}
	if !plan.Applicable() {
		// Complete, but nothing to change. That is a successful no-op, and publishing
		// a recovery observation for it would be evidence of a mutation that never
		// happened.
		res, err := restore.NewApplyResult(restore.ResultInput{
			Source: plan.Source(), Selection: plan.Selection(), Outcome: restore.OutcomeNoOp,
			Counts: plan.Counts(), Boundary: plan.Boundary(),
		})
		if err != nil {
			return Result{}, err
		}
		return sess.result(res), nil
	}

	applier, err := s.deps.Appliers(sess.id)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = applier.Discard() }()

	if st, err := sess.stageAll(ctx, applier); err != nil {
		return Result{}, err
	} else if st.refuses() {
		return sess.refusedByStop(plan, st)
	}

	if st, err := sess.publishRecovery(ctx); err != nil {
		return Result{}, err
	} else if st.refuses() {
		return sess.refusedByStop(plan, st)
	}

	// The whole selected current state is re-observed and compared against the
	// planned preconditions. A mismatch is a conflict: the plan no longer describes
	// reality, and silently re-planning inside --apply would mutate something the
	// user never previewed.
	//
	// A conflict here stops the invocation before its first write, so the recovery
	// observation published a moment ago describes an operation that never happened.
	// It is dropped rather than left in the timeline: a durable "you can undo this"
	// record for a restore that changed nothing invites an undo of work awa never
	// touched.
	conflicts, rerr := sess.revalidate(ctx)
	if rerr != nil {
		if derr := sess.dropRecovery(); derr != nil {
			return Result{}, derr
		}
		return Result{}, rerr
	}
	if len(conflicts) > 0 {
		if derr := sess.dropRecovery(); derr != nil {
			return Result{}, derr
		}
		res, err := restore.NewApplyResult(restore.ResultInput{
			Source: plan.Source(), Selection: plan.Selection(), Outcome: restore.OutcomeConflict,
			Counts: plan.Counts(), Boundary: plan.Boundary(),
			Remaining: sess.mutating, Reasons: []restore.Reason{restore.ReasonPreconditionMismatch},
			Failures: conflicts,
		})
		if err != nil {
			return Result{}, err
		}
		return sess.result(res), nil
	}

	return sess.commit(ctx, plan, applier)
}

// stop is why a fail-before-mutation stage refused: the invocation-level reason plus
// the bounded per-path facts that say where. Both halves matter — "blob-corrupt" tells
// a user what kind of problem they have, and the path tells them which file to look
// at, which for a corrupt blob or an unpreservable current file is the only actionable
// part. The zero value means "nothing refused".
type stop struct {
	reason   restore.Reason
	failures []restore.Failure
}

func (s stop) refuses() bool { return s.reason != "" }

// stoppedAt builds a stop naming one operation. A stage refuses at the first path it
// cannot serve, so the fact list is bounded by construction.
func stoppedAt(op restore.PlannedOperation, reason restore.Reason) (stop, error) {
	f, err := restore.NewFailure(op.Path(), op.Kind(), reason)
	if err != nil {
		return stop{}, err
	}
	return stop{reason: reason, failures: []restore.Failure{f}}, nil
}

// dropRecovery removes the recovery observation this invocation published, for an
// exit that turned out to happen before the first write. It is loud on failure: a
// record that could not be removed still exists, and reporting success while leaving
// it behind would make the timeline show an undo point for a restore that never ran.
func (sess *session) dropRecovery() error {
	if !sess.recovered {
		return nil
	}
	if err := sess.svc.deps.Recovery.Delete(sess.id); err != nil {
		return fmt.Errorf("restore: nothing was written, but the recovery observation %s could not be removed: %w", sess.id.Short(), err)
	}
	sess.recovered = false
	return nil
}

// refusedByPlan builds the fail-before-mutation result for evidence the plan itself
// found missing. Nothing was written, so it carries no recovery observation and no
// completed work — only why it stopped, at both levels: the distinct reasons, and the
// same bounded per-path facts the preview showed, so "why did my apply refuse" names
// paths rather than only tokens.
func (sess *session) refusedByPlan(plan restore.Plan) (Result, error) {
	failures, err := restore.FailuresFromSamples(plan)
	if err != nil {
		return Result{}, err
	}
	return sess.refuse(plan, blockedReasons(plan), failures)
}

// refusedByStop builds the fail-before-mutation result for a stage that refused while
// reading: a blob whose bytes are not what it promised, or a current file whose bytes
// could not be preserved. These are invisible to planning — only reading proves them —
// so the failing path comes from the stage rather than from the plan's blocked sample,
// which is empty for a complete plan.
func (sess *session) refusedByStop(plan restore.Plan, st stop) (Result, error) {
	return sess.refuse(plan, []restore.Reason{st.reason}, st.failures)
}

func (sess *session) refuse(plan restore.Plan, reasons []restore.Reason, failures []restore.Failure) (Result, error) {
	res, err := restore.NewApplyResult(restore.ResultInput{
		Source: plan.Source(), Selection: plan.Selection(), Outcome: restore.OutcomeRefused,
		Counts: plan.Counts(), Boundary: plan.Boundary(),
		Remaining: sess.mutating, Reasons: reasons, Failures: failures,
	})
	if err != nil {
		return Result{}, err
	}
	return sess.result(res), nil
}

// blockedReasons collects the distinct reasons the plan's bounded sample carries,
// so a refusal names what actually stopped it instead of a generic message.
func blockedReasons(plan restore.Plan) []restore.Reason {
	seen := map[restore.Reason]bool{}
	var out []restore.Reason
	for _, s := range plan.BlockedSamples() {
		for _, r := range s.Reasons {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// stageAll verifies and stages every payload the commit will need, before any
// mutation. It stops at the first source that cannot actually produce the bytes it
// promised — the case a metadata-only preview could not detect, because only reading
// proves a blob is not corrupt — and names that path.
func (sess *session) stageAll(ctx context.Context, applier Applier) (stop, error) {
	cur, err := sess.spool.Open(ctx)
	if err != nil {
		return stop{}, err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		if err := ctx.Err(); err != nil {
			return stop{}, err
		}
		op := cur.Operation()
		if !op.RequiresContent() {
			continue
		}
		want := op.Desired().Content()
		_, serr := applier.Stage(ctx, want, func() (io.ReadCloser, error) {
			return sess.svc.deps.Blobs.Open(want)
		})
		switch {
		case serr == nil:
		case errors.Is(serr, restore.ErrStagedContentMismatch), errors.Is(serr, blob.ErrCorruptBlob):
			return stoppedAt(op, restore.ReasonBlobCorrupt)
		case errors.Is(serr, context.Canceled), errors.Is(serr, context.DeadlineExceeded):
			return stop{}, serr
		case isMissingBlob(serr):
			return stoppedAt(op, restore.ReasonBlobMissing)
		default:
			return stop{}, serr
		}
	}
	return stop{}, cur.Err()
}

// isMissingBlob reports whether an error is the store not holding the requested
// bytes. A blob that vanished between planning and staging (a concurrent collector
// that beat the presence lock, or an externally emptied store) is an evidence gap,
// not an I/O fault.
func isMissingBlob(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

// publishRecovery writes the immutable pre-restore recovery observation: the exact
// current entries the commit may replace or delete, with verified bytes, plus the
// covered path set that lets a later inverse restore also undo the creates.
//
// Content is captured regardless of the project's ordinary hash-only preferences.
// A checkpoint may legitimately store identity without bytes; undo evidence may
// not, because the bytes are precisely what would be unrecoverable. If any selected
// current byte cannot be preserved, this returns a reason and apply refuses — there
// is no --force bypass.
func (sess *session) publishRecovery(ctx context.Context) (stop, error) {
	if st, err := sess.captureCurrentBytes(ctx); err != nil || st.refuses() {
		return st, err
	}
	build := restore.RecoveryBuild{
		ID:             sess.id,
		CreatedAt:      sess.svc.deps.Now().UTC(),
		AwaVersion:     sess.svc.deps.Version,
		ScanConfigHash: sess.current.Meta().ConfigHash,
		Source:         sess.sourceIdentity,
		Selection:      sess.req.Selection,
	}
	if _, err := sess.svc.deps.Recovery.Publish(ctx, build, &recoveryManifest{sess: sess}, &recoveryScope{sess: sess}); err != nil {
		return stop{}, err
	}
	sess.recovered = true
	return stop{}, nil
}

// captureCurrentBytes materializes the current content of every path the commit
// may destroy into the shared blob store, so the recovery observation's manifest
// references bytes that actually exist.
//
// The bytes are read through the WorktreeContent port, on demand, from the identity
// the observation recorded — not from an opener the scan retained. That is what makes
// this cost proportional to the selection rather than to the project, and it is also
// what makes the capture independent of storage policy: a large file the scan recorded
// hash-only is still perfectly readable, and refusing to preserve it merely because
// the project would not have stored it in a checkpoint would deny an undo for exactly
// the file most expensive to lose.
//
// It stops, naming the path, when the bytes genuinely cannot be preserved: an input
// that moved since the observation (so the bytes the plan proved are already gone), or
// a path the observation holds no entry for. Destroying either would be irreversible,
// and there is no --force bypass.
func (sess *session) captureCurrentBytes(ctx context.Context) (stop, error) {
	join, err := sess.openCoveredJoin(ctx)
	if err != nil {
		return stop{}, err
	}
	defer func() { _ = join.Close() }()
	for join.Next() {
		if err := ctx.Err(); err != nil {
			return stop{}, err
		}
		op := join.Operation()
		current := op.Current()
		if !current.Present() || current.Kind() != worktree.KindRegular {
			continue
		}
		entry, ok := join.Entry()
		if !ok {
			// The plan proved a present regular file here, and the observation the plan was
			// built from holds no usable entry for it. The two disagree, so nothing can be
			// preserved and nothing may be destroyed.
			return stoppedAt(op, restore.ReasonRecoveryIncomplete)
		}
		open := func() (io.ReadCloser, error) { return sess.svc.deps.Current.Open(entry.Path, entry.Stat) }
		if _, _, merr := sess.svc.deps.Blobs.Materialize(current.Content(), open); merr != nil {
			if errors.Is(merr, blob.ErrHashMismatch) || errors.Is(merr, worktree.ErrObservationChanged) {
				// The file changed between the observation and this read, so the bytes the
				// plan proved are already gone. Refuse rather than record evidence that
				// does not match what is about to be overwritten.
				return stoppedAt(op, restore.ReasonRecoveryIncomplete)
			}
			return stop{}, merr
		}
	}
	return stop{}, join.Err()
}

// openCoveredJoin walks the spooled plan and the current observation together. Both
// are in canonical path order, so it holds one record per side, needs no index, and
// costs nothing per covered path — the covered set may be as large as the restore.
func (sess *session) openCoveredJoin(ctx context.Context) (*coveredJoin, error) {
	scan, err := sess.current.Manifest().Open(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := sess.spool.Open(ctx)
	if err != nil {
		_ = scan.Close()
		return nil, err
	}
	return &coveredJoin{scan: worktree.Ordered(scan), ops: ops}, nil
}

// coveredJoin yields each planned operation together with the observation record for
// its path, or no record when the observation proved that path absent. The operation
// stream drives it, which is what makes "every covered path is accounted for" a
// property of the walk rather than of a caller's bookkeeping: the capture and the
// published manifest are two readings of the same join, so they cannot disagree about
// which paths the record covers.
type coveredJoin struct {
	scan worktree.ManifestCursor
	ops  restore.OperationCursor

	scanOK  bool
	scanRec worktree.ManifestRecord

	op      restore.PlannedOperation
	rec     worktree.ManifestRecord
	started bool
	err     error
	done    bool
}

func (j *coveredJoin) Next() bool {
	if j.done {
		return false
	}
	if !j.started {
		j.started = true
		j.scanOK = j.advanceScan()
	}
	if j.err != nil || !j.ops.Next() {
		j.done = true
		if err := j.ops.Err(); err != nil && j.err == nil {
			j.err = err
		}
		return false
	}
	j.op = j.ops.Operation()
	for j.scanOK && j.scanRec.Path().Less(j.op.Path()) {
		j.scanOK = j.advanceScan()
	}
	j.rec = worktree.ManifestRecord{}
	if j.scanOK && j.scanRec.Path().Equal(j.op.Path()) {
		j.rec = j.scanRec
		j.scanOK = j.advanceScan()
	}
	return j.err == nil
}

// Operation returns the planned operation at the current position.
func (j *coveredJoin) Operation() restore.PlannedOperation { return j.op }

// Record returns the observation record for the current operation's path; it is zero
// when the observation held nothing there.
func (j *coveredJoin) Record() worktree.ManifestRecord { return j.rec }

// Entry returns the observed entry for the current position, or false when the
// observation proved the path absent or recorded it as a skipped input.
func (j *coveredJoin) Entry() (worktree.Entry, bool) {
	if j.rec.IsZero() {
		return worktree.Entry{}, false
	}
	return j.rec.Entry()
}

func (j *coveredJoin) advanceScan() bool {
	if j.scan.Next() {
		j.scanRec = j.scan.Record()
		return true
	}
	if err := j.scan.Err(); err != nil && j.err == nil {
		j.err = err
	}
	return false
}

func (j *coveredJoin) Err() error { return j.err }

func (j *coveredJoin) Close() error {
	err1 := j.scan.Close()
	err2 := j.ops.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// recoveryManifest is the manifest the recovery observation publishes: the current
// observation's records, narrowed to the paths the commit may touch, with each
// regular entry advertising the blob storage this record actually has.
type recoveryManifest struct{ sess *session }

func (m *recoveryManifest) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	join, err := m.sess.openCoveredJoin(ctx)
	if err != nil {
		return nil, err
	}
	return &recoveryManifestCursor{join: join}, nil
}

type recoveryManifestCursor struct {
	join *coveredJoin
	cur  worktree.ManifestRecord
}

func (c *recoveryManifestCursor) Next() bool {
	for c.join.Next() {
		if c.join.Record().IsZero() {
			// A covered path the observation proved absent belongs in the scope, not in the
			// manifest: that absence is what lets an inverse restore delete a file this
			// restore is about to create.
			continue
		}
		c.cur = withCapturedStorage(c.join.Record())
		return true
	}
	return false
}

// withCapturedStorage makes a regular entry claim the content capability this record
// really has. It is the mirror of the run store's normalization, and for the same
// reason: a persisted manifest must describe what its own store publishes, not what
// the scanner would have done under project policy. Capture ran over this same join
// first and refused unless every regular entry's bytes were materialized, so blob
// storage here is a fact — including for an entry the scan intended as hash-only,
// which is exactly the file whose undo evidence matters most.
//
// Storage class is not part of tree identity (the canonical encoding folds path, kind,
// content hash, permission bits, and traversal), so this cannot move the tree hash the
// record commits to.
func withCapturedStorage(rec worktree.ManifestRecord) worktree.ManifestRecord {
	e, ok := rec.Entry()
	if !ok {
		return rec
	}
	if e.Kind != worktree.KindRegular || e.Storage == worktree.StorageBlob {
		return rec
	}
	e.Storage = worktree.StorageBlob
	return worktree.EntryRecord(e)
}

func (c *recoveryManifestCursor) Record() worktree.ManifestRecord { return c.cur }
func (c *recoveryManifestCursor) Err() error                      { return c.join.Err() }
func (c *recoveryManifestCursor) Close() error                    { return c.join.Close() }

// recoveryScope is the covered path set: every path the commit may touch, in
// canonical order. A path here but absent from the recovery manifest was proved
// absent at capture time, which is what lets an inverse restore delete a file this
// restore created rather than leaving it behind.
type recoveryScope struct{ sess *session }

func (s *recoveryScope) Open(ctx context.Context) (restore.CoveredScopeCursor, error) {
	ops, err := s.sess.spool.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &recoveryScopeCursor{ops: ops}, nil
}

type recoveryScopeCursor struct {
	ops restore.OperationCursor
	cur worktree.RelPath
}

func (c *recoveryScopeCursor) Next() bool {
	if !c.ops.Next() {
		return false
	}
	c.cur = c.ops.Operation().Path()
	return true
}

func (c *recoveryScopeCursor) Path() worktree.RelPath { return c.cur }
func (c *recoveryScopeCursor) Err() error             { return c.ops.Err() }
func (c *recoveryScopeCursor) Close() error           { return c.ops.Close() }

// revalidate re-observes the current worktree and requires every planned
// precondition to still hold. It is the second of the two observations the apply
// protocol takes: awa cannot lock an editor or a build tool, so a fresh whole-scope
// comparison plus the per-path guard at commit time is the honest boundary — and
// this one runs after staging, so the window it leaves open is as small as the
// commit itself. What lands inside even that window is caught by the applier's
// per-path guard, which re-derives a regular file's content identity from the
// descriptor it is about to write through.
//
// It returns bounded conflict facts rather than a refreshed plan: silently
// re-planning inside --apply would mutate something the preview never showed.
func (sess *session) revalidate(ctx context.Context) ([]restore.Failure, error) {
	fresh, err := sess.svc.observeCurrent(ctx, sess.req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fresh.Close() }()

	scan, err := fresh.Manifest().Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scan.Close() }()
	ops, err := sess.spool.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ops.Close() }()

	ordered := worktree.Ordered(scan)
	var conflicts []restore.Failure
	scanOK := ordered.Next()
	for ops.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		op := ops.Operation()
		// Advance the observation to this operation's path.
		for scanOK && ordered.Record().Path().Less(op.Path()) {
			scanOK = ordered.Next()
		}
		observed := restore.AbsentNode()
		if scanOK && ordered.Record().Path().Equal(op.Path()) {
			obs, _, ok := entryNode(ordered.Record())
			// Unreadable, unsupported, or now reached through a symlink: each means the
			// destination is no longer the one the plan proved it could write, which is a
			// changed precondition rather than a plan to execute.
			if !ok || obs.followed() {
				if f, ferr := restore.NewFailure(op.Path(), op.Kind(), restore.ReasonPreconditionMismatch); ferr == nil {
					conflicts = appendBounded(conflicts, f)
				}
				continue
			}
			observed = obs.state
		}
		if !observed.Equal(op.Current()) {
			f, ferr := restore.NewFailure(op.Path(), op.Kind(), restore.ReasonPreconditionMismatch)
			if ferr != nil {
				return nil, ferr
			}
			conflicts = appendBounded(conflicts, f)
		}
	}
	if err := ops.Err(); err != nil {
		return nil, err
	}
	if err := ordered.Err(); err != nil {
		return nil, err
	}
	return conflicts, nil
}

// maxReportedFailures bounds how many per-path facts a result carries. A restore
// that conflicts on a million paths must still produce a bounded report; the exact
// counts stay in the plan.
const maxReportedFailures = restore.MaxBlockedSamples

// maxDetailBytes bounds the human diagnostic. The text comes from an error, and an
// error can be built from a filesystem path, a command's output, or anything else an
// adapter wraps in — so the field carries a readable head rather than whatever length
// the failure happened to have.
const maxDetailBytes = 400

// boundedDetail renders a failure for the human diagnostic, truncated to a readable
// head. It returns the empty string for no failure, which is what keeps the field
// absent rather than empty-but-present in output.
func boundedDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= maxDetailBytes {
		return msg
	}
	return msg[:maxDetailBytes] + "… (truncated)"
}

func appendBounded(list []restore.Failure, f restore.Failure) []restore.Failure {
	if len(list) >= maxReportedFailures {
		return list
	}
	return append(list, f)
}

// commit walks the spooled plan in dependency-safe order and installs each
// operation. It stops at the first failure: continuing past a conflict would be
// exactly the best-effort behaviour this command refuses, and a rerun re-plans from
// current reality and converges on only the remaining work.
//
// The order is three phases plus one pass per directory depth, each a fresh cursor
// over the spool. That is what makes deepest-first directory removal possible
// without sorting a materialized set.
//
// Every exit — a stopped operation, a failed spool read, a payload that vanished —
// leaves through finishCommit, which owns the two decisions that must never be made
// twice: whether the recovery observation may be dropped, and what the worktree's
// state is called.
func (sess *session) commit(ctx context.Context, plan restore.Plan, applier Applier) (Result, error) {
	st := &commitState{}
	cause := sess.runCommitPasses(ctx, applier, st)
	return sess.finishCommit(plan, st, cause)
}

// commitState is what the commit passes observed. It is a value rather than a set of
// closure variables so the finalizer reads exactly the same facts however the commit
// ended, including through an error path.
type commitState struct {
	// completed counts operations that reached their desired state.
	completed int
	// changed records that at least one destination no longer holds what the plan
	// proved. It comes from the applier's reported effect, not from a count of
	// attempts: an install that removes the old node and then fails changes the
	// worktree while completing nothing, and only the applier knows that happened.
	changed bool
	// incomplete records that a destination changed without its operation completing.
	// It is the fact that makes "partial" honest with a completed count of zero.
	incomplete bool
	failures   []restore.Failure
	stopReason restore.Reason
	// stopErr is the failure behind stopReason, kept because one class of stop is not
	// an outcome at all: a filesystem fault that changed nothing is a failure to
	// report, not a conflict to describe.
	stopErr error
}

// note records why a commit stopped, keeping the first one: the first failure is the
// one that ended the commit, and later bookkeeping must not overwrite it.
func (st *commitState) note(reason restore.Reason, err error) {
	if st.stopReason == "" {
		st.stopReason = reason
		st.stopErr = err
	}
}

// observe folds one applied operation's report into the state. Completion comes from
// the reported effect, never from the absence of an error: those are two different
// questions, and an install that reached its desired state has completed its operation
// whatever else it went on to report.
func (st *commitState) observe(op restore.PlannedOperation, out restore.MutationResult) {
	if out.Changed() {
		st.changed = true
	}
	if out.Done() {
		st.completed++
	}
	err := out.Err()
	if err == nil {
		return
	}
	if out.Effect() == restore.EffectPartial {
		st.incomplete = true
	}
	reason := reasonForApplyError(err)
	st.note(reason, err)
	if out.Done() {
		// The path is not a failure — it holds the desired state. Only the stop is.
		return
	}
	st.fail(op, reason)
}

// observeUnreadable folds in a report that cannot be trusted to say what happened.
// The destination is treated as changed and incomplete, which is the conservative
// reading and the only safe one: the alternative discards undo evidence for a path the
// applier had already been asked to write.
func (st *commitState) observeUnreadable(op restore.PlannedOperation, err error) {
	st.changed = true
	st.incomplete = true
	st.note(restore.ReasonIOFailure, err)
	st.fail(op, restore.ReasonIOFailure)
}

// fail records one bounded per-path fact.
func (st *commitState) fail(op restore.PlannedOperation, reason restore.Reason) {
	if f, ferr := restore.NewFailure(op.Path(), op.Kind(), reason); ferr == nil {
		st.failures = appendBounded(st.failures, f)
	}
}

// runCommitPasses walks the spooled plan in dependency-safe order. It returns an
// error only for a failure of awa's own machinery — a spool that cannot be read, a
// staged payload that vanished — never for an operation that refused, which is a
// reported outcome rather than an error.
func (sess *session) runCommitPasses(ctx context.Context, applier Applier, st *commitState) error {
	runPass := func(rank, depth int) (bool, error) {
		cur, err := sess.spool.Open(ctx)
		if err != nil {
			return false, err
		}
		defer func() { _ = cur.Close() }()
		for cur.Next() {
			if err := ctx.Err(); err != nil {
				st.note(restore.ReasonCancelled, err)
				return false, nil
			}
			op := cur.Operation()
			if op.Rank() != rank {
				continue
			}
			if depth > 0 && op.PathDepth() != depth {
				continue
			}
			staged, err := sess.stagedOperation(op, applier)
			if err != nil {
				return false, err
			}
			out := applier.Apply(ctx, staged)
			if verr := out.Validate(); verr != nil {
				// A mutation port that cannot describe what it did leaves this destination's
				// state unknown, and unknown is not "untouched": the applier was already asked
				// to write here. Reading it as no-change would let the recovery observation —
				// the only description of what this path held — be discarded right after a
				// mutation that may well have landed. So it is recorded as a change that did
				// not complete, which keeps the record and reports partial.
				st.observeUnreadable(op, fmt.Errorf("restore: applying %s: %w", op.Path(), verr))
				return false, nil
			}
			st.observe(op, out)
			if out.Err() != nil {
				return false, nil
			}
		}
		return true, cur.Err()
	}

	passes := []struct{ rank, depth int }{
		{restore.RankWrite, 0},
		{restore.RankDeleteFile, 0},
	}
	for d := sess.spool.MaxDirectoryDepth(); d >= 1; d-- {
		passes = append(passes, struct{ rank, depth int }{restore.RankDeleteDirectory, d})
	}
	// Last: the type changes that replace a directory. Everything the plan proved
	// should be gone from inside them has been removed by the passes above, so the
	// empty-only removal each one performs can now succeed.
	passes = append(passes, struct{ rank, depth int }{restore.RankReplaceDirectory, 0})

	for _, p := range passes {
		ok, err := runPass(p.rank, p.depth)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// finishCommit turns what the commit did into one honest answer. It is the single
// exit for the commit phase, which is the point: the recovery-observation lifecycle
// and the outcome vocabulary are decided from the same facts, so no early return can
// leave a record behind or name a state the worktree is not in.
//
// cause is a failure of awa's own machinery. If nothing was written it is simply
// returned. If something was written it must NOT replace the result: the report of
// what landed, and of the observation it can be undone from, is the only way back — so
// the outcome stands, the failure is named as a reason, and its text travels as a human
// diagnostic rather than as prose inside the typed contract. The same is true of an
// operation's own failure, which is why the diagnostic is taken from the first failure
// recorded rather than from cause alone.
func (sess *session) finishCommit(plan restore.Plan, st *commitState, cause error) (Result, error) {
	// A filesystem fault that changed nothing is not an outcome. `conflict` says the
	// destination moved out from under the plan and `cancelled` says an operator stopped
	// it; a failed write that left the worktree exactly as it was is neither, and
	// publishing it as a conflict would send a reader looking for a change that never
	// happened. It becomes the invocation's failure instead — reported with the operating
	// system's own words, which is the actionable part.
	if !st.changed && st.stopErr != nil && st.stopReason == restore.ReasonIOFailure {
		cause = errors.Join(cause, st.stopErr)
	}
	// The recovery observation exists to undo a change. Drop it only when the commit
	// provably changed nothing — proved by the applier's effects, since a count of
	// attempts cannot see a destination that changed without completing.
	if !st.changed {
		if derr := sess.dropRecovery(); derr != nil {
			// Both facts matter: what ended the commit, and that a durable record was left
			// behind. Joining them keeps the second from being swallowed by the first.
			return Result{}, errors.Join(cause, derr)
		}
		if cause != nil {
			return Result{}, cause
		}
	}
	if cause != nil {
		st.note(restore.ReasonIOFailure, cause)
		// The worktree changed and the commit then failed on awa's own machinery. Some
		// destination therefore changed without an operation completing, whether or not an
		// applier reported it.
		st.incomplete = st.incomplete || st.completed == 0
	}
	// Whatever ended this commit, the worktree changed, so no error is returned and the
	// result is the whole report. The reason token and the path say which class of stop
	// and where; only this says what actually went wrong. It comes from the first failure
	// recorded — an operation's own error just as much as a failure of awa's machinery —
	// because losing the operating system's words leaves a reader with "io-failure" and a
	// path, which is the report this field exists to prevent.
	detail := boundedDetail(st.stopErr)

	remaining := sess.mutating - st.completed
	outcome := restore.OutcomeApplied
	var reasons []restore.Reason
	switch {
	case remaining > 0 && (st.completed > 0 || st.incomplete):
		// Something landed and something did not. That is partial whether the landed part
		// completed an operation or merely removed what stood in its way.
		outcome = restore.OutcomePartial
		reasons = append(reasons, restore.ReasonPartialApply)
		if st.stopReason != "" && st.stopReason != restore.ReasonPartialApply {
			reasons = append(reasons, st.stopReason)
		}
	case remaining > 0:
		// Nothing was written, so the honest outcome names why it stopped rather than
		// claiming a partial commit that never began.
		outcome = restore.OutcomeConflict
		if st.stopReason == restore.ReasonCancelled {
			outcome = restore.OutcomeCancelled
		}
		if st.stopReason != "" {
			reasons = append(reasons, st.stopReason)
		}
	}

	res, err := restore.NewApplyResult(restore.ResultInput{
		Source: plan.Source(), Selection: plan.Selection(), Outcome: outcome,
		Counts: plan.Counts(), Boundary: plan.Boundary(),
		Completed: st.completed, Remaining: remaining,
		Recovery: sess.recoveryID(), Reasons: reasons, Failures: st.failures,
		IncompleteMutation: st.incomplete,
	})
	if err != nil {
		return Result{}, errors.Join(cause, err)
	}
	out := sess.result(res)
	out.Diagnostic = detail
	return out, nil
}

// recoveryID returns the published recovery observation's id, or the zero id when
// none was published. Only an outcome that may have mutated the worktree needs one,
// and the domain constructor enforces that.
func (sess *session) recoveryID() restore.OperationID {
	if !sess.recovered {
		return restore.OperationID{}
	}
	return sess.id
}

// stagedOperation rebuilds the executable form of a spooled operation. The staged
// payload is looked up by content identity, so the commit needs no payload map
// whose size would scale with the plan, and the domain constructor re-checks that
// an operation needing bytes actually has them.
func (sess *session) stagedOperation(op restore.PlannedOperation, applier Applier) (restore.StagedOperation, error) {
	var payload restore.StagedPayload
	if op.RequiresContent() {
		p, err := applier.Payload(op.Desired().Content())
		if err != nil {
			return restore.StagedOperation{}, fmt.Errorf("restore: staged payload for %s: %w", op.Path(), err)
		}
		payload = p
	}
	return restore.NewStagedOperation(op, payload)
}

// reasonForApplyError maps a filesystem refusal to the closed reason the result
// publishes, so a machine consumer branches on a token rather than on prose.
func reasonForApplyError(err error) restore.Reason {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return restore.ReasonCancelled
	case errors.Is(err, restore.ErrPreconditionMismatch):
		return restore.ReasonPreconditionMismatch
	case errors.Is(err, restore.ErrDirectoryNotEmpty):
		return restore.ReasonPathConflict
	case errors.Is(err, restore.ErrSymlinkAncestor):
		return restore.ReasonSymlinkAncestor
	case errors.Is(err, restore.ErrUnsupportedNode):
		return restore.ReasonUnsupportedEntryKind
	default:
		return restore.ReasonIOFailure
	}
}
