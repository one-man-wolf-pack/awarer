package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/effectobserve"
	"awarer/internal/app/initcmd"
	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/evidence"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
)

// fakeRunner records calls and writes canned output instead of executing a real
// process, so app tests are deterministic and assert when the runner is (not)
// invoked.
type fakeRunner struct {
	calls    int
	stdout   string
	stderr   string
	exit     runcache.ExitStatus
	startErr bool
	clock    *fakeClock
	gotEnv   []string // env of the most recent Run
	// onRun, when set, runs after output is written and before the result is
	// returned, so a test can simulate a side-effecting command that mutates project
	// state (a formatter/fixer) between the pre-run and post-run mutation scans.
	onRun func()
}

func (f *fakeRunner) Run(_ context.Context, spec runcache.RunSpec) (runcache.RunResult, error) {
	f.calls++
	f.gotEnv = spec.Env
	if f.startErr {
		return runcache.RunResult{}, fmt.Errorf("%w: boom", runcache.ErrStartFailed)
	}
	_, _ = io.WriteString(spec.Stdout, f.stdout)
	_, _ = io.WriteString(spec.Stderr, f.stderr)
	if f.onRun != nil {
		f.onRun()
	}
	now := f.clock.Now()
	return runcache.RunResult{Exit: f.exit, StartedAt: now, FinishedAt: now.Add(time.Millisecond)}, nil
}

// fakeResolver records the environment it was asked to resolve under, so a test can
// prove the executable is looked up with the same environment the child receives
// rather than with awa's own.
type fakeResolver struct{ gotEnv []string }

func (r *fakeResolver) Resolve(_ string, _ string, env []string) (string, string, bool) {
	r.gotEnv = env
	return "", "", false
}

type mapEnv struct{ vars map[string]string }

func (m mapEnv) Lookup(name string) (string, bool) {
	v, ok := m.vars[name]
	return v, ok
}

// fakeClock is a controllable clock so tests can advance time past the run TTL.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type harness struct {
	svc      *apprun.Service
	runner   *fakeRunner
	resolver *fakeResolver
	env      *mapEnv
	clock    *fakeClock
	store    *runstore.Repo
	proj     projfs.Project
	cfg      config.Config
	rootAbs  string
	cleanup  func()
}

// harnessOpt customizes a harness before its service is built.
type harnessOpt func(*harnessConfig)

type harnessConfig struct {
	decorateScanner        func(apprun.Scanner) apprun.Scanner
	decorateIndex          func(worktree.Index) worktree.Index
	decorateEffectObserver func(apprun.EffectObserver) apprun.EffectObserver
}

// withScannerDecorator wraps the run service's scanner, letting a test inject
// failures (e.g. a post-run scan error) around the real scanner.
func withScannerDecorator(f func(apprun.Scanner) apprun.Scanner) harnessOpt {
	return func(c *harnessConfig) { c.decorateScanner = f }
}

// withIndexDecorator wraps the worktree index the run service's scanner is built on,
// letting a test control what the scan's reuse lookups are told. It reaches a layer
// withScannerDecorator cannot: a scanner decorator can only replace whole scan results,
// while the property under test is what the scan does with an index that disagrees with
// the disk.
func withIndexDecorator(f func(worktree.Index) worktree.Index) harnessOpt {
	return func(c *harnessConfig) { c.decorateIndex = f }
}

// withEffectObserverDecorator wraps the run service's effect observer, letting a test
// count how many times the watched generated-output roots are actually walked. That
// count is the property under test for the near-miss effect detail: enriching every
// candidate must remain a projection of one observation, so a call count is the only
// evidence that no surface quietly re-observes.
func withEffectObserverDecorator(f func(apprun.EffectObserver) apprun.EffectObserver) harnessOpt {
	return func(c *harnessConfig) { c.decorateEffectObserver = f }
}

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	var hc harnessConfig
	for _, o := range opts {
		o(&hc)
	}
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	proj, err := projfs.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	layout, err := proj.Paths()
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	cfg := config.Defaults()
	hasher := blake3hash.New()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	runner := &fakeRunner{stdout: "default-out\n", exit: runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0}, clock: clock}
	resolver := &fakeResolver{}
	env := &mapEnv{vars: map[string]string{}}
	store := runstore.New(layout, hasher)
	var index worktree.Index = idx
	if hc.decorateIndex != nil {
		index = hc.decorateIndex(index)
	}
	var scan apprun.Scanner = scanner.New(worktreefs.New(), hasher, index)
	if hc.decorateScanner != nil {
		scan = hc.decorateScanner(scan)
	}
	var observer apprun.EffectObserver = effectobserve.New(effectfs.New(hasher), hasher)
	if hc.decorateEffectObserver != nil {
		observer = hc.decorateEffectObserver(observer)
	}
	svc := apprun.New(apprun.Deps{
		Scanner:        scan,
		Store:          store,
		Runner:         runner,
		Resolver:       resolver,
		EffectObserver: observer,
		Env:            env,
		Clock:          clock,
		Hasher:         hasher,
		AwaVersion:     "test",
	})
	return &harness{
		svc: svc, runner: runner, resolver: resolver, env: env, clock: clock, store: store,
		proj: proj, cfg: cfg, rootAbs: layout.Root(),
		cleanup: func() { _ = idx.Close() },
	}
}

