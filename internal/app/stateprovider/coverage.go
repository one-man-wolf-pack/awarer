package stateprovider

import (
	"context"

	"awarer/internal/app/state"
	"awarer/internal/domain/compare"
	"awarer/internal/domain/worktree"
)

// coveredMerge counts changes only if every path present on exactly one side is
// proved absent on the other. A name hidden on one side alone is outside that
// side's evidence and supports no count, so it does not refute the relationship.
func (a *Assessor) coveredMerge(ctx context.Context, left, right *state.ResolvedState) (compare.Summary, bool, error) {
	var sum compare.Summary
	supported, err := a.walkCovered(ctx, left, right, &sum)
	return sum, supported, err
}

func (a *Assessor) walkCovered(ctx context.Context, left, right *state.ResolvedState, sum *compare.Summary) (bool, error) {
	l, err := openSide(ctx, left)
	if err != nil {
		return false, err
	}
	defer l.close()
	r, err := openSide(ctx, right)
	if err != nil {
		return false, err
	}
	defer r.close()

	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := firstErr(l.err, r.err); err != nil {
			return false, err
		}
		var p worktree.RelPath
		switch {
		case l.ok && r.ok:
			p = l.rec.Path()
			if r.rec.Path().Less(p) {
				p = r.rec.Path()
			}
		case l.ok:
			p = l.rec.Path()
		case r.ok:
			p = r.rec.Path()
		default:
			return true, firstErr(l.absence.Finish(), r.absence.Finish())
		}
		lrec, rrec := l.take(p), r.take(p)
		if err := firstErr(l.absence.Note(lrec), r.absence.Note(rrec)); err != nil {
			return false, err
		}
		if ch, changed := compare.ComparePair(lrec, rrec); changed {
			sum.Add(ch)
		}
		var missing *side
		switch {
		case lrec.IsZero():
			missing = l
		case rrec.IsZero():
			missing = r
		default:
			continue
		}
		if v, err := missing.absence.Absent(p); err != nil || v != worktree.AbsenceProved {
			return false, err
		}
	}
}

type side struct {
	manifest worktree.ManifestCursor
	absence  *worktree.AbsenceTracker
	rec      worktree.ManifestRecord
	ok       bool
	err      error
}

func openSide(ctx context.Context, rs *state.ResolvedState) (*side, error) {
	cur, err := rs.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	s := &side{manifest: worktree.Ordered(cur)}
	if s.absence, err = worktree.NewAbsenceTracker(ctx, rs.Coverage()); err != nil {
		s.close()
		return nil, err
	}
	s.advance()
	return s, nil
}

func (s *side) advance() {
	if s.ok = s.manifest.Next(); s.ok {
		s.rec = s.manifest.Record()
	} else if err := s.manifest.Err(); err != nil && s.err == nil {
		s.err = err
	}
}

func (s *side) take(p worktree.RelPath) worktree.ManifestRecord {
	if !s.ok || !s.rec.Path().Equal(p) {
		return worktree.ManifestRecord{}
	}
	rec := s.rec
	s.advance()
	return rec
}

func (s *side) close() {
	_ = s.manifest.Close()
	if s.absence != nil {
		_ = s.absence.Close()
	}
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
