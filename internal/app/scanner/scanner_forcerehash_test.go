package scanner_test

import (
	"context"
	"strings"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// staleReader answers every lookup with the real index's stat signature and a content
// hash that does not describe the bytes on disk.
//
// It is how this test reaches, without depending on a clock or a filesystem's timestamp
// granularity, the state the ForceRehash option exists for: an index entry whose stat
// signature the walker reproduces exactly, standing for content that has since changed.
// A test that instead rewrote a file at the same size and hoped the mtime would not
// advance would be asserting the property only on the filesystems where the race
// happens to be winnable.
type staleReader struct {
	inner  worktree.IndexLookup
	stale  hashing.ContentHash
	probed int
}

func (r *staleReader) Lookup(ctx context.Context, p worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	// Counted on entry, not on a hit: the assertion below is "the index was not
	// consulted", and a counter that only advanced on a found entry would report zero
	// for a scan that consulted it and missed.
	r.probed++
	e, ok, err := r.inner.Lookup(ctx, p)
	if err != nil || !ok {
		return e, ok, err
	}
	e.Content = r.stale
	return e, true, nil
}

// TestForceRehashIgnoresAMatchingIndexEntry proves the option does what the post-run
// mutation observation needs of it: it does not consult the index at all, so a matching
// stat signature cannot stand in for reading the file.
//
// The first half is not scaffolding — it establishes that the signature really does
// match and that reuse really does carry the stale hash into the tree hash. Without it
// the second half would pass just as well against an index that never matched anything.
func TestForceRehashIgnoresAMatchingIndexEntry(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.BuildPlainGo(t, root)

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
	honest := scanWith(t, scanner.New(worktreefs.New(), ch, idx), proj, config.Defaults())
	if ch.reads != 5 {
		t.Fatalf("first scan hashed %d files, want 5", ch.reads)
	}

	other, err := ch.inner.HashReader(strings.NewReader("not the bytes on disk"))
	if err != nil {
		t.Fatal(err)
	}
	stale := &staleReader{inner: idx, stale: other}
	svc := scanner.NewReadOnly(worktreefs.New(), ch, stale)

	ch.reads = 0
	poisoned := scanWith(t, svc, proj, config.Defaults())
	if ch.reads != 0 {
		t.Fatalf("the index entries did not match the walked signatures (%d files re-hashed), so this test proves nothing about ForceRehash", ch.reads)
	}
	if poisoned.TreeHash().String() == honest.TreeHash().String() {
		t.Fatal("reuse did not carry the stale hash into the tree hash, so this test proves nothing about ForceRehash")
	}

	ch.reads = 0
	stale.probed = 0
	forced, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{ForceRehash: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if stale.probed != 0 {
		t.Errorf("ForceRehash scan consulted the index %d times, want 0", stale.probed)
	}
	if ch.reads != 5 {
		t.Errorf("ForceRehash scan hashed %d files, want 5", ch.reads)
	}
	if forced.TreeHash().String() != honest.TreeHash().String() {
		t.Errorf("ForceRehash scan tree hash = %s, want the honest %s", forced.TreeHash(), honest.TreeHash())
	}
}
