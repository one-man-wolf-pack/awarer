package run_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
)

func newProbeHarness(t *testing.T, grant bool) *harness {
	t.Helper()
	h, _ := newValidationHarness(t)
	h.cfg.Run.Validation[0].IdentityProbes = []config.IdentityProbe{
		{Name: "tool", Argv: []string{"tool", "--version"}},
		{Name: "linker", Argv: []string{"linker", "-v"}},
	}
	h.setGrant(t, grant)
	h.prober.stdout["tool"] = "tool 1.0\n"
	h.prober.stdout["linker"] = "linker 7\n"
	return h
}

// setGrant approves the probe contexts of the harness's current policies, the way
// awa config probes allow would, or revokes the project's grant.
func (h *harness) setGrant(t *testing.T, grant bool) {
	t.Helper()
	if !grant {
		if _, _, err := h.grants.Revoke(h.rootAbs); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		return
	}
	var contexts []hashing.TreeHash
	for _, p := range h.cfg.Run.Validation {
		if len(p.IdentityProbes) > 0 {
			contexts = append(contexts, runcache.PolicyGrantContext(blake3hash.New(), p))
		}
	}
	if _, err := h.grants.Allow(h.rootAbs, contexts); err != nil {
		t.Fatalf("allow: %v", err)
	}
}

