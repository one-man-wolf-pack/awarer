package scanner_test

import (
	"context"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// openIndexedService builds a scanner backed by a real SQLite index for proj,
// returning the service, the counting hasher, and the index (closed on cleanup).
func openIndexedService(t *testing.T, proj projfs.Project) (*scanner.Service, *countingHasher) {
	t.Helper()
	layout, err := proj.Paths()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	ch := &countingHasher{inner: blake3hash.New()}
	return scanner.New(worktreefs.New(), ch, idx), ch
}

func scanWith(t *testing.T, svc *scanner.Service, proj projfs.Project, cfg config.Config) scanner.Result {
	t.Helper()
	res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return res
}

func TestNormalScanReusesIndexedHashes(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, ch := openIndexedService(t, proj)

	ch.reads = 0
	first := scanWith(t, svc, proj, config.Defaults())
	if ch.reads != 5 {
		t.Fatalf("first scan hashed %d files, want 5", ch.reads)
	}

	ch.reads = 0
	second := scanWith(t, svc, proj, config.Defaults())
	if ch.reads != 0 {
		t.Errorf("second normal scan re-hashed %d files, want 0 (reuse)", ch.reads)
	}
	if first.TreeHash().String() != second.TreeHash().String() {
		t.Errorf("reused scan produced a different tree hash")
	}
}

func TestNormalScanInvalidatesChangedFile(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, ch := openIndexedService(t, proj)
	first := scanWith(t, svc, proj, config.Defaults())

	// Change one tracked file; its size and mtime change, invalidating reuse.
	scantest.Write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b + 100 }\n")

	ch.reads = 0
	second := scanWith(t, svc, proj, config.Defaults())
	if ch.reads != 1 {
		t.Errorf("changed-file scan hashed %d files, want exactly 1", ch.reads)
	}
	if first.TreeHash().String() == second.TreeHash().String() {
		t.Errorf("changed file did not change tree hash")
	}
}

func TestStrictScanAlwaysRecomputes(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, ch := openIndexedService(t, proj)
	// Prime the index with a normal scan.
	scanWith(t, svc, proj, config.Defaults())

	strict := config.Defaults()
	strict.Hashing.TrustMode = config.TrustStrict
	ch.reads = 0
	scanWith(t, svc, proj, strict)
	if ch.reads != 5 {
		t.Errorf("strict scan reused hashes (%d reads), want 5 recomputed", ch.reads)
	}
}

func TestFastScanReusesOnWeakSignature(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, ch := openIndexedService(t, proj)
	fast := config.Defaults()
	fast.Hashing.TrustMode = config.TrustFast

	scanWith(t, svc, proj, fast)
	ch.reads = 0
	res := scanWith(t, svc, proj, fast)
	if ch.reads != 0 {
		t.Errorf("second fast scan re-hashed %d files, want 0", ch.reads)
	}
	if !res.Meta().FastModeWeakSignature {
		t.Errorf("fast scan must flag weak signature")
	}
}

func TestScanPersistsCompletedRow(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, _ := openIndexedService(t, proj)
	res := scanWith(t, svc, proj, config.Defaults())

	// A committed scan's entries are visible via Lookup on a freshly opened index.
	layout, _ := proj.Paths()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	got, ok, err := idx.Lookup(context.Background(), mustRel(t, "calc/calc.go"))
	if err != nil || !ok {
		t.Fatalf("lookup after persisted scan: ok=%v err=%v", ok, err)
	}
	if got.Content.IsZero() {
		t.Errorf("persisted entry missing content hash")
	}
	_ = res
}

func TestReadOnlyScanDoesNotPersist(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, _ := openIndexedService(t, proj)
	if _, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{ReadOnly: true}); err != nil {
		t.Fatalf("read-only Scan: %v", err)
	}

	// A read-only scan writes nothing: a freshly opened index has no entry for a
	// scanned file, so it could not have raced a concurrent index rebuild.
	layout, _ := proj.Paths()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if _, ok, err := idx.Lookup(context.Background(), mustRel(t, "calc/calc.go")); err != nil || ok {
		t.Fatalf("read-only scan must not persist; lookup ok=%v err=%v", ok, err)
	}
}

func TestReadOnlyScanStillReusesIndexedHashes(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc, ch := openIndexedService(t, proj)
	scanWith(t, svc, proj, config.Defaults()) // seed the index with a persisting scan

	ch.reads = 0
	if _, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{ReadOnly: true}); err != nil {
		t.Fatalf("read-only Scan: %v", err)
	}
	if ch.reads != 0 {
		t.Errorf("read-only scan re-hashed %d files, want 0 (it still consults the index)", ch.reads)
	}
}

func mustRel(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
