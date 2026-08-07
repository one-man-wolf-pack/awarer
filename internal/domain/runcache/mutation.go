package runcache

import "fmt"

// MismatchReason is a stable, machine-readable token explaining why a stored run
// is not a valid cache-hit candidate — either right now (the current-state reuse
// view) or against a given invocation (a near miss). It is the single reason
// vocabulary shared by run ls, run ls --near, run explain, the live mutation
// guard, and the persisted reuse state, so consumers (agents, tests) can switch on
// one closed set. The vocabulary is current-only: a persisted record carries one of
// these exact tokens or fails to decode.
type MismatchReason string

const (
	// ReasonInputTreeDiffers: the observed input tree no longer matches the recorded
	// one — the command's inputs changed, so its recorded result no longer applies.
	ReasonInputTreeDiffers MismatchReason = "input-tree-differs"
	// ReasonMutatedState: the command changed observed state, so its result cannot be
	// replayed. Reported by the live mutation guard and persisted on a non-reusable
	// recorded run.
	ReasonMutatedState MismatchReason = "mutated-state"
	// ReasonEffectStateDiffers: the project-local generated-output state awa watches
	// (build, dist, target, node_modules, ...) no longer matches — either the command
	// changed it while running (persisted on a non-reusable recorded run) or a watched
	// output was deleted/changed after a reusable run (the current-state reuse view).
	// It is distinct from input-tree-differs: the input scope changed vs. the watched
	// effect scope changed.
	ReasonEffectStateDiffers MismatchReason = "effect-state-differs"
	// ReasonEffectStateUnavailable: a watched generated-output root could not be
	// observed safely (unreadable, symlink-ambiguous), so the effect state is unknown
	// and the run fails closed to non-reusable.
	ReasonEffectStateUnavailable MismatchReason = "effect-state-unavailable"
	// ReasonFastTrustMode: the run was observed under the fast trust mode, whose
	// stat-only (size+mtime) input comparison can miss a same-size/same-mtime rewrite,
	// so it is recorded but never published as reusable.
	ReasonFastTrustMode MismatchReason = "fast-trust-mode"
	// ReasonExpired: a stored entry is older than the run TTL.
	ReasonExpired MismatchReason = "expired"
	// ReasonEnvMismatch: an allowlisted environment fact differs from the recorded one.
	ReasonEnvMismatch MismatchReason = "env-mismatch"
	// ReasonCWDMismatch: the execution cwd differs from the recorded one.
	ReasonCWDMismatch MismatchReason = "cwd-mismatch"
	// ReasonConfigMismatch: a recorded key fact (run config, trust mode, awa version,
	// executable identity, ...) no longer matches.
	ReasonConfigMismatch MismatchReason = "config-mismatch"
	// ReasonPlatformMismatch: the recorded run was captured on a different OS/arch, so
	// its result is not portable to the current platform.
	ReasonPlatformMismatch MismatchReason = "platform-mismatch"
	// ReasonPayloadMissing: a stored entry's output payload is absent or fails its
	// recorded hash, so the hit could not be replayed honestly.
	ReasonPayloadMissing MismatchReason = "payload-missing"
	// ReasonCorrupt: a stored entry's metadata could not be decoded/validated.
	ReasonCorrupt MismatchReason = "corrupt"
	// ReasonPostScanFailed: the post-run observation scan failed, so the resulting
	// state is unknown and must fail closed. Reported by the live mutation guard.
	ReasonPostScanFailed MismatchReason = "post-scan-failed"
	// ReasonNonCacheablePolicy: policy forbade caching this invocation (capture
	// disabled, unkeyed stdin/tty, skipped inputs without the flag, --no-cache).
	ReasonNonCacheablePolicy MismatchReason = "non-cacheable-policy"
	// ReasonRecordOnly: the run was captured as supervised history (e.g. --record) and
	// never offered for caching, even though it was otherwise cacheable. It is a
	// property of the stored run, not only of how it was invoked.
	ReasonRecordOnly MismatchReason = "record-only"
	// ReasonNoCache: --no-cache ran without reading or writing the cache.
	ReasonNoCache MismatchReason = "no-cache"
	// ReasonStdinNotKeyed: a piped or tty stdin is not part of the key, so the result
	// could not be safely reused.
	ReasonStdinNotKeyed MismatchReason = "stdin-not-keyed"
	// ReasonSkippedInputs: the scan skipped inputs and --allow-skipped-inputs was not
	// set, so the key could not faithfully cover the command's real inputs.
	ReasonSkippedInputs MismatchReason = "skipped-inputs"
	// ReasonCaptureDisabled: output capture was turned off, so there was nothing to
	// store or replay.
	ReasonCaptureDisabled MismatchReason = "capture-disabled"
	// ReasonFailedNotCached: the command failed and policy (cache_failed_runs=false,
	// or --no-cache-failures) declines to cache a failed result.
	ReasonFailedNotCached MismatchReason = "failed-not-cached"
)

