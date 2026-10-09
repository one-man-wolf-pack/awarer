package doctor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"awarer/internal/domain/config"
	dom "awarer/internal/domain/doctor"
)

// put writes a file of n bytes at the slash-relative path rel under root.
func put(t *testing.T, root, rel string, n int) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lstatBytes is the test's own oracle for one root: the lengths of the regular files
// beneath it from a plain WalkDir, which never follows symlinks, independent of fsx.
func lstatBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
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

// audit runs doctor --capacity with the given extra selectors, threshold, and limits.
func (e *env) audit(t *testing.T, extra []string, th config.ByteThreshold, lim WatchedLimits) dom.DoctorResult {
	t.Helper()
	cfg := e.cfg
	cfg.Run.WatchedEffectRoots = extra
	cfg.Doctor.WatchedRootWarningSize = th
	d := noGit()
	d.Watched = lim
	return e.run(t, d, Request{Resolved: resolvedConfig(cfg), Capacity: true})
}

// detailByPath indexes an audit's listed roots.
func detailByPath(a dom.WatchedRootAudit) map[string]dom.WatchedRoot {
	out := map[string]dom.WatchedRoot{}
	for _, r := range a.Detail() {
		out[r.Path()] = r
	}
	return out
}

// TestWatchedAuditSelectsAndMeasures covers the selection contract on one fixture:
// built-in and configured basenames at any depth, a configured exact path, an outer
// root owning a nested match, protected subtrees, symlinks, an empty root, and
// unmatched directories. Each expected sum comes from the independent oracle.
func TestWatchedAuditSelectsAndMeasures(t *testing.T) {
	e := newEnv(t)
	r := e.root
	put(t, r, "node_modules/pkg/index.js", 100)                              // built-in basename
	put(t, r, "packages/api/dist/app.js", 40)                                // built-in basename, nested
	put(t, r, "build/out.o", 7)                                              // outer root ...
	put(t, r, "build/dist/inner.js", 9)                                      // ... owning a nested match
	put(t, r, "gen/stubs/s.go", 13)                                          // configured basename
	put(t, r, "artifacts/bin/tool", 21)                                      // configured exact path
	put(t, r, "other/artifacts/bin/not-selected", 1000)                      // exact path elsewhere: not a root
	put(t, r, ".git/build/hidden", 5000)                                     // protected: never searched
	put(t, r, "src/main.go", 3000)                                           // unmatched
	if err := os.MkdirAll(filepath.Join(r, "coverage"), 0o755); err != nil { // empty root
		t.Fatal(err)
	}
	outside := t.TempDir()
	put(t, outside, "big", 9000)
	if err := os.Symlink(outside, filepath.Join(r, "out")); err != nil { // symlinked "out": not a real root
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(r, "node_modules", "linked")); err != nil { // never followed
		t.Fatal(err)
	}

	res := e.audit(t, []string{"stubs", "artifacts/bin"}, config.ThresholdOff(), WatchedLimits{})
	a := res.WatchedRoots()
	if !a.Requested() || !a.SearchComplete() {
		t.Fatalf("audit requested=%v searchComplete=%v gaps=%v, want a complete search", a.Requested(), a.SearchComplete(), a.SearchGaps())
	}
	want := map[string]int64{
		"node_modules":      lstatBytes(t, filepath.Join(r, "node_modules")),
		"packages/api/dist": lstatBytes(t, filepath.Join(r, "packages", "api", "dist")),
		"build":             lstatBytes(t, filepath.Join(r, "build")),
		"gen/stubs":         lstatBytes(t, filepath.Join(r, "gen", "stubs")),
		"artifacts/bin":     lstatBytes(t, filepath.Join(r, "artifacts", "bin")),
		"coverage":          0,
	}
	if want["build"] != 16 || want["node_modules"] != 100 {
		t.Fatalf("oracle disagrees with the fixture: %v", want)
	}
	got := detailByPath(a)
	if len(got) != len(want) {
		t.Fatalf("roots = %v, want exactly %v", keys(got), keys(want))
	}
	var sum int64
	for path, bytes := range want {
		root, ok := got[path]
		b, measured := root.ObservedBytes()
		if !ok || !measured || b != bytes || root.Measurement() != dom.CapacityExact {
			t.Errorf("root %s = %v %d (listed %v), want exact %d", path, root.Measurement(), b, ok, bytes)
		}
		sum += bytes
	}
	if total, known := a.Total(); !known || total != len(want) || a.ObservedBytes() != sum || !a.ObservedBytesComplete() {
		t.Errorf("total = %d known %v, bytes = %d complete %v; want %d roots, %d exact bytes",
			total, known, a.ObservedBytes(), a.ObservedBytesComplete(), len(want), sum)
	}
	if !slices.Contains(a.Selectors(), "stubs") || !slices.Contains(a.Selectors(), "node_modules") {
		t.Errorf("selectors = %v, want the effective built-in and configured list", a.Selectors())
	}
	if len(findingsByCode(res, dom.CodeWatchedRootCapacityThreshold)) != 0 {
		t.Error("without a threshold, size alone must not produce a finding")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestWatchedAuditNoRootsIsDistinctFromUnrequested: a complete search that finds no
// real root is a requested audit with a known zero count — not an unrequested one,
// and not an exact claim about output elsewhere (its byte sum covers zero roots).
func TestWatchedAuditNoRootsIsDistinctFromUnrequested(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "src/main.go", 10)
	a := e.audit(t, nil, config.ThresholdOff(), WatchedLimits{}).WatchedRoots()
	if total, known := a.Total(); !a.Requested() || !known || total != 0 || len(a.Detail()) != 0 {
		t.Fatalf("audit = requested %v total %d known %v, want a requested audit that found nothing", a.Requested(), total, known)
	}
	plain := e.run(t, noGit(), Request{}).WatchedRoots()
	if plain.Requested() {
		t.Fatal("plain doctor reported a watched-root audit")
	}
}

// TestPlainDoctorSearchesNoWatchedRoots: without --capacity the search lists nothing,
// proved by counting the audit's own listing seam rather than by the absence of output.
func TestPlainDoctorSearchesNoWatchedRoots(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "node_modules/x", 10)
	listed := 0
	run := func(capacity bool) dom.DoctorResult {
		svc := New(noGit())
		svc.listWatched = func(root, dir string, fn func(os.DirEntry) error) error {
			listed++
			return eachScanDirEntry(root, dir, fn)
		}
		res, err := svc.Run(context.Background(), Request{Project: e.project, Resolved: resolvedConfig(e.cfg), Capacity: capacity})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	run(false)
	if listed != 0 {
		t.Fatalf("plain doctor listed %d directories for the watched-root audit, want 0", listed)
	}
	if res := run(true); listed == 0 || res.WatchedRoots().Found() != 1 {
		t.Fatalf("doctor --capacity listed %d directories and found %d roots, want a search that finds node_modules",
			listed, res.WatchedRoots().Found())
	}
}

// TestWatchedAuditSearchBudgets: a spent search budget leaves the search incomplete
// with no total, while the root found before it keeps its measurement. Both budgets
// are sized to let the root listing finish, so build is always found first.
func TestWatchedAuditSearchBudgets(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "build/f", 5)
	for _, d := range []string{"a", "b", "c", "d"} {
		put(t, e.root, d+"/x/y/build/f", 5)
	}
	top, err := os.ReadDir(e.root)
	if err != nil {
		t.Fatal(err)
	}
	for name, lim := range map[string]WatchedLimits{
		"entries":     {SearchEntries: int64(len(top)) + 1},
		"directories": {SearchDirs: 5}, // the root and a..d; the first level below is over
	} {
		t.Run(name, func(t *testing.T) {
			a := e.audit(t, nil, config.ThresholdOff(), lim).WatchedRoots()
			if a.SearchComplete() || !slices.Equal(a.SearchGaps(), []dom.CapacityGap{dom.GapWorkBudget}) {
				t.Fatalf("search gaps = %v, want work-budget", a.SearchGaps())
			}
			if _, known := a.Total(); known {
				t.Error("an incomplete search must not claim a root total")
			}
			if a.ObservedBytesComplete() {
				t.Error("an incomplete search must not claim complete bytes")
			}
			build, ok := detailByPath(a)["build"]
			if b, _ := build.ObservedBytes(); !ok || a.Found() != 1 || build.Measurement() != dom.CapacityExact || b != 5 {
				t.Errorf("found %d, build = %v %d (listed %v); want only build, its exact measurement kept",
					a.Found(), build.Measurement(), b, ok)
			}
		})
	}
}

