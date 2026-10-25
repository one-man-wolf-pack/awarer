package run_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	if exp.Subject.Exact.Assessment != apprun.ExactComplete || exp.Subject.Exact.Hit == nil || exp.Subject.Exact.Hit.RunID != runRes.RunID {
		t.Fatalf("exact match = %+v, want complete with run %s", exp.Subject.Exact, runRes.RunID)
	}
	if !exp.Subject.Exact.Hit.Healthy {
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
	if exp.Subject.Exact.Assessment != apprun.ExactComplete || exp.Subject.Exact.Hit == nil {
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
	if exp.Subject.Exact.Assessment != apprun.ExactComplete || exp.Subject.Exact.Hit == nil || !exp.Subject.Exact.Hit.Healthy {
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

func TestExplainModesShareTheUnavailableExactAssessment(t *testing.T) {
	failure := &fs.PathError{Op: "open", Path: "entry", Err: fs.ErrPermission}
	cases := []struct {
		name   string
		inject func(*lookupStore)
		want   apprun.ExactAssessment
		found  bool
	}{
		{"lookup", func(s *lookupStore) { s.failWith = failure }, apprun.ExactLookupUnavailable, false},
		{"payload verification", func(s *lookupStore) { s.stdoutErr = failure }, apprun.ExactHealthUnavailable, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &lookupStore{}
			h := newHarness(t, withStoreDecorator(func(inner runcache.Store) runcache.Store {
				store.Store = inner
				return store
			}))
			defer h.cleanup()
			run, err := h.svc.Run(context.Background(), h.request("echo", "x"))
			if err != nil {
				t.Fatal(err)
			}
			h.cfg.Run.CaptureOutput = false
			store.answerAs = &run.Key
			c.inject(store)

			command, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
			if err != nil {
				t.Fatalf("command explain: %v", err)
			}
			last, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{Mode: apprun.ModeLast, Request: apprun.Request{Project: h.proj, Config: h.cfg}})
			if err != nil {
				t.Fatalf("explain --last: %v", err)
			}
			for mode, res := range map[string]apprun.ExplainResult{"command": command, "last": last} {
				exact := res.Subject.Exact
				if res.Outcome != apprun.OutcomeUncached || res.Reason != "capture-disabled" || len(res.Warnings) == 0 {
					t.Errorf("%s = %s/%s warnings %q, want uncached/capture-disabled with a warning", mode, res.Outcome, res.Reason, res.Warnings)
				}
				if exact.Assessment != c.want || (exact.Hit != nil) != c.found || (c.found && exact.Hit.RunID != run.RunID) {
					t.Errorf("%s exact = %+v, want assessment %d found %v (run %s)", mode, exact, c.want, c.found, run.RunID)
				}
			}
			if c.found && (len(last.Candidates) != 1 || last.Candidates[0].HealthChecked) {
				t.Errorf("stored candidate = %+v, want its unverified health left unchecked", last.Candidates)
			}
		})
	}
}

func TestExplainCancelledDuringTheExactLookupFails(t *testing.T) {
	store := &lookupStore{}
	h := newHarness(t, withStoreDecorator(func(inner runcache.Store) runcache.Store {
		store.Store = inner
		return store
	}))
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	h.cfg.Run.CaptureOutput = false
	requests := map[string]apprun.ExplainRequest{
		"command": h.explainCmd("echo", "x"),
		"last":    {Mode: apprun.ModeLast, Request: apprun.Request{Project: h.proj, Config: h.cfg}},
	}
	for mode, req := range requests {
		ctx, cancel := context.WithCancel(context.Background())
		store.onLookup = cancel
		res, err := h.svc.Explain(ctx, req)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s explain = %s/%s err %v, want the cancellation", mode, res.Outcome, res.Reason, err)
		}
	}
}

type candidateReadFailStore struct {
	runcache.Store
	failID runcache.RunID
	err    error
	onRead func()
}

func (s *candidateReadFailStore) OpenStdout(id runcache.RunID) (io.ReadCloser, error) {
	if id == s.failID && s.onRead != nil {
		s.onRead()
	}
	if id == s.failID && s.err != nil {
		return nil, s.err
	}
	return s.Store.OpenStdout(id)
}

