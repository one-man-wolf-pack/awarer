package run_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"awarer/internal/app/effectobserve"
	"awarer/internal/app/initcmd"
	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
	"awarer/internal/infra/process"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
)

// newEffectHarnessWithBudget builds a run harness whose effect observer uses a tiny
// per-root entry budget, so a watched generated-output directory with only a handful of
// files trips the fail-closed over-budget path — the pathological huge-`target/` case in
// miniature, without a million-file fixture.
func newEffectHarnessWithBudget(t *testing.T, cfg config.Config, perRoot int) *effectHarness {
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
		EffectObserver: effectobserve.New(effectfs.NewWithLimits(hasher, perRoot, perRoot*100, 2_000_000), hasher),
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

// TestRunPerformanceLargeEffectRootOverBudget proves the diagnostic end to
// end: a watched effect root that exceeds the observation budget produces a
// large-effect-root latency diagnostic on the run result, with threshold-crossing
// evidence naming the root and a record-mode hint — even though the observation itself
// fails closed (the run is not reusable).
func TestRunPerformanceLargeEffectRootOverBudget(t *testing.T) {
	h := newEffectHarnessWithBudget(t, config.Defaults(), 8)
	defer h.cleanup()

	// A watched, input-excluded generated-output root with more entries than the tiny
	// per-root budget.
	dir := filepath.Join(h.rootAbs, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := h.run(t, "true")
	if len(res.Performance) != 1 {
		t.Fatalf("Performance len = %d, want 1", len(res.Performance))
	}
	d := res.Performance[0]
	if d.Cause() != perfdiag.CauseLargeEffectRoot {
		t.Errorf("cause = %s, want large-effect-root", d.Cause())
	}
	if d.Evidence().Path() != "target" {
		t.Errorf("evidence path = %q, want target", d.Evidence().Path())
	}
	if th, ok := d.Evidence().Threshold(); !ok || th != 8 {
		t.Errorf("threshold = (%d, %v), want (8, true)", th, ok)
	}
	if _, ok := d.Evidence().ExactCount(); ok {
		t.Error("a fail-closed over-budget root must not carry an exact count")
	}
	if h, ok := d.Hint(); !ok || h.Kind() != perfdiag.LargeEffectRootHintKind {
		t.Errorf("expected a record-mode hint, ok=%v", ok)
	}
}

// TestRunPerformancePostRunCreatedRoot proves the diagnostic catches a watched root the
// command itself creates: the pre-run observation is clean (no target/), but the command
// fills target/ past the budget, so the fail-closed cost lands in the post-run
// observation. Without merging the post-run report the run would only read "not reusable"
// with no latency cause.
func TestRunPerformancePostRunCreatedRoot(t *testing.T) {
	h := newEffectHarnessWithBudget(t, config.Defaults(), 8)
	defer h.cleanup()

	// A stable source input so the pre-run observation is a clean, small tree; target/
	// does not exist yet, so the pre-run effect observation names no large root.
	if err := os.WriteFile(filepath.Join(h.rootAbs, "src.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The command creates target/ with more entries than the budget during execution.
	res := h.run(t, "sh", "-c", "mkdir -p target && i=0; while [ $i -lt 40 ]; do echo x > target/f$i; i=$((i+1)); done")
	if len(res.Performance) != 1 {
		t.Fatalf("Performance len = %d, want 1 (post-run created root)", len(res.Performance))
	}
	d := res.Performance[0]
	if d.Evidence().Path() != "target" {
		t.Errorf("evidence path = %q, want target", d.Evidence().Path())
	}
	if th, ok := d.Evidence().Threshold(); !ok || th != 8 {
		t.Errorf("threshold = (%d, %v), want (8, true)", th, ok)
	}
}

// TestRunPerformanceQuietOnSmallProject proves the no-diagnostic path: a normal project
// with a small watched root under the budget crosses no threshold, so the run result
// carries no latency diagnostics.
func TestRunPerformanceQuietOnSmallProject(t *testing.T) {
	h := newEffectHarnessWithBudget(t, config.Defaults(), 100_000)
	defer h.cleanup()

	dir := filepath.Join(h.rootAbs, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := h.run(t, "true")
	if len(res.Performance) != 0 {
		t.Errorf("Performance = %+v, want empty on a small project", res.Performance)
	}
}

// TestLsPerformanceLargeEffectRootOverBudget proves the diagnostic also reaches the
// run ls --near surface, which observes the watched roots once for the whole listing.
func TestLsPerformanceLargeEffectRootOverBudget(t *testing.T) {
	h := newEffectHarnessWithBudget(t, config.Defaults(), 8)
	defer h.cleanup()

	// Record one run so the listing has a candidate, before the root goes over budget.
	if err := os.WriteFile(filepath.Join(h.rootAbs, "src.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.run(t, "true")

	dir := filepath.Join(h.rootAbs, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ls, err := h.svc.Ls(context.Background(), apprun.LsRequest{Project: h.proj, Config: h.cfg, Near: true})
	if err != nil {
		t.Fatalf("Ls: %v", err)
	}
	if len(ls.Performance) != 1 {
		t.Fatalf("Ls Performance len = %d, want 1", len(ls.Performance))
	}
	if ls.Performance[0].Evidence().Path() != "target" {
		t.Errorf("evidence path = %q, want target", ls.Performance[0].Evidence().Path())
	}
}
