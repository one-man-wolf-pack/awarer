package run_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
)

type scanProbe struct {
	inner  apprun.Scanner
	scans  int
	failAt int
}

func (p *scanProbe) Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts scanner.Options) (scanner.Result, error) {
	p.scans++
	if p.failAt == p.scans {
		return scanner.Result{}, errors.New("injected scan failure")
	}
	return p.inner.Scan(ctx, project, cfg, scope, opts)
}

func newValidationHarness(t *testing.T, dirs ...string) (*harness, *scanProbe) {
	t.Helper()
	probe := &scanProbe{}
	h := newHarness(t, withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
		probe.inner = inner
		return probe
	}))
	t.Cleanup(h.cleanup)
	if len(dirs) == 0 {
		dirs = []string{"target"}
	}
	h.cfg.Run.Validation = []config.ValidationPolicy{{Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: dirs}}
	h.write(t, "main.src", "v1")
	return h, probe
}

func (h *harness) writeUnder(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(h.rootAbs, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidationInputConflictsAreRefusedBeforeAnyScanOrExecution(t *testing.T) {
	cases := []struct {
		name    string
		dirs    []string
		scope   apprun.ScopeOverrides
		def     []string
		want    apprun.ConflictSource
		operand string
	}{
		{name: "scope equal to the waived directory", scope: apprun.ScopeOverrides{ScopeReplace: []string{"target"}}, want: apprun.ConflictCLI, operand: "target"},
		{name: "include below it, even when absent", scope: apprun.ScopeOverrides{Include: []string{"target/missing/x"}}, want: apprun.ConflictCLI, operand: "target/missing/x"},
		{name: "default scope below it", def: []string{"src", "target/debug"}, want: apprun.ConflictConfig, operand: "target/debug"},
		{name: "the CLI conflict wins over the config one", def: []string{"target"}, scope: apprun.ScopeOverrides{Include: []string{"target/x"}}, want: apprun.ConflictCLI, operand: "target/x"},
		{name: "a nested waiver below the default scope", dirs: []string{"artifacts/cache"}, def: []string{"artifacts/cache/input"}, want: apprun.ConflictConfig, operand: "artifacts/cache/input"},
		{name: "the root itself", scope: apprun.ScopeOverrides{ScopeReplace: []string{"."}}},
		{name: "a strict ancestor", dirs: []string{"artifacts/cache"}, def: []string{"artifacts"}},
		{name: "a textual-prefix sibling", scope: apprun.ScopeOverrides{Include: []string{"target2"}}},
		{name: "a default scope replaced by --scope", def: []string{"target"}, scope: apprun.ScopeOverrides{ScopeReplace: []string{"."}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, probe := newValidationHarness(t, tc.dirs...)
			if tc.def != nil {
				h.cfg.Run.DefaultScope = tc.def
			}
			req := h.request("check")
			req.Scope = tc.scope

			_, runErr := h.svc.Run(context.Background(), req)
			_, explainErr := h.svc.Explain(context.Background(), apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: req})

			if tc.want == 0 {
				if runErr != nil || explainErr != nil {
					t.Fatalf("run=%v explain=%v, want no conflict", runErr, explainErr)
				}
				return
			}
			for name, err := range map[string]error{"run": runErr, "explain": explainErr} {
				var conflict *apprun.ValidationConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("%s err = %v, want a validation conflict", name, err)
				}
				if conflict.Source != tc.want || conflict.Operand != tc.operand || conflict.Policy != "check" {
					t.Errorf("%s conflict = %+v, want source %v operand %q", name, conflict, tc.want, tc.operand)
				}
			}
			if probe.scans != 0 || h.runner.calls != 0 {
				t.Errorf("a refused invocation scanned %d times and executed %d times", probe.scans, h.runner.calls)
			}
		})
	}
}

func TestUnmatchedInvocationIgnoresAPolicysDirectories(t *testing.T) {
	h, _ := newValidationHarness(t)
	req := h.request("check", "--extra")
	req.Scope = apprun.ScopeOverrides{Include: []string{"target/x"}}
	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("an unmatched command was refused: %v", err)
	}
	if res.Execution.KeyInput.Validation != nil {
		t.Fatal("an unmatched command borrowed the policy")
	}
}