// TestWatchedAuditUnreadable: an unreadable directory leaves the search incomplete and
// an unopenable root unmeasured, and neither discards the other roots.
func TestWatchedAuditUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs permission bits that deny listing to this user")
	}
	e := newEnv(t)
	put(t, e.root, "dist/ok", 3)
	put(t, e.root, "locked/build/hidden", 50)
	put(t, e.root, "build/hidden", 70)
	sealed := filepath.Join(e.root, "build", "sealed")
	put(t, e.root, "build/sealed", 11)
	if err := os.Chmod(sealed, 0o000); err != nil { // stat-only: still measured
		t.Fatal(err)
	}
	for _, d := range []string{"locked", "build"} {
		p := filepath.Join(e.root, d)
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}
	a := e.audit(t, nil, config.ThresholdOff(), WatchedLimits{}).WatchedRoots()
	if !slices.Equal(a.SearchGaps(), []dom.CapacityGap{dom.GapUnreadableEntries}) {
		t.Errorf("search gaps = %v, want unreadable-entries", a.SearchGaps())
	}
	got := detailByPath(a)
	if r := got["build"]; r.Measurement() != dom.CapacityUnmeasured || !slices.Equal(r.Gaps(), []dom.CapacityGap{dom.GapUnavailable}) {
		t.Errorf("build = %v %v, want unmeasured unavailable", r.Measurement(), r.Gaps())
	}
	if b, ok := got["dist"].ObservedBytes(); !ok || b != 3 {
		t.Errorf("dist = %d (%v), want its exact 3 bytes kept", b, ok)
	}

	// Readable again but with a sealed file inside: its length is measured, which only
	// a walk that never opens payloads can do.
	if err := os.Chmod(filepath.Join(e.root, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	a = e.audit(t, nil, config.ThresholdOff(), WatchedLimits{}).WatchedRoots()
	if r := detailByPath(a)["build"]; r.Measurement() != dom.CapacityExact {
		t.Errorf("build = %v %v, want exact", r.Measurement(), r.Gaps())
	} else if b, _ := r.ObservedBytes(); b != 81 {
		t.Errorf("build = %d bytes, want 81 including the unreadable-content file", b)
	}
}

// TestWatchedAuditSizingBudgets: the per-root budget makes a root a partial lower
// bound (work-budget); the shared budget does the same for the root it runs out in
// and leaves the rest unmeasured (aggregate-budget). The search stays complete.
func TestWatchedAuditSizingBudgets(t *testing.T) {
	e := newEnv(t)
	for i := range 6 {
		put(t, e.root, "build/f"+string(rune('0'+i)), 10)
	}
	put(t, e.root, "dist/g", 10)

	a := e.audit(t, nil, config.ThresholdOff(), WatchedLimits{RootEntries: 3}).WatchedRoots()
	build := detailByPath(a)["build"]
	if b, _ := build.ObservedBytes(); build.Measurement() != dom.CapacityPartial || b != 30 ||
		!slices.Equal(build.Gaps(), []dom.CapacityGap{dom.GapWorkBudget}) {
		t.Fatalf("build = %v %d %v, want a partial 30-byte lower bound with work-budget", build.Measurement(), b, build.Gaps())
	}
	if !a.SearchComplete() || a.ObservedBytesComplete() {
		t.Errorf("search complete %v, bytes complete %v; want a complete search and an incomplete sum", a.SearchComplete(), a.ObservedBytesComplete())
	}

	a = e.audit(t, nil, config.ThresholdOff(), WatchedLimits{AggregateEntries: 6}).WatchedRoots()
	exact, partial, unmeasured := a.Counts()
	if exact+partial+unmeasured != 2 || exact == 2 {
		t.Fatalf("counts = %d/%d/%d, want both roots counted and not both exact", exact, partial, unmeasured)
	}
	sawAggregate := false
	for _, r := range a.Detail() {
		if slices.Contains(r.Gaps(), dom.GapAggregateBudget) {
			sawAggregate = true
		}
	}
	if !sawAggregate {
		t.Errorf("roots = %v, want a root marked aggregate-budget", a.Detail())
	}
	if total, known := a.Total(); !known || total != 2 {
		t.Errorf("total = %d known %v; sizing budgets must not make the search incomplete", total, known)
	}
}

// TestWatchedAuditDetailIsBoundedAndCounted: past the detail limit the largest found
// roots are listed and the omitted count is exact, while counts and the byte sum
// still cover every found root.
func TestWatchedAuditDetailIsBoundedAndCounted(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "p1/build/f", 1)
	put(t, e.root, "p2/build/f", 2)
	put(t, e.root, "p3/build/f", 3)
	put(t, e.root, "p4/build/f", 4)
	a := e.audit(t, nil, config.ThresholdOff(), WatchedLimits{Detail: 2}).WatchedRoots()
	var paths []string
	for _, r := range a.Detail() {
		paths = append(paths, r.Path())
	}
	if !slices.Equal(paths, []string{"p4/build", "p3/build"}) || a.Omitted() != 2 || a.Found() != 4 || a.ObservedBytes() != 10 {
		t.Fatalf("detail = %v omitted %d found %d bytes %d; want the two largest, 2 omitted, 4 found, 10 bytes",
			paths, a.Omitted(), a.Found(), a.ObservedBytes())
	}
}

