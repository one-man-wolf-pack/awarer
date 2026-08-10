package logcmd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
)

// fakeRepo embeds checkpoint.Repository so the methods logcmd does not use are
// satisfied by the embedded nil interface. Only the bounded store-health read is
// implemented, because that is the only one the port offers. The requested windows are
// recorded so a test can distinguish "asked for one" from "asked for twenty". counts
// carries the store-wide tallies independently of the retained headers, which is what a
// real bounded read returns.
type fakeRepo struct {
	checkpoint.Repository
	all      []checkpoint.CheckpointHeader
	counts   checkpoint.StoreReadCounts
	requests *[]int
}

func (f fakeRepo) StoreHealthNewest(_ context.Context, newest int) (checkpoint.CheckpointStoreHealth, error) {
	if newest <= 0 {
		return checkpoint.CheckpointStoreHealth{}, errors.New("newest header window must be positive")
	}
	if f.requests != nil {
		*f.requests = append(*f.requests, newest)
	}
	return checkpoint.NewCheckpointStoreHealth(f.storeCounts(), f.all[:min(newest, len(f.all))]), nil
}

func (f fakeRepo) storeCounts() checkpoint.StoreReadCounts {
	counts := f.counts
	if counts.Readable == 0 {
		counts.Readable = len(f.all)
	}
	return counts
}

func id(t *testing.T, b byte) checkpoint.CheckpointID {
	t.Helper()
	v, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{b}, 20)))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func checkpoints(t *testing.T, n int) []checkpoint.CheckpointHeader {
	t.Helper()
	// The store returns headers newest-first, so give each a distinct creation time
	// with index 0 the newest — that keeps out[0] the "latest" the tests expect.
	base := time.Unix(1_700_000_000, 0).UTC()
	out := make([]checkpoint.CheckpointHeader, n)
	for i := 0; i < n; i++ {
		out[i] = checkpoint.CheckpointHeader{
			ID:        id(t, byte(i)),
			CreatedAt: base.Add(-time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestRunDefaultLimit(t *testing.T) {
	svc := New(fakeRepo{all: checkpoints(t, 25)})
	res, err := svc.Run(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Entries) != DefaultLimit {
		t.Fatalf("entries = %d, want %d", len(res.Entries), DefaultLimit)
	}
	if res.Total != 25 {
		t.Fatalf("total = %d, want 25", res.Total)
	}
}

func TestRunLatest(t *testing.T) {
	all := checkpoints(t, 5)
	svc := New(fakeRepo{all: all})
	res, _ := svc.Run(context.Background(), Request{Latest: true})
	if len(res.Entries) != 1 || res.Entries[0].ID != all[0].ID {
		t.Fatalf("latest selection wrong: %+v", res.Entries)
	}
}

func TestRunExplicitLimit(t *testing.T) {
	svc := New(fakeRepo{all: checkpoints(t, 5)})
	res, _ := svc.Run(context.Background(), Request{Limit: 2})
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(res.Entries))
	}
}

// TestRunRequestsExactlyTheRenderedWindow is the boundedness oracle: every request
// shape must ask the store for exactly the window it will render. Asking for a wider
// window fails here; asking for the whole history is not expressible at all, since the
// port logcmd holds carries only the bounded read.
func TestRunRequestsExactlyTheRenderedWindow(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want int
	}{
		{"default", Request{}, DefaultLimit},
		{"explicit limit", Request{Limit: 3}, 3},
		{"latest", Request{Latest: true}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests []int
			svc := New(fakeRepo{all: checkpoints(t, 25), requests: &requests})
			if _, err := svc.Run(context.Background(), tc.req); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(requests) != 1 {
				t.Fatalf("store read %d times, want exactly 1: %v", len(requests), requests)
			}
			if requests[0] != tc.want {
				t.Fatalf("store asked for the newest %d, want %d", requests[0], tc.want)
			}
		})
	}
}

// TestRunReportsStoreWideTotals proves the exact counts come from the read pass, not
// from the retained window: a default listing over a store far larger than its window
// still reports the store's readable total and its incompatible count.
func TestRunReportsStoreWideTotals(t *testing.T) {
	svc := New(fakeRepo{
		all:    checkpoints(t, 25),
		counts: checkpoint.StoreReadCounts{Readable: 900, Incompatible: 4},
	})
	res, err := svc.Run(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 900 {
		t.Errorf("total = %d, want the store-wide readable count 900", res.Total)
	}
	if res.Skipped != 4 {
		t.Errorf("skipped = %d, want the store-wide incompatible count 4", res.Skipped)
	}
	if len(res.Entries) != DefaultLimit {
		t.Errorf("entries = %d, want the rendered window %d", len(res.Entries), DefaultLimit)
	}
}

// TestRunFailsOnCorruptionOutsideTheWindow proves damage older than the shown window
// still fails the command: the corrupt count describes the whole store, so a bounded
// listing can never present a damaged store as a normal short one.
func TestRunFailsOnCorruptionOutsideTheWindow(t *testing.T) {
	svc := New(fakeRepo{
		all:    checkpoints(t, 25),
		counts: checkpoint.StoreReadCounts{Readable: 900, Corrupt: 1},
	})
	_, err := svc.Run(context.Background(), Request{})
	if !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Run err = %v, want ErrCorruptStore", err)
	}
}
