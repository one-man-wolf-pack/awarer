package provider

import "fmt"

// EvidenceBoundary carries the bounded evidence-boundary facts a resolved identity or
// a comparison exposes: how many inputs the observation skipped (unreadable or
// policy-excluded at scan time), plus the standing invariant that paths ignored by
// project policy are outside awa evidence entirely and are never counted here.
//
// It is deliberately O(1): a count, never a per-path slice, so it cannot grow with
// tree cardinality. "Skipped" and "ignored" are distinct — a skipped input was inside
// the observed scope but could not be captured; an ignored path was never in scope and
// is not awa's evidence to report.
type EvidenceBoundary struct {
	skippedInputs int
}

// NewEvidenceBoundary builds a boundary from a non-negative skipped-input count.
func NewEvidenceBoundary(skippedInputs int) (EvidenceBoundary, error) {
	if skippedInputs < 0 {
		return EvidenceBoundary{}, fmt.Errorf("evidence boundary has negative skipped-input count %d", skippedInputs)
	}
	return EvidenceBoundary{skippedInputs: skippedInputs}, nil
}

// SkippedInputs is the number of inputs the observation skipped.
func (b EvidenceBoundary) SkippedInputs() int { return b.skippedInputs }

// IgnoredOutsideEvidence is the standing invariant that project-ignored paths are
// outside awa evidence and are never part of any count this boundary reports. It is
// always true; it exists so a machine surface can state the invariant explicitly
// rather than leaving the consumer to assume it.
func (b EvidenceBoundary) IgnoredOutsideEvidence() bool { return true }
