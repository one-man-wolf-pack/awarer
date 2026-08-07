package restore

import (
	"fmt"

	"awarer/internal/domain/worktree"
)

// Outcome is what one restore invocation actually did. The zero value is not
// valid, so a result must state an outcome rather than defaulting to a
// success-shaped one.
type Outcome int

const (
	// OutcomePreview: nothing was written. It is what every invocation without
	// --apply produces, including the explicit --dry-run spelling.
	OutcomePreview Outcome = iota + 1
	// OutcomeApplied: every planned mutating operation completed.
	OutcomeApplied
	// OutcomeNoOp: apply was requested and the plan was complete, but the worktree
	// already matched the source, so there was nothing to write.
	OutcomeNoOp
	// OutcomeRefused: apply stopped before the first mutation because the evidence
	// it requires was not complete — a blocked operation, an unstageable payload,
	// or a recovery observation that could not preserve what would be destroyed.
	OutcomeRefused
	// OutcomeConflict: the current state stopped matching the planned precondition
	// before any mutation, so the plan no longer describes reality.
	OutcomeConflict
	// OutcomePartial: a prefix of the plan was written and then something stopped
	// it. Multi-path filesystem mutation is not transactional, and this is the
	// outcome that says so instead of reporting success for incomplete work.
	OutcomePartial
	// OutcomeCancelled: the invocation was cancelled before the first mutation.
	// A cancellation *after* the first mutation is OutcomePartial — the worktree
	// changed, and that fact outranks how the stop was requested.
	OutcomeCancelled
)

