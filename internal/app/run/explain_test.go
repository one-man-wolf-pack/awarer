package run_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
)

// explainCmd builds a command-mode explain request for argv.
func (h *harness) explainCmd(argv ...string) apprun.ExplainRequest {
	return apprun.ExplainRequest{Mode: apprun.ModeCommand, Request: h.request(argv...)}
}

func TestExplainComputesSameKeyAsRun(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "1")

	// Explain before any run computes the key the run will use; running it must
	// produce the identical key, so explain can never diverge from run.
	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "no-entry-for-key" {
		t.Errorf("pre-run explain = %s/%s, want miss/no-entry-for-key", exp.Outcome, exp.Reason)
	}
	runRes, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Subject.Key != runRes.Key {
		t.Errorf("explain key %s != run key %s", exp.Subject.Key, runRes.Key)
	}
}

func TestExplainExactHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	runRes, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	calls := h.runner.calls

	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeHit || exp.Reason != "exact-key" {
		t.Errorf("explain = %s/%s, want hit/exact-key", exp.Outcome, exp.Reason)
	}
	if exp.Subject.ExactHit == nil || exp.Subject.ExactHit.RunID != runRes.RunID {
		t.Errorf("exact hit run id mismatch: %+v", exp.Subject.ExactHit)
	}
	if !exp.Subject.ExactHit.Healthy {
		t.Error("exact hit should be healthy")
	}
	if h.runner.calls != calls {
		t.Error("explain must never execute the command")
	}
}

func TestExplainNoCacheDisabled(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	req := h.explainCmd("echo", "x")
	req.Request.Policy.NoCache = true

	exp, err := h.svc.Explain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeDisabled || exp.Reason != "no-cache" {
		t.Errorf("explain = %s/%s, want disabled/no-cache", exp.Outcome, exp.Reason)
	}
	// The entry still exists; explain reports it for information even though the run
	// would ignore it under --no-cache.
	if exp.Subject.ExactHit == nil {
		t.Error("explain should still surface the existing exact entry under --no-cache")
	}
}

func TestExplainRefreshRequested(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	req := h.explainCmd("echo", "x")
	req.Request.Policy.Refresh = true

	exp, err := h.svc.Explain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "refresh-requested" {
		t.Errorf("explain = %s/%s, want miss/refresh-requested", exp.Outcome, exp.Reason)
	}
	if !exp.Subject.Cacheable {
		t.Error("a --refresh run is still cacheable (it writes)")
	}
}

func TestExplainStdinNotKeyed(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	req := h.explainCmd("cat")
	req.Request.StdinMode = runcache.StdinPipe

	exp, err := h.svc.Explain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeUncached || exp.Reason != "stdin-not-keyed" {
		t.Errorf("explain = %s/%s, want uncached/stdin-not-keyed", exp.Outcome, exp.Reason)
	}
}

func TestExplainTTYNotKeyed(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	req := h.explainCmd("cat")
	req.Request.StdinMode = runcache.StdinTTY
	req.Request.TTYAllowed = true

	exp, err := h.svc.Explain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeUncached || exp.Reason != "tty-not-keyed" {
		t.Errorf("explain = %s/%s, want uncached/tty-not-keyed", exp.Outcome, exp.Reason)
	}
}

func TestExplainSkippedInputs(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Hashing.MaxFileSize = 1
	h.cfg.Hashing.LargeFilePolicy = config.LargeFileSkip
	h.write(t, "big.txt", "more than one byte")

	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeUncached || exp.Reason != "skipped-inputs" {
		t.Errorf("explain = %s/%s, want uncached/skipped-inputs", exp.Outcome, exp.Reason)
	}
	if exp.Subject.Skipped.Count == 0 {
		t.Error("explain should report the skipped input count")
	}
}

func TestExplainInputTreeChanged(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "one")
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	h.write(t, "a.txt", "two")

	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "input-tree-differs" {
		t.Errorf("explain = %s/%s, want miss/input-tree-differs", exp.Outcome, exp.Reason)
	}
	if len(exp.Candidates) == 0 {
		t.Fatal("expected a nearest candidate")
	}
	c := exp.Candidates[0]
	if !contains(c.Score.Different, "input_tree") {
		t.Errorf("candidate different = %v, want input_tree", c.Score.Different)
	}
	if !contains(c.Score.Matched, "command") || !contains(c.Score.Matched, "cwd") {
		t.Errorf("candidate matched = %v, want command and cwd", c.Score.Matched)
	}
	if !c.HealthChecked || !c.Healthy {
		t.Error("returned candidate should have verified, healthy payloads")
	}
}