func (h *harness) request(argv ...string) apprun.Request {
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		panic(err)
	}
	return apprun.Request{
		Project:   h.proj,
		Config:    h.cfg,
		Argv:      argv,
		CWD:       cwd,
		AbsDir:    h.rootAbs,
		StdinMode: runcache.StdinNull,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	}
}

func (h *harness) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.rootAbs, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestRmReportsWhyMetadataWouldNotDecode pins that an explicit removal names what it
// actually found. The incompatible-evidence guidance sends users to "run rm <id>" to
// discard a record in a schema this build cannot read, so answering "corrupt" there
// would tell them their store is damaged when it is intact — the exact distinction
// the rest of the diagnostics maintain.
func TestRmReportsWhyMetadataWouldNotDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta string
		want evidence.DiagnosticToken
	}{
		{"incompatible schema", `{"schema_version": 99}`, evidence.TokenMetadataIncompatible},
		{"truncated record", `{"schema_version": 1, "id"`, evidence.TokenMetadataCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			defer h.cleanup()
			res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
			if err != nil {
				t.Fatal(err)
			}
			meta := filepath.Join(h.store.EntryPath(res.RunID), "meta.json")
			if err := os.Chmod(meta, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(meta, []byte(tc.meta), 0o644); err != nil {
				t.Fatal(err)
			}

			rm, err := h.svc.Rm(context.Background(), apprun.RmRequest{IDs: []string{res.RunID.String()}})
			if err != nil {
				t.Fatalf("Rm: %v", err)
			}
			if len(rm.Removed) != 0 {
				t.Errorf("a record that will not decode was reported as a clean removal: %+v", rm.Removed)
			}
			if len(rm.RemovedUnreadable) != 1 {
				t.Fatalf("RemovedUnreadable = %+v, want exactly one entry", rm.RemovedUnreadable)
			}
			if got := rm.RemovedUnreadable[0].Reason; got != tc.want {
				t.Errorf("reason = %q, want %q", got, tc.want)
			}
			if rm.RemovedUnreadable[0].ID != res.RunID {
				t.Errorf("removed id = %s, want %s", rm.RemovedUnreadable[0].ID, res.RunID)
			}
		})
	}
}

func TestRmRejectsMixedIDAndFilter(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()

	// Mixing an explicit id with a filter is ambiguous: the id would be deleted
	// regardless of the filter. Rm must reject it before touching the store.
	for _, req := range []apprun.RmRequest{
		{IDs: []string{"deadbeef"}, Command: "go"},
		{IDs: []string{"deadbeef"}, OlderThan: time.Hour, HasOlderThan: true},
	} {
		if _, err := h.svc.Rm(context.Background(), req); err == nil {
			t.Errorf("Rm(%+v) = nil, want an error for mixed id and filter", req)
		}
	}
}

func TestChildEnvIsSanitized(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	// PATH is in the built-in baseline; CI is in the default allowlist; SECRET is in
	// neither. Only the first two may reach the child, and SECRET must not.
	h.env.vars["PATH"] = "/usr/bin"
	h.env.vars["CI"] = "true"
	h.env.vars["SECRET"] = "leak"

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, kv := range h.runner.gotEnv {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("child PATH = %q, want /usr/bin (baseline must be passed)", got["PATH"])
	}
	if got["CI"] != "true" {
		t.Errorf("child CI = %q, want true (allowlisted must be passed)", got["CI"])
	}
	if _, ok := got["SECRET"]; ok {
		t.Error("SECRET leaked to the child: unkeyed env must not be inherited")
	}
}

// An allowlisted environment value — even a secret-looking one — must reach the child
// and key the run, but must never appear as a raw string in any durable run file. The
// record keeps the value's redacted identity (presence + fingerprint) instead, which is
// enough to still hit on an unchanged value and to explain a miss when it changes.
func TestAllowlistedSecretNotPersisted(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()

	const secret = "super-secret-value-for-test"
	h.cfg.Run.EnvAllowlist = append(h.cfg.Run.EnvAllowlist, "NPM_TOKEN")
	h.env.vars["NPM_TOKEN"] = secret

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Status != runcache.CacheMiss {
		t.Fatalf("first status = %s, want miss", res.Status)
	}

	// The value must reach the child (it is a real, keyed input)...
	if !slices.Contains(h.runner.gotEnv, "NPM_TOKEN="+secret) {
		t.Fatalf("child env missing NPM_TOKEN=%s: %v", secret, h.runner.gotEnv)
	}

	// ...but must not appear as a raw string in any durable run file.
	runsDir := filepath.Join(h.rootAbs, ".awa", "runs")
	var metaData []byte
	err = filepath.WalkDir(runsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("raw secret found in durable run file %s", path)
		}
		if filepath.Base(path) == "meta.json" {
			metaData = data
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk runs dir: %v", err)
	}
	// The record keeps the redacted identity: a "set" presence and a fingerprint, so a
	// mismatch is still explainable without the value.
	if metaData == nil {
		t.Fatal("no meta.json found")
	}
	if !bytes.Contains(metaData, []byte(`"presence": "set"`)) {
		t.Errorf("meta.json does not record NPM_TOKEN as presence=set:\n%s", metaData)
	}

	// An unchanged value still hits.
	res2, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.Status != runcache.CacheHit {
		t.Errorf("unchanged secret status = %s, want hit", res2.Status)
	}

	// A changed value misses: the fingerprint keys the run without leaking the value.
	h.env.vars["NPM_TOKEN"] = "a-different-secret"
	res3, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res3.Status == runcache.CacheHit {
		t.Error("changed secret still hit: the value fingerprint must be part of the key")
	}
}

