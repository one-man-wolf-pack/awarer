package runcache

import (
	"fmt"

	"awarer/internal/domain/hashing"
)

// EffectHash is the identity of a run's project-local effect-observation state:
// the digest of a bounded metadata signature over the configured watched
// generated-output roots (build, dist, target, node_modules, ...). It is a
// distinct type from the input TreeHash so an input-tree identity can never be
// assigned where an effect identity belongs, even though both share the validated
// "blake3:hex" persisted form. The zero value is invalid; IsZero reports it.
//
// The signature is deliberately stat-only (existence + size/mtime/mode), never a
// content hash: a watched root such as node_modules is far too large to content
// hash, and the correctness goal is to notice that generated output was deleted or
// grossly changed, not to prove every byte inside it.
type EffectHash struct {
	th hashing.TreeHash
}

// EffectHashFromTree adapts a digest produced by Hasher.HashBytes (a TreeHash, the
// port's general byte-hashing result) into an EffectHash. Both share the validated
// "blake3:hex" representation; this keeps the effect signature a first-class
// value without adding another hashing method to the port.
func EffectHashFromTree(th hashing.TreeHash) EffectHash { return EffectHash{th: th} }

// ParseEffectHash parses a persisted "blake3:hex" effect hash.
func ParseEffectHash(s string) (EffectHash, error) {
	th, err := hashing.ParseTreeHash(s)
	if err != nil {
		return EffectHash{}, fmt.Errorf("invalid effect hash: %w", err)
	}
	return EffectHash{th: th}, nil
}

// IsZero reports whether h is unset — no effect signature computed.
func (h EffectHash) IsZero() bool { return h.th.IsZero() }

// String renders the canonical "blake3:hex" form; the zero value renders as the
// empty string.
func (h EffectHash) String() string { return h.th.String() }

// EffectStatus is the closed classification of a run's effect observation. It is
// part of the key, so a run whose effect could not be observed keys differently
// from one whose watched roots were observed clean.
type EffectStatus int

// The watch set is non-empty product policy, so every execution observes it and
// lands on exactly one of these two states. The zero value is not one of them: it
// is the invalid "never observed" gap a forgotten initialization would leave, and
// Valid rejects it.
const (
	// EffectObserved means the watched roots were observed and reduced to a bounded
	// signature. It is safe for reuse; the signature is folded into the key so a
	// later change to the watched state produces a different key and a clean miss.
	EffectObserved EffectStatus = iota + 1
	// EffectUnavailable means at least one watched root could not be observed safely
	// (an unreadable directory, an ambiguous symlink). The effect state is unknown,
	// so the run fails closed: it executes and records but is never reusable.
	EffectUnavailable
)

// Valid reports whether s is a known effect status.
func (s EffectStatus) Valid() bool {
	return s == EffectObserved || s == EffectUnavailable
}

