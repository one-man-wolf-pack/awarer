package compare

import (
	"errors"

	"awarer/internal/domain/worktree"
)

// Qualification explains the missing side of an added or deleted entry from that
// side's recorded coverage. It describes the observation, never a filesystem event:
// a proved absence does not mean the path was created or deleted between the two
// observations, and an outside-observation side does not mean the path existed.
type Qualification uint8

const (
	// QualAbsent: the side's completed coverage proves the path absent there.
	QualAbsent Qualification = iota + 1
	// QualOutsideObservation: an exclusion boundary, or the limit of a recovery
	// observation's covered path set, leaves the lookup outside the side's evidence.
	QualOutsideObservation
	// QualSkippedBoundary: a skipped or unavailable traversal node leaves the lookup
	// unproved.
	QualSkippedBoundary
	// QualUnknown: the side's retained observation carries no coverage proof.
	QualUnknown
)

// String returns the stable machine token.
func (q Qualification) String() string {
	switch q {
	case QualAbsent:
		return "absent"
	case QualOutsideObservation:
		return "outside-observation"
	case QualSkippedBoundary:
		return "skipped-boundary"
	case QualUnknown:
		return "unknown"
	default:
		return ""
	}
}

// Side names one operand of a comparison.
type Side uint8

const (
	SideLeft Side = iota + 1
	SideRight
)

func (s Side) String() string {
	switch s {
	case SideLeft:
		return "left"
	case SideRight:
		return "right"
	default:
		return ""
	}
}

// Counterpart is the qualification of an added or deleted entry's missing side: the
// left observation for an add, the right for a delete. The zero value means the
// comparison was not asked to qualify counterparts, or the change is not one-sided.
type Counterpart struct {
	Side          Side
	Qualification Qualification
}

// Known reports whether the change carries a counterpart qualification.
func (c Counterpart) Known() bool { return c.Side != 0 && c.Qualification != 0 }

// PathSetCursor is an ordered stream of exact paths: the covered path set of a
// recovery observation. It matches restore.CoveredScopeCursor so that cursor can be
// passed without this package depending on restore.
type PathSetCursor interface {
	Next() bool
	Path() worktree.RelPath
	Err() error
	Close() error
}

// SideEvidence is what one side's absence can be proved from. Absence is the shared
// absence-proof owner for the side's coverage. A non-nil Scope confines the proof to
// its exact paths: a path outside it is outside the side's observation, whatever the
// coverage kind would otherwise answer.
type SideEvidence struct {
	Absence *worktree.AbsenceTracker
	Scope   PathSetCursor
}

// CounterpartEvidence carries both sides' evidence into a comparison. The comparison
// takes ownership: it verifies every stream at the end of the merge and closes them.
type CounterpartEvidence struct {
	Left, Right SideEvidence
	// Interrupted, when set, reports cancellation (typically ctx.Err). It is checked
	// before each qualification and around the final verification, so a cancelled
	// comparison fails instead of ending as a complete, qualified report.
	Interrupted func() error
}

func (e *CounterpartEvidence) interrupted() error {
	if e == nil || e.Interrupted == nil {
		return nil
	}
	return e.Interrupted()
}

// Close releases every stream the evidence holds. It is safe on a nil receiver.
func (e *CounterpartEvidence) Close() error {
	if e == nil {
		return nil
	}
	return errors.Join(e.Left.close(), e.Right.close())
}

// sideQualifier feeds one side's records into its evidence in canonical order and
// answers qualifications for paths that side does not record.
type sideQualifier struct {
	ev       SideEvidence
	scopeOK  bool
	scopeCur worktree.RelPath
	started  bool
	finished bool
}

func (s *sideQualifier) note(rec worktree.ManifestRecord) error {
	if s.ev.Absence == nil {
		return nil
	}
	return s.ev.Absence.Note(rec)
}

func (s *sideQualifier) qualify(p worktree.RelPath) (Qualification, error) {
	if s.ev.Scope != nil {
		in, err := s.inScope(p)
		if err != nil {
			return 0, err
		}
		if !in {
			return QualOutsideObservation, nil
		}
	}
	if s.ev.Absence == nil {
		return QualUnknown, nil
	}
	v, err := s.ev.Absence.Absent(p)
	if err != nil {
		return 0, err
	}
	switch v {
	case worktree.AbsenceProved:
		return QualAbsent, nil
	case worktree.AbsenceExcluded:
		return QualOutsideObservation, nil
	case worktree.AbsenceSkippedBoundary:
		return QualSkippedBoundary, nil
	default:
		return QualUnknown, nil
	}
}

// inScope advances the covered path set to p and reports exact membership. Queries
// arrive in canonical order, so the set is read once, never reopened.
func (s *sideQualifier) inScope(p worktree.RelPath) (bool, error) {
	if !s.started {
		s.started = true
		s.scopeOK = s.advanceScope()
	}
	for s.scopeOK && s.scopeCur.Less(p) {
		s.scopeOK = s.advanceScope()
	}
	if err := s.ev.Scope.Err(); err != nil {
		return false, err
	}
	return s.scopeOK && s.scopeCur.Equal(p), nil
}

func (s *sideQualifier) advanceScope() bool {
	if s.ev.Scope.Next() {
		s.scopeCur = s.ev.Scope.Path()
		return true
	}
	return false
}

// finish drains the rest of the side's proof streams so their declared counts,
// digests and ordering are verified even when the last reported change came early.
func (s *sideQualifier) finish() error {
	if s.finished {
		return nil
	}
	s.finished = true
	var err error
	if s.ev.Absence != nil {
		err = s.ev.Absence.Finish()
	}
	if s.ev.Scope != nil {
		for s.ev.Scope.Next() {
		}
		if serr := s.ev.Scope.Err(); serr != nil && err == nil {
			err = serr
		}
	}
	return err
}

func (e SideEvidence) close() error {
	var err error
	if e.Absence != nil {
		err = e.Absence.Close()
	}
	if e.Scope != nil {
		if cerr := e.Scope.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