// The partial-evidence proof at the execution level: a command that exceeds the capture
// limit records a truncated stored payload, and a later cache hit that replays it still
// reports the stored evidence as truncated (with the omitted byte count), so a replay
// can never look like a complete one.
func TestTruncatedOutputStaysTruncatedOnReplay(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.MaxStdoutSize = 8
	h.runner.stdout = "this output is far longer than eight bytes\n"

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Status != runcache.CacheMiss {
		t.Fatalf("first status = %s, want miss", res.Status)
	}
	if res.Execution.Output == nil || !res.Execution.Output.Stdout.Truncated {
		t.Fatalf("first run stdout not marked truncated: %+v", res.Execution.Output)
	}
	if res.Execution.Output.Stdout.OmittedBytes <= 0 {
		t.Errorf("omitted bytes = %d, want > 0", res.Execution.Output.Stdout.OmittedBytes)
	}

	res2, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res2.Status != runcache.CacheHit {
		t.Fatalf("replay status = %s, want hit", res2.Status)
	}
	if res2.Execution.Output == nil || !res2.Execution.Output.Stdout.Truncated {
		t.Error("cache-hit replay lost the truncation fact: a partial payload must never look complete")
	}
	if res2.Execution.Output.Stdout.OmittedBytes != res.Execution.Output.Stdout.OmittedBytes {
		t.Errorf("replay omitted = %d, want %d", res2.Execution.Output.Stdout.OmittedBytes, res.Execution.Output.Stdout.OmittedBytes)
	}
}

func TestUnkeyedEnvDoesNotAffectHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()

	h.env.vars["SECRET"] = "1"
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	// Change a variable that is neither baseline nor allowlisted: it is not in the
	// key and not passed to the child, so the next run must still hit.
	h.env.vars["SECRET"] = "2"
	r, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheHit {
		t.Errorf("status = %s, want hit (unkeyed env must not bust the cache)", r.Status)
	}
}

func TestMissThenHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "1")
	h.runner.stdout = "hello-out\n"

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Status != runcache.CacheMiss {
		t.Errorf("first status = %s, want miss", res.Status)
	}
	if !res.Written {
		t.Error("first run should be stored")
	}
	if h.runner.calls != 1 {
		t.Errorf("runner calls = %d, want 1", h.runner.calls)
	}

	res2, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.Status != runcache.CacheHit {
		t.Errorf("second status = %s, want hit", res2.Status)
	}
	if h.runner.calls != 1 {
		t.Errorf("runner called again on hit: calls = %d", h.runner.calls)
	}
	// Replay the hit and confirm stored stdout.
	rc, err := h.svc.OpenStdout(res2.RunID)
	if err != nil {
		t.Fatalf("OpenStdout: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello-out\n" {
		t.Errorf("replayed stdout = %q, want %q", got, "hello-out\n")
	}
}

func TestInputChangeMisses(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "one")

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	// Identical inputs hit.
	r2, _ := h.svc.Run(context.Background(), h.request("echo", "x"))
	if r2.Status != runcache.CacheHit {
		t.Fatalf("expected hit, got %s", r2.Status)
	}
	calls := h.runner.calls

	h.write(t, "a.txt", "two")
	r3, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != runcache.CacheMiss {
		t.Errorf("after input change status = %s, want miss", r3.Status)
	}
	if h.runner.calls != calls+1 {
		t.Errorf("runner not called after input change")
	}
}

// TestGitignoredFileAffectsRunInputByDefault locks the headline run-cache
// correctness rule: .gitignore is off by default, so a file git ignores still
// keys a command. A build output or local fixture that git hides must not be
// invisible to the run input tree.
func TestGitignoredFileAffectsRunInputByDefault(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, ".gitignore", "*.log\n")
	h.write(t, "debug.log", "one")

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	r2, _ := h.svc.Run(context.Background(), h.request("echo", "x"))
	if r2.Status != runcache.CacheHit {
		t.Fatalf("identical inputs should hit, got %s", r2.Status)
	}
	calls := h.runner.calls

	// Changing a gitignored file must miss, because it is a real input by default.
	h.write(t, "debug.log", "two")
	r3, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != runcache.CacheMiss {
		t.Errorf("gitignored file change status = %s, want miss (gitignore off by default)", r3.Status)
	}
	if h.runner.calls != calls+1 {
		t.Error("runner not called after gitignored input changed")
	}
}

