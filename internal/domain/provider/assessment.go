package provider

import "fmt"

// Outcome is the closed result of a single `state resolve`. It determines whether an
// identity or a reason is present, so a resolved-without-identity or an
// unavailable-without-reason assessment has no code path.
type Outcome int

const (
	// OutcomeResolved means the reference resolved to a concrete state identity.
	OutcomeResolved Outcome = iota + 1
	// OutcomeUnavailable means the reference could not be resolved to a state; the
	// assessment carries a stable reason. It is a complete, successful assessment, not
	// a failure — the provider is optional.
	OutcomeUnavailable
)

// String returns the stable machine token for the outcome.
func (o Outcome) String() string {
	switch o {
	case OutcomeResolved:
		return "resolved"
	case OutcomeUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// Assessment is the resolved verdict for one input reference. It pairs the exact
// actor-supplied reference with either a resolved identity or a stable unavailable
// reason — never both, never neither — so a rendered document can never claim
// "resolved" without an identity or "unavailable" without a reason.
type Assessment struct {
	inputRef string
	outcome  Outcome
	identity *Identity
	reason   Reason
}

// Resolved builds a resolved assessment. It requires the exact input reference and a
// concrete identity; a reason is forbidden.
func Resolved(inputRef string, identity Identity) (Assessment, error) {
	if inputRef == "" {
		return Assessment{}, fmt.Errorf("resolved assessment requires the input reference")
	}
	if !identity.kind.Valid() {
		return Assessment{}, fmt.Errorf("resolved assessment requires a valid identity kind")
	}
	id := identity
	return Assessment{inputRef: inputRef, outcome: OutcomeResolved, identity: &id}, nil
}

// Unavailable builds an unavailable assessment. It requires the exact input reference
// and a valid reason; an identity is forbidden.
func Unavailable(inputRef string, reason Reason) (Assessment, error) {
	if inputRef == "" {
		return Assessment{}, fmt.Errorf("unavailable assessment requires the input reference")
	}
	if !reason.Valid() {
		return Assessment{}, fmt.Errorf("unavailable assessment requires a valid reason, got %q", reason)
	}
	return Assessment{inputRef: inputRef, outcome: OutcomeUnavailable, reason: reason}, nil
}

// InputRef returns the exact actor-supplied reference.
func (a Assessment) InputRef() string { return a.inputRef }

// Outcome returns the resolve outcome.
func (a Assessment) Outcome() Outcome { return a.outcome }

// Identity returns the resolved identity and true, or the zero identity and false when
// the outcome is unavailable.
func (a Assessment) Identity() (Identity, bool) {
	if a.outcome != OutcomeResolved || a.identity == nil {
		return Identity{}, false
	}
	return *a.identity, true
}

// Reason returns the unavailable reason and true, or the empty reason and false when
// the outcome is resolved.
func (a Assessment) Reason() (Reason, bool) {
	if a.outcome != OutcomeUnavailable {
		return "", false
	}
	return a.reason, true
}
