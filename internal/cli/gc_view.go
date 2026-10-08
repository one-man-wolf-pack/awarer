package cli

import (
	"sort"
	"strconv"
	"strings"

	gcdom "awarer/internal/domain/gc"
	"awarer/internal/output"
)

// gcView is the stable JSON shape of a gc report. It carries the plan summary, the
// execution summary, every candidate with both a human reason and a stable machine
// code, and any warnings. Candidates are emitted in plan order so the output is
// deterministic.
type gcView struct {
	DryRun bool `json:"dry_run"`
	// LockSuppressed is true when a real run took the collector lease but withheld
	// all deletion because an active or unknown writer lock appeared under it. It lets
	// an agent tell "cleanup deliberately skipped to avoid racing a writer" apart from
	// "nothing to delete" and from a plan-time lock block (summary.blocked > 0).
	LockSuppressed bool          `json:"lock_suppressed"`
	Summary        gcSummaryView `json:"summary"`
	// StoreFootprint is the exact blob-store footprint (count and bytes) measured while
	// planning. It is the deep accounting ordinary "awa status" deliberately omits, moved
	// here because planning already lists every blob. Absent only on a plan built without
	// a blob pass.
	StoreFootprint *gcStoreFootprintView `json:"store_footprint,omitempty"`
	Candidates     []gcCandidateView     `json:"candidates"`
	// CandidatesBounded reports whether candidates lists every planned candidate or an
	// intentionally capped prefix. The plan itself is always computed over the whole
	// store — the summary counts drive the destructive decision, and the byte totals and
	// their completeness facts cover every candidate — so the cap is on the emitted
	// per-candidate detail only, never on the safety planning or byte accounting.
	// summary.* remains the authority on totals.
	CandidatesBounded boundedOutputView `json:"candidates_bounded"`
	Failures          []gcFailureView   `json:"failures"`
	Warnings          []string          `json:"warnings"`
}

// gcStoreFootprintView is the wire shape of the total blob-store footprint.
type gcStoreFootprintView struct {
	BlobCount int64 `json:"blob_count"`
	BlobBytes int64 `json:"blob_bytes"`
	Complete  bool  `json:"complete"`
}

type gcSummaryView struct {
	PlannedDelete int `json:"planned_delete"`
	Deleted       int `json:"deleted"`
	Failed        int `json:"failed"`
	Retained      int `json:"retained"`
	Blocked       int `json:"blocked"`
	Skipped       int `json:"skipped"`
	// BytesPlanned/BytesDeleted are accounted logical bytes: the lengths of the regular
	// files inside the selected checkpoint, run, recovery, and temp deletion boundaries,
	// plus each swept blob once. They are not allocated blocks or a promise of physical
	// space freed. Blocked, corrupt, incompatible, and unreadable records contribute 0.
	BytesPlanned int64 `json:"bytes_planned"`
	BytesDeleted int64 `json:"bytes_deleted"`
	// BytesPlannedComplete is false when some selected deletion candidate was only
	// partially measured or not measured, so bytes_planned is a lower bound.
	// BytesDeletedComplete is false when an attempted deletion failed (it is credited
	// 0) or succeeded with an incomplete measurement, so bytes_deleted is a lower bound.
	// Both describe byte accounting only — not candidates_bounded, store_footprint, or
	// whether the operation completed — and are computed over every candidate, never
	// the capped detail list.
	BytesPlannedComplete bool `json:"bytes_planned_complete"`
	BytesDeletedComplete bool `json:"bytes_deleted_complete"`
}

// gcFailureView is one deletion that was attempted but failed, so an agent can see
// exactly which state was left behind rather than trusting a bare exit code.
type gcFailureView struct {
	Kind  string `json:"kind"`
	ID    string `json:"id,omitempty"`
	Path  string `json:"path"`
	Error string `json:"error"`
}

type gcCandidateView struct {
	Kind    string `json:"kind"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	ID      string `json:"id,omitempty"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Bytes   int64  `json:"bytes"`
	// BytesMeasurement qualifies bytes: "exact" (a known value, zero included),
	// "partial" (a lower bound), or "unmeasured" (no size was established — retained,
	// filtered, and blocked records are not measured — so bytes carries no meaning).
	BytesMeasurement string `json:"bytes_measurement"`
}

