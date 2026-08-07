package restore

import (
	"fmt"

	"awarer/internal/domain/worktree"
)

// MaxBlockedSamples bounds how many blocked paths a plan carries as examples. A
// plan summarizes a comparison that may cover a million paths, so it keeps counts
// plus a bounded sample rather than the full set — the counts stay exact, and output
// says how many were sampled out of how many exist.
const MaxBlockedSamples = 10

// Counts are the deterministic per-kind operation totals of a plan. They are
// exact even when the plan holds only a bounded sample of blocked paths: counting
// is folded while the comparison streams, so the totals never depend on retaining
// the operations they count.
type Counts struct {
	Create          int `json:"create"`
	Replace         int `json:"replace"`
	TypeChange      int `json:"type_change"`
	Symlink         int `json:"symlink"`
	DeleteFile      int `json:"delete_file"`
	DeleteDirectory int `json:"delete_directory"`
	Equal           int `json:"equal"`
	Blocked         int `json:"blocked"`
}

// Add folds one planned operation into the counts. It is the incremental
// counterpart of a materializing summarizer, so a streaming planner accumulates
// bounded totals without retaining the operations.
func (c *Counts) Add(op PlannedOperation) {
	if op.Availability() == Blocked {
		c.Blocked++
		return
	}
	switch op.Kind() {
	case OpCreate:
		c.Create++
	case OpReplace:
		c.Replace++
	case OpTypeChange:
		c.TypeChange++
	case OpRestoreSymlink:
		c.Symlink++
	case OpDeleteFile:
		c.DeleteFile++
	case OpDeleteEmptyDirectory:
		c.DeleteDirectory++
	case OpEqual:
		c.Equal++
	}
}

// Delete returns the total number of ready deletions, files and directories.
func (c Counts) Delete() int { return c.DeleteFile + c.DeleteDirectory }

// Mutating returns how many ready operations would change the worktree. A plan
// whose only ready operations are equal ones applies as a no-op.
func (c Counts) Mutating() int {
	return c.Create + c.Replace + c.TypeChange + c.Symlink + c.Delete()
}

// Unavailable returns how many operations cannot run as planned. Planning has
// exactly one unavailable state, so this is the blocked count — named separately
// because "can this plan be applied at all" is the question every caller asks, and
// it must not become a hand-maintained sum if the vocabulary ever grows.
func (c Counts) Unavailable() int { return c.Blocked }

// Total returns every counted operation.
func (c Counts) Total() int { return c.Mutating() + c.Equal + c.Unavailable() }

// Boundary states the evidence limits a plan was built under, so a reader never
// has to infer them from the counts. Every field is a fact the planner observed,
// not an estimate.
type Boundary struct {
	// SourceSkipped and CurrentSkipped are the skipped (unreadable or
	// policy-excluded) input counts each side recorded. A skipped record blocks the
	// paths it intersects, never the whole plan.
	SourceSkipped  int `json:"source_skipped"`
	CurrentSkipped int `json:"current_skipped"`
	// PolicyCompatible reports whether the source's persisted scan identity and the
	// current effective scan identity agree. When they do not, absence in the source
	// is not proof of absence in awa scope, so no deletion can be justified.
	PolicyCompatible bool `json:"policy_compatible"`
	// ScopeBounded reports that the source proves only a recorded subset of the
	// project — true for a recovery-observation source, whose selection at capture
	// time is exactly what it can restore.
	ScopeBounded bool `json:"scope_bounded"`
}

// IgnoredOutsideEvidence is a constant product fact, not a computed field:
// ignored paths are never enumerated evidence, so they are never restored and
// never deleted for being absent from a source manifest. It is a method rather
// than a struct field precisely so no code path can set it false.
func (b Boundary) IgnoredOutsideEvidence() bool { return true }

// BlockedSample is one bounded example of an operation that cannot run, carrying
// the path and the closed reasons. It is what output shows under "blocked:".
type BlockedSample struct {
	Path    worktree.RelPath
	Kind    OperationKind
	Reasons []Reason
}