// TestWatchedAuditThreshold: exact or lower-bound bytes at or above the threshold warn
// (non-repairable, exit 0); below it, nothing; and a partial figure below it is
// neither a warning nor a pass.
func TestWatchedAuditThreshold(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "build/a", 60)
	put(t, e.root, "build/b", 40)
	for _, c := range []struct {
		name      string
		threshold int64
		lim       WatchedLimits
		warn      bool
	}{
		{"exact below", 101, WatchedLimits{}, false},
		{"exact at", 100, WatchedLimits{}, true},
		{"exact above", 99, WatchedLimits{}, true},
		{"partial reaching", 30, WatchedLimits{RootEntries: 1}, true},
		{"partial below", 101, WatchedLimits{RootEntries: 1}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := e.audit(t, nil, config.ThresholdAt(config.ByteSize(c.threshold)), c.lim)
			got := findingsByCode(res, dom.CodeWatchedRootCapacityThreshold)
			if (len(got) == 1) != c.warn || len(got) > 1 {
				t.Fatalf("findings = %d, want warn=%v", len(got), c.warn)
			}
			if c.warn {
				f := got[0]
				if f.Repairable() || f.Severity() != dom.SeverityWarning || f.Subject() != "build" {
					t.Errorf("finding = %+v, want a non-repairable warning about build", f)
				}
				if res.NeedsAttention() {
					t.Error("a capacity warning alone must leave doctor at exit 0")
				}
			}
			if n := res.WatchedRoots().Reaching(); (n == 1) != c.warn {
				t.Errorf("reaching = %d, want warn=%v", n, c.warn)
			}
		})
	}

	off := e.audit(t, nil, config.ThresholdOff(), WatchedLimits{}).WatchedRoots()
	if b, ok := detailByPath(off)["build"].ObservedBytes(); !ok || b != 100 {
		t.Errorf("threshold off measured %d (%v); off must suppress the warning, not the measurement", b, ok)
	}
}

