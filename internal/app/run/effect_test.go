package run_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/app/effectobserve"
	"awarer/internal/app/initcmd"
	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
	"awarer/internal/infra/process"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
)

// effectHarness builds a run service wired with a real child-process runner and the
// real bounded effect observer, so the effect-observation scenarios exercise the
// production path end to end: a real command creates or deletes an excluded
// generated-output directory and the cache must not serve a false hit.
type effectHarness struct {
	svc     *apprun.Service
	proj    projfs.Project
	cfg     config.Config
	rootAbs string
	cleanup func()
}

func newEffectHarness(t *testing.T, cfg config.Config) *effectHarness {
	t.Helper()
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
	hasher := blake3hash.New()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	proc := process.New()
	svc := apprun.New(apprun.Deps{
		Scanner:        scanner.New(worktreefs.New(), hasher, idx),
		Store:          runstore.New(layout, hasher),
		Runner:         proc,
		Resolver:       proc,
		EffectObserver: effectobserve.New(effectfs.New(hasher), hasher),
		Env:            osEnvReader{},
		Clock:          realishClock{},
		Hasher:         hasher,
		AwaVersion:     "test",
	})
	return &effectHarness{
		svc:     svc,
		proj:    proj,
		cfg:     cfg,
		rootAbs: layout.Root(),
		cleanup: func() { _ = idx.Close() },
	}
}

type osEnvReader struct{}

func (osEnvReader) Lookup(name string) (string, bool) { return os.LookupEnv(name) }

type realishClock struct{}

func (realishClock) Now() time.Time { return time.Now() }

func (h *effectHarness) run(t *testing.T, argv ...string) apprun.Result {
	t.Helper()
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.Run(context.Background(), apprun.Request{
		Project:   h.proj,
		Config:    h.cfg,
		Argv:      argv,
		CWD:       cwd,
		AbsDir:    h.rootAbs,
		StdinMode: runcache.StdinNull,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err != nil {
		t.Fatalf("run %v: %v", argv, err)
	}
	return res
}

// A run that depends on generated output under a watched, input-excluded root (target/)
// becomes reusable, but deleting that output makes the next run miss with
// effect-state-differs rather than replay a stale hit.
func TestEffectDeletedGeneratedOutputInvalidatesHit(t *testing.T) {
	h := newEffectHarness(t, config.Defaults())
	defer h.cleanup()

	// Stable source input and stable generated output under target/ (excluded from
	// the input scan by config.baselineExcludes).
	if err := os.WriteFile(filepath.Join(h.rootAbs, "src.txt"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.rootAbs, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.rootAbs, "target", "app"), []byte("built"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A no-op command that changes nothing: it becomes a reusable entry.
	first := h.run(t, "true")
	if !first.Written {
		t.Fatalf("first run should publish a reusable entry, got reuse=%s decision=%+v", first.Reuse, first.Execution.Decision)
	}
	second := h.run(t, "true")
	if second.Status != runcache.CacheHit {
		t.Fatalf("second run should hit; got status=%s", second.Status)
	}

	// Delete the watched generated output without touching the source input.
	if err := os.RemoveAll(filepath.Join(h.rootAbs, "target")); err != nil {
		t.Fatal(err)
	}

	third := h.run(t, "true")
	if third.Status == runcache.CacheHit {
		t.Fatal("after deleting the watched generated output the run must not hit")
	}
}

// TestEffectNestedGeneratedOutputInvalidatesHit proves effect observation covers
// generated-output directories nested inside a monorepo package, not only at the
// project root: deleting packages/api/target after a reusable run misses.
func TestEffectNestedGeneratedOutputInvalidatesHit(t *testing.T) {
	h := newEffectHarness(t, config.Defaults())
	defer h.cleanup()

	nested := filepath.Join(h.rootAbs, "packages", "api", "target")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "app"), []byte("built"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.rootAbs, "packages", "api", "src.txt"), []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}

	if first := h.run(t, "true"); !first.Written {
		t.Fatalf("first run should be reusable, got reuse=%s", first.Reuse)
	}
	if second := h.run(t, "true"); second.Status != runcache.CacheHit {
		t.Fatalf("second run should hit; got %s", second.Status)
	}

	// Delete the nested generated output without touching source inputs.
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}
	if third := h.run(t, "true"); third.Status == runcache.CacheHit {
		t.Fatal("deleting a nested watched generated dir must not hit")
	}
}

// TestEffectMutationDoesNotPublishHit proves a command that writes into an excluded
// generated-output root is recorded but not reusable (effect-state-differs), so it
// can never silently publish a replayable hit.
func TestEffectMutationDoesNotPublishHit(t *testing.T) {
	h := newEffectHarness(t, config.Defaults())
	defer h.cleanup()

	res := h.run(t, "sh", "-c", "mkdir -p target && date +%N > target/out")
	if res.Written {
		t.Fatalf("a run that writes into a watched effect root must not publish a reusable entry")
	}
	if !res.Recorded {
		t.Fatal("the run should still be recorded as history")
	}
	if got := res.Reuse.Reason(); got != runcache.ReasonEffectStateDiffers {
		t.Fatalf("reuse reason = %q, want effect-state-differs", got)
	}
}

// TestEffectFastTrustModeNeverReusable proves a run under the fast trust mode is
// recorded but never published as reusable, so a same-size/same-mtime rewrite that
// fast mode could miss can never be served from the cache.
func TestEffectFastTrustModeNeverReusable(t *testing.T) {
	cfg := config.Defaults()
	cfg.Hashing.TrustMode = config.TrustFast
	h := newEffectHarness(t, cfg)
	defer h.cleanup()

	first := h.run(t, "true")
	if first.Written {
		t.Fatalf("a fast-trust-mode run must not publish a reusable entry")
	}
	if got := first.Reuse.Reason(); got != runcache.ReasonFastTrustMode {
		t.Fatalf("reuse reason = %q, want fast-trust-mode", got)
	}
	// No fast entry is ever published, so a second run cannot be served a hit.
	second := h.run(t, "true")
	if second.Status == runcache.CacheHit {
		t.Fatal("fast trust mode must never serve a cache hit")
	}
}

// TestEffectUnreadableRootFailsClosed proves an unreadable watched root makes the
// run record-but-not-reusable with effect-state-unavailable, never a hit.
func TestEffectUnreadableRootFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory, so an unreadable root cannot be simulated")
	}
	h := newEffectHarness(t, config.Defaults())
	defer h.cleanup()

	dir := filepath.Join(h.rootAbs, "target")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	res := h.run(t, "true")
	if res.Written {
		t.Fatal("a run with an unobservable watched root must not publish a reusable entry")
	}
	if !res.Recorded {
		t.Fatal("the run should still be recorded as history")
	}
	if got := res.Reuse.Reason(); got != runcache.ReasonEffectStateUnavailable {
		t.Fatalf("reuse reason = %q, want effect-state-unavailable", got)
	}
}

