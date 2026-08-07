package cli

import (
	"strings"
	"testing"

	gcdom "awarer/internal/domain/gc"
	"awarer/internal/output"
)

func TestRenderRetainedByReasonGroupsAndCounts(t *testing.T) {
	mustCand := func(kind gcdom.CandidateKind, reason gcdom.RetentionReason, id, path string) gcdom.GCCandidate {
		c, err := gcdom.NewCandidate(kind, reason, id, path, "retained", 0)
		if err != nil {
			t.Fatalf("NewCandidate: %v", err)
		}
		return c
	}
	cands := []gcdom.GCCandidate{
		mustCand(gcdom.KindCheckpoint, gcdom.ReasonCheckpointLatest, "id1", "p1"),
		mustCand(gcdom.KindCheckpoint, gcdom.ReasonCheckpointWithinKeepN, "id2", "p2"),
		mustCand(gcdom.KindCheckpoint, gcdom.ReasonCheckpointWithinKeepN, "id3", "p3"),
		mustCand(gcdom.KindCheckpoint, gcdom.ReasonGitUnavailable, "id4", "p4"),
		mustCand(gcdom.KindRun, gcdom.ReasonGitUnavailable, "id5", "p5"),
	}
	plan := gcdom.NewPlan(true, cands, nil)

	var buf strings.Builder
	w := output.New(&buf, &buf)
	renderRetainedByReason(w, plan)
	got := buf.String()

	if !strings.Contains(got, "retained: 5 records") {
		t.Errorf("missing retained total:\n%s", got)
	}
	// Each reason token appears with its count (spacing is column-aligned).
	for _, want := range []string{"git-unavailable:", "checkpoint-within-keep-last:", "checkpoint-latest:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing reason %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(lineWith(got, "checkpoint-latest:")), "1") {
		t.Errorf("checkpoint-latest should count 1:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(lineWith(got, "git-unavailable:")), "2") {
		t.Errorf("git-unavailable should count 2:\n%s", got)
	}
	// Ordered by descending count: a count-2 reason appears before the count-1 reason.
	if strings.Index(got, "checkpoint-latest:") < strings.Index(got, "git-unavailable:") {
		t.Errorf("expected higher-count reasons first:\n%s", got)
	}
}

// lineWith returns the first line of s containing sub, or "".
func lineWith(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

func TestRenderRetainedByReasonEmpty(t *testing.T) {
	plan := gcdom.NewPlan(true, nil, nil)
	var buf strings.Builder
	w := output.New(&buf, &buf)
	renderRetainedByReason(w, plan)
	if !strings.Contains(buf.String(), "retained: 0 records") {
		t.Errorf("empty plan should report 0 retained records:\n%s", buf.String())
	}
}