// TestWatchedAuditRepairDeletesNothing: --repair --capacity repairs only store state;
// a root over its threshold keeps every byte and its warning stays unrepaired.
func TestWatchedAuditRepairDeletesNothing(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "dist/bundle.js", 500)
	if err := os.Remove(filepath.Join(e.root, ".awa", ".gitignore")); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	cfg.Doctor.WatchedRootWarningSize = config.ThresholdAt(1)
	res := e.run(t, noGit(), Request{Resolved: resolvedConfig(cfg), Capacity: true, Repair: true})
	if got := lstatBytes(t, filepath.Join(e.root, "dist")); got != 500 {
		t.Fatalf("dist holds %d bytes after --repair --capacity, want 500", got)
	}
	for _, f := range findingsByCode(res, dom.CodeWatchedRootCapacityThreshold) {
		if f.Repaired() {
			t.Error("a watched-root warning must never be repaired")
		}
	}
	if len(findingsByCode(res, dom.CodeStateGitignoreMissing)) != 1 {
		t.Error("the integrity checks must still run and report beside the audit")
	}
	if _, err := os.Stat(filepath.Join(e.root, ".awa", ".gitignore")); err != nil {
		t.Errorf("the safe store repair did not run: %v", err)
	}
}

// TestWatchedAuditCancellationIsAnInterruption: a cancelled audit is an error, never a
// partial diagnosis.
func TestWatchedAuditCancellationIsAnInterruption(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "build/f", 1)
	ctx, cancel := context.WithCancel(context.Background())
	svc := New(noGit())
	svc.listWatched = func(root, dir string, fn func(os.DirEntry) error) error {
		cancel()
		return eachScanDirEntry(root, dir, fn)
	}
	_, err := svc.Run(ctx, Request{Project: e.project, Resolved: resolvedConfig(e.cfg), Capacity: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

// TestWatchedAuditKeepsNativeNames: on Unix a backslash is an ordinary name character,
// so a root below such a directory is found under its native path and a file whose
// name carries one is measured, not refused by store path grammar.
func TestWatchedAuditKeepsNativeNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`"\" is a path separator on windows`)
	}
	e := newEnv(t)
	put(t, e.root, `we\ird/build/a\b`, 6)
	put(t, e.root, `we\ird/build/c`, 4)
	a := e.audit(t, nil, config.ThresholdOff(), WatchedLimits{}).WatchedRoots()
	r, ok := detailByPath(a)[`we\ird/build`]
	if b, _ := r.ObservedBytes(); !ok || r.Measurement() != dom.CapacityExact || b != 10 {
		t.Fatalf("roots = %v; want we\\ird/build exact at 10 bytes", keys(detailByPath(a)))
	}
}

// TestWatchedAuditExcludesProtectedSubtreesInsideRoots: .git and .awa stay outside the
// figure at any depth inside a selected root, so their bytes neither count nor raise
// a threshold warning.
func TestWatchedAuditExcludesProtectedSubtreesInsideRoots(t *testing.T) {
	e := newEnv(t)
	put(t, e.root, "build/visible", 1)
	put(t, e.root, "build/.git/object", 100)
	put(t, e.root, "build/sub/.awa/blob", 200)
	res := e.audit(t, nil, config.ThresholdAt(2), WatchedLimits{})
	r := detailByPath(res.WatchedRoots())["build"]
	if b, ok := r.ObservedBytes(); !ok || b != 1 || r.Measurement() != dom.CapacityExact {
		t.Fatalf("build = %v %d, want exact 1 byte without protected subtrees", r.Measurement(), b)
	}
	if got := findingsByCode(res, dom.CodeWatchedRootCapacityThreshold); len(got) != 0 {
		t.Fatalf("protected bytes raised %d threshold warning(s)", len(got))
	}
}
