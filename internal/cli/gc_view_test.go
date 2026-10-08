package cli

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	gcdom "awarer/internal/domain/gc"
	"awarer/internal/output"
)

func TestRenderRetainedByReasonGroupsAndCounts(t *testing.T) {
	mustCand := func(kind gcdom.CandidateKind, reason gcdom.RetentionReason, id, path string) gcdom.GCCandidate {
		c, err := gcdom.NewCandidate(kind, reason, id, path, "retained", gcdom.UnmeasuredBytes())
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

func mustGCCandidate(t *testing.T, kind gcdom.CandidateKind, reason gcdom.RetentionReason, id string, size gcdom.ByteMeasure) gcdom.GCCandidate {
	t.Helper()
	c, err := gcdom.NewCandidate(kind, reason, id, ".awa/"+id, "msg", size)
	if err != nil {
		t.Fatalf("NewCandidate: %v", err)
	}
	return c
}

// TestGCJSONByteCompletenessSurvivesDetailTruncation: the one partially measured
// selected candidate sits past the detail cap, behind a complete blob footprint, yet
// the summary still reports bytes_planned as a lower bound.
func TestGCJSONByteCompletenessSurvivesDetailTruncation(t *testing.T) {
	cands := make([]gcdom.GCCandidate, 0, defaultGCCandidatesJSONLimit+1)
	for i := 0; i < defaultGCCandidatesJSONLimit; i++ {
		cands = append(cands, mustGCCandidate(t, gcdom.KindBlob, gcdom.ReasonBlobReferenced, "b"+strconv.Itoa(i), gcdom.ExactBytes(1)))
	}
	cands = append(cands, mustGCCandidate(t, gcdom.KindRun, gcdom.ReasonRunExpired, "r-last", gcdom.PartialBytes(70)))
	plan := gcdom.NewPlan(true, cands, nil).WithBlobFootprint(gcdom.BlobFootprint{Count: 1000, Bytes: 1000, Complete: true})
	v := gcViewOf(gcdom.NewResult(plan, nil, nil))

	if v.CandidatesBounded.Complete || len(v.Candidates) != defaultGCCandidatesJSONLimit {
		t.Fatalf("fixture must truncate the detail list: %+v", v.CandidatesBounded)
	}
	if v.Summary.BytesPlanned != 70 || v.Summary.BytesPlannedComplete {
		t.Fatalf("summary = %+v, want bytes_planned 70 as a lower bound", v.Summary)
	}
	if !v.StoreFootprint.Complete {
		t.Fatal("blob footprint completeness is independent and stays complete")
	}
	if v.Summary.BytesDeleted != 0 || !v.Summary.BytesDeletedComplete {
		t.Fatalf("a dry run claims no deleted bytes: %+v", v.Summary)
	}
	if got := v.Candidates[0].BytesMeasurement; got != "exact" {
		t.Fatalf("candidate measurement = %q, want exact", got)
	}
}

func TestGCJSONCandidateMeasurementTokens(t *testing.T) {
	plan := gcdom.NewPlan(true, []gcdom.GCCandidate{
		mustGCCandidate(t, gcdom.KindCheckpoint, gcdom.ReasonCheckpointLatest, "c1", gcdom.UnmeasuredBytes()),
		mustGCCandidate(t, gcdom.KindCheckpoint, gcdom.ReasonCheckpointExpired, "c2", gcdom.ExactBytes(0)),
		mustGCCandidate(t, gcdom.KindRun, gcdom.ReasonRunExpired, "r1", gcdom.PartialBytes(3)),
	}, nil)
	v := gcViewOf(gcdom.NewResult(plan, nil, nil))
	var got []string
	for _, c := range v.Candidates {
		got = append(got, c.BytesMeasurement)
	}
	if strings.Join(got, ",") != "unmeasured,exact,partial" {
		t.Fatalf("measurement tokens = %v", got)
	}
}

func renderGCString(res gcdom.GCResult) (stdout, stderr string) {
	var out, errb strings.Builder
	renderGC(output.New(&out, &errb), res)
	return out.String(), errb.String()
}

// TestGCHumanBytesNameLogicalDataAndLowerBounds drives the real projection: an exact
// total is plain logical bytes; an incomplete one says "at least" with one bounded
// warning on stderr; a failed deletion stays visible on its own line.
func TestGCHumanBytesNameLogicalDataAndLowerBounds(t *testing.T) {
	exact := mustGCCandidate(t, gcdom.KindCheckpoint, gcdom.ReasonCheckpointExpired, "c1", gcdom.ExactBytes(2048))
	partial := mustGCCandidate(t, gcdom.KindRun, gcdom.ReasonRunExpired, "r1", gcdom.PartialBytes(10))
	failing := mustGCCandidate(t, gcdom.KindRun, gcdom.ReasonRunExpired, "r2", gcdom.ExactBytes(5000))

	out, errOut := renderGCString(gcdom.NewResult(gcdom.NewPlan(true, []gcdom.GCCandidate{exact}, nil), nil, nil))
	if !strings.Contains(out, "planned bytes: 2.0 KiB of logical file data\n") || strings.Contains(out+errOut, "lower bound") {
		t.Fatalf("exact dry run:\nstdout=%s\nstderr=%s", out, errOut)
	}

	out, errOut = renderGCString(gcdom.NewResult(gcdom.NewPlan(true, []gcdom.GCCandidate{exact, partial}, nil), nil, nil))
	if !strings.Contains(out, "planned bytes: at least 2.0 KiB of logical file data") {
		t.Fatalf("incomplete dry run must qualify the amount:\n%s", out)
	}
	if !strings.Contains(errOut, "warning: byte total is a lower bound: 1 selected item could not be fully measured") || strings.Contains(out, "warning:") {
		t.Fatalf("incomplete dry run warning belongs on stderr:\nstdout=%s\nstderr=%s", out, errOut)
	}

	plan := gcdom.NewPlan(false, []gcdom.GCCandidate{exact, partial, failing}, nil)
	res := gcdom.NewResult(plan, []gcdom.DeletionResult{
		gcdom.NewDeletionResult(exact, nil),
		gcdom.NewDeletionResult(partial, nil),
		gcdom.NewDeletionResult(failing, errors.New("sync failed")),
	}, nil)
	out, errOut = renderGCString(res)
	if !strings.Contains(out, "removed bytes: at least 2.0 KiB of logical file data") || !strings.Contains(out, "failed: 1 deletions could not be completed") {
		t.Fatalf("partial execution:\n%s", out)
	}
	if !strings.Contains(errOut, "warning: removed byte total is a lower bound: 1 failed deletion credited 0, 1 removed item not fully measured") {
		t.Fatalf("partial execution warning:\n%s", errOut)
	}
	if strings.Contains(out+errOut, "free") {
		t.Fatalf("output must not promise freed space:\n%s%s", out, errOut)
	}
}