// mismatchReasons is the canonical enumeration of the closed vocabulary. It is the
// one list both ParseMismatchReason and MismatchReasons read, so the parser and the
// published vocabulary cannot disagree. Go cannot enumerate the constants above, so
// adding one here is still a manual step; the count assertion in the package tests
// is what makes it a deliberate one, and it names the consumers — notably the
// documentation coverage matrix — that a new reason obliges.
//
// The order is semantic and deliberate, not alphabetical: what the observed state
// says, then what the recorded key says, then what the stored payload says, then
// what policy says. Consumers may rely on it being stable.
var mismatchReasons = []MismatchReason{
	// observed state no longer matches
	ReasonInputTreeDiffers,
	ReasonMutatedState,
	ReasonEffectStateDiffers,
	ReasonEffectStateUnavailable,
	ReasonFastTrustMode,
	// a recorded key fact no longer matches
	ReasonExpired,
	ReasonEnvMismatch,
	ReasonCWDMismatch,
	ReasonConfigMismatch,
	ReasonPlatformMismatch,
	// the stored evidence cannot be replayed honestly
	ReasonPayloadMissing,
	ReasonCorrupt,
	ReasonPostScanFailed,
	// policy declined to make the run reusable
	ReasonNonCacheablePolicy,
	ReasonRecordOnly,
	ReasonNoCache,
	ReasonStdinNotKeyed,
	ReasonSkippedInputs,
	ReasonCaptureDisabled,
	ReasonFailedNotCached,
}

// MismatchReasons returns the closed reason vocabulary in canonical order. The
// result is a copy: the catalog is package state, and a consumer that walks it
// must not be able to reorder or truncate what every other consumer sees.
func MismatchReasons() []MismatchReason {
	return append([]MismatchReason(nil), mismatchReasons...)
}

// knownMismatchReasons is the membership view of mismatchReasons, built once so
// ParseMismatchReason is a lookup rather than a switch that could drift from the
// enumeration.
var knownMismatchReasons = func() map[MismatchReason]bool {
	set := make(map[MismatchReason]bool, len(mismatchReasons))
	for _, r := range mismatchReasons {
		set[r] = true
	}
	return set
}()

// ParseMismatchReason resolves a persisted reason token, rejecting an empty or
// unknown one so a corrupt record cannot smuggle a blank reason past decode. The
// vocabulary is closed and current-only: every reason a record can carry is one of
// the constants above, and no other spelling maps to one of them — a record
// carrying an unrecognized token fails strict decoding rather than being translated.
func ParseMismatchReason(s string) (MismatchReason, error) {
	if r := MismatchReason(s); knownMismatchReasons[r] {
		return r, nil
	}
	return "", fmt.Errorf("invalid mismatch reason %q", s)
}

// String returns the stable token.
func (r MismatchReason) String() string { return string(r) }

// ReasonCanSampleChangedPaths reports whether a mismatch reason can yield a useful
// changed-input-path sample. Only input-tree-differs can: the sample compares the
// stored run's before-input-manifest against current input state, so it answers
// "which inputs changed?" for exactly that reason. Every other reason — a record-only
// or mutating run that was never re-keyed, an effect-state change (a different scope
// entirely), a corrupt/missing/incompatible payload, a policy disqualification — cannot
// produce a meaningful input-path diff, so callers must not pay for the comparison
// (nor even open the stored manifest) for those. It is the single gate the near-miss
// surfaces (run explain, run ls --near, the inline run footer) share so they cannot
// drift; the derived token is the honest classifier, not the raw diff code, which is
// empty for a short-circuited never-compared candidate.
func ReasonCanSampleChangedPaths(r MismatchReason) bool {
	return r == ReasonInputTreeDiffers
}

// ReasonFromDiffCode maps a key-input difference code to the shared mismatch reason
// vocabulary. The content, location, environment, and platform differences each map
// to their own precise reason (input-tree-differs, cwd-mismatch, env-mismatch,
// platform-mismatch); every other keyed fact (command, executable, scope, trust,
// run-config, stdin, tty, skipped, schema, awa version) collapses onto the
// catch-all config-mismatch — they are all "a recorded key fact changed". An empty
// or unknown code yields config-mismatch rather than a blank reason, so a
// non-reusable entry can never carry an empty reason.
func ReasonFromDiffCode(code DiffCode) MismatchReason {
	switch code {
	case DiffInputTreeChanged:
		return ReasonInputTreeDiffers
	case DiffEffectStateChanged:
		return ReasonEffectStateDiffers
	case DiffEffectStateUnavailable:
		return ReasonEffectStateUnavailable
	case DiffCWDChanged:
		return ReasonCWDMismatch
	case DiffEnvChanged:
		return ReasonEnvMismatch
	case DiffPlatformChanged:
		return ReasonPlatformMismatch
	default:
		return ReasonConfigMismatch
	}
}

