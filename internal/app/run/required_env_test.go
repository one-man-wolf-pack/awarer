package run_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// requirementHarness wires counting decorators around the scanner and the store, so a
// test can prove a refusal happened before any observation or lookup, and declares a
// granted probe-bearing validation policy for "check" so it can prove no probe ran
// either. "check" also requires NEED_DIR; nothing else is declared.
func newRequirementHarness(t *testing.T) (*harness, *scanProbe, *lookupStore) {
	t.Helper()
	probe := &scanProbe{}
	store := &lookupStore{}
	h := newHarness(t,
		withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
			probe.inner = inner
			return probe
		}),
		withStoreDecorator(func(inner runcache.Store) runcache.Store {
			store.Store = inner
			return store
		}))
	t.Cleanup(h.cleanup)
	h.cfg.Run.Validation = []config.ValidationPolicy{{
		Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: []string{"target"},
		IdentityProbes: []config.IdentityProbe{{Name: "tool", Argv: []string{"tool", "--version"}}},
	}}
	h.cfg.Run.RequiredEnv = []config.EnvRequirement{{Argv: []string{"check"}, CWD: ".", Names: []string{"NEED_DIR", "HOME"}}}
	h.setGrant(t, true)
	h.prober.stdout["tool"] = "tool 1.0\n"
	h.env.vars["HOME"] = "/home/u"
	h.write(t, "main.src", "v1")
	return h, probe, store
}

func storedRunCount(t *testing.T, h *harness) int {
	t.Helper()
	ids, err := h.store.ListRefs(context.Background())
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	return len(ids)
}

func TestUnmetRequirementRefusesBeforeAnyProbeObservationLookupOrExecution(t *testing.T) {
	modes := map[string]apprun.Policy{
		"ordinary": {},
		"record":   {Record: true},
		"no-cache": {NoCache: true},
		"refresh":  {Refresh: true},
	}
	for _, state := range []struct {
		name  string
		set   bool
		value string
		want  string
	}{{"unset", false, "", "unset"}, {"empty", true, "", "empty"}} {
		for mode, policy := range modes {
			t.Run(state.name+"/"+mode, func(t *testing.T) {
				h, scans, store := newRequirementHarness(t)
				if state.set {
					h.env.vars["NEED_DIR"] = state.value
				}
				req := h.request("check")
				req.Policy = policy
				_, err := h.svc.Run(context.Background(), req)
				var refusal *apprun.EnvRequirementError
				if !errors.As(err, &refusal) {
					t.Fatalf("Run error = %v, want an environment requirement refusal", err)
				}
				var ve *config.ValidationError
				if errors.As(err, &ve) {
					t.Fatal("a missing caller value must not read as invalid configuration")
				}
				unmet := refusal.Requirement.Check.Unmet()
				if len(unmet) != 1 || unmet[0].Name != "NEED_DIR" || unmet[0].Presence.String() != state.want {
					t.Fatalf("unmet = %+v, want NEED_DIR %s", unmet, state.want)
				}
				if h.prober.calls != 0 || scans.scans != 0 || len(store.keys) != 0 || h.runner.calls != 0 {
					t.Fatalf("refusal after work: probes=%d scans=%d lookups=%d runs=%d", h.prober.calls, scans.scans, len(store.keys), h.runner.calls)
				}
				if n := storedRunCount(t, h); n != 0 {
					t.Fatalf("refusal published %d run record(s)", n)
				}
			})
		}
	}
}

func TestRequirementGateCannotBeBypassedByAReusableEntry(t *testing.T) {
	h, scans, store := newRequirementHarness(t)
	h.env.vars["NEED_DIR"] = "/cache/a"
	first := h.mustRun(t, h.request("check"))
	if !first.Written {
		t.Fatalf("satisfied run not published: %s/%s", first.Reuse.Kind(), first.Reuse.Reason())
	}
	if hit := h.mustRun(t, h.request("check")); hit.Status != runcache.CacheHit {
		t.Fatalf("second satisfied run = %s, want a hit", hit.Status)
	}
	runs, probes, nscans, lookups := h.runner.calls, h.prober.calls, scans.scans, len(store.keys)

	delete(h.env.vars, "NEED_DIR")
	_, err := h.svc.Run(context.Background(), h.request("check"))
	var refusal *apprun.EnvRequirementError
	if !errors.As(err, &refusal) {
		t.Fatalf("Run = %v, want refusal although a reusable entry exists", err)
	}
	if h.runner.calls != runs || h.prober.calls != probes || scans.scans != nscans || len(store.keys) != lookups {
		t.Fatal("refusal replayed, probed, scanned or looked up")
	}
}

