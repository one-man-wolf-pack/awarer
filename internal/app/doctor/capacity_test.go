package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/config"
	dom "awarer/internal/domain/doctor"
	"awarer/internal/infra/fsx"
)

// awaLogicalBytes is the test's own oracle for the store metric: the lengths of the
// regular files under .awa/, found with a plain lstat walk independent of fsx.
func awaLogicalBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(filepath.Join(root, ".awa"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// settle runs doctor once so the derived files its own index check opens exist
// before the oracle measures; the measurement covers derived files too.
func settle(t *testing.T, e *env) {
	t.Helper()
	e.run(t, noGit(), Request{})
}

func withStoreWarning(e *env, th config.ByteThreshold) Request {
	cfg := e.cfg
	cfg.Doctor.StoreWarningSize = th
	return Request{Resolved: resolvedConfig(cfg)}
}

// TestStoreCapacityThresholdBoundary: the advisory fires exactly when the observed
// logical bytes reach the threshold, is a non-repairable warning that leaves doctor
// out of "needs attention" (exit 0), and below the threshold an exact measurement is
// a pass with no finding.
func TestStoreCapacityThresholdBoundary(t *testing.T) {
	e := newEnv(t)
	e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	settle(t, e)
	total := awaLogicalBytes(t, e.root)

	for _, c := range []struct {
		name      string
		threshold int64
		warn      bool
	}{
		{"below", total + 1, false},
		{"at", total, true},
		{"above", total - 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := e.run(t, noGit(), withStoreWarning(e, config.ThresholdAt(config.ByteSize(c.threshold))))
			capa := res.StoreCapacity()
			if b, ok := capa.ObservedBytes(); !ok || b != total || capa.Measurement() != dom.CapacityExact {
				t.Fatalf("capacity = %v %d, want exact %d", capa.Measurement(), b, total)
			}
			if th, ok := capa.Threshold(); !ok || th != c.threshold {
				t.Fatalf("threshold = %d, want %d", th, c.threshold)
			}
			got := findingsByCode(res, dom.CodeStoreCapacityThreshold)
			if !c.warn {
				if len(got) != 0 || res.Health() != dom.HealthOK {
					t.Fatalf("below threshold: findings %+v health %v, want none and ok", res.Findings(), res.Health())
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want one store-capacity-threshold, got %+v", res.Findings())
			}
			f := got[0]
			if f.Severity() != dom.SeverityWarning || f.Repairable() || f.Subsystem() != dom.SubsystemCapacity {
				t.Errorf("finding = %v/%v/%v, want non-repairable capacity warning", f.Severity(), f.Repairable(), f.Subsystem())
			}
			if res.NeedsAttention() || res.Health() != dom.HealthWarning {
				t.Errorf("capacity alone: attention %v health %v, want exit 0 with warning", res.NeedsAttention(), res.Health())
			}
		})
	}
}

// TestStoreCapacityDefaultIsQuiet: a fresh project is far below the 1GiB default.
func TestStoreCapacityDefaultIsQuiet(t *testing.T) {
	e := newEnv(t)
	res := e.run(t, noGit(), Request{})
	capa := res.StoreCapacity()
	if th, ok := capa.Threshold(); !ok || th != 1<<30 {
		t.Fatalf("default threshold = %d %v, want 1GiB", th, ok)
	}
	if b, ok := capa.ObservedBytes(); !ok || b <= 0 || capa.Measurement() != dom.CapacityExact {
		t.Fatalf("fresh store = %v %d, want an exact positive size", capa.Measurement(), b)
	}
	if len(findingsByCode(res, dom.CodeStoreCapacityThreshold)) != 0 {
		t.Fatal("a fresh store must not cross the default threshold")
	}
}

// TestStoreCapacityOffWalksNothing: "off" reports a disabled advisory, adds no
// finding, and never invokes the sizing walk.
func TestStoreCapacityOffWalksNothing(t *testing.T) {
	e := newEnv(t)
	svc := New(noGit())
	calls := 0
	svc.sizeTree = func(ctx context.Context, dir *os.File, name string, lim fsx.TreeLimits) (fsx.BoundedTreeSize, error) {
		calls++
		return fsx.SizeTreeWithin(ctx, dir, name, lim)
	}
	req := withStoreWarning(e, config.ThresholdOff())
	req.Project = e.project
	res, err := svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("disabled advisory walked the store %d times", calls)
	}
	if res.StoreCapacity().Measurement() != dom.CapacityDisabled || len(findingsByCode(res, dom.CodeStoreCapacityThreshold)) != 0 {
		t.Fatalf("off = %v with %+v, want disabled and no finding", res.StoreCapacity().Measurement(), res.Findings())
	}
}

// runWithSizing runs doctor with the sizing walk replaced, so a test can pin a
// measurement state the filesystem cannot produce deterministically.
func runWithSizing(t *testing.T, e *env, threshold int64, size fsx.BoundedTreeSize, sizeErr error) dom.DoctorResult {
	t.Helper()
	svc := New(noGit())
	svc.sizeTree = func(context.Context, *os.File, string, fsx.TreeLimits) (fsx.BoundedTreeSize, error) {
		return size, sizeErr
	}
	req := withStoreWarning(e, config.ThresholdAt(config.ByteSize(threshold)))
	req.Project = e.project
	res, err := svc.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestStoreCapacityPartialAndUnmeasured: a partial sum that reaches the threshold
// warns and says it is a lower bound; one below discloses its gaps without a pass or
// a finding; a sizing failure is unmeasured, never zero.
func TestStoreCapacityPartialAndUnmeasured(t *testing.T) {
	e := newEnv(t)

	res := runWithSizing(t, e, 100, fsx.BoundedTreeSize{Bytes: 150, EntryBudgetReached: true}, nil)
	if res.StoreCapacity().Measurement() != dom.CapacityPartial || len(findingsByCode(res, dom.CodeStoreCapacityThreshold)) != 1 {
		t.Fatalf("partial crossing = %v %+v, want partial with a warning", res.StoreCapacity().Measurement(), res.Findings())
	}

	below := runWithSizing(t, e, 100, fsx.BoundedTreeSize{Bytes: 50, DepthLimitReached: true, Unreadable: true}, nil)
	capa := below.StoreCapacity()
	if capa.Measurement() != dom.CapacityPartial || len(capa.Gaps()) != 2 || len(findingsByCode(below, dom.CodeStoreCapacityThreshold)) != 0 {
		t.Fatalf("partial below = %v %v %+v, want disclosed gaps and no finding", capa.Measurement(), capa.Gaps(), below.Findings())
	}
	if below.Summary().Passed != res.Summary().Passed {
		t.Errorf("a partial sum below the threshold must not count as a pass")
	}

	failed := runWithSizing(t, e, 100, fsx.BoundedTreeSize{}, errors.New("permission denied"))
	if _, ok := failed.StoreCapacity().ObservedBytes(); ok || failed.StoreCapacity().Measurement() != dom.CapacityUnmeasured {
		t.Fatalf("failed sizing = %v, want unmeasured without a byte figure", failed.StoreCapacity().Measurement())
	}
	if len(findingsByCode(failed, dom.CodeStoreCapacityThreshold)) != 0 || failed.NeedsAttention() {
		t.Fatalf("failed sizing must neither warn nor fail doctor: %+v", failed.Findings())
	}
}

// TestStoreCapacityBudgetDoesNotCurtailIntegrity: with a sizing budget of one entry
// the store measurement stops at once, yet the integrity checks still find a corrupt
// checkpoint — the two do not share a budget.
func TestStoreCapacityBudgetDoesNotCurtailIntegrity(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	overwrite(t, filepath.Join(layout.CheckpointsDir(), h.ID.String(), "header.json"), "{ not valid json")

	res := e.run(t, Deps{Git: noGit().Git, Sizing: fsx.TreeLimits{MaxEntries: 1}}, Request{})
	capa := res.StoreCapacity()
	if capa.Measurement() != dom.CapacityPartial || capa.Gaps()[0] != dom.GapWorkBudget {
		t.Fatalf("capacity = %v %v, want partial on the work budget", capa.Measurement(), capa.Gaps())
	}
	if len(findingsByCode(res, dom.CodeCheckpointCorrupt)) != 1 || !res.NeedsAttention() {
		t.Fatalf("integrity finding lost under a spent sizing budget: %+v", res.Findings())
	}
}

// TestStoreCapacityRepairDeletesNothing: --repair leaves a capacity warning
// unrepaired and the store byte-for-byte the same size.
func TestStoreCapacityRepairDeletesNothing(t *testing.T) {
	e := newEnv(t)
	e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	settle(t, e)
	before := awaLogicalBytes(t, e.root)
	req := withStoreWarning(e, config.ThresholdAt(1))
	req.Repair = true
	res := e.run(t, noGit(), req)
	got := findingsByCode(res, dom.CodeStoreCapacityThreshold)
	if len(got) != 1 || got[0].Repaired() || res.NeedsAttention() {
		t.Fatalf("repair pass: %+v attention %v, want an unrepaired warning and exit 0", got, res.NeedsAttention())
	}
	if after := awaLogicalBytes(t, e.root); after != before {
		t.Fatalf("store changed from %d to %d bytes under --repair", before, after)
	}
}

// TestStoreCapacityCancellationIsAnInterruption: a cancelled sizing walk ends doctor
// as an interruption, not as a successful partial report.
func TestStoreCapacityCancellationIsAnInterruption(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := New(noGit())
	svc.sizeTree = func(ctx context.Context, dir *os.File, name string, lim fsx.TreeLimits) (fsx.BoundedTreeSize, error) {
		cancel()
		return fsx.SizeTreeWithin(ctx, dir, name, lim)
	}
	_, err := svc.Run(ctx, Request{Project: e.project, Resolved: resolvedConfig(e.cfg)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

// TestStoreCapacityNonDirectoryStoreIsUnmeasured: a .awa replaced by a symlink after
// the project was opened is not a known-empty store; the walk never follows it and
// the store is reported unmeasured rather than as an exact zero.
func TestStoreCapacityNonDirectoryStoreIsUnmeasured(t *testing.T) {
	e := newEnv(t)
	elsewhere := t.TempDir()
	awaDir := filepath.Join(e.root, ".awa")
	if err := os.Rename(awaDir, filepath.Join(elsewhere, "store")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "store"), awaDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	svc := New(noGit())
	layout, err := e.project.Paths()
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.measureStore(context.Background(), layout, config.ThresholdAt(1), &report{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ObservedBytes(); ok || c.Measurement() != dom.CapacityUnmeasured {
		t.Fatalf("symlinked store = %v, want unmeasured without a figure", c.Measurement())
	}
}