func gcViewOf(res gcdom.GCResult) gcView {
	plan := res.Plan()
	ps := plan.Summary()
	es := res.ExecSummary()
	v := gcView{
		DryRun:         res.DryRun(),
		LockSuppressed: res.LockSuppressed(),
		Summary: gcSummaryView{
			PlannedDelete: ps.PlannedDelete,
			Deleted:       es.Deleted,
			Failed:        es.Failed,
			Retained:      ps.Retained,
			Blocked:       ps.Blocked,
			Skipped:       ps.Skipped,
			BytesPlanned:  ps.BytesPlanned,
			BytesDeleted:  es.BytesDeleted,

			BytesPlannedComplete: ps.BytesPlannedComplete(),
			BytesDeletedComplete: es.BytesDeletedComplete(),
		},
		Candidates: []gcCandidateView{},
		Failures:   []gcFailureView{},
		Warnings:   res.Warnings(),
	}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	if fp, ok := plan.BlobFootprint(); ok {
		v.StoreFootprint = &gcStoreFootprintView{
			BlobCount: int64(fp.Count),
			BlobBytes: fp.Bytes,
			Complete:  fp.Complete,
		}
	}
	for _, d := range res.Deletions() {
		if d.Err() == nil {
			continue
		}
		c := d.Candidate()
		v.Failures = append(v.Failures, gcFailureView{
			Kind:  c.Kind().String(),
			ID:    c.ID(),
			Path:  c.Path(),
			Error: d.Err().Error(),
		})
	}
	// The plan is decided over every candidate (summary counts above), but the emitted
	// per-candidate list is capped so gc --json on a huge store stays a bounded report
	// rather than an unbounded dump. The bounded fact carries the cap and the full count.
	cands := plan.Candidates()
	for i, c := range cands {
		if i >= defaultGCCandidatesJSONLimit {
			break
		}
		v.Candidates = append(v.Candidates, gcCandidateView{
			Kind:    c.Kind().String(),
			Action:  c.Action().String(),
			Reason:  c.Reason().String(),
			ID:      c.ID(),
			Path:    c.Path(),
			Message: c.Message(),
			Bytes:   c.Bytes(),

			BytesMeasurement: c.ByteMeasure().Measurement().String(),
		})
	}
	v.CandidatesBounded = boundedOutputOf(len(v.Candidates), len(cands), defaultGCCandidatesJSONLimit)
	return v
}

// defaultGCCandidatesJSONLimit caps how many per-candidate records gc --json emits. The
// candidate list grows with store size; the summary counts and byte totals still cover
// every candidate (the plan is computed over the whole store), so a consumer that hits
// the cap still has the authoritative totals and learns the full candidate count from
// candidates_bounded.
const defaultGCCandidatesJSONLimit = 1000

// renderGC prints the concise human report. A clean run is boring; a deleting run
// makes the decision visible; a blocked run says why nothing was removed.
func renderGC(w *output.Writer, res gcdom.GCResult) {
	plan := res.Plan()
	ps := plan.Summary()
	es := res.ExecSummary()

	prefix := "gc"
	if res.DryRun() {
		prefix = "gc (dry-run)"
	}

	switch {
	case res.DryRun():
		// A dry run reports the plan: what would be removed (the blocked line below
		// notes when an obstruction would actually prevent it).
		if ps.PlannedDelete == 0 {
			w.Linef("%s: nothing to remove", prefix)
		} else {
			w.Linef("%s: would remove %s", prefix, kindCountList(deleteCountsByKind(plan)))
			w.Linef("planned bytes: %s", logicalBytes(ps.BytesPlanned, ps.BytesPlannedComplete()))
		}
	case res.LockSuppressed():
		// A real run took the lease but withheld deletion because a writer lock
		// appeared: say so rather than "nothing to remove", which would read as a
		// clean store. The warning below carries the lock detail.
		w.Linef("%s: deletion withheld — a writer lock appeared; nothing removed", prefix)
	case es.Deleted == 0:
		// Nothing was actually removed. When the plan was blocked, the blocked line
		// below carries the reason, so this stays a plain statement of fact.
		w.Linef("%s: nothing to remove", prefix)
	default:
		// Report what execution actually removed, not what was merely planned.
		w.Linef("%s: removed %s", prefix, kindCountList(executedCountsByKind(res)))
		w.Linef("removed bytes: %s", logicalBytes(es.BytesDeleted, es.BytesDeletedComplete()))
	}

	renderRetainedByReason(w, plan)

	if plan.StateActionBlocked() {
		w.Linef("blocked: %s prevents safe deletion; run 'awa doctor' (nothing removed)", blockedCause(plan))
	} else if plan.LockBlocked() {
		w.Linef("blocked: an active or unknown lock prevents deletion; nothing removed")
	}

	for _, warn := range res.Warnings() {
		w.Diagnostic("warning: " + warn)
	}
	if warn := byteAccountingWarning(res); warn != "" {
		w.Diagnostic("warning: " + warn)
	}

	// Surface failed deletions so a partial run is not mistaken for a clean one.
	if es.Failed > 0 {
		w.Linef("failed: %d deletions could not be completed", es.Failed)
	}
}

// logicalBytes renders an accounted byte total, qualified as a lower bound when its
// measurement is incomplete. The figure is logical file length, not disk space.
func logicalBytes(n int64, complete bool) string {
	s := humanizeBytes(n) + " of logical file data"
	if !complete {
		return "at least " + s
	}
	return s
}