// TestHistoryOnlyExcludeDoesNotAffectRunKey confirms a generated-artifact dir
// hidden from history (dist/build/coverage) is still a live run input: changing
// a file under it misses, so a test that depends on build output keys correctly.
func TestHistoryOnlyExcludeDoesNotAffectRunKey(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if err := os.MkdirAll(filepath.Join(h.rootAbs, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.write(t, "dist/bundle.js", "v1")

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	r2, _ := h.svc.Run(context.Background(), h.request("echo", "x"))
	if r2.Status != runcache.CacheHit {
		t.Fatalf("identical inputs should hit, got %s", r2.Status)
	}
	calls := h.runner.calls

	h.write(t, "dist/bundle.js", "v2")
	r3, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != runcache.CacheMiss {
		t.Errorf("dist change status = %s, want miss (history-only excludes do not hide run inputs)", r3.Status)
	}
	if h.runner.calls != calls+1 {
		t.Error("runner not called after dist input changed")
	}
}

func TestCwdChangeMisses(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if err := os.Mkdir(filepath.Join(h.rootAbs, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	calls := h.runner.calls

	req := h.request("echo", "x")
	cwd, _ := runcache.NewExecutionCWD("sub")
	req.CWD = cwd
	req.AbsDir = filepath.Join(h.rootAbs, "sub")
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheMiss {
		t.Errorf("different cwd status = %s, want miss", r.Status)
	}
	if h.runner.calls != calls+1 {
		t.Error("runner not called for new cwd")
	}
}

func TestEnvChangeMisses(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	// The default allowlist includes CI; key on it.
	h.env.vars["CI"] = "true"
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	calls := h.runner.calls

	h.env.vars["CI"] = "false"
	r, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheMiss {
		t.Errorf("env change status = %s, want miss", r.Status)
	}
	if h.runner.calls != calls+1 {
		t.Error("runner not called after env change")
	}
}

func TestFailedRunCachedByDefault(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 3}
	h.runner.stdout = "fail-out\n"

	r, err := h.svc.Run(context.Background(), h.request("false"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", r.ExitCode)
	}
	if !r.Written {
		t.Error("failed run should be cached by default")
	}
	r2, _ := h.svc.Run(context.Background(), h.request("false"))
	if r2.Status != runcache.CacheHit || r2.ExitCode != 3 {
		t.Errorf("second failed run = %s exit %d, want hit exit 3", r2.Status, r2.ExitCode)
	}
}

func TestNoCacheFailures(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 1}

	req := h.request("false")
	req.Policy.NoCacheFailures = true
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Written {
		t.Error("failed run should not be cached with --no-cache-failures")
	}
	// Next run still misses.
	r2, _ := h.svc.Run(context.Background(), h.request("false"))
	if r2.Status == runcache.CacheHit {
		t.Error("should not hit a result that was never stored")
	}
}

func TestNoCacheFailuresIgnoresCachedFailure(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 1}

	// A first plain run caches the failure (default policy caches failures).
	if r, err := h.svc.Run(context.Background(), h.request("false")); err != nil || !r.Written {
		t.Fatalf("first run: written=%v err=%v, want a cached failure", r.Written, err)
	}
	calls := h.runner.calls

	// With --no-cache-failures, the stored failure must not replay: the run is a miss
	// (re-executed) and the failure is not re-stored.
	req := h.request("false")
	req.Policy.NoCacheFailures = true
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status == runcache.CacheHit {
		t.Error("a cached failure must not be served under --no-cache-failures")
	}
	if h.runner.calls != calls+1 {
		t.Error("the command should be re-executed, not replayed")
	}
	if r.Written {
		t.Error("the failure must not be re-stored under --no-cache-failures")
	}
}

func TestNoCacheBypassesReadAndWrite(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	calls := h.runner.calls

	req := h.request("echo", "x")
	req.Policy.NoCache = true
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheDisabled {
		t.Errorf("status = %s, want disabled", r.Status)
	}
	if h.runner.calls != calls+1 {
		t.Error("--no-cache should execute even when a hit exists")
	}
	if r.Written {
		t.Error("--no-cache should not write")
	}
}

func TestRefreshBypassesHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	first, _ := h.svc.Run(context.Background(), h.request("echo", "x"))
	calls := h.runner.calls

	req := h.request("echo", "x")
	req.Policy.Refresh = true
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if h.runner.calls != calls+1 {
		t.Error("--refresh should execute despite a hit")
	}
	if !r.Written {
		t.Error("--refresh should write a new entry")
	}
	if r.RunID == first.RunID {
		t.Error("--refresh should create a new run id")
	}
	// A subsequent normal run hits the refreshed result.
	r3, _ := h.svc.Run(context.Background(), h.request("echo", "x"))
	if r3.Status != runcache.CacheHit {
		t.Errorf("after refresh, normal run = %s, want hit", r3.Status)
	}
}

func TestStartFailureNotCached(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.startErr = true

	_, err := h.svc.Run(context.Background(), h.request("nope"))
	if err == nil {
		t.Fatal("expected start failure error")
	}
	ids, lerr := h.store.ListRefs(context.Background())
	if lerr != nil {
		t.Fatalf("ListRefs: %v", lerr)
	}
	if len(ids) != 0 {
		t.Errorf("start failure should store nothing, got %d entries", len(ids))
	}
}

func TestSkippedInputsBlockHitByDefault(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	// Force a skipped input: tiny max file size with the skip policy means any
	// non-empty file is skipped rather than hashed.
	h.cfg.Hashing.MaxFileSize = 1
	h.cfg.Hashing.LargeFilePolicy = config.LargeFileSkip
	h.write(t, "big.txt", "more than one byte")

	r, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.Status != runcache.CacheUncached {
		t.Errorf("status = %s, want uncached", r.Status)
	}
	if r.Written {
		t.Error("skipped inputs should block caching by default")
	}
	calls := h.runner.calls
	// A second default run executes again (no hit).
	r2, _ := h.svc.Run(context.Background(), h.request("echo", "x"))
	if r2.Status != runcache.CacheUncached || h.runner.calls != calls+1 {
		t.Errorf("second run should re-execute uncached, status=%s calls=%d", r2.Status, h.runner.calls)
	}

	// With --allow-skipped-inputs, the run caches and a later allowed run hits.
	req := h.request("echo", "x")
	req.Policy.AllowSkippedInputs = true
	r3, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !r3.Written {
		t.Error("--allow-skipped-inputs should allow caching")
	}
	req2 := h.request("echo", "x")
	req2.Policy.AllowSkippedInputs = true
	r4, _ := h.svc.Run(context.Background(), req2)
	if r4.Status != runcache.CacheHit {
		t.Errorf("allowed-skip rerun = %s, want hit", r4.Status)
	}
}

func TestTTLExpiresHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.TTL = config.Duration(time.Hour)

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	// Within the TTL window: a hit.
	if r, _ := h.svc.Run(context.Background(), h.request("echo", "x")); r.Status != runcache.CacheHit {
		t.Fatalf("within TTL status = %s, want hit", r.Status)
	}

	// Advance past the TTL: the entry is stale and the run re-executes.
	h.clock.now = h.clock.now.Add(2 * time.Hour)
	calls := h.runner.calls
	r, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheMiss {
		t.Errorf("after TTL status = %s, want miss", r.Status)
	}
	if h.runner.calls != calls+1 {
		t.Error("an expired entry should re-execute")
	}
}

