package provider

import "fmt"

// Relationship is the closed outcome of a `state compare`. It determines which of
// operands, counts, and reason are present, and whether the comparison is complete, so a
// rendered document can never carry counts without a complete comparison, an
// equal/different verdict without both operands, or an incomparable/unavailable verdict
// without a reason.
type Relationship int

const (
	// RelEqual means both operands resolved and are the same state.
	RelEqual Relationship = iota + 1
	// RelDifferent means both operands resolved, are provably compatible, and differ;
	// the comparison carries complete path-keyed counts.
	RelDifferent
	// RelIncomparable means both operands resolved but their scope/observation policy
	// compatibility is not proven, so no honest counts exist.
	RelIncomparable
	// RelUnavailable means at least one operand could not be resolved.
	RelUnavailable
)

// String returns the stable machine token for the relationship.
func (r Relationship) String() string {
	switch r {
	case RelEqual:
		return "equal"
	case RelDifferent:
		return "different"
	case RelIncomparable:
		return "incomparable"
	case RelUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// Counts are the path-keyed per-kind change counts of a complete comparison. They are
// summary-only: there is deliberately no changed-path list, no rename pairing, and no
// per-path slice, so retained memory never grows with tree cardinality.
//
// skipped counts a both-present path whose comparison status is skipped: a readable entry
// that became a skipped input, a skipped input that became readable, and two skips that
// differ. It is a fact about the delta between two observations, and is therefore distinct
// from either operand's EvidenceBoundary.SkippedInputs, which describes what one
// observation skipped. A skipped record present on only one side stays an add or a delete,
// exactly as the comparator classifies it.
type Counts struct {
	added       int
	modified    int
	deleted     int
	typeChanged int
	skipped     int
}

// NewCounts builds counts from non-negative per-kind totals.
func NewCounts(added, modified, deleted, typeChanged, skipped int) (Counts, error) {
	if added < 0 || modified < 0 || deleted < 0 || typeChanged < 0 || skipped < 0 {
		return Counts{}, fmt.Errorf("comparison counts must be non-negative, got a=%d m=%d d=%d t=%d s=%d", added, modified, deleted, typeChanged, skipped)
	}
	return Counts{added: added, modified: modified, deleted: deleted, typeChanged: typeChanged, skipped: skipped}, nil
}

// Added returns the number of added paths.
func (c Counts) Added() int { return c.added }

// Modified returns the number of modified paths.
func (c Counts) Modified() int { return c.modified }

// Deleted returns the number of deleted paths.
func (c Counts) Deleted() int { return c.deleted }

// TypeChanged returns the number of type-changed paths.
func (c Counts) TypeChanged() int { return c.typeChanged }

// Skipped returns the number of changed paths whose comparison status is skipped.
func (c Counts) Skipped() int { return c.skipped }

// Comparison is the resolved verdict for a `state compare` range. It is built only
// through the relationship-specific constructors, which enforce the operand, count, and
// reason shape of each relationship. Completeness is not a stored field: it is a property
// of the relationship (equal and different are complete observations; incomparable and
// unavailable are not), derived by Complete, so it can never disagree with the outcome.
type Comparison struct {
	left, right *Identity
	rel         Relationship
	counts      Counts
	reason      Reason
}

// Equal builds an equal comparison. Both operands are required (they are passed by
// value) and no reason is needed, so construction cannot fail: it returns a Comparison
// directly rather than forcing callers to handle an error that never occurs.
func Equal(left, right Identity) Comparison {
	l, r := left, right
	return Comparison{left: &l, right: &r, rel: RelEqual}
}

// Different builds a different comparison from its per-kind counts. Both operands are
// required (passed by value) and a difference is by definition a complete observation
// of both states — a partial observation resolves to incomparable or unavailable and
// never reaches here — so construction cannot fail.
func Different(left, right Identity, counts Counts) Comparison {
	l, r := left, right
	return Comparison{left: &l, right: &r, rel: RelDifferent, counts: counts}
}

// Incomparable builds an incomparable comparison. Both operands resolved (they are the
// two states that could not be honestly compared) and a valid reason is required.
func Incomparable(left, right Identity, reason Reason) (Comparison, error) {
	if !reason.Valid() {
		return Comparison{}, fmt.Errorf("incomparable comparison requires a valid reason, got %q", reason)
	}
	l, r := left, right
	return Comparison{left: &l, right: &r, rel: RelIncomparable, reason: reason}, nil
}

// ComparisonUnavailable builds an unavailable comparison for a range where at least one
// operand could not be resolved. A valid reason is required; each operand is optional
// (the failing side has no identity). It reuses the resolve reason catalog.
func ComparisonUnavailable(left, right *Identity, reason Reason) (Comparison, error) {
	if !reason.Valid() {
		return Comparison{}, fmt.Errorf("unavailable comparison requires a valid reason, got %q", reason)
	}
	return Comparison{left: left, right: right, rel: RelUnavailable, reason: reason}, nil
}

// Relationship returns the comparison relationship.
func (c Comparison) Relationship() Relationship { return c.rel }

// Left returns the left operand identity and whether it resolved.
func (c Comparison) Left() (Identity, bool) {
	if c.left == nil {
		return Identity{}, false
	}
	return *c.left, true
}

// Right returns the right operand identity and whether it resolved.
func (c Comparison) Right() (Identity, bool) {
	if c.right == nil {
		return Identity{}, false
	}
	return *c.right, true
}

// Counts returns the per-kind counts and true when the comparison is a difference, or
// the zero counts and false otherwise.
func (c Comparison) Counts() (Counts, bool) {
	if c.rel != RelDifferent {
		return Counts{}, false
	}
	return c.counts, true
}

// Complete reports whether the comparison is a complete observation of both states.
// It is derived from the relationship: equal and different are complete; incomparable
// and unavailable are not, because no full observation was made. A consumer surfaces the
// completeness fact only when this is true.
func (c Comparison) Complete() bool {
	return c.rel == RelEqual || c.rel == RelDifferent
}

// Reason returns the incomparable/unavailable reason and true, or the empty reason and
// false for equal/different.
func (c Comparison) Reason() (Reason, bool) {
	if c.rel != RelIncomparable && c.rel != RelUnavailable {
		return "", false
	}
	return c.reason, true
}

// CompatibilityVerdict is the pure, pre-merge classification of an operand pair: it
// says how two resolved identities relate before any manifest is read, so the
// equal/needs-merge/incomparable decision is testable without I/O. Only VerdictNeedsMerge
// requires draining the streaming merge to obtain counts.
type CompatibilityVerdict int

const (
	// VerdictEqual: the pair is provably equal without a merge — the same immutable
	// record, or a proven-compatible pair with identical tree hashes.
	VerdictEqual CompatibilityVerdict = iota + 1
	// VerdictNeedsMerge: the pair is provably compatible and its tree hashes differ, so
	// the per-kind counts must come from the canonical streaming merge.
	VerdictNeedsMerge
	// VerdictIncomparable: scope/observation policy compatibility is not proven, so no
	// honest comparison exists.
	VerdictIncomparable
)

// Classify decides how two resolved identities relate before any manifest is read.
//
// The rule is deliberately strict: equal tree-hash bytes alone never prove equality
// across unproven observation policies. A pair is equal only when it is the same
// immutable record, or when its policy compatibility is proven (both scan identities
// valid, same scan-config) and the tree hashes match. Anything else with proven
// compatibility and differing hashes needs the merge; everything else is
// incomparable.
func Classify(left, right Identity) CompatibilityVerdict {
	if left.sameImmutableRecord(right) {
		return VerdictEqual
	}
	if !provablyCompatible(left, right) {
		return VerdictIncomparable
	}
	if left.treeHash == right.treeHash {
		return VerdictEqual
	}
	return VerdictNeedsMerge
}

// provablyCompatible reports whether two identities share a proven scope/observation
// policy: both scan identities valid (non-zero-value) and the same scan-config hash.
// Without both, a counted comparison would invent a relationship the evidence does not
// support.
func provablyCompatible(left, right Identity) bool {
	ls, rs := left.scan, right.scan
	if !ls.Valid() || !rs.Valid() {
		return false
	}
	return ls.scanConfig == rs.scanConfig
}