func TestExplainCwdChanged(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if err := os.Mkdir(filepath.Join(h.rootAbs, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	req := h.explainCmd("echo", "x")
	cwd, _ := runcache.NewExecutionCWD("sub")
	req.Request.CWD = cwd
	req.Request.AbsDir = filepath.Join(h.rootAbs, "sub")

	exp, err := h.svc.Explain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "cwd-mismatch" {
		t.Errorf("explain = %s/%s, want miss/cwd-mismatch", exp.Outcome, exp.Reason)
	}
}

func TestExplainEnvChanged(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["CI"] = "true"
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	h.env.vars["CI"] = "false"

	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "env-mismatch" {
		t.Errorf("explain = %s/%s, want miss/env-mismatch", exp.Outcome, exp.Reason)
	}
	var sawCI bool
	for _, d := range exp.Differences.Differences {
		if d.Code == runcache.DiffEnvChanged && d.EnvName == "CI" {
			sawCI = true
		}
	}
	if !sawCI {
		t.Error("expected an env difference for CI")
	}
}

func TestExplainNoCacheFailuresIgnoresCachedFailure(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 1}
	// Cache a failure under the default policy.
	if _, err := h.svc.Run(context.Background(), h.request("false")); err != nil {
		t.Fatal(err)
	}

	// Plain explain hits the cached failure...
	plain, err := h.svc.Explain(context.Background(), h.explainCmd("false"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Outcome != apprun.OutcomeHit {
		t.Errorf("plain explain = %s, want hit", plain.Outcome)
	}

	// ...but under --no-cache-failures the run would re-execute, so explain must say
	// miss, not hit — even though the entry exists and is healthy.
	req := h.explainCmd("false")
	req.Request.Policy.NoCacheFailures = true
	exp, err := h.svc.Explain(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "cached-failure-ignored" {
		t.Errorf("explain = %s/%s, want miss/cached-failure-ignored", exp.Outcome, exp.Reason)
	}
	if exp.Subject.ExactHit == nil || !exp.Subject.ExactHit.Healthy {
		t.Error("the cached failure should still be reported as an existing healthy entry")
	}
}

func TestExplainTTLExpired(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.TTL = config.Duration(time.Hour)
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	// Within the TTL, explain hits.
	if exp, _ := h.svc.Explain(context.Background(), h.explainCmd("echo", "x")); exp.Outcome != apprun.OutcomeHit {
		t.Fatalf("within TTL explain = %s, want hit", exp.Outcome)
	}

	// Past the TTL, the run would re-execute: explain reports miss/ttl-expired.
	h.clock.now = h.clock.now.Add(2 * time.Hour)
	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "ttl-expired" {
		t.Errorf("explain past TTL = %s/%s, want miss/ttl-expired", exp.Outcome, exp.Reason)
	}
}

func TestExplainExactEntryUnhealthy(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.stdout = "trusted\n"
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(h.rootAbs, ".awa", "runs", "entries",
		res.RunID.String()[:2], res.RunID.String(), "stdout.log")
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}

	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeCorrupt || exp.Reason != "exact-entry-unhealthy" {
		t.Errorf("explain = %s/%s, want corrupt/exact-entry-unhealthy", exp.Outcome, exp.Reason)
	}
}

func TestExplainCorruptMetadataSurfaced(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(h.rootAbs, ".awa", "runs", "entries",
		res.RunID.String()[:2], res.RunID.String(), "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Explain a different command: the exact key misses cleanly, and the corrupt
	// entry must be counted/warned, not silently treated as "no match".
	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "y"))
	if err != nil {
		t.Fatalf("explain should not fail on a corrupt candidate: %v", err)
	}
	if exp.Outcome != apprun.OutcomeMiss {
		t.Errorf("outcome = %s, want miss", exp.Outcome)
	}
	if len(exp.Warnings) == 0 {
		t.Error("expected a warning about the corrupt stored run")
	}
}

func TestExplainLast(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	exp, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeLast,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeHit {
		t.Errorf("--last on an unchanged workspace = %s, want hit", exp.Outcome)
	}
}

func TestExplainLastReproducesStoredScope(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if err := os.Mkdir(filepath.Join(h.rootAbs, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.write(t, "sub/a.txt", "x")
	h.write(t, "other.txt", "y") // outside the scoped subtree

	req := h.request("echo", "x")
	req.Scope = apprun.ScopeOverrides{ScopeReplace: []string{"sub"}}
	if _, err := h.svc.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	// --last on the unchanged workspace must reproduce the stored --scope subtree and
	// hit, not report scope-changed against the current default scope.
	exp, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeLast,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeHit || exp.Reason != "exact-key" {
		t.Errorf("--last after a --scope run = %s/%s, want hit/exact-key", exp.Outcome, exp.Reason)
	}
}

func TestExplainFromRunToNow(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "one")
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}

	// Unchanged: a re-invocation of the stored run hits.
	exp, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeFromRunToNow,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
		RunRef:  res.RunID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeHit {
		t.Errorf("from-run unchanged = %s, want hit", exp.Outcome)
	}

	// After editing an input, the same stored run would now miss on the tree.
	h.write(t, "a.txt", "two")
	exp2, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeFromRunToNow,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
		RunRef:  res.RunID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp2.Outcome != apprun.OutcomeMiss || exp2.Reason != "input-tree-differs" {
		t.Errorf("from-run after edit = %s/%s, want miss/input-tree-differs", exp2.Outcome, exp2.Reason)
	}
}

func TestExplainFromRunRespectsTTL(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.TTL = config.Duration(time.Hour)
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}

	// The stored run is unchanged, but past the TTL a real run re-executes. Stored-mode
	// explain must use the actual lookup/TTL rules, not just match the stored entry.
	h.clock.now = h.clock.now.Add(2 * time.Hour)
	exp, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeFromRunToNow,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
		RunRef:  res.RunID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome != apprun.OutcomeMiss || exp.Reason != "ttl-expired" {
		t.Errorf("from-run past TTL = %s/%s, want miss/ttl-expired", exp.Outcome, exp.Reason)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
