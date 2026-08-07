package runstore

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

// commitAt commits one run with an explicit id timestamp and a disc-derived random
// suffix, so a sequence of commits has distinct, time-ordered ids regardless of the
// wall clock — letting newest-first assertions be deterministic.
func commitAt(t *testing.T, r *Repo, h hashing.Hasher, disc string, unixNano int64) runcache.RunID {
	t.Helper()
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: 1 << 20, MaxStderr: 1 << 20})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := io.WriteString(pending.Stdout(), "o"); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := io.WriteString(pending.Stderr(), "e"); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	id, err := runcache.NewRunID(unixNano, strings.NewReader(strings.Repeat(disc, 8)))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	ki := internalKeyInput(h, disc)
	start := time.Unix(0, unixNano)
	entry := runcache.RunEntry{
		ID:          id,
		Key:         ki.Compute(h),
		KeyInput:    ki,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      so,
		Stderr:      se,
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    internalUnchanged(),
		EffectGuard: internalUnchangedEffect(),
	}
	if err := pending.Commit(context.Background(), entry, internalUnchangedObs()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return id
}

func TestListRefsNewest(t *testing.T) {
	r, _, h := newInternalStore(t)

	base := int64(1_700_000_000) * int64(time.Second)
	ids := make([]runcache.RunID, 5)
	for i := range ids {
		// Distinct, increasing timestamps make ids[4] the newest.
		ids[i] = commitAt(t, r, h, string(rune('a'+i)), base+int64(i))
	}

	// A limit smaller than the store returns only the newest limit, newest first, and
	// never decodes or returns the older entries.
	got, err := r.ListRefsNewest(context.Background(), 2)
	if err != nil {
		t.Fatalf("ListRefsNewest(2): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListRefsNewest(2) returned %d ids, want 2", len(got))
	}
	if got[0] != ids[4] || got[1] != ids[3] {
		t.Errorf("ListRefsNewest(2) = [%s %s], want newest-first [%s %s]",
			got[0].Short(), got[1].Short(), ids[4].Short(), ids[3].Short())
	}

	// A limit at or above the store size returns every entry, still newest first.
	all, err := r.ListRefsNewest(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRefsNewest(10): %v", err)
	}
	if len(all) != len(ids) {
		t.Fatalf("ListRefsNewest(10) returned %d ids, want %d", len(all), len(ids))
	}
	for i := range all {
		if want := ids[len(ids)-1-i]; all[i] != want {
			t.Errorf("ListRefsNewest(10)[%d] = %s, want %s", i, all[i].Short(), want.Short())
		}
	}

	// A non-positive limit asks for nothing and gets nothing.
	if z, err := r.ListRefsNewest(context.Background(), 0); err != nil || z != nil {
		t.Errorf("ListRefsNewest(0) = (%v, %v), want (nil, nil)", z, err)
	}
}
