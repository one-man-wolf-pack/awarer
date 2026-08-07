package scanner_test

import (
	"context"
	"errors"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// failingReader returns an error from every Lookup, modelling a corrupt or
// unreadable index. It implements only the lookup facet (worktree.IndexLookup): the
// read-only scanner path needs neither a write method nor a handle-lifecycle method,
// so this fake carries neither — the compiler enforces that the reuse path depends
// on lookups alone.
type failingReader struct{}

func (failingReader) Lookup(context.Context, worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	return worktree.IndexedEntry{}, false, errors.New("index read failed")
}

// Compile-time proof the fake is a pure lookup, with no write or Close method.
var _ worktree.IndexLookup = failingReader{}

// TestScanFailsLoudlyOnIndexError proves an index lookup failure aborts the scan
// rather than being silently swallowed as a cache miss, and that a read-only
// scanner can be driven by a fake with no write method.
func TestScanFailsLoudlyOnIndexError(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

	svc := scanner.NewReadOnly(worktreefs.New(), mustBlake3(t), failingReader{})
	// Normal mode consults the index, so the lookup error must surface.
	_, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{})
	if err == nil {
		t.Fatalf("expected scan to fail on index lookup error")
	}
}

func mustBlake3(t *testing.T) hashing.Hasher {
	t.Helper()
	return blake3hash.New()
}
