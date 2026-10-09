package scanner_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

func tmpEntries(t *testing.T, root string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(paths.New(root).TmpDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return entries
}

func TestScanFrontierSpillsWithoutChangingIdentity(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "src/a.go", "package a\n")
	cfg := config.Defaults()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)

	before, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{BufferRecords: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close() }()

	scantest.Write(t, root, ".awaignore", "*.log\n")
	for i := 0; i < 30; i++ {
		scantest.Write(t, root, fmt.Sprintf("logs/%02d.log", i), "x")
	}
	after, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{BufferRecords: 4})
	if err != nil {
		t.Fatal(err)
	}
	stream, digest, ok := after.Coverage().Frontier()
	if !ok || digest.Count != 31 {
		t.Fatalf("frontier = %+v ok=%v, want .awa plus 30 excluded leaves", digest, ok)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d, err := worktree.ReduceFrontier(blake3hash.New(), cur); err != nil || d != digest {
		t.Fatalf("re-reduced frontier = %+v, %v; want %+v", d, err, digest)
	}
	if after.Meta().ConfigHash != before.Meta().ConfigHash {
		t.Error("the frontier reached the scan configuration hash")
	}
	if len(tmpEntries(t, root)) == 0 {
		t.Fatal("a spilling scan left no temp state to release; the fixture no longer exercises the spill")
	}
	if err := after.Close(); err != nil {
		t.Fatal(err)
	}
	_ = before.Close()
	if left := tmpEntries(t, root); len(left) != 0 {
		t.Errorf("Close left temp state: %v", left)
	}
}

type failingWalker struct{ inner worktree.Walker }

var errWalk = errors.New("walk failed")

func (w failingWalker) Walk(ctx context.Context, layout paths.Layout, scope config.ScanScope, visit func(worktree.Node) error, omit func(worktree.FrontierRecord) error) error {
	if err := w.inner.Walk(ctx, layout, scope, visit, omit); err != nil {
		return err
	}
	return errWalk
}

func TestScanWalkFailureReleasesFrontierSpill(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, ".awaignore", "*.log\n")
	for i := 0; i < 30; i++ {
		scantest.Write(t, root, fmt.Sprintf("logs/%02d.log", i), "x")
	}
	cfg := config.Defaults()
	svc := scanner.New(failingWalker{inner: worktreefs.New()}, blake3hash.New(), nil)
	if _, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{BufferRecords: 4}); !errors.Is(err, errWalk) {
		t.Fatalf("Scan = %v, want the walk failure", err)
	}
	if left := tmpEntries(t, root); len(left) != 0 {
		t.Errorf("a failed scan left temp state: %v", left)
	}
	if (scanner.Result{}).Coverage().Kind().Known() {
		t.Error("a zero result claims coverage")
	}
}

func TestScanFrontierKeepsInvalidUTF8NamesExactly(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	names := []string{"bad\xff.log", "bad\xfe.log"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o600); err != nil {
			t.Skipf("this filesystem does not accept a non-UTF-8 name: %v", err)
		}
	}
	scantest.Write(t, root, ".awaignore", "*.log\n")
	scantest.Write(t, root, "bad\uFFFD.log", "x")
	cfg := config.Defaults()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
	for _, buffer := range []int{0, 1} {
		res, err := svc.Scan(context.Background(), proj, cfg, cfg.HistoryScanScope(), scanner.Options{BufferRecords: buffer})
		if err != nil {
			t.Fatal(err)
		}
		stream, _, _ := res.Coverage().Frontier()
		cur, err := stream.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for cur.Next() {
			seen[cur.Record().Path.String()] = true
		}
		_ = cur.Close()
		for _, n := range append(names, "bad\uFFFD.log") {
			if !seen[n] {
				t.Errorf("buffer %d: frontier lost %q: %v", buffer, n, seen)
			}
		}
		_ = res.Close()
	}
}