// NewBlockedSample projects an unavailable operation into a sample. It rejects a
// ready operation: a "blocked" sample that is not blocked would misreport the
// evidence boundary.
func NewBlockedSample(op PlannedOperation) (BlockedSample, error) {
	if op.Availability() == Ready {
		return BlockedSample{}, fmt.Errorf("operation for %q is ready and cannot be a blocked sample", op.Path())
	}
	return BlockedSample{Path: op.Path(), Kind: op.Kind(), Reasons: op.Reasons()}, nil
}

// Plan is the validated result of planning one restore: what it is proven from,
// what it covers, exactly how much work of each kind it found, the evidence
// boundary it was built under, a bounded sample of what it could not do, and
// whether it is complete.
//
// It deliberately does not own the operations. A plan over a million paths keeps
// a bounded summary; the operations themselves live in the application's spool
// and are streamed. There is no accessor here that could quietly materialize them.
type Plan struct {
	source    Source
	selection Selection
	counts    Counts
	boundary  Boundary
	samples   []BlockedSample
	complete  bool
}

// NewPlan builds a validated plan. It rejects the combinations that would let
// output lie: an unresolved source, an unbuilt selection, a plan that claims
// completeness while holding unavailable operations, and a blocked count with no
// sample to explain it. The sample slice is copied and must stay within
// MaxBlockedSamples — a planner that accumulated more has lost its bound.
func NewPlan(source Source, selection Selection, counts Counts, boundary Boundary, samples []BlockedSample, complete bool) (Plan, error) {
	if source.IsZero() {
		return Plan{}, fmt.Errorf("a restore plan requires a resolved source identity")
	}
	if selection.IsZero() {
		return Plan{}, fmt.Errorf("a restore plan requires a normalized selection")
	}
	if complete && counts.Unavailable() > 0 {
		return Plan{}, fmt.Errorf("a plan with %d unavailable operation(s) is not complete", counts.Unavailable())
	}
	if counts.Unavailable() > 0 && len(samples) == 0 {
		return Plan{}, fmt.Errorf("a plan with %d unavailable operation(s) requires at least one sample", counts.Unavailable())
	}
	if len(samples) > MaxBlockedSamples {
		return Plan{}, fmt.Errorf("a plan carries at most %d blocked samples, got %d", MaxBlockedSamples, len(samples))
	}
	out := make([]BlockedSample, len(samples))
	copy(out, samples)
	return Plan{source: source, selection: selection, counts: counts, boundary: boundary, samples: out, complete: complete}, nil
}

// Source returns the immutable evidence the plan is proven from.
func (p Plan) Source() Source { return p.source }

// Selection returns the normalized scope.
func (p Plan) Selection() Selection { return p.selection }

// Counts returns the exact per-kind totals.
func (p Plan) Counts() Counts { return p.counts }

// Boundary returns the evidence limits the plan was built under.
func (p Plan) Boundary() Boundary { return p.boundary }

// BlockedSamples returns the bounded examples as a copy, so a consumer cannot
// edit a validated plan's explanation of its own gaps.
func (p Plan) BlockedSamples() []BlockedSample {
	if len(p.samples) == 0 {
		return nil
	}
	out := make([]BlockedSample, len(p.samples))
	copy(out, p.samples)
	for i := range out {
		if len(p.samples[i].Reasons) > 0 {
			rs := make([]Reason, len(p.samples[i].Reasons))
			copy(rs, p.samples[i].Reasons)
			out[i].Reasons = rs
		}
	}
	return out
}

// Complete reports whether every selected path resolved to an available
// operation. A plan that is not complete may still be previewed — that is how a
// user learns what is missing — but it may never be applied.
func (p Plan) Complete() bool { return p.complete }

// Applicable reports whether apply may proceed: the plan is complete and has
// mutating work to do. A complete plan with no mutating work is a no-op, which is
// a successful outcome rather than an error.
func (p Plan) Applicable() bool { return p.complete && p.counts.Mutating() > 0 }

// IsZero reports whether the plan was never built through NewPlan.
func (p Plan) IsZero() bool { return p.source.IsZero() }