func (h *harness) mustRun(t *testing.T, req apprun.Request) apprun.Result {
	t.Helper()
	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func factStatuses(facts []runcache.ExternalFact) []runcache.FactStatus {
	out := make([]runcache.FactStatus, len(facts))
	for i, f := range facts {
		out[i] = f.Status
	}
	return out
}

func TestProbesNeverRunWithoutThePrivateGrant(t *testing.T) {
	h := newProbeHarness(t, false)
	h.runner.exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 3}
	h.runner.stdout = "child output\n"
	var out []byte
	req := h.request("check")
	req.Stdout = writerFunc(func(p []byte) { out = append(out, p...) })

	res := h.mustRun(t, req)
	if h.prober.calls != 0 {
		t.Fatalf("ungranted probes ran %d times", h.prober.calls)
	}
	if res.ExitCode != 3 || string(out) != "child output\n" || h.runner.calls != 1 {
		t.Fatalf("child exit %d output %q calls %d, want its own result", res.ExitCode, out, h.runner.calls)
	}
	if res.Written || res.Reuse.Kind() != runcache.ReuseRecordOnly || res.Reuse.Reason() != runcache.ReasonExternalFactsUnavailable {
		t.Fatalf("reuse = %s/%s written=%v, want record-only external-facts-unavailable", res.Reuse.Kind(), res.Reuse.Reason(), res.Written)
	}
	got := factStatuses(res.Execution.KeyInput.Validation.Facts)
	if !slices.Equal(got, []runcache.FactStatus{runcache.FactDenied, runcache.FactDenied}) {
		t.Fatalf("facts = %v, want both denied", got)
	}
	stored, err := h.store.Get(res.RunID)
	if err != nil || stored.PostRunFacts != nil {
		t.Fatalf("stored = %v post=%v, want a readable record without reacquisition", err, stored.PostRunFacts)
	}
	h.mustRun(t, h.request("check"))
	if h.runner.calls != 2 {
		t.Fatal("a run without facts was reused")
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func TestGrantedProbesAreFreshOnEveryLookupIncludingAHit(t *testing.T) {
	h := newProbeHarness(t, true)
	first := h.mustRun(t, h.request("check"))
	if !first.Written || h.prober.calls != 4 {
		t.Fatalf("written=%v probes=%d reuse=%s/%s, want a published result after pre and post phases", first.Written, h.prober.calls, first.Reuse.Kind(), first.Reuse.Reason())
	}
	for _, spec := range h.prober.gotSpec {
		if spec.Dir != h.rootAbs || !slices.Equal(spec.Env, h.runner.gotEnv) || spec.Timeout <= 0 || spec.MaxStdout <= 0 || spec.MaxStderr <= 0 {
			t.Fatalf("probe spec %+v, want the child's cwd and env with finite bounds", spec)
		}
	}
	hit := h.mustRun(t, h.request("check"))
	if hit.Status != runcache.CacheHit || h.runner.calls != 1 || h.prober.calls != 6 {
		t.Fatalf("status=%v child=%d probes=%d, want a hit that reacquired the pre-run phase only", hit.Status, h.runner.calls, h.prober.calls)
	}

	h.prober.stdout["linker"] = "linker 7 \n"
	miss := h.mustRun(t, h.request("check"))
	if miss.Status == runcache.CacheHit || h.runner.calls != 2 {
		t.Fatal("a changed probe report replayed the old result")
	}
}

func TestPostRunFactsGateThePublication(t *testing.T) {
	cases := []struct {
		name   string
		during func(h *harness)
		want   runcache.MismatchReason
	}{
		{"changed report", func(h *harness) { h.prober.stdout["tool"] = "tool 2.0\n" }, runcache.ReasonExternalFactsChanged},
		{"unavailable report", func(h *harness) { h.prober.status["linker"] = runcache.FactTimedOut }, runcache.ReasonExternalFactsUnavailablePostRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeHarness(t, true)
			h.runner.onRun = func() { tc.during(h) }
			res := h.mustRun(t, h.request("check"))
			if res.Written || res.Reuse.Kind() != runcache.ReuseNonReusable || res.Reuse.Reason() != tc.want {
				t.Fatalf("reuse = %s/%s written=%v, want non-reusable %s", res.Reuse.Kind(), res.Reuse.Reason(), res.Written, tc.want)
			}
			if res.Execution.Mutation.ScanFailed() {
				t.Fatal("a probe failure was recorded as a failed post-run scan")
			}
			stored, err := h.store.Get(res.RunID)
			if err != nil || len(stored.PostRunFacts) != 2 {
				t.Fatalf("stored = %v post=%v, want the reacquired phase recorded", err, stored.PostRunFacts)
			}
		})
	}
}

func TestSuccessfulPostRunFactsCannotRepairAnUnavailablePrestate(t *testing.T) {
	h := newProbeHarness(t, true)
	h.prober.status["tool"] = runcache.FactExitNonzero
	h.runner.onRun = func() { delete(h.prober.status, "tool") }
	res := h.mustRun(t, h.request("check"))
	if res.Written || res.Reuse.Reason() != runcache.ReasonExternalFactsUnavailable {
		t.Fatalf("reuse = %s/%s written=%v", res.Reuse.Kind(), res.Reuse.Reason(), res.Written)
	}
	got := factStatuses(res.Execution.KeyInput.Validation.Facts)
	if !slices.Equal(got, []runcache.FactStatus{runcache.FactSkipped, runcache.FactExitNonzero}) && !slices.Equal(got, []runcache.FactStatus{runcache.FactExitNonzero, runcache.FactSkipped}) {
		t.Fatalf("facts = %v, want the failure and a skipped remainder", got)
	}
	if h.prober.calls != 1 || res.Execution.PostRunFacts != nil {
		t.Fatalf("probes=%d post=%v, want no reacquisition after an unavailable prestate", h.prober.calls, res.Execution.PostRunFacts)
	}
}

func TestPolicyThatRulesOutReuseRunsNoProbes(t *testing.T) {
	for _, pol := range []apprun.Policy{{Record: true}, {NoCache: true}} {
		h := newProbeHarness(t, true)
		req := h.request("check")
		req.Policy = pol
		res := h.mustRun(t, req)
		if h.prober.calls != 0 {
			t.Fatalf("%+v ran %d probes merely to enrich history", pol, h.prober.calls)
		}
		got := factStatuses(res.Execution.KeyInput.Validation.Facts)
		if !slices.Equal(got, []runcache.FactStatus{runcache.FactNotAcquired, runcache.FactNotAcquired}) {
			t.Fatalf("%+v facts = %v", pol, got)
		}
		if _, err := h.store.Get(res.RunID); err != nil {
			t.Fatalf("%+v record unreadable: %v", pol, err)
		}
	}
}

func TestAProbeThatChangesAnInputMeetsTheMutationGuard(t *testing.T) {
	h := newProbeHarness(t, true)
	h.prober.onProbe = func(spec runcache.ProbeSpec) {
		if h.runner.calls == 1 {
			h.write(t, "main.src", "rewritten by a probe")
		}
	}
	res := h.mustRun(t, h.request("check"))
	if res.Written || res.Reuse.Reason() != runcache.ReasonMutatedState {
		t.Fatalf("reuse = %s/%s written=%v, want mutated-state", res.Reuse.Kind(), res.Reuse.Reason(), res.Written)
	}
}

func TestProbeLabelsAreNotIdentityButDefinitionsAre(t *testing.T) {
	h := newProbeHarness(t, true)
	h.mustRun(t, h.request("check"))
	h.cfg.Run.Validation[0].IdentityProbes[0].Name = "renamed"
	if res := h.mustRun(t, h.request("check")); res.Status != runcache.CacheHit {
		t.Fatalf("renaming a probe missed: %v", res.Status)
	}
	h.cfg.Run.Validation[0].IdentityProbes[0].Argv = []string{"tool", "-V"}
	h.prober.stdout["tool"] = "tool 1.0\n"
	// Approve the changed definition so this checks identity, not authorization.
	h.setGrant(t, true)
	if res := h.mustRun(t, h.request("check")); res.Status == runcache.CacheHit {
		t.Fatal("a changed probe argv reused the old result")
	}
}

func TestCancellationDuringProbesStartsNoChild(t *testing.T) {
	h := newProbeHarness(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	h.prober.onProbe = func(runcache.ProbeSpec) { cancel() }
	_, err := h.svc.Run(ctx, h.request("check"))
	if !errors.Is(err, context.Canceled) || h.runner.calls != 0 {
		t.Fatalf("err=%v child=%d, want cancellation before the child", err, h.runner.calls)
	}
}

func TestDiagnosticsNeverExecuteProbes(t *testing.T) {
	h := newProbeHarness(t, true)
	first := h.mustRun(t, h.request("check"))
	calls := h.prober.calls
	ctx := context.Background()

	for _, req := range []apprun.ExplainRequest{
		{Mode: apprun.ModeCommand, Request: h.request("check")},
		{Mode: apprun.ModeLast, Request: h.request("check")},
		{Mode: apprun.ModeFromRunToNow, Request: h.request("check"), RunRef: first.RunID.String()},
	} {
		if res, err := h.svc.Explain(ctx, req); err != nil || res.Outcome != apprun.OutcomeIncomplete {
			t.Errorf("explain mode %d = %s, %v; want incomplete without fresh facts", req.Mode, res.Outcome, err)
		}
	}
	for _, near := range []bool{false, true} {
		if res, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, Near: near}); err != nil || len(res.Unassessed) != 1 {
			t.Errorf("ls near=%v = %+v, %v; want the run unassessed", near, res.Unassessed, err)
		}
	}
	if _, err := h.svc.Log(ctx, apprun.LogRequest{}); err != nil {
		t.Errorf("log: %v", err)
	}
	if _, err := h.svc.Audit(ctx, apprun.AuditRequest{}); err != nil {
		t.Errorf("audit: %v", err)
	}
	if h.prober.calls != calls {
		t.Fatalf("diagnostics ran %d probes", h.prober.calls-calls)
	}

	h.write(t, "main.src", "v2")
	res, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil || len(res.Reusable) != 1 || res.Reusable[0].Reusability.Reason() != runcache.ReasonInputTreeDiffers {
		t.Fatalf("ls with a proven mismatch = %+v, %v", res.Reusable, err)
	}
	if h.prober.calls != calls {
		t.Fatalf("ls ran %d probes", h.prober.calls-calls)
	}
}