func TestTTLZeroNeverExpires(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.TTL = 0

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	h.clock.now = h.clock.now.Add(1000 * time.Hour)
	if r, _ := h.svc.Run(context.Background(), h.request("echo", "x")); r.Status != runcache.CacheHit {
		t.Errorf("TTL=0 status = %s, want hit (never expires)", r.Status)
	}
}

func TestHitVerifiesPayloads(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.runner.stdout = "trusted-output\n"

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}

	// Tamper the stored stdout payload. The next run resolves a key hit, but the
	// payload no longer matches its hash — it must surface as corruption, not a
	// successful hit replaying wrong bytes.
	payload := filepath.Join(h.rootAbs, ".awa", "runs", "entries",
		res.RunID.String()[:2], res.RunID.String(), "stdout.log")
	// Published payloads are read-only; make it writable to simulate tampering.
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(payload, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err = h.svc.Run(context.Background(), h.request("echo", "x"))
	if !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("hit on tampered payload err = %v, want ErrCorruptStore", err)
	}
}

func TestCaptureDisabledHasNoOutput(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.CaptureOutput = false

	r, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheUncached {
		t.Errorf("status = %s, want uncached", r.Status)
	}
	if r.Written {
		t.Error("a capture-disabled run must not be stored")
	}
	if !r.RunID.IsZero() {
		t.Errorf("uncached run has a run id %s, want none", r.RunID)
	}
	// Output was not captured at all, so the result carries no output metadata —
	// honest, where a zero OutputCapture would read as "captured, empty".
	if r.Execution.Output != nil {
		t.Errorf("Execution.Output = %+v, want nil when capture is disabled", r.Execution.Output)
	}
}

func TestPipeStdinUncached(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	req := h.request("cat")
	req.StdinMode = runcache.StdinPipe
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheUncached {
		t.Errorf("status = %s, want uncached", r.Status)
	}
	if r.Execution.Decision.Reason != "stdin-not-keyed" {
		t.Errorf("reason = %q, want stdin-not-keyed", r.Execution.Decision.Reason)
	}
	if r.Written {
		t.Error("piped stdin run must not be cached")
	}
}

func TestTTYStdinUncached(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	req := h.request("cat")
	req.StdinMode = runcache.StdinTTY
	req.TTYAllowed = true
	r, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != runcache.CacheUncached {
		t.Errorf("status = %s, want uncached", r.Status)
	}
	if r.Execution.Decision.Reason != "stdin-not-keyed" {
		t.Errorf("reason = %q, want stdin-not-keyed", r.Execution.Decision.Reason)
	}
	if r.Written {
		t.Error("TTY stdin run must not be cached")
	}
}

