package gc

// PlanSummary counts a plan's candidates by disposition and totals the bytes GC
// would reclaim. It is derived from the candidates, never assembled by hand, so the
// numbers a report shows cannot drift from the candidates it lists.
type PlanSummary struct {
	PlannedDelete int
	Retained      int
	Blocked       int
	Skipped       int
	BytesPlanned  int64
}

// BlobFootprint is the total on-disk blob-store footprint measured during planning:
// how many content blobs exist and their combined size. It is the exact accounting
// that ordinary "awa status" deliberately does not compute (a whole-store walk),
// surfaced here because planning already lists every blob.
//
// Complete states the guarantee behind the numbers: true means the footprint was
// computed from a complete blob listing, not a sample. The current planner fails the
// plan outright when it cannot list the whole store, so it never returns a partial
// footprint — Complete is therefore true on every plan that carries one. It is an
// explicit contract flag so an agent can rely on the total being exhaustive rather than
// having to assume it, and so a future bounded/partial listing mode would be an honest
// value change rather than a silent one.
type BlobFootprint struct {
	Count    int
	Bytes    int64
	Complete bool
}

// GCPlan is the immutable result of planning, produced before any mutation. Dry-run
// renders it directly; execute consumes its delete actions. The candidate and
// warning slices are copied at construction so later mutation of the inputs cannot
// change a built plan.
type GCPlan struct {
	dryRun        bool
	candidates    []GCCandidate
	warnings      []string
	blobFootprint *BlobFootprint
}

// NewPlan builds an immutable plan from the planner's candidates and the non-fatal
// warnings it accumulated (a suppressed blob sweep, an unavailable git for
// --committed, an unsupported retention policy), so dry-run and execute both surface
// why GC held back. Safety-critical obstructions like an active lock or an unreadable
// locks directory are blocked candidates, not warnings, so they gate execution.
func NewPlan(dryRun bool, candidates []GCCandidate, warnings []string) GCPlan {
	cs := make([]GCCandidate, len(candidates))
	copy(cs, candidates)
	ws := make([]string, len(warnings))
	copy(ws, warnings)
	return GCPlan{dryRun: dryRun, candidates: cs, warnings: ws}
}

// WithBlobFootprint returns a copy of the plan carrying the measured blob-store
// footprint. It keeps GCPlan immutable — the planner attaches the footprint once, at
// construction — so callers that never measure it (most tests) simply leave it absent.
func (p GCPlan) WithBlobFootprint(f BlobFootprint) GCPlan {
	fp := f
	p.blobFootprint = &fp
	return p
}

// BlobFootprint returns the measured blob-store footprint and whether one was
// recorded. It is present on a real plan and absent on a plan built without a blob
// pass.
func (p GCPlan) BlobFootprint() (BlobFootprint, bool) {
	if p.blobFootprint == nil {
		return BlobFootprint{}, false
	}
	return *p.blobFootprint, true
}

// DryRun reports whether this plan was built for a dry run.
func (p GCPlan) DryRun() bool { return p.dryRun }

// Warnings returns the non-fatal diagnostics recorded during planning. The returned
// slice is a copy.
func (p GCPlan) Warnings() []string {
	out := make([]string, len(p.warnings))
	copy(out, p.warnings)
	return out
}

// Candidates returns every candidate the plan considered. The returned slice is a
// copy.
func (p GCPlan) Candidates() []GCCandidate {
	out := make([]GCCandidate, len(p.candidates))
	copy(out, p.candidates)
	return out
}

// Deletions returns the candidates marked for deletion, in plan order. The returned
// slice is a copy. Execution iterates these; ordering across kinds is the planner's
// responsibility.
func (p GCPlan) Deletions() []GCCandidate {
	out := make([]GCCandidate, 0, len(p.candidates))
	for _, c := range p.candidates {
		if c.action == ActionDelete {
			out = append(out, c)
		}
	}
	return out
}

// Summary derives the plan counts. Delete, retain, blocked, and skipped partition
// every candidate; BytesPlanned totals only the delete candidates.
func (p GCPlan) Summary() PlanSummary {
	var s PlanSummary
	for _, c := range p.candidates {
		switch c.action {
		case ActionDelete:
			s.PlannedDelete++
			s.BytesPlanned += c.bytes
		case ActionRetain:
			s.Retained++
		case ActionBlocked:
			s.Blocked++
		case ActionSkipped:
			s.Skipped++
		}
	}
	return s
}

// StateActionBlocked reports whether any candidate is blocked by durable state only a
// person can resolve: corruption GC must not delete, or a record whose schema this
// build cannot read. It is the single condition the CLI maps to the
// state-action-required exit code, and the condition under which execution performs
// no deletion at all.
func (p GCPlan) StateActionBlocked() bool {
	for _, c := range p.candidates {
		if c.action == ActionBlocked && c.reason.needsUserAction() {
			return true
		}
	}
	return false
}

// LockBlocked reports whether any candidate is blocked by an active or unknown
// lock. A lock-blocked plan performs no destructive deletion but exits 0 — the lock
// is a normal "try again later", not damage.
func (p GCPlan) LockBlocked() bool {
	for _, c := range p.candidates {
		if c.action == ActionBlocked && c.reason.isLock() {
			return true
		}
	}
	return false
}

// Blocked reports whether destructive deletion must be suppressed for any reason
// (state needing user action, or a lock). Execute is a no-op for deletes when this is
// true.
func (p GCPlan) Blocked() bool {
	return p.StateActionBlocked() || p.LockBlocked()
}