type cancellingEffectObserver struct {
	inner  apprun.EffectObserver
	cancel context.CancelFunc
}

func (o *cancellingEffectObserver) Observe(project string, roots, waived []string) (runcache.EffectObservation, runcache.EffectReport, error) {
	if o.cancel != nil {
		o.cancel()
		o.cancel = nil
	}
	return o.inner.Observe(project, roots, waived)
}

func TestCancellationAfterTheKeyStartsNoChild(t *testing.T) {
	obs := &cancellingEffectObserver{}
	h := newHarness(t, withEffectObserverDecorator(func(inner apprun.EffectObserver) apprun.EffectObserver {
		obs.inner = inner
		return obs
	}))
	h.write(t, "main.src", "v1")
	ctx, cancel := context.WithCancel(context.Background())
	obs.cancel = cancel
	_, err := h.svc.Run(ctx, h.request("check"))
	if !errors.Is(err, context.Canceled) || h.runner.calls != 0 {
		t.Fatalf("err=%v child=%d, want cancellation before the child", err, h.runner.calls)
	}
}

func TestKnownDenialAnswersDiagnosticsWithoutProbes(t *testing.T) {
	h := newProbeHarness(t, true)
	first := h.mustRun(t, h.request("check"))
	h.setGrant(t, false)
	calls := h.prober.calls
	ctx := context.Background()
	denied := []runcache.FactStatus{runcache.FactDenied, runcache.FactDenied}

	for _, req := range []apprun.ExplainRequest{
		{Mode: apprun.ModeCommand, Request: h.request("check")},
		{Mode: apprun.ModeLast, Request: h.request("check")},
		{Mode: apprun.ModeFromRunToNow, Request: h.request("check"), RunRef: first.RunID.String()},
	} {
		res, err := h.svc.Explain(ctx, req)
		if err != nil {
			t.Fatalf("explain mode %d: %v", req.Mode, err)
		}
		if res.Outcome == apprun.OutcomeHit || res.Reason != runcache.ReasonExternalFactsUnavailable.String() {
			t.Errorf("explain mode %d = %s/%s, want the denial blocker", req.Mode, res.Outcome, res.Reason)
		}
		if got := factStatuses(res.Subject.KeyInput.Validation.Facts); !slices.Equal(got, denied) {
			t.Errorf("explain mode %d facts = %v, want denied", req.Mode, got)
		}
	}
	ls, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil || len(ls.Reusable) != 1 || ls.Reusable[0].Reusability.Reason() != runcache.ReasonExternalFactsUnavailable {
		t.Fatalf("ls = %+v, %v; want the stored run blocked by the denial", ls.Reusable, err)
	}
	near, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, Near: true})
	if err != nil || len(near.NearMisses) != 1 || near.NearMisses[0].Reason != runcache.ReasonExternalFactsUnavailable {
		t.Fatalf("ls --near = %+v, %v; want the denial as the near-miss reason", near.NearMisses, err)
	}
	if h.prober.calls != calls {
		t.Fatalf("diagnostics ran %d probes", h.prober.calls-calls)
	}

	h.setGrant(t, true)
	if ls, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg}); err != nil || len(ls.Unassessed) != 1 || len(ls.Reusable) != 0 {
		t.Fatalf("granted ls = %+v, %v; want facts still unassessed", ls, err)
	}
}