// mutateFile makes the fake runner modify project state during execution, turning
// it into a side-effecting command (a formatter/fixer) so the post-run scan differs
// from the pre-run baseline.
func (h *harness) mutateFile(name, content string) {
	h.runner.onRun = func() {
		_ = os.WriteFile(filepath.Join(h.rootAbs, name), []byte(content), 0o644)
	}
}

func TestMutatingRunIsRecordedButNotReusable(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "before")
	h.mutateFile("a.txt", "after")

	res, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Execution.Mutation.Changed() {
		t.Error("mutation not observed; guard must detect the changed file")
	}
	if res.Written {
		t.Error("a mutating run must not publish a reusable cache pointer")
	}
	// A mutating run is now durable history: it is recorded and inspectable, just not
	// reusable.
	if !res.Recorded {
		t.Error("a mutating run must be recorded as history")
	}
	if res.RunID.IsZero() {
		t.Error("a recorded mutating run must have a resolvable run id")
	}
	if res.Reuse.Kind() != runcache.ReuseNonReusable {
		t.Errorf("reuse kind = %s, want non-reusable", res.Reuse)
	}
	if res.Execution.Decision.Cacheable {
		t.Error("a mutating run must be non-cacheable")
	}
	if got := res.Execution.Decision.Reason; got != string(runcache.ReasonMutatedState) {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonMutatedState)
	}
	// The store holds the run as history (one entry), but no key pointer references
	// it, so a later identical run can never hit it: a run that changed observed state
	// is history, not a cache hit.
	refs, err := h.store.ListRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Errorf("store has %d entries after a mutating run, want 1 (history)", len(refs))
	}
	if _, ok, err := h.store.Lookup(res.Key); err != nil || ok {
		t.Errorf("Lookup of a mutating run's key = (ok=%v, err=%v), want a clean miss", ok, err)
	}
}

func TestFailedMutatingRunIsNotCached(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	// cache_failed_runs defaults to true, so a failed non-mutating run would cache; a
	// failed run that also mutates state must still be refused.
	h.runner.exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 1}
	h.mutateFile("out.txt", "generated")

	res, err := h.svc.Run(context.Background(), h.request("fix", "out.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Written {
		t.Error("a failed mutating run must not be stored")
	}
	if res.Execution.Decision.Reason != string(runcache.ReasonMutatedState) {
		t.Errorf("reason = %q, want %q", res.Execution.Decision.Reason, runcache.ReasonMutatedState)
	}
}

func TestRefreshDoesNotPublishMutatingRun(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.mutateFile("gen.txt", "x")
	req := h.request("fix", "gen.txt")
	req.Policy.Refresh = true

	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Written {
		t.Error("--refresh must still obey the mutation guard and not publish")
	}
	if !res.Execution.Mutation.Changed() {
		t.Error("mutation facts must be reported under --refresh")
	}
}

func TestNoCacheReportsMutationFacts(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.mutateFile("gen.txt", "x")
	req := h.request("fix", "gen.txt")
	req.Policy.NoCache = true

	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != runcache.CacheDisabled {
		t.Errorf("status = %s, want disabled", res.Status)
	}
	if res.Written {
		t.Error("--no-cache must never publish")
	}
	if !res.Execution.Mutation.Changed() {
		t.Error("--no-cache should still report mutation facts when it scans")
	}
}

func TestNoOpFixerCachesAfterMutation(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	// First run mutates the file: not cached.
	h.mutateFile("a.txt", "v2")
	r1, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Written {
		t.Fatal("mutating fixer run must not be cached")
	}
	// Second run is a true no-op (file already at the target content): the runner
	// leaves state unchanged, so the result becomes cacheable.
	h.runner.onRun = nil
	r2, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Execution.Mutation.Changed() {
		t.Error("no-op run must observe unchanged state")
	}
	if !r2.Written {
		t.Error("a non-mutating run must be cacheable")
	}
}

// TestPostRunScanHealsAStaleIndex is the deterministic form of the CI failure behind
// TestNoOpFixerCachesAfterMutation, and it covers what that test cannot: what the run
// after the mutating one sees.
//
// That test depends on the host filesystem letting a same-size rewrite reproduce the
// pre-run stat signature, so it exercises the interesting path on CI and a trivial one
// on a developer's machine. Here the condition is constructed instead of hoped for:
// under "fast" trust only size and mtime are compared, so restoring the mtime after a
// same-size rewrite makes the mutation invisible to stat on every platform — the same
// blindness a coarse timestamp granularity produces under "normal", reached by a
// portable route.
//
// The mutation must be seen anyway (the post-run scan reads the bytes), and — this is
// the part the index owns — the run after it must see a clean worktree. If the post-run
// scan did not persist what it read, the index would still describe the pre-mutation
// content under a signature that still matches, so the next run would derive its
// baseline from content no longer on disk and report a mutation for a command that
// touched nothing, forever.
func TestPostRunScanHealsAStaleIndex(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	// Fast trust is the lever that makes the mutation stat-invisible portably. It also
	// makes every run non-reusable on its own (a weak signature is never publishable),
	// so nothing here asserts anything about publication: "a mutating run is not
	// published" would pass under this mode no matter what the guard did, and it is
	// already covered under normal trust by TestMutatingRunIsRecordedButNotReusable.
	// What this test owns is the observation, before and after.
	h.cfg.Hashing.TrustMode = config.TrustFast
	h.write(t, "a.txt", "v1")

	abs := filepath.Join(h.rootAbs, "a.txt")
	before, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	mtime := before.ModTime()
	h.runner.onRun = func() {
		if err := os.WriteFile(abs, []byte("v2"), 0o644); err != nil {
			panic(err)
		}
		// Same size, same mtime: nothing "fast" compares has moved.
		if err := os.Chtimes(abs, mtime, mtime); err != nil {
			panic(err)
		}
	}

	r1, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Execution.Mutation.Changed() {
		t.Fatal("a mutation the stat signature cannot witness must still be observed: the post-run scan reads the bytes")
	}

	h.runner.onRun = nil
	r2, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r2.Execution.Mutation.Changed() {
		t.Error("the run after the mutating one changed nothing, so it must observe an unchanged worktree; a stale index makes it accuse itself")
	}
}

