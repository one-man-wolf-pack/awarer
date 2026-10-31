package state

import (
	"context"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/worktree"
)

// OpenComparison opens both resolved states' manifests together with the evidence
// that qualifies each added/deleted entry's missing side, and starts the streamed
// comparison. It is the shared front of the changes and diff observation deltas, so
// the qualification comes from one place rather than from each renderer.
//
// Each side's evidence is its recorded coverage, read through the shared absence
// tracker, and — for a recovery observation — its covered path set, which confines
// any proof to those exact paths. Nothing is rescanned and no proof is materialized:
// every stream is read once, in step with the merge, and verified when it ends.
//
// opts.Counterparts is set here; on success the returned cursor owns the manifests
// and the evidence and its Close releases them. On error everything opened is closed.
func OpenComparison(ctx context.Context, left, right *ResolvedState, opts compare.Options) (compare.ChangeCursor, error) {
	ev := &compare.CounterpartEvidence{Interrupted: ctx.Err}
	var lc, rc worktree.ManifestCursor
	transferred := false
	defer func() {
		if transferred {
			return
		}
		if lc != nil {
			_ = lc.Close()
		}
		if rc != nil {
			_ = rc.Close()
		}
		_ = ev.Close()
	}()

	var err error
	if lc, err = left.Manifest(ctx); err != nil {
		return nil, err
	}
	if rc, err = right.Manifest(ctx); err != nil {
		return nil, err
	}
	if ev.Left, err = left.counterpartEvidence(ctx); err != nil {
		return nil, err
	}
	if ev.Right, err = right.counterpartEvidence(ctx); err != nil {
		return nil, err
	}
	opts.Counterparts = ev
	cur, err := compare.CompareStream(lc, rc, opts)
	// CompareStream owns the inputs from here: its rename stage closes them on error,
	// and on success the returned cursor does.
	transferred = true
	if err != nil {
		return nil, err
	}
	return cur, nil
}

// counterpartEvidence opens the streams that can prove a path absent from this
// observation. A recovery observation proves nothing outside its covered path set,
// including a predecessor record whose coverage is otherwise unknown.
func (s *ResolvedState) counterpartEvidence(ctx context.Context) (compare.SideEvidence, error) {
	tracker, err := worktree.NewAbsenceTracker(ctx, s.coverage)
	if err != nil {
		return compare.SideEvidence{}, err
	}
	ev := compare.SideEvidence{Absence: tracker}
	if s.Kind == KindRestoreObservation && s.recoveryScope != nil {
		scope, err := s.recoveryScope.Open(ctx)
		if err != nil {
			_ = tracker.Close()
			return compare.SideEvidence{}, err
		}
		ev.Scope = scope
	}
	return ev, nil
}