func TestValidationReplaysAResultDespiteWaivedChanges(t *testing.T) {
	h, _ := newValidationHarness(t)
	h.runner.onRun = func() { h.writeUnder(t, "target/debug/out.bin", h.runner.stdout) }

	first, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Written || first.Execution.KeyInput.Validation == nil {
		t.Fatalf("first run written=%v validation=%v reuse=%v, want a reusable waived result", first.Written, first.Execution.KeyInput.Validation, first.Reuse)
	}

	if err := os.RemoveAll(filepath.Join(h.rootAbs, "target")); err != nil {
		t.Fatal(err)
	}
	hit, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}
	if hit.Status != runcache.CacheHit || h.runner.calls != 1 {
		t.Fatalf("status=%v calls=%d, want a replay after the waived directory was removed", hit.Status, h.runner.calls)
	}
	if _, err := os.Stat(filepath.Join(h.rootAbs, "target")); !os.IsNotExist(err) {
		t.Errorf("a replay recreated the waived directory: %v", err)
	}

	h.write(t, "main.src", "v2")
	miss, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}
	if miss.Status == runcache.CacheHit || h.runner.calls != 2 {
		t.Fatalf("a required input edit replayed: status=%v calls=%d", miss.Status, h.runner.calls)
	}
}

func TestWithoutAPolicyTheSameCommandIsNotReusable(t *testing.T) {
	h, _ := newValidationHarness(t)
	h.cfg.Run.Validation = nil
	h.runner.onRun = func() { h.writeUnder(t, "target/debug/out.bin", "x") }
	res, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Written || res.Reuse.Reason() != runcache.ReasonEffectStateDiffers {
		t.Fatalf("written=%v reason=%v, want the ordinary effect-state guard", res.Written, res.Reuse.Reason())
	}
}

