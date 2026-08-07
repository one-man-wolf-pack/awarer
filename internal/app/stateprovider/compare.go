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
		// The merge streams both manifests in full, so it is itself the readability
		// confirmation for a difference — no separate verify pass. A mid-merge failure
		// never yields a partial count; it becomes unavailable, publishing only the side
		// that still verifies readable.
		counts, err := a.mergeCounts(ctx, left.rs, right.rs)
		if err != nil {
			reason, isAvailability := reasonFor(err)
			if !isAvailability {
				return provider.Comparison{}, err
			}
			// Both operands resolved, so their unavailability (if any) is a manifest
			// read failure; re-verify each to publish the readable side and pick the
			// left-first reason, falling back to the merge's reason.
			return a.unavailable(ctx, left, right, "", "", reason)
		}
		return provider.Different(left.id, right.id, counts), nil

	case provider.VerdictEqual, provider.VerdictIncomparable:
		// equal (hash fast path) and incomparable read neither manifest, so finalize both
		// here — the single verification pass for this verdict. If a side is unreadable the
		// comparison is unavailable, publishing only the readable side(s).
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
		if verdict == provider.VerdictEqual {
			return provider.Equal(left.id, right.id), nil
		}
		return provider.Incomparable(left.id, right.id, provider.ReasonIncompatibleIdentityPolicy)

	default:
		return provider.Comparison{}, fmt.Errorf("state provider: unknown compatibility verdict %d", verdict)
	}
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

// mergeCounts drains the canonical streaming merge into bounded per-kind counts. It
// keeps only a fixed-size Summary — never a changed-path slice — so retained memory is
// O(1) with respect to tree cardinality. Rename detection is disabled, so a rename
// appears as a separate delete and add (rename pairing is outside this summary surface);
// the returned counts must therefore carry no renamed entries.
//
// Its three self-raised failures — a merge it cannot construct over two manifests it just
// opened, a renamed count under disabled pairing, and a monotonic non-negative summary the
// counts constructor rejects — are impossible internal states, not evidence degradation, so
// they carry errInternalFault and propagate as faults. Manifest open/iteration failures and
// interruption are left unmarked for the availability classifier.
func (a *Assessor) mergeCounts(ctx context.Context, left, right *state.ResolvedState) (provider.Counts, error) {
	lc, err := left.Manifest(ctx)
	if err != nil {
		return provider.Counts{}, err
	}
	rc, err := right.Manifest(ctx)
	if err != nil {
		_ = lc.Close()
		return provider.Counts{}, err
	}
	// CompareStream takes ownership of lc and rc: its cursor's Close closes them, so we
	// must not close them separately once it is constructed.
	cur, err := compare.CompareStream(lc, rc, compare.Options{DetectRenames: false})
	if err != nil {
		_ = lc.Close()
		_ = rc.Close()
		return provider.Counts{}, fmt.Errorf("state provider: %w: constructing the non-rename merge over two opened manifests: %w", errInternalFault, err)
	}
	defer func() { _ = cur.Close() }()

	var sum compare.Summary
	for cur.Next() {
		if cerr := ctx.Err(); cerr != nil {
			return provider.Counts{}, cerr
		}
		sum.Add(cur.Change())
	}
	if cerr := cur.Err(); cerr != nil {
		return provider.Counts{}, cerr
	}
	if sum.Renamed != 0 {
		return provider.Counts{}, fmt.Errorf("state provider: %w: rename count %d with rename detection disabled", errInternalFault, sum.Renamed)
	}
	counts, err := provider.NewCounts(sum.Added, sum.Modified, sum.Deleted, sum.TypeChanged)
	if err != nil {
		return provider.Counts{}, fmt.Errorf("state provider: %w: summarizing the merge counts: %w", errInternalFault, err)
	}
	return counts, nil
}
