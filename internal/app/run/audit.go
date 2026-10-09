package run

import (
	"context"
	"errors"
	"fmt"

	"awarer/internal/domain/runcache"
)

// The recorded-execution audit window: the newest DefaultAuditLimit stored runs
// unless -n asks for another count, never more than MaxAuditLimit.
const (
	DefaultAuditLimit = 200
	MaxAuditLimit     = 1000
)

// The audit's metadata read budgets. A stored meta.json is a few kilobytes; the
// per-record bound only stops one pathological document from dominating the work,
// and the aggregate bound caps the whole report at a fixed byte cost however large
// the window. Both are diagnostic limits, never validity rules.
const (
	auditRecordBudget    int64 = 1 << 20
	auditAggregateBudget int64 = 64 << 20
)

// AuditExampleLimit caps the run ids retained per classification or availability
// state, so a report's memory and output stay fixed however large the window.
const AuditExampleLimit = 3

// AuditGroup is the fixed family a stored primary reason belongs to. It groups the
// canonical kind/reason pairs for reading; it re-derives nothing.
type AuditGroup string

// The audit groups. Only the stored reasons record-only and no-cache are a
// deliberate choice; the record-only kind also carries technical and policy
// blockers, so the kind alone never establishes deliberate recording.
const (
	AuditReusableWhenStored     AuditGroup = "reusable-when-stored"
	AuditDeliberateRecording    AuditGroup = "deliberate-recording"
	AuditPolicyBlocker          AuditGroup = "policy-blocker"
	AuditChangedState           AuditGroup = "changed-state"
	AuditObservationUnavailable AuditGroup = "observation-unavailable"
	AuditFailureCachePolicy     AuditGroup = "failure-cache-policy"
)

// AuditHistory is what the store's run history is, independent of the window and
// of read coverage. Missing and empty are both known: neither is corruption, and
// neither is an unknown total.
type AuditHistory string

const (
	// AuditHistoryPresent: at least one retained run was selected.
	AuditHistoryPresent AuditHistory = "present"
	// AuditHistoryEmpty: the history exists and holds no retained run.
	AuditHistoryEmpty AuditHistory = "empty"
	// AuditHistoryMissing: the store has no run history location (none was created,
	// or it was removed); it says nothing about runs before that.
	AuditHistoryMissing AuditHistory = "missing"
)

// AuditAvailability is why a selected candidate contributed no classification. Each
// keeps its place in the window; none is replaced by an older record.
type AuditAvailability string

const (
	// AuditCorrupt: the metadata failed canonical decoding or validation.
	AuditCorrupt AuditAvailability = "corrupt"
	// AuditIncompatible: the metadata declares a schema this build does not read.
	AuditIncompatible AuditAvailability = "incompatible"
	// AuditVanished: the record disappeared between selection and reading.
	AuditVanished AuditAvailability = "vanished"
	// AuditUnreadable: the metadata could not be read for another reason.
	AuditUnreadable AuditAvailability = "unreadable"
	// AuditOverRecordBudget: the metadata is larger than the per-record budget.
	AuditOverRecordBudget AuditAvailability = "over-record-budget"
	// AuditUnreadBudget: the aggregate budget did not cover reading this record.
	AuditUnreadBudget AuditAvailability = "unread-budget"
)

// auditAvailabilities is the fixed presentation order of the availability states.
var auditAvailabilities = []AuditAvailability{
	AuditCorrupt, AuditIncompatible, AuditVanished, AuditUnreadable, AuditOverRecordBudget, AuditUnreadBudget,
}

// auditClass is one canonical stored (kind, reason) pair and its group.
type auditClass struct {
	group  AuditGroup
	kind   runcache.ReuseKind
	reason runcache.MismatchReason
}