// byteAccountingWarning explains, in one bounded line, why the byte total the report
// printed is a lower bound, or returns "" when it is exact. It speaks only about the
// figure the report shows: the plan's for a dry run, execution's for a run that
// attempted deletion. A blocked or suppressed run printed no figure.
func byteAccountingWarning(res gcdom.GCResult) string {
	if res.DryRun() {
		ps := res.Plan().Summary()
		if ps.PlannedDelete == 0 || ps.BytesPlannedComplete() {
			return ""
		}
		return "byte total is a lower bound: " + plural(ps.PlannedNotFullyMeasured, "selected items") +
			" could not be fully measured"
	}
	es := res.ExecSummary()
	if es.Deleted == 0 || es.BytesDeletedComplete() {
		return ""
	}
	var parts []string
	if es.Failed > 0 {
		parts = append(parts, plural(es.Failed, "failed deletions")+" credited 0")
	}
	if es.DeletedNotFullyMeasured > 0 {
		parts = append(parts, plural(es.DeletedNotFullyMeasured, "removed items")+" not fully measured")
	}
	return "removed byte total is a lower bound: " + strings.Join(parts, ", ")
}

// blockedCause names what a state-action-blocked plan is actually blocked on, so the
// line does not report an unreadable schema as damage. Both can hold at once, and
// both need the same next step (inspect with doctor, then repair or reset), so they
// are named together rather than one silently standing in for the other.
func blockedCause(plan gcdom.GCPlan) string {
	var corrupt, incompatible bool
	for _, c := range plan.Candidates() {
		if c.Action() != gcdom.ActionBlocked {
			continue
		}
		switch {
		case c.Reason().Incompatible():
			incompatible = true
		case c.Reason() == gcdom.ReasonLockActive || c.Reason() == gcdom.ReasonLockUnknown:
			// A lock has its own line; it is not what makes a plan need user action.
		default:
			corrupt = true
		}
	}
	switch {
	case corrupt && incompatible:
		return "storage corruption and evidence in an incompatible schema"
	case incompatible:
		return "evidence in an incompatible schema"
	default:
		return "storage corruption"
	}
}

// renderRetainedByReason prints the retained total and a compact per-reason
// breakdown, so a reader (especially of "gc --committed --dry-run", a trust-and-safety
// command) sees WHY nothing was deleted without parsing the JSON candidates. It groups
// by the stable retention-reason token, ordered by descending count then token for
// determinism, and aligns the reason column. The full per-candidate detail stays in
// the JSON output.
func renderRetainedByReason(w *output.Writer, plan gcdom.GCPlan) {
	counts := map[gcdom.RetentionReason]int{}
	total := 0
	for _, c := range plan.Candidates() {
		if c.Action() != gcdom.ActionRetain {
			continue
		}
		counts[c.Reason()]++
		total++
	}
	w.Linef("retained: %s", plural(total, "records"))
	if total == 0 {
		return
	}
	type rc struct {
		reason string
		n      int
	}
	rows := make([]rc, 0, len(counts))
	width := 0
	for reason, n := range counts {
		rows = append(rows, rc{reason: reason.String(), n: n})
		if len(reason.String()) > width {
			width = len(reason.String())
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].reason < rows[j].reason
	})
	for _, r := range rows {
		w.Linef("  %-*s %d", width+1, r.reason+":", r.n)
	}
}

// kindCounts holds per-kind tallies for the human one-liners.
type kindCounts struct {
	checkpoints int
	runs        int
	restores    int
	blobs       int
	temp        int
}

func deleteCountsByKind(plan gcdom.GCPlan) kindCounts {
	var c kindCounts
	for _, cand := range plan.Candidates() {
		if cand.Action() != gcdom.ActionDelete {
			continue
		}
		c.tally(cand.Kind())
	}
	return c
}

// executedCountsByKind tallies the deletions that actually succeeded, so the human
// summary reflects what was removed rather than what was planned.
func executedCountsByKind(res gcdom.GCResult) kindCounts {
	var c kindCounts
	for _, d := range res.Deletions() {
		if d.Err() != nil {
			continue
		}
		c.tally(d.Candidate().Kind())
	}
	return c
}

func (c *kindCounts) tally(k gcdom.CandidateKind) {
	switch k {
	case gcdom.KindCheckpoint:
		c.checkpoints++
	case gcdom.KindRun:
		c.runs++
	case gcdom.KindRestore:
		c.restores++
	case gcdom.KindBlob:
		c.blobs++
	case gcdom.KindTemp:
		c.temp++
	}
}

func kindCountList(c kindCounts) string {
	return joinCounts(
		count{c.checkpoints, "checkpoints"},
		count{c.runs, "runs"},
		count{c.restores, "restore recovery observations"},
		count{c.blobs, "blobs"},
		count{c.temp, "temp artifacts"},
	)
}

type count struct {
	n     int
	label string
}

func joinCounts(parts ...count) string {
	out := ""
	for _, p := range parts {
		if out != "" {
			out += ", "
		}
		out += plural(p.n, p.label)
	}
	return out
}

// plural renders "N label", dropping the trailing "s" of the plural label for a
// count of one so the line reads "1 checkpoint, 0 runs" rather than "1 checkpoints".
// The four labels GC uses all singularize by trimming a trailing "s" ("temp
// artifacts" → "temp artifact").
func plural(n int, label string) string {
	if n == 1 {
		label = strings.TrimSuffix(label, "s")
	}
	return strconv.Itoa(n) + " " + label
}