func newGrantedExplainHarness(t *testing.T) (*harness, *lookupStore) {
	t.Helper()
	h, store := newDeniedExplainHarness(t)
	h.setGrant(t, true)
	h.prober.stdout["tool"] = "tool 1.0\n"
	return h, store
}

func TestGrantedExplainIsIncompleteWithoutALookupInEveryMode(t *testing.T) {
	h, store := newGrantedExplainHarness(t)
	first := h.mustRun(t, h.request("check"))
	if !first.Written {
		t.Fatalf("granted run not published: %s/%s", first.Reuse.Kind(), first.Reuse.Reason())
	}
	calls := h.prober.calls
	store.keys = nil
	ctx := context.Background()

	for _, req := range []apprun.ExplainRequest{
		{Mode: apprun.ModeCommand, Request: h.request("check")},
		{Mode: apprun.ModeLast, Request: h.request("check")},
		{Mode: apprun.ModeFromRunToNow, Request: h.request("check"), RunRef: first.RunID.String()},
	} {
		res, err := h.svc.Explain(ctx, req)
		if err != nil {
			t.Fatalf("explain mode %d: %v", req.Mode, err)
		}
		if res.Outcome != apprun.OutcomeIncomplete || res.Reason != apprun.ReasonExternalFactsUnacquired {
			t.Errorf("explain mode %d = %s/%s, want incomplete", req.Mode, res.Outcome, res.Reason)
		}
		if !res.Subject.IdentityUnavailable || !res.Subject.Key.IsZero() || res.Subject.CacheableKnown() || res.Subject.Exact != (apprun.ExactMatch{Assessment: apprun.ExactIdentityUnavailable}) {
			t.Errorf("explain mode %d subject = %+v, want unknown key, cacheability and exact entry", req.Mode, res.Subject.Exact)
		}
		if !res.Differences.Equal() {
			t.Errorf("explain mode %d claims a delta %+v", req.Mode, res.Differences)
		}
		wantCands := 1
		if req.Mode == apprun.ModeCommand {
			wantCands = 0
		}
		if len(res.Candidates) != wantCands {
			t.Fatalf("explain mode %d candidates = %d, want %d", req.Mode, len(res.Candidates), wantCands)
		}
		for _, c := range res.Candidates {
			if c.Reason != "" || len(c.Score.Matched) != 0 || len(c.Score.Different) != 0 || !c.Comparison.Equal() {
				t.Errorf("explain mode %d candidate = %s %+v, want reason-only without a score", req.Mode, c.Reason, c.Score)
			}
		}
	}
	if len(store.keys) != 0 {
		t.Fatalf("explain looked up %v without a current key", store.keys)
	}

	refresh := h.request("check")
	refresh.Policy.Refresh = true
	res, err := h.svc.Explain(ctx, apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: refresh})
	if err != nil || res.Outcome != apprun.OutcomeMiss || res.Reason != "refresh-requested" || res.Subject.CacheableKnown() || res.Subject.Exact.Assessment != apprun.ExactIdentityUnavailable {
		t.Fatalf("refresh explain = %s/%s exact %+v, %v; want the refresh decision with unknown identity", res.Outcome, res.Reason, res.Subject.Exact, err)
	}
	noCache := h.request("check")
	noCache.Policy.NoCache = true
	res, err = h.svc.Explain(ctx, apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: noCache})
	if err != nil || res.Outcome != apprun.OutcomeDisabled || res.Subject.IdentityUnavailable || !res.Subject.CacheableKnown() {
		t.Fatalf("no-cache explain = %s identity-unavailable=%v, %v; want the ordinary disabled answer", res.Outcome, res.Subject.IdentityUnavailable, err)
	}
	if len(store.keys) != 1 || h.prober.calls != calls {
		t.Fatalf("lookups %d probes %d; want only the no-cache informational lookup and no probe", len(store.keys), h.prober.calls-calls)
	}

	h.write(t, "main.src", "v2")
	res, err = h.svc.Explain(ctx, apprun.ExplainRequest{Mode: apprun.ModeFromRunToNow, Request: h.request("check"), RunRef: first.RunID.String()})
	if err != nil || res.Outcome != apprun.OutcomeIncomplete || len(res.Candidates) != 1 || !res.Differences.Equal() {
		t.Fatalf("stored explain after an input change = %+v, %v; want incomplete, not a global miss", res, err)
	}
	if c := res.Candidates[0]; c.Reason != runcache.ReasonInputTreeDiffers || len(c.Score.Matched) != 0 || !c.Comparison.Equal() || c.Changed == nil {
		t.Fatalf("candidate = %s %+v, want its proven input change reason-only with the changed paths", c.Reason, c.Score)
	}
}

