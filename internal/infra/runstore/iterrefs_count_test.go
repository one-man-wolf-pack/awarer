package runstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
)

// TestRefEnumerationHonorsCancellation proves the run-ref enumeration loops honor a
// cancelled context mid-walk rather than reading every id to completion: with at least
// one entry present, a pre-cancelled context makes CountRefs, ListRefs, and IterRefsNewest
// return context.Canceled — the raw error (not wrapped as ErrCorruptStore) so a caller
// classifies it as an interruption, not storage damage.
func TestRefEnumerationHonorsCancellation(t *testing.T) {
	r, _, _ := newInternalStore(t)
	const shard = "18"
	shardAbs := filepath.Join(r.root, filepath.FromSlash(r.entriesRel()), shard)
	if err := os.MkdirAll(shardAbs, 0o755); err != nil {
		t.Fatalf("mkdir shard: %v", err)
	}
	if err := os.Mkdir(filepath.Join(shardAbs, shard+fmt.Sprintf("%030x", 1)), 0o755); err != nil {
		t.Fatalf("mkdir entry: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.CountRefs(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("CountRefs err = %v, want context.Canceled", err)
	}
	if _, err := r.ListRefs(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ListRefs err = %v, want context.Canceled", err)
	}
	err := r.IterRefsNewest(ctx, func(runcache.RunID) (bool, error) { return false, nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("IterRefsNewest err = %v, want context.Canceled", err)
	}
	// Resolve enumerates the ids to match a prefix, so it honors cancellation too.
	if _, err := r.Resolve(ctx, "18"); !errors.Is(err, context.Canceled) {
		t.Errorf("Resolve err = %v, want context.Canceled", err)
	}
}

// TestEachEntryNameStreamsLargeShard proves CountRefs and IterRefsNewest stay correct
// when far more run ids share one shard than a single directory-read batch holds — the
// realistic case, since RunID shards come from the timestamp prefix. It writes the
// entry directories directly (no full commit) so it can cheaply exceed the batch size.
func TestEachEntryNameStreamsLargeShard(t *testing.T) {
	r, _, _ := newInternalStore(t)
	const shard = "18"
	shardAbs := filepath.Join(r.root, filepath.FromSlash(r.entriesRel()), shard)
	if err := os.MkdirAll(shardAbs, 0o755); err != nil {
		t.Fatalf("mkdir shard: %v", err)
	}
	// Comfortably exceed fsx.dirEntryBatch (1024) so the streamed listing crosses at
	// least one batch boundary. Every id lives in shard "18" (its first two hex chars).
	const n = 1500
	var maxID string
	for i := 0; i < n; i++ {
		id := shard + fmt.Sprintf("%030x", i) // 2 + 30 = 32 lowercase hex chars
		if err := os.Mkdir(filepath.Join(shardAbs, id), 0o755); err != nil {
			t.Fatalf("mkdir entry: %v", err)
		}
		if id > maxID {
			maxID = id
		}
	}

	// CountRefs must count every entry across the batch boundary.
	got, err := r.CountRefs(context.Background())
	if err != nil {
		t.Fatalf("CountRefs: %v", err)
	}
	if got != n {
		t.Fatalf("CountRefs = %d, want %d (a batch boundary dropped entries?)", got, n)
	}

	// IterRefsNewest must find the true newest (largest id) across the whole shard, not
	// just within the first batch. Early-stop keeps this a single bounded pass.
	var newest runcache.RunID
	if err := r.IterRefsNewest(context.Background(), func(id runcache.RunID) (bool, error) {
		newest = id
		return true, nil
	}); err != nil {
		t.Fatalf("IterRefsNewest: %v", err)
	}
	if newest.String() != maxID {
		t.Fatalf("IterRefsNewest newest = %s, want the max id %s across the batch", newest.Short(), maxID[:12])
	}
}

func TestCountRefs(t *testing.T) {
	r, _, h := newInternalStore(t)

	// An empty store counts zero without error.
	if n, err := r.CountRefs(context.Background()); err != nil || n != 0 {
		t.Fatalf("CountRefs on empty store = (%d, %v), want (0, nil)", n, err)
	}

	base := int64(1_700_000_000) * int64(time.Second)
	const total = 5
	for i := 0; i < total; i++ {
		commitAt(t, r, h, string(rune('a'+i)), base+int64(i))
	}

	n, err := r.CountRefs(context.Background())
	if err != nil {
		t.Fatalf("CountRefs: %v", err)
	}
	// CountRefs must agree with the id enumeration it summarizes.
	refs, err := r.ListRefs(context.Background())
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if n != len(refs) || n != total {
		t.Fatalf("CountRefs = %d, ListRefs len = %d, want %d", n, len(refs), total)
	}
}

func TestIterRefsNewestOrderAndEarlyStop(t *testing.T) {
	r, _, h := newInternalStore(t)

	base := int64(1_700_000_000) * int64(time.Second)
	ids := make([]runcache.RunID, 5)
	for i := range ids {
		ids[i] = commitAt(t, r, h, string(rune('a'+i)), base+int64(i))
	}

	// Full drain yields every id newest first.
	var got []runcache.RunID
	if err := r.IterRefsNewest(context.Background(), func(id runcache.RunID) (bool, error) {
		got = append(got, id)
		return false, nil
	}); err != nil {
		t.Fatalf("IterRefsNewest: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("IterRefsNewest yielded %d ids, want %d", len(got), len(ids))
	}
	for i := range got {
		if want := ids[len(ids)-1-i]; got[i] != want {
			t.Errorf("IterRefsNewest[%d] = %s, want %s", i, got[i].Short(), want.Short())
		}
	}

	// Early stop after the first id yields exactly the newest and does not continue.
	count := 0
	var first runcache.RunID
	if err := r.IterRefsNewest(context.Background(), func(id runcache.RunID) (bool, error) {
		count++
		first = id
		return true, nil
	}); err != nil {
		t.Fatalf("IterRefsNewest early stop: %v", err)
	}
	if count != 1 {
		t.Fatalf("early stop yielded %d ids, want 1", count)
	}
	if first != ids[len(ids)-1] {
		t.Errorf("early-stop first id = %s, want newest %s", first.Short(), ids[len(ids)-1].Short())
	}
}

func TestIterRefsNewestEmptyStore(t *testing.T) {
	r, _, _ := newInternalStore(t)
	called := false
	if err := r.IterRefsNewest(context.Background(), func(runcache.RunID) (bool, error) {
		called = true
		return false, nil
	}); err != nil {
		t.Fatalf("IterRefsNewest on empty store: %v", err)
	}
	if called {
		t.Fatal("yield must not be called on an empty store")
	}
}