func TestSatisfiedRequirementTransmitsAndKeysTheValue(t *testing.T) {
	h, _, _ := newRequirementHarness(t)
	h.cfg.Run.Validation = nil // a declaration needs no validation policy
	h.env.vars["NEED_DIR"] = "/cache/a"
	h.env.vars["AMBIENT_ONLY"] = "x"

	first := h.mustRun(t, h.request("check"))
	got := childEnvMap(t, h)
	if got["NEED_DIR"] != "/cache/a" {
		t.Fatalf("child NEED_DIR = %q, want the caller value without an allowlist entry", got["NEED_DIR"])
	}
	if _, leaked := got["AMBIENT_ONLY"]; leaked {
		t.Fatal("an undeclared ambient name reached the child")
	}
	if !slices.Contains(h.resolver.gotEnv, "NEED_DIR=/cache/a") {
		t.Fatal("executable resolution did not see the checked environment")
	}
	if hit := h.mustRun(t, h.request("check")); hit.Status != runcache.CacheHit {
		t.Fatalf("unchanged value = %s, want a hit", hit.Status)
	}
	h.env.vars["NEED_DIR"] = "/cache/b"
	changed := h.mustRun(t, h.request("check"))
	if changed.Status == runcache.CacheHit || changed.Key == first.Key {
		t.Fatal("a changed required value must change the run identity")
	}
	if hits := grepDir(t, h.rootAbs, "/cache/b"); len(hits) != 0 {
		t.Fatalf("raw required value persisted in %v", hits)
	}

	// An unmatched invocation keeps predecessor behavior: not transmitted, not checked.
	delete(h.env.vars, "NEED_DIR")
	for _, argv := range [][]string{{"check", "--all"}, {"other"}} {
		h.env.vars["NEED_DIR"] = "/cache/c"
		h.mustRun(t, h.request(argv...))
		if _, ok := childEnvMap(t, h)["NEED_DIR"]; ok {
			t.Fatalf("%q received NEED_DIR without a declaration selecting it", argv)
		}
		delete(h.env.vars, "NEED_DIR")
		h.mustRun(t, h.request(argv...))
	}
	nested := h.request("check")
	nestedCWD, err := runcache.NewExecutionCWD("sub")
	if err != nil {
		t.Fatal(err)
	}
	nested.CWD = nestedCWD
	if _, err := h.svc.Run(context.Background(), nested); err != nil {
		t.Fatalf("same argv in another cwd was gated: %v", err)
	}
}

