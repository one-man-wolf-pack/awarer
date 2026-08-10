package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/checkpointjson"
)

// recordingCheckpoints wraps a real checkpoint repository and records the window each
// positional reference asked for. It delegates to the real store, so the resolver's
// behavior is the production one and only the request shape is observed.
type recordingCheckpoints struct {
	*checkpointjson.Repo
	newest []int
}

func (r *recordingCheckpoints) StoreHealthNewest(ctx context.Context, newest int) (checkpoint.CheckpointStoreHealth, error) {
	r.newest = append(r.newest, newest)
	return r.Repo.StoreHealthNewest(ctx, newest)
}

// TestPositionalReferencesRequestOnlyTheirWindow is the boundedness oracle for
// checkpoint position references: "latest" needs one header and "@-N" needs N, so each
// must ask the store for exactly that. Asking for a wider window fails here; asking for
// the whole history is not expressible at all, since the resolver's port carries only
// the bounded read.
func TestPositionalReferencesRequestOnlyTheirWindow(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		name string
		ref  state.Ref
		want int
	}{
		{"latest", state.Ref{Kind: state.RefLatest, Display: "latest"}, 1},
		{"at-3", state.Ref{Kind: state.RefAtN, N: 3, Display: "@-3"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout := paths.New(t.TempDir())
			rec := &recordingCheckpoints{Repo: checkpointjson.NewRepo(layout)}
			for i := 0; i < 5; i++ {
				putCheckpoint(t, rec.Repo, byte(0x10+i), base.Add(time.Duration(i)*time.Minute), fullHex)
			}
			r := state.NewResolver(state.Deps{Checkpoints: rec, Hasher: blake3hash.New()})

			if _, err := r.Resolve(context.Background(), tc.ref, state.NowContext{}); err != nil {
				t.Fatalf("Resolve(%s): %v", tc.ref.Display, err)
			}
			if len(rec.newest) != 1 || rec.newest[0] != tc.want {
				t.Fatalf("%s requested windows %v, want exactly [%d]", tc.ref.Display, rec.newest, tc.want)
			}
		})
	}
}

// shortWindowCheckpoints reports a large readable total but retains fewer headers than
// asked for — a store that broke the bounded-read contract. It exists to prove @-N
// refuses such an answer instead of indexing past the headers it actually received.
type shortWindowCheckpoints struct {
	*checkpointjson.Repo
	retained int
	readable int
}

func (s *shortWindowCheckpoints) StoreHealthNewest(context.Context, int) (checkpoint.CheckpointStoreHealth, error) {
	headers := make([]checkpoint.CheckpointHeader, s.retained)
	return checkpoint.NewCheckpointStoreHealth(checkpoint.StoreReadCounts{Readable: s.readable}, headers), nil
}

// TestPositionalReferencesRefuseAShortRetainedWindow proves positions are answered
// from the window the read actually returned, not from the store-wide count: a store
// that reports five readable records but retains fewer headers must fail loudly.
// Reporting it as an ordinary out-of-range or as an empty store would send a user to
// record a checkpoint instead of to the broken read, and indexing the count into the
// window would panic.
func TestPositionalReferencesRefuseAShortRetainedWindow(t *testing.T) {
	cases := []struct {
		name     string
		retained int
		ref      state.Ref
	}{
		{"at-3", 1, state.Ref{Kind: state.RefAtN, N: 3, Display: "@-3"}},
		{"latest", 0, state.Ref{Kind: state.RefLatest, Display: "latest"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &shortWindowCheckpoints{Repo: checkpointjson.NewRepo(paths.New(t.TempDir())), retained: tc.retained, readable: 5}
			r := state.NewResolver(state.Deps{Checkpoints: repo, Hasher: blake3hash.New()})

			_, err := r.Resolve(context.Background(), tc.ref, state.NowContext{})
			if err == nil {
				t.Fatalf("Resolve(%s) over a short retained window = nil error, want a loud failure", tc.ref.Display)
			}
			if errors.Is(err, state.ErrOutOfRange) || errors.Is(err, state.ErrNoCheckpoints) {
				t.Fatalf("a broken read contract was reported as an ordinary user error: %v", err)
			}
		})
	}
}

// TestLatestRejectsUnreadableRecordOutsideTheWindow proves the bounded read does not
// weaken the fully-readable-store requirement: the damaged checkpoint is older than
// every header "latest" retains, so a scan that stopped at its window would resolve a
// reference out of a store it cannot fully order.
func TestLatestRejectsUnreadableRecordOutsideTheWindow(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	base := time.Unix(1_700_000_000, 0).UTC()
	// The corrupt record is the oldest; four readable ones sit above it.
	corrupt := putCheckpoint(t, repo, 0x01, base, fullHex)
	for i := 1; i <= 4; i++ {
		putCheckpoint(t, repo, byte(0x10+i), base.Add(time.Duration(i)*time.Minute), fullHex)
	}
	header := filepath.Join(layout.CheckpointsDir(), corrupt.String(), "header.json")
	if err := os.Chmod(header, 0o644); err != nil {
		t.Fatalf("chmod header: %v", err)
	}
	if err := os.WriteFile(header, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}

	r := state.NewResolver(state.Deps{Checkpoints: repo, Hasher: blake3hash.New()})
	if _, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefLatest, Display: "latest"}, state.NowContext{}); !errors.Is(err, state.ErrLatestCorrupt) {
		t.Fatalf("Resolve(latest) err = %v, want ErrLatestCorrupt", err)
	}
	if _, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefAtN, N: 1, Display: "@-1"}, state.NowContext{}); !errors.Is(err, state.ErrLatestCorrupt) {
		t.Fatalf("Resolve(@-1) err = %v, want ErrLatestCorrupt", err)
	}
}

// TestAtNOutOfRangeReportsTheExactReadableTotal proves the out-of-range message counts
// the whole store rather than the retained window: @-9 over five checkpoints must name
// five, not the nine headers it asked to retain.
func TestAtNOutOfRangeReportsTheExactReadableTotal(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 5; i++ {
		putCheckpoint(t, repo, byte(0x10+i), base.Add(time.Duration(i)*time.Minute), fullHex)
	}
	r := state.NewResolver(state.Deps{Checkpoints: repo, Hasher: blake3hash.New()})

	_, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefAtN, N: 9, Display: "@-9"}, state.NowContext{})
	if !errors.Is(err, state.ErrOutOfRange) {
		t.Fatalf("Resolve(@-9) err = %v, want ErrOutOfRange", err)
	}
	if want := "only 5 checkpoint(s) exist"; !strings.Contains(err.Error(), want) {
		t.Fatalf("Resolve(@-9) err = %q, want it to report %q", err, want)
	}
}