// TestEffectRootsConfigChangeMisses proves that changing the watched-roots policy
// changes the run identity, so a prior reusable entry no longer hits.
func TestEffectRootsConfigChangeMisses(t *testing.T) {
	h := newEffectHarness(t, config.Defaults())
	defer h.cleanup()

	first := h.run(t, "true")
	if !first.Written {
		t.Fatalf("first run should be reusable, got reuse=%s", first.Reuse)
	}
	// Widen the watch list: the effect identity — and thus the key — changes, so the
	// prior entry cannot be served.
	h.cfg.Run.WatchedEffectRoots = []string{"generated"}
	if got := h.run(t, "true"); got.Status == runcache.CacheHit {
		t.Fatal("changing the watched-roots policy must miss the prior entry")
	}
}

// TestFailedRunReusableWhenEffectSafe proves the cache_failed_runs policy still
// holds: a failed but non-mutating run is reusable, while a failed run that also
// mutates a watched effect root is not.
func TestFailedRunReusableWhenEffectSafe(t *testing.T) {
	h := newEffectHarness(t, config.Defaults())
	defer h.cleanup()

	// Failed, no effect change: reusable under the default cache_failed_runs=true.
	failed := h.run(t, "sh", "-c", "exit 3")
	if failed.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", failed.ExitCode)
	}
	if !failed.Written {
		t.Fatalf("a failed non-mutating run should be reusable under cache_failed_runs; reuse=%s", failed.Reuse)
	}
	if got := h.run(t, "sh", "-c", "exit 3"); got.Status != runcache.CacheHit {
		t.Fatalf("the failed run should replay as a hit; status=%s", got.Status)
	}

	// Failed AND mutates a watched effect root: the effect guard overrides the
	// failed-run cache policy, so it is not reusable.
	failedMut := h.run(t, "sh", "-c", "mkdir -p target && date +%N > target/x && exit 3")
	if failedMut.Written {
		t.Fatal("a failed run that mutates a watched effect root must not be reusable")
	}
	if got := failedMut.Reuse.Reason(); got != runcache.ReasonEffectStateDiffers {
		t.Fatalf("reuse reason = %q, want effect-state-differs", got)
	}
}