func TestExplainReportsAKnownRefusalWithoutKeyLookupOrProbes(t *testing.T) {
	h, _, store := newRequirementHarness(t)
	h.env.vars["NEED_DIR"] = "/cache/a"
	first := h.mustRun(t, h.request("check"))
	delete(h.env.vars, "NEED_DIR")
	probes := h.prober.calls
	store.keys = nil

	for _, req := range []apprun.ExplainRequest{
		{Mode: apprun.ModeCommand, Request: h.request("check")},
		{Mode: apprun.ModeLast, Request: h.request("check")},
		{Mode: apprun.ModeFromRunToNow, Request: h.request("check"), RunRef: first.RunID.String()},
	} {
		res, err := h.svc.Explain(context.Background(), req)
		if err != nil {
			t.Fatalf("explain mode %d: %v", req.Mode, err)
		}
		if res.Outcome != apprun.OutcomeRefused || res.Reason != runcache.ReasonEnvRequirementUnmet.String() {
			t.Errorf("mode %d = %s/%s, want refused/env-requirement-unmet", req.Mode, res.Outcome, res.Reason)
		}
		s := res.Subject
		if !s.IdentityUnavailable || !s.Key.IsZero() || s.Cacheable || !s.CacheableKnown() {
			t.Errorf("mode %d subject claims identity/cacheability: %+v", req.Mode, s)
		}
		if s.Exact.Assessment != apprun.ExactIdentityUnavailable || s.Exact.Hit != nil {
			t.Errorf("mode %d exact = %+v, want an unperformed lookup", req.Mode, s.Exact)
		}
		if len(res.Candidates) != 0 || len(res.Differences.Differences) != 0 {
			t.Errorf("mode %d ranked or compared candidates for a refusal", req.Mode)
		}
		if s.KeyInput.InputTreeHash.IsZero() {
			t.Errorf("mode %d dropped its read-only input observation", req.Mode)
		}
		if s.EnvRequirement == nil || s.EnvRequirement.Satisfied() || s.EnvRequirement.Index != 0 {
			t.Fatalf("mode %d requirement = %+v", req.Mode, s.EnvRequirement)
		}
	}
	if len(store.keys) != 0 || h.prober.calls != probes {
		t.Fatalf("explain looked up %d keys and ran %d probes", len(store.keys), h.prober.calls-probes)
	}

	h.env.vars["NEED_DIR"] = "/cache/a"
	h.cfg.Run.Validation = nil
	h.mustRun(t, h.request("check"))
	res, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: h.request("check")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != apprun.OutcomeHit || res.Subject.EnvRequirement == nil || !res.Subject.EnvRequirement.Satisfied() {
		t.Fatalf("satisfied explain = %s with requirement %+v", res.Outcome, res.Subject.EnvRequirement)
	}
}

func TestListKeepsTheWindowCompleteAroundABlockedRow(t *testing.T) {
	h, _, _ := newRequirementHarness(t)
	h.cfg.Run.RequiredEnv = append(h.cfg.Run.RequiredEnv, config.EnvRequirement{Argv: []string{"deploy"}, CWD: ".", Names: []string{"DEPLOY_DIR"}})
	h.env.vars["NEED_DIR"] = "/cache/a"
	h.env.vars["DEPLOY_DIR"] = "/d"
	h.mustRun(t, h.request("check"))  // granted probes: unassessed in diagnostics
	h.mustRun(t, h.request("deploy")) // will be blocked
	h.mustRun(t, h.request("plain"))  // ordinary
	delete(h.env.vars, "DEPLOY_DIR")
	probes := h.prober.calls

	ls, err := h.svc.Ls(context.Background(), apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]apprun.LsEntry{}
	for _, e := range ls.Reusable {
		rows[strings.Join(e.Entry.KeyInput.Command.Argv, " ")] = e
	}
	if e, ok := rows["deploy"]; !ok || e.Reusable() || e.Reusability.Reason() != runcache.ReasonEnvRequirementUnmet {
		t.Fatalf("deploy row = %+v, want assessed not reusable with env-requirement-unmet", e)
	}
	if e, ok := rows["plain"]; !ok || !e.Reusable() {
		t.Fatalf("plain row = %+v, want it still reusable", e)
	}
	if len(ls.Unassessed) != 1 || ls.Unassessed[0].Entry.KeyInput.Command.Argv[0] != "check" {
		t.Fatalf("unassessed = %+v, want only the probe-bearing row", ls.Unassessed)
	}
	if h.prober.calls != probes {
		t.Fatal("ls ran a probe")
	}

	// Only the probe row makes the window incomplete; the refusal is a known answer.
	h.cfg.Run.Validation = nil
	ls, err = h.svc.Ls(context.Background(), apprun.LsRequest{Project: h.proj, Config: h.cfg})
	if err != nil || ls.Incomplete() {
		t.Fatalf("ls = %+v, %v; a blocked row must not make the window unknown", ls, err)
	}
	for _, e := range ls.Reusable {
		if e.Entry.KeyInput.Command.Argv[0] == "deploy" {
			t.Fatal("blocked invocation listed as reusable")
		}
	}

	// Stored history keeps its original facts regardless of today's requirement.
	log, err := h.svc.Log(context.Background(), apprun.LogRequest{})
	if err != nil || len(log) != 3 {
		t.Fatalf("log = %d rows, %v; want every recorded run", len(log), err)
	}
}