func TestMixedWindowSeparatesUnassessedRows(t *testing.T) {
	h := newProbeHarness(t, true)
	ctx := context.Background()
	// Run ids are time-ordered, so each run gets its own instant to fix the window.
	tick := func() { h.clock.now = h.clock.now.Add(time.Second) }
	disproved := h.mustRun(t, h.request("check"))
	tick()
	h.write(t, "main.src", "v2")
	unassessed := h.mustRun(t, h.request("check"))
	tick()
	ordinary := h.mustRun(t, h.request("other"))
	for _, r := range []apprun.Result{disproved, unassessed, ordinary} {
		if !r.Written {
			t.Fatalf("run %s not published", r.RunID.Short())
		}
	}
	calls := h.prober.calls
	ids := func(es []apprun.LsEntry) []runcache.RunID {
		var out []runcache.RunID
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}

	plain, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg})
	if err != nil || !slices.Equal(ids(plain.Reusable), []runcache.RunID{ordinary.RunID}) || !slices.Equal(ids(plain.Unassessed), []runcache.RunID{unassessed.RunID}) || !plain.Incomplete() {
		t.Fatalf("plain ls = %v unassessed %v, %v", ids(plain.Reusable), ids(plain.Unassessed), err)
	}
	all, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil || !slices.Equal(ids(all.Reusable), []runcache.RunID{ordinary.RunID, disproved.RunID}) || !slices.Equal(ids(all.Unassessed), []runcache.RunID{unassessed.RunID}) {
		t.Fatalf("ls --all = %v unassessed %v, %v; want each row once", ids(all.Reusable), ids(all.Unassessed), err)
	}
	if r := all.Reusable[1].Reusability.Reason(); r != runcache.ReasonInputTreeDiffers {
		t.Fatalf("disproved row reason = %s", r)
	}
	near, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, Near: true})
	if err != nil || len(near.NearMisses) != 1 || near.NearMisses[0].Entry.ID != disproved.RunID || !slices.Equal(ids(near.Unassessed), []runcache.RunID{unassessed.RunID}) {
		t.Fatalf("ls --near = %+v unassessed %v, %v", near.NearMisses, ids(near.Unassessed), err)
	}
	if c := near.NearMisses[0]; c.Reason != runcache.ReasonInputTreeDiffers || len(c.Score.Matched) != 0 || len(c.Score.Different) != 0 || !c.Comparison.Equal() {
		t.Fatalf("near candidate = %s %+v, want a reason-only comparison made without the facts", c.Reason, c.Score)
	}
	window, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true, Limit: 2})
	if err != nil || !slices.Equal(ids(window.Reusable), []runcache.RunID{ordinary.RunID}) || !slices.Equal(ids(window.Unassessed), []runcache.RunID{unassessed.RunID}) {
		t.Fatalf("ls --all -n 2 = %v unassessed %v, %v; want the window kept", ids(window.Reusable), ids(window.Unassessed), err)
	}
	if h.prober.calls != calls {
		t.Fatalf("ls ran %d probes", h.prober.calls-calls)
	}
}