// TestAMutatingRunIsFollowedByAStorableRunAndThenAHit carries the sequence above to
// its end — mutate, settle, reuse — which the test above cannot reach: "fast" trust is
// never publishable, so nothing there can be stored or replayed.
//
// It is the reusable half of the same story and nothing more. It cannot reproduce the
// stale-index blindness, because normal trust compares ctime and a portable rewrite
// cannot forge it, so the mutation here is an ordinary size change that any trust mode
// sees. What it does prove is that the run after a mutating one settles into a real
// cache hit rather than a permanent miss.
func TestAMutatingRunIsFollowedByAStorableRunAndThenAHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	h.mutateFile("a.txt", "mutated")

	if _, err := h.svc.Run(context.Background(), h.request("fix", "a.txt")); err != nil {
		t.Fatal(err)
	}
	h.runner.onRun = nil

	r2, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Written {
		t.Fatalf("the no-op run after a mutating one must be storable, got status %v reason %q", r2.Status, r2.Execution.Decision.Reason)
	}

	r3, err := h.svc.Run(context.Background(), h.request("fix", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != runcache.CacheHit {
		t.Errorf("status = %v, want a real cache hit against the post-run index", r3.Status)
	}
}

// staleHashIndex is a worktree index that keeps telling the truth about a file's stat
// signature while lying about its content hash. Everything but Lookup is the real
// index, so scans still persist normally.
type staleHashIndex struct {
	worktree.Index
	stale hashing.ContentHash
	on    bool
}

func (s *staleHashIndex) Lookup(ctx context.Context, p worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	e, ok, err := s.Index.Lookup(ctx, p)
	if s.on && ok && err == nil && !e.Content.IsZero() {
		e.Content = s.stale
	}
	return e, ok, err
}

// TestAStaleIndexedHashCannotProduceACacheHit is the false-hit proof: when the index
// vouches for content that is no longer on disk, no surface may say the stored run can
// be replayed — the run must execute the command, and explain and ls must agree.
//
// The stale entry is constructed rather than raced for. On disk a same-size rewrite is
// only invisible to a stat signature within the filesystem's timestamp granularity, so
// reproducing it by writing files would test the host's clock resolution, not awa. What
// the granularity actually produces is an index that reports the file's current
// signature next to the previous content's hash — which is what this index does, on
// demand, on every platform.
//
// The sequence matters. The stored run is published while the index is honest, so the
// entry being protected is a genuinely reusable one; the file is then rewritten and
// re-observed, so the index holds the new signature; only then does the index start
// lying about the hash. That is the exact state a coarse-granularity rewrite leaves
// behind, and the state in which reusing an indexed hash computes the key of content
// that no longer exists.
func TestAStaleIndexedHashCannotProduceACacheHit(t *testing.T) {
	stale := &staleHashIndex{}
	h := newHarness(t, withIndexDecorator(func(idx worktree.Index) worktree.Index {
		stale.Index = idx
		return stale
	}))
	defer h.cleanup()

	hasher := blake3hash.New()
	v1, err := hasher.HashReader(strings.NewReader("v1"))
	if err != nil {
		t.Fatal(err)
	}
	stale.stale = v1

	h.write(t, "a.txt", "v1")
	published, err := h.svc.Run(context.Background(), h.request("build", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !published.Written {
		t.Fatalf("the first run must publish a reusable entry, got status %v reason %q", published.Status, published.Execution.Decision.Reason)
	}

	// An external editor rewrites the file at the same size; a further run lets the
	// index record the new signature honestly.
	h.write(t, "a.txt", "v2")
	if _, err := h.svc.Run(context.Background(), h.request("touch", "a.txt")); err != nil {
		t.Fatal(err)
	}

	// From here the index reports the current signature with the previous content's
	// hash — the state a stat-invisible rewrite leaves behind.
	stale.on = true

	// The projections are asked first, while the stored run under the old content is
	// still the only entry that could be offered. Asking after the re-execution would
	// prove nothing: the run would by then have published an entry for the new content,
	// and explain would honestly hit that one.
	exp, err := h.svc.Explain(context.Background(), h.explainCmd("build", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if exp.Outcome == apprun.OutcomeHit {
		t.Errorf("explain claims a hit for changed content (reason %q)", exp.Reason)
	}
	entries, err := lsEntries(h.svc, h.lsRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ID == published.RunID {
			t.Error("run ls lists the stored run as replayable now, though its recorded inputs no longer match the worktree")
		}
	}

	calls := h.runner.calls
	replayed, err := h.svc.Run(context.Background(), h.request("build", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status == runcache.CacheHit {
		t.Error("a run whose inputs changed was served from the cache: the key was computed from an indexed hash instead of the bytes")
	}
	if h.runner.calls == calls {
		t.Error("the command did not execute; a cache hit on stale content skips exactly the work that mattered")
	}
	if replayed.Key == published.Key {
		t.Errorf("key %s equals the stored run's, so the changed content never reached the key", replayed.Key)
	}
}

// scanOptionsRecorder keeps the options of every scan a run issued, so a test can
// assert the shape of a scan rather than only its outcome.
type scanOptionsRecorder struct {
	inner apprun.Scanner
	seen  []scanner.Options
}

func (r *scanOptionsRecorder) Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts scanner.Options) (scanner.Result, error) {
	r.seen = append(r.seen, opts)
	return r.inner.Scan(ctx, project, cfg, scope, opts)
}

// TestBothRunScansForceARehashAndPersist pins the option choices a run's cache
// decisions depend on, directly rather than through their effects.
//
// Both scans read the bytes: the baseline decides whether a stored result may be
// replayed instead of running the command, and the observation decides whether this
// result may be stored, so neither may rest on an indexed hash a stat signature merely
// vouched for. Both persist: the pre-run scan seeds the index other commands read, and
// the post-run scan corrects it to whatever the child left.
//
// TestNoOpFixerCachesAfterMutation, TestPostRunScanHealsAStaleIndex and
// TestAStaleIndexedHashCannotProduceACacheHit above are the behavioural tests; this one
// names the mechanism, so a scan that quietly stops forcing a rehash or stops
// persisting is reported as itself rather than as a puzzling caching failure.
func TestBothRunScansForceARehashAndPersist(t *testing.T) {
	rec := &scanOptionsRecorder{}
	h := newHarness(t, withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
		rec.inner = inner
		return rec
	}))
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 2 {
		t.Fatalf("run issued %d scans, want the pre-run and post-run pair", len(rec.seen))
	}
	pre, post := rec.seen[0], rec.seen[1]
	if !pre.ForceRehash {
		t.Error("the pre-run scan must force a rehash: a key computed from an indexed hash can name content that is no longer on disk, and the hit it produces never runs the command")
	}
	if !post.ForceRehash {
		t.Error("the post-run scan must force a rehash: an indexed hash keyed on the stat signature cannot witness a same-size rewrite")
	}
	if pre.ReadOnly || post.ReadOnly {
		t.Errorf("both scans must persist the worktree index — the pre-run one as the baseline other commands read, the post-run one to correct it to what the child left; got ReadOnly %v then %v", pre.ReadOnly, post.ReadOnly)
	}
}

// failPostScan wraps a scanner and fails the post-run scan, leaving the pre-run scan
// intact, so the mutation guard's fail-closed path is tested.
//
// The two are told apart by order, not by their options: a run's scans are now
// deliberately alike — both persist, both force a rehash — because the post-run
// observation must be as honest as the baseline it is compared against. Order is the
// only thing left that distinguishes them, and it is stable: Run scans once before the
// child and once after.
type failPostScan struct {
	inner apprun.Scanner
	scans int
}

func (f *failPostScan) Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts scanner.Options) (scanner.Result, error) {
	f.scans++
	if f.scans == 2 {
		return scanner.Result{}, fmt.Errorf("post-run scan boom")
	}
	return f.inner.Scan(ctx, project, cfg, scope, opts)
}

func TestPostScanFailureFailsClosed(t *testing.T) {
	h := newHarness(t, withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
		return &failPostScan{inner: inner}
	}))
	defer h.cleanup()
	// The command does not mutate state, but the post-run scan fails: an unknown
	// post-state must never be published as reusable.
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Written {
		t.Error("a run whose post-scan failed must not be stored")
	}
	if !res.Execution.Mutation.ScanFailed() {
		t.Error("post-scan failure must be recorded as a scan-failed mutation status")
	}
	if res.Execution.Decision.Reason != string(runcache.ReasonPostScanFailed) {
		t.Errorf("reason = %q, want %q", res.Execution.Decision.Reason, runcache.ReasonPostScanFailed)
	}
}

// countingScanner counts read-only scans so a test can assert run ls groups its
// worktree scans by recorded policy instead of scanning once per entry.
type countingScanner struct {
	inner    apprun.Scanner
	readOnly int
}

func (c *countingScanner) Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts scanner.Options) (scanner.Result, error) {
	if opts.ReadOnly {
		c.readOnly++
	}
	return c.inner.Scan(ctx, project, cfg, scope, opts)
}