// auditClasses is the closed classification vocabulary in presentation order. It
// lists every pair the stored-metadata decoder admits; a pair outside it is an
// internal fault, not a silently misfiled record.
var auditClasses = []auditClass{
	{AuditReusableWhenStored, runcache.ReuseReusable, ""},
	{AuditDeliberateRecording, runcache.ReuseRecordOnly, runcache.ReasonRecordOnly},
	{AuditDeliberateRecording, runcache.ReuseRecordOnly, runcache.ReasonNoCache},
	{AuditPolicyBlocker, runcache.ReuseRecordOnly, runcache.ReasonStdinNotKeyed},
	{AuditPolicyBlocker, runcache.ReuseRecordOnly, runcache.ReasonSkippedInputs},
	{AuditPolicyBlocker, runcache.ReuseRecordOnly, runcache.ReasonFastTrustMode},
	{AuditPolicyBlocker, runcache.ReuseRecordOnly, runcache.ReasonNonCacheablePolicy},
	{AuditPolicyBlocker, runcache.ReuseRecordOnly, runcache.ReasonCaptureDisabled},
	{AuditChangedState, runcache.ReuseNonReusable, runcache.ReasonMutatedState},
	{AuditChangedState, runcache.ReuseNonReusable, runcache.ReasonEffectStateDiffers},
	{AuditObservationUnavailable, runcache.ReuseNonReusable, runcache.ReasonEffectStateUnavailable},
	{AuditObservationUnavailable, runcache.ReuseUnknownPostState, runcache.ReasonPostScanFailed},
	{AuditFailureCachePolicy, runcache.ReuseNonReusable, runcache.ReasonFailedNotCached},
}

// auditStore is the only store access the audit has: newest-id selection and the
// bounded canonical metadata read. Payloads, manifests, key pointers, and current
// state are deliberately out of reach.
type auditStore interface {
	ListRefsNewest(ctx context.Context, limit int) ([]runcache.RunID, error)
	GetWithin(id runcache.RunID, maxBytes int64) (runcache.RunEntry, int64, error)
	HistoryPresent() (bool, error)
}

// AuditRequest selects the audit window. A zero Limit means DefaultAuditLimit.
type AuditRequest struct {
	Limit int
	// recordBudget and aggregateBudget override the read budgets for tests; zero
	// selects the production values.
	recordBudget    int64
	aggregateBudget int64
}

// AuditExamples is a bounded sample of full run ids, newest first, out of Total.
type AuditExamples struct {
	IDs   []runcache.RunID
	Total int
}

// AuditClassCount is the number of classified candidates with one stored pair.
type AuditClassCount struct {
	Group    AuditGroup
	Kind     runcache.ReuseKind
	Reason   runcache.MismatchReason
	Count    int
	Examples AuditExamples
}

// AuditAvailabilityCount is the number of selected candidates in one availability
// state.
type AuditAvailabilityCount struct {
	State    AuditAvailability
	Count    int
	Examples AuditExamples
}

// AuditResult is a recorded-execution audit over one newest window. Selected counts
// candidates in the window; Classified counts those whose metadata validated, and
// the class counts sum to it. Older reports whether retained records exist beyond
// the window, and History whether an empty window means empty or missing history.
// Classes and Availability hold only non-zero entries, in fixed order.
type AuditResult struct {
	History         AuditHistory
	Requested       int
	Selected        int
	Older           bool
	Classified      int
	Unavailable     int
	Classes         []AuditClassCount
	Availability    []AuditAvailabilityCount
	RecordBudget    int64
	AggregateBudget int64
	BytesRead       int64
}

// Audit summarizes the primary stored reuse classifications of the newest retained
// executions. See auditRecorded.
func (s *Service) Audit(ctx context.Context, req AuditRequest) (AuditResult, error) {
	return auditRecorded(ctx, s.deps.Store, req)
}