func TestAnIndependentGateSettlesAGrantedRowWithoutFacts(t *testing.T) {
	h := newProbeHarness(t, true)
	h.cfg.Run.TTL = config.Duration(time.Hour)
	h.mustRun(t, h.request("check"))
	h.clock.now = h.clock.now.Add(2 * time.Hour)
	ls, err := h.svc.Ls(context.Background(), apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil || len(ls.Unassessed) != 0 || len(ls.Reusable) != 1 || ls.Reusable[0].Reusability.Reason() != runcache.ReasonExpired {
		t.Fatalf("expired granted row = %+v unassessed %d, %v; want it assessed as expired", ls.Reusable, len(ls.Unassessed), err)
	}
}

type lookupStore struct {
	runcache.Store
	keys      []runcache.RunKey
	failWith  error
	answerAs  *runcache.RunKey
	stdoutErr error
	onLookup  func()
}

func (s *lookupStore) Lookup(key runcache.RunKey) (runcache.CacheEntry, bool, error) {
	s.keys = append(s.keys, key)
	if s.onLookup != nil {
		s.onLookup()
	}
	if s.failWith != nil {
		return runcache.CacheEntry{}, false, s.failWith
	}
	if s.answerAs != nil {
		key = *s.answerAs
	}
	return s.Store.Lookup(key)
}

func (s *lookupStore) OpenStdout(id runcache.RunID) (io.ReadCloser, error) {
	if s.stdoutErr != nil {
		return nil, s.stdoutErr
	}
	return s.Store.OpenStdout(id)
}

func newDeniedExplainHarness(t *testing.T) (*harness, *lookupStore) {
	t.Helper()
	ls := &lookupStore{}
	h := newHarness(t, withStoreDecorator(func(inner runcache.Store) runcache.Store {
		ls.Store = inner
		return ls
	}))
	t.Cleanup(h.cleanup)
	h.cfg.Run.Validation = []config.ValidationPolicy{{
		Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: []string{"target"},
		IdentityProbes: []config.IdentityProbe{{Name: "tool", Argv: []string{"tool", "--version"}}},
	}}
	h.write(t, "main.src", "v1")
	return h, ls
}

func TestDeniedExplainProjectsTheRunsOwnKeyAndAnActualLookup(t *testing.T) {
	h, store := newDeniedExplainHarness(t)
	run := h.mustRun(t, h.request("check"))
	if run.Reuse.Reason() != runcache.ReasonExternalFactsUnavailable {
		t.Fatalf("run reason = %s", run.Reuse.Reason())
	}
	store.keys = nil

	res, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: h.request("check")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subject.Key != run.Key || !slices.Equal(store.keys, []runcache.RunKey{run.Key}) {
		t.Fatalf("explain key %s looked up %v, want the run's denied-state key %s", res.Subject.Key, store.keys, run.Key)
	}
	if res.Subject.Cacheable || res.Outcome != apprun.OutcomeUncached || res.Reason != runcache.ReasonExternalFactsUnavailable.String() {
		t.Fatalf("explain = cacheable %v %s/%s", res.Subject.Cacheable, res.Outcome, res.Reason)
	}
	for _, f := range res.Subject.KeyInput.Validation.Facts {
		if f.Status != runcache.FactDenied || !f.Fingerprint.IsZero() {
			t.Fatalf("fact %+v, want denied without a fingerprint", f)
		}
	}
	if res.Subject.Exact != (apprun.ExactMatch{Assessment: apprun.ExactComplete}) || !res.Differences.Equal() || len(res.Candidates) != 0 || h.prober.calls != 0 {
		t.Fatalf("exact %+v diffs %+v candidates %d probes %d, want an established absence", res.Subject.Exact, res.Differences, len(res.Candidates), h.prober.calls)
	}
}

func TestDeniedExplainReportsAFailedLookupAsUnknown(t *testing.T) {
	failure := &fs.PathError{Op: "open", Path: "entry", Err: fs.ErrPermission}
	explainDenied := func(t *testing.T, h *harness) apprun.ExplainResult {
		t.Helper()
		calls := h.prober.calls
		res, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: h.request("check")})
		if err != nil {
			t.Fatalf("explain = %v, want a complete degraded report", err)
		}
		if res.Outcome != apprun.OutcomeUncached || res.Reason != runcache.ReasonExternalFactsUnavailable.String() || len(res.Warnings) == 0 || h.prober.calls != calls {
			t.Fatalf("explain = %s/%s warnings %q probes %d", res.Outcome, res.Reason, res.Warnings, h.prober.calls-calls)
		}
		return res
	}
	t.Run("lookup", func(t *testing.T) {
		h, store := newDeniedExplainHarness(t)
		store.failWith = failure
		if exact := explainDenied(t, h).Subject.Exact; exact != (apprun.ExactMatch{Assessment: apprun.ExactLookupUnavailable}) {
			t.Fatalf("exact = %+v, want lookup-unavailable with no entry", exact)
		}
	})
	t.Run("payload verification", func(t *testing.T) {
		h, store := newDeniedExplainHarness(t)
		h.setGrant(t, true)
		h.prober.stdout["tool"] = "tool 1\n"
		granted := h.mustRun(t, h.request("check"))
		h.setGrant(t, false)
		store.answerAs = &granted.Key
		store.stdoutErr = failure
		exact := explainDenied(t, h).Subject.Exact
		if exact.Assessment != apprun.ExactHealthUnavailable || exact.Hit == nil || exact.Hit.RunID != granted.RunID {
			t.Fatalf("exact = %+v, want health-unavailable keeping run %s", exact, granted.RunID)
		}
	})
}

