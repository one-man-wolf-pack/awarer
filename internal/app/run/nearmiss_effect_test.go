package run_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

// This file proves the contract for the shared near-miss effect detail: a candidate
// carries the existing application EffectDiagnosis for exactly the two effect-state
// reasons, its root is whatever the one observation pass already named (and nothing when
// it named none), and enriching candidates costs no extra observation.

// countingEffectObserver counts how many times the watched generated-output roots are
// actually walked, so a test can prove a surface projects one observation rather than
// quietly taking a second.
type countingEffectObserver struct {
	inner apprun.EffectObserver
	calls int
}

func (o *countingEffectObserver) Observe(project string, roots []string) (runcache.EffectObservation, runcache.EffectReport, error) {
	o.calls++
	return o.inner.Observe(project, roots)
}

// effectFixture is the shared setup for these scenarios: a stable source input, a
// watched generated-output root holding one file, and one published reusable run. The
// caller then disturbs the watched root and asks a near-miss surface why the run no
// longer replays.
type effectFixture struct {
	h     *harness
	runID runcache.RunID
}

func newEffectFixture(t *testing.T, opts ...harnessOpt) *effectFixture {
	t.Helper()
	h := newHarness(t, opts...)
	t.Cleanup(h.cleanup)
	h.write(t, "data.txt", "v1")
	writeTarget(t, h, "app", "built")

	res, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !res.Written {
		t.Fatalf("first run should publish a reusable entry, got reuse=%s", res.Reuse)
	}
	return &effectFixture{h: h, runID: res.RunID}
}

