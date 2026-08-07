package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

func TestParseRunRef(t *testing.T) {
	rng, err := ParseRange("run:abc123:before..run:abc123:after")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if rng.Left.Kind != RefRunObservation || rng.Left.RunRef != "abc123" || rng.Left.RunSel != RunBefore {
		t.Errorf("left = %+v", rng.Left)
	}
	if rng.Right.Kind != RefRunObservation || rng.Right.RunSel != RunAfter {
		t.Errorf("right = %+v", rng.Right)
	}

	rng2, err := ParseRange("run:abc:after..now")
	if err != nil || rng2.Left.Kind != RefRunObservation || rng2.Right.Kind != RefNow {
		t.Errorf("after..now = %v, %+v", err, rng2)
	}

	for _, bad := range []string{"run:abc", "run::before", "run:abc:bogus"} {
		if _, err := ParseRange(bad + "..now"); err == nil {
			t.Errorf("ParseRange(%q) must fail", bad)
		}
	}
}

// stubRuns is a fake RunObservations port.
type stubRuns struct {
	id       runcache.RunID
	resolErr error
	obsErr   error
	tree     hashing.TreeHash
	cfg      hashing.ConfigHash
}

func (s stubRuns) Resolve(context.Context, string) (runcache.RunID, error) {
	if s.resolErr != nil {
		return runcache.RunID{}, s.resolErr
	}
	return s.id, nil
}

func (s stubRuns) Observation(runcache.RunID, bool) (runcache.RunObservationRead, error) {
	if s.obsErr != nil {
		return runcache.RunObservationRead{}, s.obsErr
	}
	return runcache.NewRunObservationRead(scantest.CanonicalStream(nil, nil), s.tree, s.cfg, runcache.ReusableCacheEntry())
}

func TestResolveRunObservation(t *testing.T) {
	hasher := blake3hash.New()
	id, _ := runcache.NewRunID(1, strings.NewReader("0123456789abcdef"))
	tree, _ := worktree.ReduceCursor(hasher, scantest.CanonicalCursor(nil, nil))

	r := NewResolver(Deps{
		Hasher: hasher,
		Runs:   stubRuns{id: id, tree: tree.Hash, cfg: hashing.ConfigHashFromTree(tree.Hash)},
	})
	st, err := r.Resolve(context.Background(), Ref{Kind: RefRunObservation, Display: "run:x:before", RunRef: "x", RunSel: RunBefore}, NowContext{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if st.Kind != KindRunObservation {
		t.Errorf("kind = %v, want run-observation", st.Kind)
	}
	if st.ContentAvailable() {
		t.Error("a run observation must report no content available")
	}
	if rid, sel, _, _, ok := st.RunObservation(); !ok || sel != RunBefore || rid != id.String() {
		t.Errorf("RunObservation() = %q %v %v", rid, sel, ok)
	}
}

func TestResolveRunObservationFailsLoud(t *testing.T) {
	hasher := blake3hash.New()
	r := NewResolver(Deps{
		Hasher: hasher,
		Runs:   stubRuns{obsErr: runcache.ErrObservationUnavailable},
	})
	_, err := r.Resolve(context.Background(), Ref{Kind: RefRunObservation, RunRef: "x", RunSel: RunAfter}, NowContext{})
	if !errors.Is(err, runcache.ErrObservationUnavailable) {
		t.Errorf("err = %v, want ErrObservationUnavailable", err)
	}

	// A resolver with no run port rejects run references rather than failing silently.
	r2 := NewResolver(Deps{Hasher: hasher})
	if _, err := r2.Resolve(context.Background(), Ref{Kind: RefRunObservation, RunRef: "x"}, NowContext{}); err == nil {
		t.Error("a resolver with no run port must reject a run reference")
	}
}