func assertReasonOnly(t *testing.T, where string, c apprun.RunReuseCandidate, reason runcache.MismatchReason) {
	t.Helper()
	if c.Reason != reason || len(c.Comparison.Differences) != 0 || len(c.Score.Matched) != 0 || len(c.Score.Different) != 0 {
		t.Errorf("%s candidate = reason %s diffs %+v score %+v, want reason-only %s", where, c.Reason, c.Comparison.Differences, c.Score, reason)
	}
}

func TestRevokedGrantLeavesCandidatesReasonOnly(t *testing.T) {
	h := newProbeHarness(t, false)
	h.cfg.Run.Validation[0].IdentityProbes = h.cfg.Run.Validation[0].IdentityProbes[:1]
	h.setGrant(t, true)
	granted := h.mustRun(t, h.request("check"))
	ordinary := h.mustRun(t, h.request("other"))
	h.write(t, "main.src", "v2")
	h.setGrant(t, false)
	calls := h.prober.calls
	ctx := context.Background()

	near, err := h.svc.Ls(ctx, apprun.LsRequest{Project: h.proj, Config: h.cfg, Near: true})
	if err != nil || len(near.NearMisses) != 2 {
		t.Fatalf("ls --near = %+v, %v", near.NearMisses, err)
	}
	if near.NearMisses[0].Entry.ID != ordinary.RunID || len(near.NearMisses[0].Score.Different) == 0 {
		t.Errorf("first near miss = %s score %+v, want the scored ordinary drift ahead", near.NearMisses[0].Entry.ID, near.NearMisses[0].Score)
	}
	assertReasonOnly(t, "ls --near", near.NearMisses[1], runcache.ReasonExternalFactsUnavailable)
	if near.NearMisses[1].Entry.ID != granted.RunID {
		t.Errorf("reason-only candidate lost its recorded run: %s", near.NearMisses[1].Entry.ID)
	}

	stored, err := h.svc.Explain(ctx, apprun.ExplainRequest{Mode: apprun.ModeFromRunToNow, Request: h.request("check"), RunRef: granted.RunID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Differences.Differences) != 0 || len(stored.Candidates) != 1 {
		t.Fatalf("stored explain diffs %+v candidates %d", stored.Differences.Differences, len(stored.Candidates))
	}
	assertReasonOnly(t, "explain", stored.Candidates[0], runcache.ReasonExternalFactsUnavailable)

	if h.prober.calls != calls {
		t.Fatalf("diagnostics ran %d probes", h.prober.calls-calls)
	}
}

func TestRevokedGrantInlineDiagnosisIsReasonOnly(t *testing.T) {
	h := newProbeHarness(t, true)
	h.mustRun(t, h.request("check"))
	h.setGrant(t, false)
	calls := h.prober.calls
	denied := h.mustRun(t, h.request("check"))
	diag, err := h.svc.DiagnoseRun(context.Background(), denied, h.cfg.Run.TTL)
	if err != nil || diag.Nearest == nil {
		t.Fatalf("inline = %+v, %v", diag, err)
	}
	assertReasonOnly(t, "inline", *diag.Nearest, runcache.ReasonExternalFactsUnavailable)
	if h.prober.calls != calls {
		t.Fatalf("a denied run or its diagnosis ran %d probes", h.prober.calls-calls)
	}
}

func TestChangedAcquiredFactKeepsItsDeltaWithoutAnAllMatchedScore(t *testing.T) {
	h := newProbeHarness(t, true)
	h.mustRun(t, h.request("check"))
	h.prober.stdout["tool"] = "tool 2.0\n"
	second := h.mustRun(t, h.request("check"))
	diag, err := h.svc.DiagnoseRun(context.Background(), second, h.cfg.Run.TTL)
	if err != nil || diag.Nearest == nil {
		t.Fatalf("inline = %+v, %v", diag, err)
	}
	n := diag.Nearest
	if n.Reason != runcache.ReasonExternalFactsChanged || len(n.Comparison.Differences) != 1 || n.Comparison.Differences[0].Code != runcache.DiffExternalFactsChanged {
		t.Fatalf("nearest = %s %+v, want the actual changed-fact delta", n.Reason, n.Comparison.Differences)
	}
	if len(n.Score.Matched) != 0 || len(n.Score.Different) != 0 {
		t.Fatalf("score %+v reads as every category matching", n.Score)
	}
}

func TestInlineCandidateRecordedUnderDenialIsReasonOnly(t *testing.T) {
	h := newProbeHarness(t, false)
	h.mustRun(t, h.request("check"))
	h.setGrant(t, true)
	granted := h.mustRun(t, h.request("check"))
	diag, err := h.svc.DiagnoseRun(context.Background(), granted, h.cfg.Run.TTL)
	if err != nil || diag.Nearest == nil {
		t.Fatalf("inline = %+v, %v", diag, err)
	}
	assertReasonOnly(t, "inline", *diag.Nearest, runcache.ReasonExternalFactsUnavailable)
}

func TestRevocationBetweenPhasesStopsTheLaterPhase(t *testing.T) {
	h := newProbeHarness(t, true)
	h.prober.onProbe = func(runcache.ProbeSpec) {
		if h.prober.calls == 2 {
			h.setGrant(t, false)
		}
	}
	res := h.mustRun(t, h.request("check"))
	if h.prober.calls != 2 || h.runner.calls != 1 {
		t.Fatalf("probes %d child %d, want the started pre-run phase only and the child", h.prober.calls, h.runner.calls)
	}
	if got := factStatuses(res.Execution.PostRunFacts); !slices.Equal(got, []runcache.FactStatus{runcache.FactDenied, runcache.FactDenied}) {
		t.Fatalf("post-run facts = %v, want denied", got)
	}
	if res.Written || res.Reuse.Reason() != runcache.ReasonExternalFactsUnavailablePostRun {
		t.Fatalf("reuse = %s/%s written=%v, want non-reusable post-run unavailability", res.Reuse.Kind(), res.Reuse.Reason(), res.Written)
	}
}

func TestGrantCoversOnlyTheApprovedProbeContext(t *testing.T) {
	h := newProbeHarness(t, false)
	h.cfg.Run.Validation = append(h.cfg.Run.Validation, config.ValidationPolicy{
		Name: "lint", Argv: []string{"lint"}, CWD: ".", UnobservedDirs: []string{"target"},
		IdentityProbes: []config.IdentityProbe{{Name: "tool", Argv: []string{"tool", "--version"}}},
	})
	h.setGrant(t, true)

	h.cfg.Run.Validation[0].IdentityProbes[0].Name = "renamed"
	h.cfg.Run.Validation[0].Name = "renamed-policy"
	if res := h.mustRun(t, h.request("check")); !res.Execution.ProbeAuthorization.Granted() || h.prober.calls == 0 {
		t.Fatalf("a label-only change lost approval: %+v", res.Execution.ProbeAuthorization)
	}

	calls := h.prober.calls
	h.cfg.Run.Validation[0].IdentityProbes[0].Argv = []string{"tool", "--other"}
	res := h.mustRun(t, h.request("check"))
	if h.prober.calls != calls || res.Execution.ProbeAuthorization.Cause != runcache.ProbeCauseContextChanged {
		t.Fatalf("a changed probe argv ran %d probes, authorization %+v", h.prober.calls-calls, res.Execution.ProbeAuthorization)
	}
	if got := factStatuses(res.Execution.KeyInput.Validation.Facts); !slices.Equal(got, []runcache.FactStatus{runcache.FactDenied, runcache.FactDenied}) {
		t.Fatalf("facts = %v, want denied", got)
	}
	if res := h.mustRun(t, h.request("lint")); !res.Execution.ProbeAuthorization.Granted() || h.prober.calls != calls+2 {
		t.Fatalf("an unrelated unchanged policy lost approval: %+v", res.Execution.ProbeAuthorization)
	}

	calls = h.prober.calls
	other, err := runcache.NewExecutionCWD("sub")
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.Run.Validation[1].CWD = "sub"
	h.writeUnder(t, "sub/.keep", "")
	req := h.request("lint")
	req.CWD, req.AbsDir = other, h.rootAbs+"/sub"
	if res := h.mustRun(t, req); h.prober.calls != calls || res.Execution.ProbeAuthorization.Granted() {
		t.Fatalf("a changed policy cwd kept approval: %+v", res.Execution.ProbeAuthorization)
	}

	h.setGrant(t, true)
	h.setGrant(t, false)
	if res := h.mustRun(t, req); h.prober.calls != calls || res.Execution.ProbeAuthorization.Cause != runcache.ProbeCauseNoGrant {
		t.Fatalf("a revoked grant still authorized: %+v", res.Execution.ProbeAuthorization)
	}
}
