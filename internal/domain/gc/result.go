package gc

// DeletionResult records the outcome of executing one delete candidate: the
// candidate acted on and any error. A failed deletion keeps Err non-nil and is
// folded into the summary as a failure, so a partial execution stays visible rather
// than being silently dropped.
//
// The bytes it credits are derived, never supplied: a successful deletion credits
// the candidate's measure as established when it was planned (a partial measure
// stays partial), and a failed one credits nothing — even if the repository removed
// some files before failing — because no per-file record says how much went. A
// successful deletion of an already-absent record therefore credits only what its
// measurement established, never a size it did not observe.
type DeletionResult struct {
	candidate GCCandidate
	err       error
}

// NewDeletionResult builds a deletion outcome; err is nil on success.
func NewDeletionResult(candidate GCCandidate, err error) DeletionResult {
	return DeletionResult{candidate: candidate, err: err}
}

// Credited returns the bytes this deletion is credited with: the candidate's
// measure on success, an unmeasured zero on failure.
func (d DeletionResult) Credited() ByteMeasure {
	if d.err != nil {
		return UnmeasuredBytes()
	}
	return d.candidate.size
}

func (d DeletionResult) Candidate() GCCandidate { return d.candidate }
func (d DeletionResult) Err() error             { return d.err }

// ExecSummary counts what execution actually did, derived from the deletion
// records alone — never from the plan's byte completeness. Deleted and Failed
// partition the attempted deletions; BytesDeleted totals the logical bytes credited
// to the successful ones. DeletedNotFullyMeasured counts the successes whose
// measurement was partial or unmeasured. Every failure is credited nothing, so while
// either Failed or DeletedNotFullyMeasured is non-zero BytesDeleted is a lower bound.
// Candidates never attempted (a dry run, a blocked or suppressed run, an interrupted
// phase) are not part of the execution total.
type ExecSummary struct {
	Deleted                 int
	Failed                  int
	BytesDeleted            int64
	DeletedNotFullyMeasured int
}

// BytesDeletedComplete reports whether BytesDeleted is an exact total of the
// attempted deletions rather than a lower bound.
func (s ExecSummary) BytesDeletedComplete() bool {
	return s.Failed == 0 && s.DeletedNotFullyMeasured == 0
}

// GCResult aggregates a GC run: the plan it was built from and the deletion records
// execution produced (empty for a dry run). Its summaries are computed from those,
// so the verdict always matches the evidence.
type GCResult struct {
	plan           GCPlan
	deletions      []DeletionResult
	warnings       []string
	lockSuppressed bool
}

// NewResult builds a result from the executed plan, the deletion records, and any
// warnings. The slices are copied so later mutation of the inputs cannot change a
// built result. For a dry run, pass a nil deletions slice.
func NewResult(plan GCPlan, deletions []DeletionResult, warnings []string) GCResult {
	ds := make([]DeletionResult, len(deletions))
	copy(ds, deletions)
	ws := make([]string, len(warnings))
	copy(ws, warnings)
	return GCResult{plan: plan, deletions: ds, warnings: ws}
}

// NewLockSuppressedResult builds a result for a destructive run that took the
// collector lease but then found an active or unknown writer lock (or an unreadable
// locks directory) at the under-lease recheck and deleted nothing. It is a distinct,
// machine-legible outcome from a plan-time lock block (which shows as blocked
// candidates) and from a run that simply had nothing to delete: an agent reads
// LockSuppressed to know cleanup was withheld to avoid racing a writer, not because
// the store was already clean. Deletions are always empty; the warnings explain it.
//
// Suppression is meaningful only for a real, unblocked run: a dry run takes no lease
// and an already-blocked plan never reaches the writer recheck. Constructing it from
// either is a programmer error, so it fails loud rather than emitting a nonsensical
// "suppressed dry run".
func NewLockSuppressedResult(plan GCPlan, warnings []string) GCResult {
	if plan.DryRun() || plan.Blocked() {
		panic("gc: NewLockSuppressedResult requires a real, unblocked plan")
	}
	r := NewResult(plan, nil, warnings)
	r.lockSuppressed = true
	return r
}

// Plan returns the plan this result was executed from.
func (r GCResult) Plan() GCPlan { return r.plan }

// Deletions returns the executed deletion records. The returned slice is a copy.
func (r GCResult) Deletions() []DeletionResult {
	out := make([]DeletionResult, len(r.deletions))
	copy(out, r.deletions)
	return out
}

// Warnings returns the non-fatal warnings recorded during planning or execution.
func (r GCResult) Warnings() []string {
	out := make([]string, len(r.warnings))
	copy(out, r.warnings)
	return out
}

// DryRun reports whether the underlying plan was a dry run (no execution).
func (r GCResult) DryRun() bool { return r.plan.dryRun }

// LockSuppressed reports whether a destructive run withheld all deletion because an
// active or unknown writer lock appeared under the held collector lease. It is the
// explicit, machine-legible signal that cleanup was skipped to avoid racing a
// writer — distinct from a clean run with nothing to delete.
func (r GCResult) LockSuppressed() bool { return r.lockSuppressed }

// ExecSummary derives the execution counts from the deletion records.
func (r GCResult) ExecSummary() ExecSummary {
	var s ExecSummary
	for _, d := range r.deletions {
		if d.err != nil {
			s.Failed++
			continue
		}
		credit := d.Credited()
		s.Deleted++
		s.BytesDeleted += credit.bytes
		if !credit.Exact() {
			s.DeletedNotFullyMeasured++
		}
	}
	return s
}
