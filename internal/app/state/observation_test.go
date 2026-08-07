package state_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// changingWalker yields one regular node whose content read reports that the node
// changed during the scan (worktree.ErrObservationChanged), deterministically modelling
// an in-scope input that moved between the walk's stat and its read — the exact signal
// the verified content opener produces for a racing file, without real timing.
type changingWalker struct{ path string }

func (w changingWalker) Walk(_ context.Context, _ paths.Layout, _ config.ScanScope, visit func(worktree.Node) error) error {
	p, err := worktree.ParseRelPath(w.path)
	if err != nil {
		return err
	}
	return visit(worktree.Node{
		Path: p,
		Kind: worktree.KindRegular,
		Stat: worktree.StatSignature{Size: 5, MtimeNs: 1, Mode: 0o644},
		Open: func() (io.ReadCloser, error) { return nil, worktree.ErrObservationChanged },
	})
}

func nowResolver(t *testing.T, walker worktree.Walker) (*state.Resolver, state.NowContext) {
	t.Helper()
	proj := scantest.InitProject(t, t.TempDir())
	hasher := blake3hash.New()
	r := state.NewResolver(state.Deps{
		Scanner: scanner.New(walker, hasher, nil),
		Hasher:  hasher,
	})
	return r, state.NowContext{Project: proj, Config: config.Defaults()}
}

// TestResolveNowStrictSurfacesUnstable proves a strict "now" observation aborts with
// ErrObservationUnstable when an in-scope input changes during the scan.
//
// Mutation proof: removing the FailOnObservationChange guard in the scanner processor
// (internal/app/scanner/processor.go), so the changed input falls through to the skip
// path, turns this test red — Resolve then succeeds with a skipped input instead of
// returning ErrObservationUnstable.
func TestResolveNowStrictSurfacesUnstable(t *testing.T) {
	r, nowCtx := nowResolver(t, changingWalker{path: "a.txt"})
	nowCtx.RequireStableObservation = true
	if _, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefNow}, nowCtx); !errors.Is(err, state.ErrObservationUnstable) {
		t.Fatalf("strict now with a changing input: got %v, want ErrObservationUnstable", err)
	}
}

// TestResolveNowHasObservationTime proves a "now" state reports the scan's completion
// time (observation time is known for a current-worktree observation, not fabricated or
// dropped). It uses the real worktree walker so the scan produces genuine metadata.
func TestResolveNowHasObservationTime(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "a.txt", "hi")
	hasher := blake3hash.New()
	r := state.NewResolver(state.Deps{Scanner: scanner.New(worktreefs.New(), hasher, nil), Hasher: hasher})
	rs, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefNow},
		state.NowContext{Project: proj, Config: config.Defaults()})
	if err != nil {
		t.Fatalf("resolve now: %v", err)
	}
	defer func() { _ = rs.Close() }()
	at, ok := rs.ObservedAt()
	if !ok || at.IsZero() {
		t.Errorf("now observation time = (%v,%v), want a known non-zero time", at, ok)
	}
}

// TestResolveNowTolerantSkipsChangedInput proves the default (tolerant) observation
// keeps its behavior: a changed input becomes a skipped input, not a failure. The
// oracle is the skipped-input count, independent of the strict guard.
func TestResolveNowTolerantSkipsChangedInput(t *testing.T) {
	r, nowCtx := nowResolver(t, changingWalker{path: "a.txt"})
	rs, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefNow}, nowCtx)
	if err != nil {
		t.Fatalf("tolerant now: %v", err)
	}
	if rs.Skipped() != 1 {
		t.Errorf("tolerant now must skip the changed input, Skipped() = %d, want 1", rs.Skipped())
	}
}
