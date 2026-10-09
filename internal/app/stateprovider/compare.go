package stateprovider

import (
	"context"
	"fmt"

	"awarer/internal/app/state"
	"awarer/internal/domain/compare"
	"awarer/internal/domain/provider"
)

// Compare produces the provider comparison for one parsed range. Each side resolves
// atomically and independently — an unresolved operand does not short-circuit the other,
// so an unavailable comparison keeps whichever side did resolve regardless of operand
// order. Two resolved operands are classified by the pure compatibility rule and, when a
// counted difference is possible, by the canonical streaming merge.
//
// A published operand identity is always manifest-verified: the merge confirms both for a
// difference, an explicit pass confirms both for equal/incomparable, and every unavailable
// path publishes only operands that independently verify readable — an unavailable
// comparison never presents an unreadable operand as a reconstructable identity. A genuine
// fault (interruption, impossible internal state) is a non-nil error; every expected
// degradation is a complete comparison document.
func (a *Assessor) Compare(ctx context.Context, rng state.Range, now state.NowContext) (provider.Comparison, error) {
	left, leftReason, fault := a.resolveCandidate(ctx, rng.Left, now)
	if fault != nil {
		return provider.Comparison{}, fault
	}
	if left != nil {
		defer func() { _ = left.rs.Close() }()
	}
	right, rightReason, fault := a.resolveCandidate(ctx, rng.Right, now)
	if fault != nil {
		return provider.Comparison{}, fault
	}
	if right != nil {
		defer func() { _ = right.rs.Close() }()
	}

	// A side that did not resolve makes the comparison unavailable. The reason is chosen
	// left-first across both operands' final outcomes (an unresolved side or a resolved
	// side whose manifest is unreadable), and only operands that verify readable are
	// published.
	if left == nil || right == nil {
		return a.unavailable(ctx, left, right, leftReason, rightReason, "")
	}

	switch verdict := provider.Classify(left.id, right.id); verdict {
	case provider.VerdictNeedsMerge:
		// The covered merge drains both manifests, so it is the readability
		// confirmation; a mid-merge failure never yields a partial count.
		sum, supported, err := a.coveredMerge(ctx, left.rs, right.rs)
		if err != nil {
			return a.mergeFailed(ctx, left, right, err)
		}
		if !supported {
			return a.settle(ctx, left, right, provider.ReasonAbsenceUnproved)
		}
		if provider.SameTree(left.id, right.id) && sum.Total() == 0 {
			return provider.Equal(left.id, right.id), nil
		}
		counts, err := countsOf(sum)
		if err != nil {
			return provider.Comparison{}, err
		}
		return provider.Different(left.id, right.id, counts), nil

	case provider.VerdictEqual:
		return a.settle(ctx, left, right, "")

	case provider.VerdictIncomparable:
		return a.settle(ctx, left, right, provider.ReasonIncompatibleIdentityPolicy)

	case provider.VerdictAbsenceUnproved:
		return a.settle(ctx, left, right, provider.ReasonAbsenceUnproved)

	default:
		return provider.Comparison{}, fmt.Errorf("state provider: unknown compatibility verdict %d", verdict)
	}
}

func (a *Assessor) mergeFailed(ctx context.Context, left, right *candidate, err error) (provider.Comparison, error) {
	reason, isAvailability := reasonFor(err)
	if !isAvailability {
		return provider.Comparison{}, err
	}
	return a.unavailable(ctx, left, right, "", "", reason)
}

// settle verifies both operands for a verdict that did not drain them, so an
// unreadable side is published as unavailable rather than as an identity.
func (a *Assessor) settle(ctx context.Context, left, right *candidate, reason provider.Reason) (provider.Comparison, error) {
	leftOut, fault := a.finalize(ctx, left, "")
	if fault != nil {
		return provider.Comparison{}, fault
	}
	rightOut, fault := a.finalize(ctx, right, "")
	if fault != nil {
		return provider.Comparison{}, fault
	}
	_, leftOK := leftOut.identity()
	_, rightOK := rightOut.identity()
	if !leftOK || !rightOK {
		return unavailableFrom(leftOut, rightOut, "")
	}
	if reason == "" {
		return provider.Equal(left.id, right.id), nil
	}
	return provider.Incomparable(left.id, right.id, reason)
}

// unavailable builds an unavailable comparison from each operand's final outcome. It
// reports the left-first unavailable operand's reason — a resolved-but-unreadable left
// beats a not-found right, matching the documented left-first policy — and publishes only
// operands that verify readable, never an unverified or unreadable identity. leftReason
// and rightReason are the operands' resolve-failure reasons (empty when they resolved);
// fallback covers a failure not tied to either operand (a merge that failed while both
// manifests still verify), so the comparison always carries a reason. It runs on failure
// paths, where an extra verification pass to find the surviving side is acceptable.
func (a *Assessor) unavailable(ctx context.Context, left, right *candidate, leftReason, rightReason, fallback provider.Reason) (provider.Comparison, error) {
	leftOut, fault := a.finalize(ctx, left, leftReason)
	if fault != nil {
		return provider.Comparison{}, fault
	}
	rightOut, fault := a.finalize(ctx, right, rightReason)
	if fault != nil {
		return provider.Comparison{}, fault
	}
	return unavailableFrom(leftOut, rightOut, fallback)
}

// unavailableFrom assembles an unavailable comparison from two already-finalized operand
// outcomes. It reports the left-first unavailable reason — a resolved-but-unreadable left
// beats a not-found right — and publishes only operands that verified readable, never an
// unreadable identity. fallback covers a failure tied to neither operand (a merge that
// failed while both manifests still verify), so the comparison always carries a reason.
func unavailableFrom(leftOut, rightOut operandOutcome, fallback provider.Reason) (provider.Comparison, error) {
	reason := fallback
	if r, ok := rightOut.unavailableReason(); ok {
		reason = r
	}
	if r, ok := leftOut.unavailableReason(); ok {
		reason = r
	}
	leftID, _ := leftOut.identity()
	rightID, _ := rightOut.identity()
	return provider.ComparisonUnavailable(leftID, rightID, reason)
}

func countsOf(sum compare.Summary) (provider.Counts, error) {
	if sum.Renamed != 0 {
		return provider.Counts{}, fmt.Errorf("state provider: %w: rename count %d with rename detection disabled", errInternalFault, sum.Renamed)
	}
	counts, err := provider.NewCounts(sum.Added, sum.Modified, sum.Deleted, sum.TypeChanged, sum.Skipped)
	if err != nil {
		return provider.Counts{}, fmt.Errorf("state provider: %w: summarizing the merge counts: %w", errInternalFault, err)
	}
	return counts, nil
}