// MutationOutcome is the result of the run mutation guard's pre/post observation.
type MutationOutcome int

const (
	// MutationUnobserved means the guard did not observe state (no execution, or the
	// observation was deliberately skipped). It is not a cacheable signal on its own.
	MutationUnobserved MutationOutcome = iota
	// MutationUnchanged means the post-run observed state equals the pre-run state.
	MutationUnchanged
	// MutationChanged means the command changed observed state between the scans.
	MutationChanged
	// MutationScanFailed means the post-run scan could not be computed; the state is
	// unknown and must fail closed.
	MutationScanFailed
)

func (o MutationOutcome) valid() bool {
	return o >= MutationUnobserved && o <= MutationScanFailed
}

// String returns the stable persisted token for the outcome.
func (o MutationOutcome) String() string {
	switch o {
	case MutationUnobserved:
		return "unobserved"
	case MutationUnchanged:
		return "unchanged"
	case MutationChanged:
		return "changed"
	case MutationScanFailed:
		return "scan-failed"
	default:
		return "unknown"
	}
}

// ParseMutationOutcome resolves a persisted outcome token.
func ParseMutationOutcome(s string) (MutationOutcome, error) {
	switch s {
	case "unobserved":
		return MutationUnobserved, nil
	case "unchanged":
		return MutationUnchanged, nil
	case "changed":
		return MutationChanged, nil
	case "scan-failed":
		return MutationScanFailed, nil
	default:
		return 0, fmt.Errorf("invalid mutation outcome %q", s)
	}
}

// MutationStatus is the typed outcome of the mutation guard for one execution. It
// is a value object so callers cannot fabricate an impossible "changed but
// cacheable" state: only an explicitly unchanged observation permits caching, and
// the non-cacheable reason is derived from the outcome rather than passed in.
type MutationStatus struct {
	outcome MutationOutcome
}

// NewMutationStatus builds a status from an outcome, rejecting an unknown value so a
// caller bug surfaces here rather than as a silently un-cacheable run.
func NewMutationStatus(o MutationOutcome) (MutationStatus, error) {
	if !o.valid() {
		return MutationStatus{}, fmt.Errorf("invalid mutation outcome %d", o)
	}
	return MutationStatus{outcome: o}, nil
}

// Outcome returns the underlying outcome.
func (m MutationStatus) Outcome() MutationOutcome { return m.outcome }

// Changed reports whether the command changed observed state.
func (m MutationStatus) Changed() bool { return m.outcome == MutationChanged }

// ScanFailed reports whether the post-run observation could not be computed.
func (m MutationStatus) ScanFailed() bool { return m.outcome == MutationScanFailed }

// CacheableUnderMutation reports whether the mutation guard permits publishing a
// reusable entry. Only an explicitly unchanged observation qualifies: a changed,
// failed, or unobserved state must never be published as a replayable hit.
func (m MutationStatus) CacheableUnderMutation() bool {
	return m.outcome == MutationUnchanged
}

// Reason returns the non-cacheable reason the guard imposes, or the empty reason
// when the guard does not block caching (unchanged or unobserved).
func (m MutationStatus) Reason() MismatchReason {
	switch m.outcome {
	case MutationChanged:
		return ReasonMutatedState
	case MutationScanFailed:
		return ReasonPostScanFailed
	default:
		return ""
	}
}

// RunReusability is the typed verdict the current-state reuse view (run ls) reaches
// for one stored entry: whether it is a valid cache-hit candidate right now and, if
// not, why. Like CacheDecision it forbids the contradictory states — a reusable
// verdict with a reason, or a non-reusable verdict without one — so a rendered row
// can never show a blank or contradictory reason.
type RunReusability struct {
	reusable bool
	reason   MismatchReason
}

// Reusable builds a reusable verdict (no reason).
func Reusable() RunReusability { return RunReusability{reusable: true} }

// NotReusable builds a non-reusable verdict with a required reason. An empty reason
// is a caller bug and is rejected, so a non-reusable row always names why.
func NotReusable(reason MismatchReason) (RunReusability, error) {
	if reason == "" {
		return RunReusability{}, fmt.Errorf("non-reusable verdict needs a reason")
	}
	return RunReusability{reusable: false, reason: reason}, nil
}

// IsReusable reports whether the entry is a valid cache-hit candidate now.
func (r RunReusability) IsReusable() bool { return r.reusable }

// Reason returns the non-reusable reason, or the empty reason when reusable.
func (r RunReusability) Reason() MismatchReason { return r.reason }