func TestExplainStoredDistinctCandidateReadFailurePropagates(t *testing.T) {
	failure := &fs.PathError{Op: "open", Path: "candidate", Err: fs.ErrPermission}
	store := &candidateReadFailStore{err: failure}
	h := newHarness(t, withStoreDecorator(func(inner runcache.Store) runcache.Store {
		store.Store = inner
		return store
	}))
	defer h.cleanup()
	h.write(t, "a.txt", "1")
	older, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	h.write(t, "a.txt", "2")
	current, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x")); err != nil ||
		exp.Subject.Exact.Assessment != apprun.ExactComplete || exp.Subject.Exact.Hit == nil ||
		exp.Subject.Exact.Hit.RunID != current.RunID || !exp.Subject.Exact.Hit.Healthy {
		t.Fatalf("precondition: exact = %+v err %v, want healthy %s", exp.Subject.Exact, err, current.RunID)
	}
	store.failID = older.RunID

	res, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeFromRunToNow,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
		RunRef:  older.RunID.String(),
	})
	if !errors.Is(err, failure) {
		t.Fatalf("explain --from-run = %+v err %v, want the candidate read failure", res, err)
	}
}

func TestExplainStoredCancelledDuringADistinctCandidateReadFails(t *testing.T) {
	store := &candidateReadFailStore{}
	h := newHarness(t, withStoreDecorator(func(inner runcache.Store) runcache.Store {
		store.Store = inner
		return store
	}))
	defer h.cleanup()
	older, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	refresh := h.request("echo", "x")
	refresh.Policy.Refresh = true
	current, err := h.svc.Run(context.Background(), refresh)
	if err != nil {
		t.Fatal(err)
	}
	if older.Key != current.Key || older.RunID == current.RunID {
		t.Fatalf("runs %s/%s keys %s/%s, want distinct runs sharing one key", older.RunID, current.RunID, older.Key, current.Key)
	}
	store.failID = older.RunID
	explainOlder := func(ctx context.Context) (apprun.ExplainResult, error) {
		return h.svc.Explain(ctx, apprun.ExplainRequest{
			Mode:    apprun.ModeFromRunToNow,
			Request: apprun.Request{Project: h.proj, Config: h.cfg},
			RunRef:  older.RunID.String(),
		})
	}

	res, err := explainOlder(context.Background())
	if err != nil || res.Outcome != apprun.OutcomeHit || res.Subject.Exact.Hit == nil ||
		res.Subject.Exact.Hit.RunID != current.RunID || len(res.Candidates) != 1 || !res.Candidates[0].HealthChecked {
		t.Fatalf("uncancelled explain = %+v err %v, want a hit on %s with the candidate verified", res, err, current.RunID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.onRead = cancel
	if res, err := explainOlder(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled explain = %s/%s err %v, want the cancellation", res.Outcome, res.Reason, err)
	}
}

func TestExplainFromRunCorruptSourceFails(t *testing.T) {
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

	exp, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeFromRunToNow,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
		RunRef:  res.RunID.String(),
	})
	if !errors.Is(err, runcache.ErrCorruptStore) || !strings.Contains(err.Error(), res.RunID.Short()) {
		t.Fatalf("explain --from-run corrupt source = %+v err %v, want a corruption error naming %s", exp, err, res.RunID.Short())
	}
	if exp.Outcome != "" || exp.Reason != "" || !exp.Subject.Key.IsZero() || exp.Warnings != nil {
		t.Errorf("explain returned a partial result alongside its error: %+v", exp)
	}
}

type corruptOnSourceReadStore struct {
	runcache.Store
	target runcache.RunID
	reads  int
}

func (s *corruptOnSourceReadStore) Get(id runcache.RunID) (runcache.RunEntry, error) {
	if id == s.target {
		s.reads++
		if s.reads > 1 {
			return runcache.RunEntry{}, fmt.Errorf("%w: injected", runcache.ErrCorruptStore)
		}
	}
	return s.Store.Get(id)
}

func TestExplainLastSourceCorruptAfterSelectionFails(t *testing.T) {
	store := &corruptOnSourceReadStore{}
	h := newHarness(t, withStoreDecorator(func(inner runcache.Store) runcache.Store {
		store.Store = inner
		return store
	}))
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "old")); err != nil {
		t.Fatal(err)
	}
	h.clock.now = h.clock.now.Add(time.Second)
	newest, err := h.svc.Run(context.Background(), h.request("echo", "new"))
	if err != nil {
		t.Fatal(err)
	}
	store.target = newest.RunID

	exp, err := h.svc.Explain(context.Background(), apprun.ExplainRequest{
		Mode:    apprun.ModeLast,
		Request: apprun.Request{Project: h.proj, Config: h.cfg},
	})
	if !errors.Is(err, runcache.ErrCorruptStore) || !strings.Contains(err.Error(), newest.RunID.Short()) {
		t.Fatalf("explain --last = %+v err %v, want the selected run's corruption naming %s", exp, err, newest.RunID.Short())
	}
	if store.reads != 2 {
		t.Errorf("target metadata read %d times, want selection plus one source read", store.reads)
	}
}