// writeTarget writes a file into the watched "target" root, which config.Defaults()
// both excludes from the input scan and watches as generated output — so touching it
// changes effect state without changing the input tree.
func writeTarget(t *testing.T, h *harness, name, content string) {
	t.Helper()
	dir := filepath.Join(h.rootAbs, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nearMissFor returns the near-miss candidate for one stored run from a Ls(Near) pass.
func (f *effectFixture) nearMissFor(t *testing.T, id runcache.RunID) apprun.RunReuseCandidate {
	t.Helper()
	ls, err := f.h.svc.Ls(context.Background(), apprun.LsRequest{Project: f.h.proj, Config: f.h.cfg, Near: true})
	if err != nil {
		t.Fatalf("Ls(Near): %v", err)
	}
	for _, c := range ls.NearMisses {
		if c.Entry.ID == id {
			return c
		}
	}
	t.Fatalf("run %s is not among the %d near misses", id.Short(), len(ls.NearMisses))
	return apprun.RunReuseCandidate{}
}

// TestNearMissEffectDiffersNamesObservedRoot proves the primary case: after the watched
// generated output changes, the candidate's reason is effect-state-differs and it
// carries the diagnosis naming the root the same observation already identified.
func TestNearMissEffectDiffersNamesObservedRoot(t *testing.T) {
	f := newEffectFixture(t)
	// Change the watched generated output only; the input tree is untouched.
	writeTarget(t, f.h, "app", "built-again-and-longer")

	c := f.nearMissFor(t, f.runID)
	if c.Reason != runcache.ReasonEffectStateDiffers {
		t.Fatalf("reason = %q, want effect-state-differs", c.Reason)
	}
	if c.Effect == nil {
		t.Fatal("effect-state-differs candidate carries no effect diagnosis")
	}
	if c.Effect.Reason != runcache.ReasonEffectStateDiffers {
		t.Errorf("diagnosis reason = %q, want the candidate's own reason", c.Effect.Reason)
	}
	if c.Effect.Root != "target" {
		t.Errorf("diagnosis root = %q, want the observed dominant root %q", c.Effect.Root, "target")
	}
}

// TestNearMissEffectDiffersOmitsUnobservedRoot proves the honesty rule: when the watched
// root is gone the observation names no root, so the diagnosis omits it rather than
// naming the path convention it was deleted from. The reason survives.
func TestNearMissEffectDiffersOmitsUnobservedRoot(t *testing.T) {
	f := newEffectFixture(t)
	// Remove the watched generated output entirely: the observation discovers no root,
	// so its report names none.
	if err := os.RemoveAll(filepath.Join(f.h.rootAbs, "target")); err != nil {
		t.Fatal(err)
	}

	c := f.nearMissFor(t, f.runID)
	if c.Reason != runcache.ReasonEffectStateDiffers {
		t.Fatalf("reason = %q, want effect-state-differs", c.Reason)
	}
	if c.Effect == nil {
		t.Fatal("effect-state-differs candidate carries no effect diagnosis")
	}
	if c.Effect.Root != "" {
		t.Errorf("diagnosis root = %q, want it omitted: the observation named no root", c.Effect.Root)
	}
}

// TestNearMissEffectUnavailableCarriesDiagnosis proves the second effect reason is
// covered: a watched root that cannot be observed safely yields
// effect-state-unavailable with the same diagnosis shape.
func TestNearMissEffectUnavailableCarriesDiagnosis(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory, so an unreadable root cannot be simulated")
	}
	f := newEffectFixture(t)
	dir := filepath.Join(f.h.rootAbs, "target")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	c := f.nearMissFor(t, f.runID)
	if c.Reason != runcache.ReasonEffectStateUnavailable {
		t.Fatalf("reason = %q, want effect-state-unavailable", c.Reason)
	}
	if c.Effect == nil {
		t.Fatal("effect-state-unavailable candidate carries no effect diagnosis")
	}
	if c.Effect.Reason != runcache.ReasonEffectStateUnavailable {
		t.Errorf("diagnosis reason = %q, want the candidate's own reason", c.Effect.Reason)
	}
}

// TestNearMissUnrelatedReasonHasNoEffectDiagnosis proves the gate: an input-tree-differs
// miss leaves the diagnosis absent, so a surface never teaches the effect decision where
// it is irrelevant — even though the same observation report was available to attach.
func TestNearMissUnrelatedReasonHasNoEffectDiagnosis(t *testing.T) {
	f := newEffectFixture(t)
	f.h.write(t, "data.txt", "v2")

	c := f.nearMissFor(t, f.runID)
	if c.Reason != runcache.ReasonInputTreeDiffers {
		t.Fatalf("reason = %q, want input-tree-differs", c.Reason)
	}
	if c.Effect != nil {
		t.Errorf("non-effect candidate carries an effect diagnosis: %+v", c.Effect)
	}
}

// TestNearMissEffectDetailCostsNoExtraObservation is the boundedness proof: a Ls(Near)
// pass that enriches candidates with the effect diagnosis still walks the watched roots
// exactly once. A surface that re-observed to name a root would fail here.
func TestNearMissEffectDetailCostsNoExtraObservation(t *testing.T) {
	var counter *countingEffectObserver
	f := newEffectFixture(t, withEffectObserverDecorator(func(inner apprun.EffectObserver) apprun.EffectObserver {
		counter = &countingEffectObserver{inner: inner}
		return counter
	}))
	writeTarget(t, f.h, "app", "built-again-and-longer")

	counter.calls = 0
	c := f.nearMissFor(t, f.runID)
	if counter.calls != 1 {
		t.Fatalf("Ls(Near) performed %d effect observations, want exactly 1", counter.calls)
	}
	if c.Effect == nil || c.Effect.Root != "target" {
		t.Fatalf("the single observation did not produce the diagnosis: %+v", c.Effect)
	}
}

// TestNearMissStoredEffectVerdictOmitsTodaysRoot is the provenance pair. A run that
// itself disturbed the watched output is recorded as non-reusable history, so run
// ls --near short-circuits to that stored verdict: the reason describes the past
// execution, and today's observation — which does name a dominant root — is not evidence
// about it, so the root is omitted while the diagnosis survives. The reusable run stored
// alongside it reaches the same reason by comparison and keeps the observed root, so one
// listing shows both provenances at once.
func TestNearMissStoredEffectVerdictOmitsTodaysRoot(t *testing.T) {
	f := newEffectFixture(t)

	// A second command that writes into the watched root while it runs: its post-run
	// effect state differs from its pre-run state, so it is recorded as non-reusable
	// history with the stored effect-state-differs verdict and was never a cache entry.
	f.h.runner.onRun = func() { writeTarget(t, f.h, "generated", "by-the-command") }
	mutating, err := f.h.svc.Run(context.Background(), f.h.request("cmd", "-cat", "data.txt", "--build"))
	if err != nil {
		t.Fatalf("mutating run: %v", err)
	}
	f.h.runner.onRun = nil
	if mutating.Written {
		t.Fatal("a run that disturbed the watched root must not publish a reusable entry")
	}
	if got := mutating.Reuse.Reason(); got != runcache.ReasonEffectStateDiffers {
		t.Fatalf("stored reuse reason = %q, want effect-state-differs", got)
	}

	// That same write also moved the watched state out from under the first run, so it is
	// a comparison-derived effect-state near miss in the very same listing.
	stored := f.nearMissFor(t, mutating.RunID)
	compared := f.nearMissFor(t, f.runID)

	if compared.Reason != runcache.ReasonEffectStateDiffers {
		t.Fatalf("comparison-derived reason = %q, want effect-state-differs", compared.Reason)
	}
	if compared.Effect == nil || compared.Effect.Root != "target" {
		t.Errorf("comparison-derived diagnosis = %+v, want the observed root %q", compared.Effect, "target")
	}

	if stored.Reason != runcache.ReasonEffectStateDiffers {
		t.Fatalf("stored-verdict reason = %q, want effect-state-differs", stored.Reason)
	}
	if stored.Effect == nil {
		t.Fatal("stored-verdict candidate lost its effect diagnosis; only the root should be dropped")
	}
	if stored.Effect.Reason != runcache.ReasonEffectStateDiffers {
		t.Errorf("stored-verdict diagnosis reason = %q, want the candidate's own", stored.Effect.Reason)
	}
	if stored.Effect.Root != "" {
		t.Errorf("stored-verdict root = %q, want it omitted: today's dominant root says nothing about that past run", stored.Effect.Root)
	}
	// The score rule this mirrors still holds, so the two honesty gates cannot drift apart.
	if len(stored.Score.Matched) != 0 || len(stored.Score.Different) != 0 {
		t.Errorf("stored-verdict score = %+v, want none for a candidate that was never re-keyed", stored.Score)
	}
}

// TestNearMissEffectDiagnosisIsIdenticalAcrossSurfaces proves the one-fact rule for a
// comparison-derived near miss: the same stored run, examined through run ls --near, run
// explain, and the inline run diagnosis, reports one reason and one root. Rendering may
// differ; the fact may not. The subject is deliberately a run that was a real cache entry
// and drifted, because that is the case where all three surfaces derive the reason the
// same way; a run recorded as non-reusable history is instead short-circuited by two of
// them and re-keyed by explain, which is a reason difference that predates this detail
// and is what TestNearMissStoredEffectVerdictOmitsTodaysRoot pins the root rule against.
func TestNearMissEffectDiagnosisIsIdenticalAcrossSurfaces(t *testing.T) {
	f := newEffectFixture(t)
	writeTarget(t, f.h, "app", "built-again-and-longer")

	fromLs := f.nearMissFor(t, f.runID)

	explained, err := f.h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeFromRunToNow,
		Request: apprun.Request{Project: f.h.proj, Config: f.h.cfg},
		RunRef:  f.runID.String(),
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(explained.Candidates) == 0 {
		t.Fatal("explain reported no candidate for the stored run")
	}
	fromExplain := explained.Candidates[0]

	// A fresh run of the same command misses (the effect state moved), so its inline
	// diagnosis considers the earlier run as the nearest candidate.
	miss, err := f.h.svc.Run(context.Background(), f.h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if miss.Status == runcache.CacheHit {
		t.Fatal("a run whose watched effect state moved must not hit")
	}
	diag, err := f.h.svc.DiagnoseRun(context.Background(), miss, f.h.cfg.Run.TTL)
	if err != nil {
		t.Fatalf("DiagnoseRun: %v", err)
	}
	if diag.Nearest == nil {
		t.Fatal("inline diagnosis found no nearest candidate")
	}
	fromInline := *diag.Nearest
	if fromInline.Entry.ID != f.runID {
		t.Fatalf("inline nearest = %s, want the earlier run %s", fromInline.Entry.ID.Short(), f.runID.Short())
	}

	for _, tc := range []struct {
		surface string
		cand    apprun.RunReuseCandidate
	}{
		{"run ls --near", fromLs},
		{"run explain", fromExplain},
		{"inline run diagnosis", fromInline},
	} {
		if tc.cand.Effect == nil {
			t.Fatalf("%s: candidate carries no effect diagnosis", tc.surface)
		}
		if tc.cand.Effect.Reason != runcache.ReasonEffectStateDiffers {
			t.Errorf("%s: reason = %q, want effect-state-differs", tc.surface, tc.cand.Effect.Reason)
		}
		if tc.cand.Effect.Root != "target" {
			t.Errorf("%s: root = %q, want the observed root %q", tc.surface, tc.cand.Effect.Root, "target")
		}
	}
}
