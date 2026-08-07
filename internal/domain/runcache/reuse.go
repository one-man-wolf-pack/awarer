package runcache

import "fmt"

// ReuseKind is the closed classification of a recorded run's reuse state. Every
// persisted run is exactly one of these; the value object below forbids the
// contradictory combinations (a reusable run carrying a reason, a non-reusable run
// without one) so an impossible state such as "cacheable but mutated" is
// unrepresentable rather than merely discouraged.
type ReuseKind int

const (
	// ReuseReusable: a recorded run that is a valid cache-hit candidate. Only these
	// have a published key pointer, so only these are reachable by cache lookup.
	ReuseReusable ReuseKind = iota
	// ReuseNonReusable: a real execution disqualified from reuse after the fact — it
	// changed observed state (mutated) or failed under a no-cache-failures policy.
	ReuseNonReusable
	// ReuseRecordOnly: captured supervised history that policy never offered for
	// caching at all (--record, --no-cache, unkeyed stdin, skipped inputs).
	ReuseRecordOnly
	// ReuseUnknownPostState: the post-run observation scan failed, so whether the
	// command mutated state is unknown; it fails closed and is never reusable.
	ReuseUnknownPostState
)

// String returns the stable machine token for the kind, part of the run JSON and
// timeline contract.
func (k ReuseKind) String() string {
	switch k {
	case ReuseReusable:
		return "reusable"
	case ReuseNonReusable:
		return "non-reusable"
	case ReuseRecordOnly:
		return "record-only"
	case ReuseUnknownPostState:
		return "unknown-post-state"
	default:
		return "unknown"
	}
}

// ParseReuseKind resolves a persisted reuse-kind token.
func ParseReuseKind(s string) (ReuseKind, error) {
	switch s {
	case "reusable":
		return ReuseReusable, nil
	case "non-reusable":
		return ReuseNonReusable, nil
	case "record-only":
		return ReuseRecordOnly, nil
	case "unknown-post-state":
		return ReuseUnknownPostState, nil
	default:
		return 0, fmt.Errorf("invalid reuse kind %q", s)
	}
}

// ReuseState is the typed reuse classification of one recorded run. It pairs a
// kind with a machine-stable reason that is required for every non-reusable kind
// and forbidden for the reusable one, so a rendered row or a persisted record can
// never show a reusable state carrying a reason or a non-reusable state without
// one. Construct it only through the constructors below.
type ReuseState struct {
	kind   ReuseKind
	reason MismatchReason
}

// ReusableCacheEntry builds the reusable state (no reason): a recorded run that is
// a valid cache-hit candidate and carries a published key pointer.
func ReusableCacheEntry() ReuseState {
	return ReuseState{kind: ReuseReusable}
}

// disqualifiedReason reports whether r is a reason a real execution that was
// eligible to cache is disqualified from reuse: it changed observed input state
// (mutated-state) or watched effect state (effect-state-differs), its effect state
// could not be observed safely (effect-state-unavailable), or it failed and policy
// declines to cache failures. These are the only reasons a non-reusable recorded run
// can carry.
func (r MismatchReason) disqualifiedReason() bool {
	switch r {
	case ReasonMutatedState, ReasonEffectStateDiffers, ReasonEffectStateUnavailable, ReasonFailedNotCached:
		return true
	default:
		return false
	}
}

// policyReason reports whether r is a reason policy kept a run out of the cache from
// the start (not an after-the-fact disqualification). These are the only reasons a
// record-only run can carry.
func (r MismatchReason) policyReason() bool {
	switch r {
	case ReasonRecordOnly, ReasonNoCache, ReasonStdinNotKeyed, ReasonSkippedInputs,
		ReasonCaptureDisabled, ReasonNonCacheablePolicy, ReasonFastTrustMode:
		return true
	default:
		return false
	}
}

// NonReusableRun builds a non-reusable state: a run that executed and was eligible to
// cache but is disqualified from reuse (it mutated state, or failed and policy will
// not cache a failure). The reason must be one of those disqualification reasons —
// not a policy reason, which is a record-only run's concern.
func NonReusableRun(reason MismatchReason) (ReuseState, error) {
	if !reason.disqualifiedReason() {
		return ReuseState{}, fmt.Errorf("non-reusable run cannot carry reason %q (want a disqualification reason: %q, %q, %q, or %q)", reason, ReasonMutatedState, ReasonEffectStateDiffers, ReasonEffectStateUnavailable, ReasonFailedNotCached)
	}
	return ReuseState{kind: ReuseNonReusable, reason: reason}, nil
}

// RecordOnly builds a record-only state: a run policy kept out of the cache from the
// start (record-only, no-cache, stdin-not-keyed, skipped-inputs, capture-disabled).
// The reason must be one of those policy reasons — not a disqualification reason like
// mutated-state, which is a non-reusable run's concern.
func RecordOnly(reason MismatchReason) (ReuseState, error) {
	if !reason.policyReason() {
		return ReuseState{}, fmt.Errorf("record-only run cannot carry reason %q (want a policy reason)", reason)
	}
	return ReuseState{kind: ReuseRecordOnly, reason: reason}, nil
}

// UnknownPostState builds the unknown-post-state state. Its reason is fixed to
// post-scan-failed: the only way to reach it is a failed post-run observation.
func UnknownPostState() ReuseState {
	return ReuseState{kind: ReuseUnknownPostState, reason: ReasonPostScanFailed}
}

// NewReuseState rebuilds a state from a persisted kind and reason by delegating to
// the same constructors the write path uses, so a hand-edited record cannot decode
// into a kind/reason pairing the constructors would reject.
func NewReuseState(kind ReuseKind, reason MismatchReason) (ReuseState, error) {
	switch kind {
	case ReuseReusable:
		if reason != "" {
			return ReuseState{}, fmt.Errorf("reusable run carries a reason %q", reason)
		}
		return ReusableCacheEntry(), nil
	case ReuseNonReusable:
		return NonReusableRun(reason)
	case ReuseRecordOnly:
		return RecordOnly(reason)
	case ReuseUnknownPostState:
		if reason != ReasonPostScanFailed {
			return ReuseState{}, fmt.Errorf("unknown-post-state run must carry reason %q, got %q", ReasonPostScanFailed, reason)
		}
		return UnknownPostState(), nil
	default:
		return ReuseState{}, fmt.Errorf("invalid reuse kind %d", kind)
	}
}

// Kind returns the reuse kind.
func (s ReuseState) Kind() ReuseKind { return s.kind }

// Reason returns the non-reusable reason, or the empty reason when reusable.
func (s ReuseState) Reason() MismatchReason { return s.reason }

// IsReusable reports whether the run is a valid cache-hit candidate.
func (s ReuseState) IsReusable() bool { return s.kind == ReuseReusable }

// IsUnknownPostState reports whether the run's post-execution scan failed, leaving its
// post-state unknown. It is the one reuse state whose run has no after-observation, so an
// evidence record's after-observation presence is exactly its complement.
func (s ReuseState) IsUnknownPostState() bool { return s.kind == ReuseUnknownPostState }

// String returns the stable kind token.
func (s ReuseState) String() string { return s.kind.String() }