// auditRecorded selects the newest window of stored run ids, then reads each one's
// metadata under the per-record and aggregate byte budgets and counts its stored
// primary kind/reason once. The window is fixed before any read: a candidate that
// is corrupt, incompatible, vanished, unreadable, or over budget keeps its place
// and is disclosed, never refilled from older history. Failure to select the
// window, and cancellation, fail the audit; a per-record gap does not.
func auditRecorded(ctx context.Context, store auditStore, req AuditRequest) (AuditResult, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultAuditLimit
	}
	if limit > MaxAuditLimit {
		return AuditResult{}, fmt.Errorf("audit window %d exceeds the limit %d", limit, MaxAuditLimit)
	}
	recordBudget := req.recordBudget
	if recordBudget <= 0 {
		recordBudget = auditRecordBudget
	}
	aggregateBudget := req.aggregateBudget
	if aggregateBudget <= 0 {
		aggregateBudget = auditAggregateBudget
	}

	// One id past the window tells whether older history exists without a second
	// enumeration; that extra id is never read.
	ids, err := store.ListRefsNewest(ctx, limit+1)
	if err != nil {
		return AuditResult{}, err
	}
	res := AuditResult{Requested: limit, RecordBudget: recordBudget, AggregateBudget: aggregateBudget}
	if len(ids) > limit {
		res.Older = true
		ids = ids[:limit]
	}
	res.Selected = len(ids)
	res.History = AuditHistoryPresent
	if len(ids) == 0 {
		// Only an empty selection asks whether history exists at all; asking after
		// listing keeps a run published in between from reading as missing history.
		present, err := store.HistoryPresent()
		if err != nil {
			return AuditResult{}, err
		}
		res.History = AuditHistoryMissing
		if present {
			res.History = AuditHistoryEmpty
		}
	}

	classes := make([]AuditClassCount, len(auditClasses))
	for i, c := range auditClasses {
		classes[i] = AuditClassCount{Group: c.group, Kind: c.kind, Reason: c.reason}
	}
	avail := make([]AuditAvailabilityCount, len(auditAvailabilities))
	for i, a := range auditAvailabilities {
		avail[i] = AuditAvailabilityCount{State: a}
	}
	unavailable := func(state AuditAvailability, id runcache.RunID) {
		for i := range avail {
			if avail[i].State == state {
				avail[i].Count++
				avail[i].Examples.add(id)
				res.Unavailable++
				return
			}
		}
	}

	exhausted := false
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return AuditResult{}, err
		}
		remaining := aggregateBudget - res.BytesRead
		if exhausted || remaining <= 0 {
			exhausted = true
			unavailable(AuditUnreadBudget, id)
			continue
		}
		bound := min(recordBudget, remaining)
		entry, n, err := store.GetWithin(id, bound)
		res.BytesRead += n
		switch {
		case err == nil:
		case errors.Is(err, runcache.ErrReadBudgetExceeded):
			if bound < recordBudget {
				// Refused only because the aggregate budget ran short; the rest of the
				// window is not read either, so the report's cost stays bounded.
				exhausted = true
				unavailable(AuditUnreadBudget, id)
			} else {
				unavailable(AuditOverRecordBudget, id)
			}
			continue
		case errors.Is(err, runcache.ErrNotFound):
			unavailable(AuditVanished, id)
			continue
		case errors.Is(err, runcache.ErrIncompatibleEntry):
			unavailable(AuditIncompatible, id)
			continue
		case errors.Is(err, runcache.ErrCorruptStore):
			unavailable(AuditCorrupt, id)
			continue
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return AuditResult{}, err
		default:
			unavailable(AuditUnreadable, id)
			continue
		}
		i := auditClassIndex(entry.Reuse.Kind(), entry.Reuse.Reason())
		if i < 0 {
			return AuditResult{}, fmt.Errorf("audit: run %s has an unclassified stored reuse state %s/%s", id.Short(), entry.Reuse.Kind(), entry.Reuse.Reason())
		}
		classes[i].Count++
		classes[i].Examples.add(id)
		res.Classified++
	}

	// A cancellation that arrived after the last read still means the report was not
	// wanted; it is never presented as a completed audit.
	if err := ctx.Err(); err != nil {
		return AuditResult{}, err
	}
	for _, c := range classes {
		if c.Count > 0 {
			res.Classes = append(res.Classes, c)
		}
	}
	for _, a := range avail {
		if a.Count > 0 {
			res.Availability = append(res.Availability, a)
		}
	}
	return res, nil
}

// add records one more member, keeping at most AuditExampleLimit ids.
func (e *AuditExamples) add(id runcache.RunID) {
	e.Total++
	if len(e.IDs) < AuditExampleLimit {
		e.IDs = append(e.IDs, id)
	}
}

func auditClassIndex(kind runcache.ReuseKind, reason runcache.MismatchReason) int {
	for i, c := range auditClasses {
		if c.kind == kind && c.reason == reason {
			return i
		}
	}
	return -1
}