// String returns the stable machine token for the outcome.
func (o Outcome) String() string {
	switch o {
	case OutcomePreview:
		return "preview"
	case OutcomeApplied:
		return "applied"
	case OutcomeNoOp:
		return "no-op"
	case OutcomeRefused:
		return "refused"
	case OutcomeConflict:
		return "conflict"
	case OutcomePartial:
		return "partial"
	case OutcomeCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool { return o >= OutcomePreview && o <= OutcomeCancelled }

// Mutated reports whether this outcome means the worktree may have changed. It
// is the single predicate that decides whether a recovery observation is
// mandatory, so no caller has to remember the list.
func (o Outcome) Mutated() bool { return o == OutcomeApplied || o == OutcomePartial }

// Failure is one bounded fact about an operation that did not complete: which
// path, what it was going to do, and the closed reason it stopped.
type Failure struct {
	Path    worktree.RelPath
	Kind    OperationKind
	Reasons []Reason
}

// NewFailure records a bounded failure fact. At least one valid reason is
// required: an unexplained failure is exactly what restore refuses to emit.
func NewFailure(path worktree.RelPath, kind OperationKind, reasons ...Reason) (Failure, error) {
	if path.IsZero() {
		return Failure{}, fmt.Errorf("a restore failure requires a path")
	}
	if len(reasons) == 0 {
		return Failure{}, fmt.Errorf("a restore failure for %q requires at least one reason", path)
	}
	for _, r := range reasons {
		if !r.Valid() {
			return Failure{}, fmt.Errorf("invalid restore reason %q", r)
		}
	}
	out := make([]Reason, len(reasons))
	copy(out, reasons)
	return Failure{Path: path, Kind: kind, Reasons: out}, nil
}

// ApplyResult is the validated result of one restore invocation. Its constructor
// enforces the honesty rules that matter most: a result always names its source;
// a preview never claims mutations; an applied outcome never leaves remaining
// work; and any outcome that touched the worktree carries the immutable recovery
// observation the user can undo it from.
type ApplyResult struct {
	source    Source
	selection Selection
	outcome   Outcome
	counts    Counts
	boundary  Boundary
	completed int
	remaining int
	recovery  OperationID
	reasons   []Reason
	failures  []Failure
}

// ResultInput carries the facts a result is built from. It exists so the many
// interdependent fields are validated together in one place rather than assigned
// one at a time by a caller who must remember which combinations are legal.
type ResultInput struct {
	Source    Source
	Selection Selection
	Outcome   Outcome
	Counts    Counts
	Boundary  Boundary
	// Completed and Remaining are mutating operations only; an equal operation is
	// not work, so it is never counted as completed.
	Completed int
	Remaining int
	// Recovery is the pre-restore recovery observation. It is required for any
	// outcome that mutated the worktree and forbidden for a preview.
	Recovery OperationID
	// Reasons are invocation-level facts (refused, cancelled, partial); Failures
	// are the bounded per-path facts.
	Reasons  []Reason
	Failures []Failure
	// IncompleteMutation reports that the commit changed a destination without
	// completing its operation — an install that removed the old node and then failed.
	// It is what makes a partial outcome legal with no completed operations: the
	// worktree moved, so the result must not claim it stopped before the first write,
	// and it must carry the recovery observation the change can be undone from.
	IncompleteMutation bool
}

// NewApplyResult validates and builds the result. Every rejection below
// corresponds to an output line that would otherwise be able to lie.
func NewApplyResult(in ResultInput) (ApplyResult, error) {
	if in.Source.IsZero() {
		return ApplyResult{}, fmt.Errorf("a restore result requires a resolved source identity")
	}
	if in.Selection.IsZero() {
		return ApplyResult{}, fmt.Errorf("a restore result requires a normalized selection")
	}
	if !in.Outcome.Valid() {
		return ApplyResult{}, fmt.Errorf("invalid restore outcome %d", in.Outcome)
	}
	if in.Completed < 0 || in.Remaining < 0 {
		return ApplyResult{}, fmt.Errorf("restore result has negative counts (completed=%d remaining=%d)", in.Completed, in.Remaining)
	}
	for _, r := range in.Reasons {
		if !r.Valid() {
			return ApplyResult{}, fmt.Errorf("invalid restore reason %q", r)
		}
	}

	switch in.Outcome {
	case OutcomePreview:
		if in.Completed > 0 {
			return ApplyResult{}, fmt.Errorf("a preview cannot report %d completed operation(s)", in.Completed)
		}
		if !in.Recovery.IsZero() {
			return ApplyResult{}, fmt.Errorf("a preview cannot carry a recovery observation: it mutates nothing")
		}
	case OutcomeApplied:
		if in.Remaining > 0 {
			return ApplyResult{}, fmt.Errorf("an applied restore cannot leave %d remaining operation(s)", in.Remaining)
		}
		if in.Completed == 0 {
			return ApplyResult{}, fmt.Errorf("an applied restore completed no operations; that is a no-op, not an apply")
		}
	case OutcomePartial:
		// Partial means the worktree moved and the plan did not finish. Completing an
		// operation is the usual way that happens, and an install that changed its
		// destination without completing is the other one — so one of the two must be
		// true, or "partial" would describe an invocation that wrote nothing.
		if in.Completed == 0 && !in.IncompleteMutation {
			return ApplyResult{}, fmt.Errorf("a partial restore must have completed an operation or changed a destination without completing it")
		}
		if in.Remaining == 0 {
			return ApplyResult{}, fmt.Errorf("a partial restore must have remaining work; otherwise it is applied")
		}
	case OutcomeNoOp, OutcomeRefused, OutcomeConflict, OutcomeCancelled:
		if in.Completed > 0 {
			return ApplyResult{}, fmt.Errorf("a %s restore stopped before the first mutation and cannot report %d completed operation(s)", in.Outcome, in.Completed)
		}
		// These four outcomes all promise the worktree was not touched, which is exactly
		// why an incomplete mutation may not hide behind one of them.
		if in.IncompleteMutation {
			return ApplyResult{}, fmt.Errorf("a %s restore changed a destination without completing it; that is a partial restore", in.Outcome)
		}
	}
	// Any outcome that may have changed the worktree must name the evidence the
	// change can be undone from. This is the type-level form of "every apply is
	// reversible from its reported recovery observation".
	if in.Outcome.Mutated() && in.Recovery.IsZero() {
		return ApplyResult{}, fmt.Errorf("a %s restore mutated the worktree and must carry a recovery observation", in.Outcome)
	}
	if in.Outcome == OutcomeRefused && len(in.Reasons) == 0 && len(in.Failures) == 0 {
		return ApplyResult{}, fmt.Errorf("a refused restore must say why")
	}

	res := ApplyResult{
		source:    in.Source,
		selection: in.Selection,
		outcome:   in.Outcome,
		counts:    in.Counts,
		boundary:  in.Boundary,
		completed: in.Completed,
		remaining: in.Remaining,
		recovery:  in.Recovery,
	}
	if len(in.Reasons) > 0 {
		res.reasons = make([]Reason, len(in.Reasons))
		copy(res.reasons, in.Reasons)
	}
	if len(in.Failures) > 0 {
		res.failures = make([]Failure, len(in.Failures))
		copy(res.failures, in.Failures)
	}
	return res, nil
}

// PreviewResult builds the result for a non-mutating invocation from its plan. It
// is a named constructor rather than a caller-assembled ResultInput because a
// preview's shape is fully determined by its plan — there is nothing for a caller
// to decide, and therefore nothing to get wrong.
//
// The plan's bounded blocked sample travels into the result as per-path failure
// facts. Without it a preview could only say how many operations cannot run, which
// is the answer this command exists to improve on: "1 operation cannot run" and
// "generated/client/openapi.json: hash-only-content" are the difference between a
// user knowing what to do next and guessing.
func PreviewResult(p Plan) (ApplyResult, error) {
	if p.IsZero() {
		return ApplyResult{}, fmt.Errorf("cannot build a preview result from an unbuilt plan")
	}
	failures, err := FailuresFromSamples(p)
	if err != nil {
		return ApplyResult{}, err
	}
	return NewApplyResult(ResultInput{
		Source:    p.Source(),
		Selection: p.Selection(),
		Outcome:   OutcomePreview,
		Counts:    p.Counts(),
		Boundary:  p.Boundary(),
		Failures:  failures,
	})
}

// FailuresFromSamples projects a plan's bounded blocked sample into the per-path
// failure facts a result publishes. Preview and a fail-before-mutation refusal both
// need it, and they must report the same paths and the same reason tokens for the
// same plan, so the projection lives here rather than in each caller.
func FailuresFromSamples(p Plan) ([]Failure, error) {
	samples := p.BlockedSamples()
	if len(samples) == 0 {
		return nil, nil
	}
	out := make([]Failure, 0, len(samples))
	for _, s := range samples {
		f, err := NewFailure(s.Path, s.Kind, s.Reasons...)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// Source returns the immutable evidence the restore was proven from.
func (r ApplyResult) Source() Source { return r.source }

// Selection returns the normalized scope.
func (r ApplyResult) Selection() Selection { return r.selection }

// Outcome returns what the invocation did.
func (r ApplyResult) Outcome() Outcome { return r.outcome }

// Counts returns the plan's per-kind totals, so an apply result can report the
// same deterministic figures the preview showed.
func (r ApplyResult) Counts() Counts { return r.counts }

// Boundary returns the evidence limits the plan was built under.
func (r ApplyResult) Boundary() Boundary { return r.boundary }

// Completed returns how many mutating operations were written.
func (r ApplyResult) Completed() int { return r.completed }

// Remaining returns how many mutating operations were not written.
func (r ApplyResult) Remaining() int { return r.remaining }

// Recovery returns the pre-restore recovery observation id; it is zero for an
// outcome that never touched the worktree.
func (r ApplyResult) Recovery() OperationID { return r.recovery }

// Reasons returns the invocation-level reasons as a copy.
func (r ApplyResult) Reasons() []Reason {
	if len(r.reasons) == 0 {
		return nil
	}
	out := make([]Reason, len(r.reasons))
	copy(out, r.reasons)
	return out
}

// Failures returns the bounded per-path failure facts as a copy, including
// copies of each failure's reason slice, so a consumer cannot edit a validated
// result's account of what went wrong.
func (r ApplyResult) Failures() []Failure {
	if len(r.failures) == 0 {
		return nil
	}
	out := make([]Failure, len(r.failures))
	copy(out, r.failures)
	for i := range out {
		if len(r.failures[i].Reasons) > 0 {
			rs := make([]Reason, len(r.failures[i].Reasons))
			copy(rs, r.failures[i].Reasons)
			out[i].Reasons = rs
		}
	}
	return out
}

// IsZero reports whether the result was never built through a constructor.
func (r ApplyResult) IsZero() bool { return r.outcome == 0 }