// String returns the stable persisted token.
func (s EffectStatus) String() string {
	switch s {
	case EffectObserved:
		return "observed"
	case EffectUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// ParseEffectStatus resolves a persisted effect-status token.
func ParseEffectStatus(s string) (EffectStatus, error) {
	switch s {
	case "observed":
		return EffectObserved, nil
	case "unavailable":
		return EffectUnavailable, nil
	default:
		return 0, fmt.Errorf("invalid effect status %q", s)
	}
}

// EffectObservation is the typed effect-observation identity folded into a run
// key: the observation status, the bounded signature (set only when observed), and
// the number of watched roots the observation covered. It is a distinct concept
// from the input tree hash — input identity answers "would the command receive the
// same observed inputs?", while this answers "is the project-local generated state
// we promised to watch still in the same condition?". Construct it only through the
// constructors below so an impossible pairing (observed without a signature,
// unavailable with one) is unrepresentable; the zero value is invalid.
type EffectObservation struct {
	status    EffectStatus
	signature EffectHash
	rootCount int
}

// ObservedEffect builds an observed effect identity from a non-zero signature and
// the count of watched roots it covered. It is safe for reuse.
func ObservedEffect(sig EffectHash, rootCount int) (EffectObservation, error) {
	if sig.IsZero() {
		return EffectObservation{}, fmt.Errorf("observed effect must carry a signature")
	}
	if rootCount < 1 {
		return EffectObservation{}, fmt.Errorf("observed effect must cover at least one watched root, got %d", rootCount)
	}
	return EffectObservation{status: EffectObserved, signature: sig, rootCount: rootCount}, nil
}

// UnavailableEffect builds the unavailable effect identity: watched roots existed
// but could not be observed safely, so the state is unknown. It carries no
// signature and is never safe for reuse. rootCount records how many roots were
// configured for diagnostics.
func UnavailableEffect(rootCount int) (EffectObservation, error) {
	if rootCount < 1 {
		return EffectObservation{}, fmt.Errorf("unavailable effect must have at least one watched root, got %d", rootCount)
	}
	return EffectObservation{status: EffectUnavailable, rootCount: rootCount}, nil
}

// NewEffectObservation rebuilds an effect observation from persisted fields by
// delegating to the same constructors the write path uses, so a hand-edited record
// cannot decode into a status/signature pairing the constructors would reject.
func NewEffectObservation(status EffectStatus, sig EffectHash, rootCount int) (EffectObservation, error) {
	switch status {
	case EffectObserved:
		return ObservedEffect(sig, rootCount)
	case EffectUnavailable:
		if !sig.IsZero() {
			return EffectObservation{}, fmt.Errorf("unavailable effect carries a signature %q", sig)
		}
		return UnavailableEffect(rootCount)
	default:
		return EffectObservation{}, fmt.Errorf("invalid effect status %d", status)
	}
}

// Status returns the observation status.
func (o EffectObservation) Status() EffectStatus { return o.status }

// Signature returns the bounded effect signature, or the zero hash when the effect
// was unavailable.
func (o EffectObservation) Signature() EffectHash { return o.signature }

// RootCount returns the number of watched roots the observation covered.
func (o EffectObservation) RootCount() int { return o.rootCount }

// SafeForReuse reports whether this effect observation permits a reusable entry.
// Only an observed state qualifies — its signature is folded into the key, so a
// later change to the watched state misses. Unavailable state is never safe, and
// neither is the invalid zero value.
func (o EffectObservation) SafeForReuse() bool {
	return o.status == EffectObserved
}

// Equal reports whether two effect observations are identical, used by the key
// comparator to detect a change in watched generated-output state.
func (o EffectObservation) Equal(other EffectObservation) bool {
	return o.status == other.status &&
		o.signature.String() == other.signature.String() &&
		o.rootCount == other.rootCount
}

// Validate checks that an effect observation is internally consistent: a known
// status, a signature present exactly when observed, and a root count that matches
// the status.
func (o EffectObservation) Validate() error {
	if !o.status.Valid() {
		return fmt.Errorf("effect observation has invalid status %v", o.status)
	}
	switch o.status {
	case EffectObserved:
		if o.signature.IsZero() {
			return fmt.Errorf("observed effect observation has no signature")
		}
		if o.rootCount < 1 {
			return fmt.Errorf("observed effect observation covers %d roots, want at least 1", o.rootCount)
		}
	case EffectUnavailable:
		if !o.signature.IsZero() {
			return fmt.Errorf("unavailable effect observation carries a signature")
		}
		if o.rootCount < 1 {
			return fmt.Errorf("unavailable effect observation covers %d roots, want at least 1", o.rootCount)
		}
	}
	return nil
}

// effectDisplay renders an effect observation as a non-secret difference value for
// explain and near-miss output: watched-root paths and a signature digest, never
// user data, so it needs no redaction.
func (o EffectObservation) effectDisplay() string {
	switch o.status {
	case EffectObserved:
		return fmt.Sprintf("observed (%d roots, %s)", o.rootCount, o.signature)
	case EffectUnavailable:
		return fmt.Sprintf("unavailable (%d roots)", o.rootCount)
	default:
		return "unknown"
	}
}

// EffectOutcome is the result of the post-run effect guard's pre/post comparison of
// the watched generated-output roots. It mirrors MutationOutcome for the input
// scope: it says whether the command changed the watched state, left it unchanged,
// or could not observe it.
type EffectOutcome int

const (
	// EffectGuardUnset is the invalid zero value: the effect guard did not run. Like
	// MutationUnobserved it belongs only to a replayed hit's transient result, never
	// to durable history, and a persisted entry carrying it is corrupt.
	EffectGuardUnset EffectOutcome = iota
	// EffectGuardUnchanged means the post-run watched state equals the pre-run state.
	// Safe for reuse.
	EffectGuardUnchanged
	// EffectGuardChanged means the command changed the watched generated-output state
	// between the pre- and post-run observations, so its result cannot be replayed.
	EffectGuardChanged
	// EffectGuardUnavailable means the watched state could not be observed safely, so
	// it is unknown and must fail closed.
	EffectGuardUnavailable
)

func (o EffectOutcome) valid() bool {
	return o >= EffectGuardUnchanged && o <= EffectGuardUnavailable
}

// String returns the stable persisted token for the outcome.
func (o EffectOutcome) String() string {
	switch o {
	case EffectGuardUnset:
		return "unset"
	case EffectGuardUnchanged:
		return "unchanged"
	case EffectGuardChanged:
		return "changed"
	case EffectGuardUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// ParseEffectOutcome resolves a persisted outcome token. It rejects "unset": a
// persisted entry must have run the guard, so the invalid zero value can never be
// decoded back in.
func ParseEffectOutcome(s string) (EffectOutcome, error) {
	switch s {
	case "unchanged":
		return EffectGuardUnchanged, nil
	case "changed":
		return EffectGuardChanged, nil
	case "unavailable":
		return EffectGuardUnavailable, nil
	default:
		return 0, fmt.Errorf("invalid effect guard outcome %q", s)
	}
}

// EffectGuardStatus is the typed outcome of the effect guard for one execution. It
// is a value object so callers cannot fabricate a "changed but cacheable" state:
// only an unchanged outcome permits caching, and the non-cacheable reason is derived
// from the outcome rather than passed in.
type EffectGuardStatus struct {
	outcome EffectOutcome
}

// NewEffectGuardStatus builds a status from an outcome, rejecting an unknown value
// so a caller bug surfaces here rather than as a silently un-cacheable run.
func NewEffectGuardStatus(o EffectOutcome) (EffectGuardStatus, error) {
	if !o.valid() {
		return EffectGuardStatus{}, fmt.Errorf("invalid effect guard outcome %d", o)
	}
	return EffectGuardStatus{outcome: o}, nil
}

// Outcome returns the underlying outcome.
func (g EffectGuardStatus) Outcome() EffectOutcome { return g.outcome }

// Changed reports whether the command changed the watched generated-output state.
func (g EffectGuardStatus) Changed() bool { return g.outcome == EffectGuardChanged }

// Unavailable reports whether the watched state could not be observed.
func (g EffectGuardStatus) Unavailable() bool { return g.outcome == EffectGuardUnavailable }

// CacheableUnderEffect reports whether the effect guard permits publishing a
// reusable entry. Only an explicitly unchanged observation qualifies; a changed or
// unavailable state must never be published.
func (g EffectGuardStatus) CacheableUnderEffect() bool {
	return g.outcome == EffectGuardUnchanged
}

// Reason returns the non-cacheable reason the guard imposes, or the empty reason
// when the guard does not block caching.
func (g EffectGuardStatus) Reason() MismatchReason {
	switch g.outcome {
	case EffectGuardChanged:
		return ReasonEffectStateDiffers
	case EffectGuardUnavailable:
		return ReasonEffectStateUnavailable
	default:
		return ""
	}
}
