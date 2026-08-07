package scanner

import (
	"fmt"
	"testing"

	"awarer/internal/domain/worktree"
)

// TestRecordSkippedSampleKeepsCanonicalHead proves the bounded sample retains the
// canonically-smallest paths in sorted order regardless of insertion order, even when
// far more inputs are skipped than the buffer holds — so SkippedSummary is a
// deterministic canonical head, not a walk-order artifact.
func TestRecordSkippedSampleKeepsCanonicalHead(t *testing.T) {
	p := &processor{}
	// Insert 4x the buffer in descending path order (the worst case for a walk-order
	// prefix: the smallest paths arrive last).
	total := maxSkippedSampleBuffer * 4
	for i := total - 1; i >= 0; i-- {
		p.recordSkippedSample(worktree.SkippedSample{Path: fmt.Sprintf("p%06d", i), Reason: "read-error"})
	}

	if len(p.samples) != maxSkippedSampleBuffer {
		t.Fatalf("retained %d samples, want the cap %d", len(p.samples), maxSkippedSampleBuffer)
	}
	// The retained samples must be exactly the maxSkippedSampleBuffer smallest paths,
	// ascending.
	for i, s := range p.samples {
		want := fmt.Sprintf("p%06d", i)
		if s.Path != want {
			t.Fatalf("sample[%d] = %q, want the %d-th smallest path %q", i, s.Path, i, want)
		}
	}
}

// TestRecordSkippedSampleUnderCapSorts proves that below the cap every sample is kept,
// in canonical order.
func TestRecordSkippedSampleUnderCapSorts(t *testing.T) {
	p := &processor{}
	for _, path := range []string{"z", "m", "a", "q", "b"} {
		p.recordSkippedSample(worktree.SkippedSample{Path: path, Reason: "special-file"})
	}
	want := []string{"a", "b", "m", "q", "z"}
	if len(p.samples) != len(want) {
		t.Fatalf("retained %d, want %d", len(p.samples), len(want))
	}
	for i := range want {
		if p.samples[i].Path != want[i] {
			t.Fatalf("samples = %v, want canonical %v", p.samples, want)
		}
	}
}