func TestValidationKeepsEveryGateOutsideItsDirectories(t *testing.T) {
	t.Run("an unwaived effect root changes", func(t *testing.T) {
		h, _ := newValidationHarness(t)
		h.runner.onRun = func() {
			h.writeUnder(t, "target/out", "x")
			h.writeUnder(t, "node_modules/pkg/index.js", "x")
		}
		res, err := h.svc.Run(context.Background(), h.request("check"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Written || res.Reuse.Reason() != runcache.ReasonEffectStateDiffers {
			t.Fatalf("written=%v reason=%v", res.Written, res.Reuse.Reason())
		}
	})
	t.Run("an input outside the waiver changes", func(t *testing.T) {
		h, _ := newValidationHarness(t)
		h.runner.onRun = func() { h.write(t, "main.src", "rewritten") }
		res, err := h.svc.Run(context.Background(), h.request("check"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Written || res.Reuse.Reason() != runcache.ReasonMutatedState {
			t.Fatalf("written=%v reason=%v", res.Written, res.Reuse.Reason())
		}
	})
	t.Run("the post-run scan fails", func(t *testing.T) {
		h, probe := newValidationHarness(t)
		probe.failAt = 2
		res, err := h.svc.Run(context.Background(), h.request("check"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Written || res.Reuse.Kind() != runcache.ReuseUnknownPostState {
			t.Fatalf("written=%v reuse=%v", res.Written, res.Reuse)
		}
	})
	for name, policy := range map[string]apprun.Policy{"record": {Record: true}, "no-cache": {NoCache: true}} {
		t.Run(name, func(t *testing.T) {
			h, _ := newValidationHarness(t)
			req := h.request("check")
			req.Policy = policy
			res, err := h.svc.Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Written {
				t.Fatal("record-only or no-cache published a pointer under a policy")
			}
		})
	}
	t.Run("unkeyed stdin", func(t *testing.T) {
		h, _ := newValidationHarness(t)
		req := h.request("check")
		req.StdinMode = runcache.StdinPipe
		res, err := h.svc.Run(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Written || res.Reuse.Reason() != runcache.ReasonStdinNotKeyed {
			t.Fatalf("written=%v reason=%v", res.Written, res.Reuse.Reason())
		}
	})
}

func TestRecordedWaiverIsNeverRebuiltFromCurrentConfig(t *testing.T) {
	h, _ := newValidationHarness(t)
	first, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}

	h.cfg.Run.Validation[0].UnobservedDirs = []string{"target", "build"}
	h.cfg.Run.Validation[0].Name = "renamed"
	entries, err := lsEntries(h.svc, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil {
		t.Fatal(err)
	}
	row := onlyEntry(t, entries)
	if row.Reusable() || row.Reusability.Reason() != runcache.ReasonConfigMismatch {
		t.Fatalf("changed policy semantics: reusable=%v reason=%v", row.Reusable(), row.Reusability.Reason())
	}
	if got := row.Entry.KeyInput.Validation; got == nil || got.Label != "check" || !slices.Equal(got.UnobservedDirs, []string{"target"}) {
		t.Fatalf("the stored run reports %+v, want its recorded policy", got)
	}

	h.cfg.Run.Validation[0].UnobservedDirs = []string{"target"}
	entries, err = lsEntries(h.svc, h.lsRequest())
	if err != nil {
		t.Fatal(err)
	}
	if row := onlyEntry(t, entries); row.ID != first.RunID {
		t.Fatalf("a label-only change lost reuse: %+v", row)
	}
}

func TestOrdinaryRecordNeverAcquiresAWaiver(t *testing.T) {
	h, _ := newValidationHarness(t)
	h.cfg.Run.Validation = nil
	if _, err := h.svc.Run(context.Background(), h.request("check")); err != nil {
		t.Fatal(err)
	}
	h.cfg.Run.Validation = []config.ValidationPolicy{{Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: []string{"target"}}}
	entries, err := lsEntries(h.svc, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil {
		t.Fatal(err)
	}
	row := onlyEntry(t, entries)
	if row.Reusable() || row.Entry.KeyInput.Validation != nil {
		t.Fatalf("an ordinary record became reusable under a later policy: %+v", row)
	}
	res, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == runcache.CacheHit {
		t.Fatal("an ordinary record served a waived invocation")
	}
}

func TestRecordedScopeConflictingWithTodaysPolicyIsAMismatchNotDamage(t *testing.T) {
	h, _ := newValidationHarness(t)
	h.cfg.Run.Validation = nil
	req := h.request("check")
	req.Scope = apprun.ScopeOverrides{ScopeReplace: []string{"target"}}
	h.writeUnder(t, "target/in", "x")
	first, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	h.cfg.Run.Validation = []config.ValidationPolicy{{Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: []string{"target"}}}
	entries, err := lsEntries(h.svc, apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true})
	if err != nil {
		t.Fatalf("ls aborted on a recorded scope: %v", err)
	}
	if row := onlyEntry(t, entries); row.Reusable() || row.Reusability.Reason() != runcache.ReasonConfigMismatch {
		t.Fatalf("row = %+v, want config-mismatch", row)
	}

	_, err = h.svc.Explain(context.Background(), apprun.ExplainRequest{Mode: apprun.ModeFromRunToNow, Request: apprun.Request{Project: h.proj, Config: h.cfg}, RunRef: first.RunID.String()})
	var conflict *apprun.ValidationConflictError
	if !errors.As(err, &conflict) || conflict.Source != apprun.ConflictRecorded || errors.Is(err, runcache.ErrCorruptStore) {
		t.Fatalf("explain --from-run = %v, want a located recorded-scope refusal", err)
	}
	if _, err := h.store.Get(first.RunID); err != nil {
		t.Fatalf("the stored run became unreadable: %v", err)
	}
}

// A restore from a run state can refuse a deletion for policy-incompatible before the
// waiver's boundary is consulted, so the published boundary is checked on its own: a
// fresh store reads both observations back with the waived subtree as an exclusion.
func TestPublishedObservationsKeepTheWaivedBoundary(t *testing.T) {
	h, _ := newValidationHarness(t, "gen")
	h.writeUnder(t, "gen/before.bin", "x")
	h.runner.onRun = func() { h.writeUnder(t, "gen/after.bin", "x") }
	res, err := h.svc.Run(context.Background(), h.request("check"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Written {
		t.Fatalf("reuse = %v, want a published waived run", res.Reuse)
	}

	layout, err := h.proj.Paths()
	if err != nil {
		t.Fatal(err)
	}
	reopened := runstore.New(layout, blake3hash.New())
	for _, after := range []bool{false, true} {
		read, err := reopened.Observation(res.RunID, after)
		if err != nil {
			t.Fatal(err)
		}
		if read.Coverage().Kind() != worktree.CoverageRecorded {
			t.Fatalf("after=%v coverage = %v, want recorded", after, read.Coverage().Kind())
		}
		tracker, err := worktree.NewAbsenceTracker(context.Background(), read.Coverage())
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			path string
			want worktree.AbsenceVerdict
		}{
			{"gen/after.bin", worktree.AbsenceExcluded},
			{"gen/before.bin", worktree.AbsenceExcluded},
			{"genx", worktree.AbsenceProved},
		} {
			got, err := tracker.Absent(mustRelPath(t, tc.path))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("after=%v Absent(%s) = %v, want %v", after, tc.path, got, tc.want)
			}
		}
	}
}

func mustRelPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type waiverRecordingObserver struct {
	inner apprun.EffectObserver
	calls [][]string
}

func (o *waiverRecordingObserver) Observe(project string, roots, waived []string) (runcache.EffectObservation, runcache.EffectReport, error) {
	o.calls = append(o.calls, slices.Clone(waived))
	return o.inner.Observe(project, roots, waived)
}

func TestLsObservesOnlyTheWaiverSetsItsReKeyedCandidatesUse(t *testing.T) {
	obs := &waiverRecordingObserver{}
	h := newHarness(t, withEffectObserverDecorator(func(inner apprun.EffectObserver) apprun.EffectObserver {
		obs.inner = inner
		return obs
	}))
	t.Cleanup(h.cleanup)
	h.cfg.Run.Validation = []config.ValidationPolicy{{Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: []string{"target"}}}
	h.write(t, "main.src", "v1")
	run := func(req apprun.Request) {
		t.Helper()
		if _, err := h.svc.Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	ls := func(req apprun.LsRequest) [][]string {
		t.Helper()
		obs.calls = nil
		if _, err := h.svc.Ls(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return obs.calls
	}
	target := [][]string{{"target"}}

	run(h.request("check"))
	if got := ls(h.lsRequest()); !reflect.DeepEqual(got, target) {
		t.Fatalf("policy-only window observed %q, want only the waived set", got)
	}

	run(h.request("other"))
	got := ls(h.lsRequest())
	if len(got) != 2 || !slices.ContainsFunc(got, func(w []string) bool { return len(w) == 0 }) || !slices.ContainsFunc(got, func(w []string) bool { return slices.Equal(w, []string{"target"}) }) {
		t.Fatalf("mixed window observed %q, want each set once", got)
	}

	filtered := h.lsRequest()
	filtered.Command = "check"
	if got := ls(filtered); !reflect.DeepEqual(got, target) {
		t.Fatalf("a filtered-out candidate was observed for: %q", got)
	}
}

func TestLsDoesNotObserveEffectsForAShortCircuitedCandidate(t *testing.T) {
	obs := &waiverRecordingObserver{}
	h := newHarness(t, withEffectObserverDecorator(func(inner apprun.EffectObserver) apprun.EffectObserver {
		obs.inner = inner
		return obs
	}))
	t.Cleanup(h.cleanup)
	h.cfg.Run.Validation = []config.ValidationPolicy{{Name: "check", Argv: []string{"check"}, CWD: ".", UnobservedDirs: []string{"target"}}}
	req := h.request("check")
	req.Policy = apprun.Policy{Record: true}
	if _, err := h.svc.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	obs.calls = nil
	res, err := h.svc.Ls(context.Background(), apprun.LsRequest{Project: h.proj, Config: h.cfg, All: true, Near: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.NearMisses) != 1 || len(obs.calls) != 0 {
		t.Fatalf("near=%d observations=%q, want the record-only run listed without any effect walk", len(res.NearMisses), obs.calls)
	}
}
