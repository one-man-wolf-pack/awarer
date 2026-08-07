package logcmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
)

// fakeRepo embeds checkpoint.Repository so the methods logcmd does not use are
// satisfied by the embedded nil interface; logcmd reads store health, so only
// StoreHealth is overridden here. skipped lets a test add unreadable records so the
// partial-listing behavior is exercised.
type fakeRepo struct {
	checkpoint.Repository
	list    []checkpoint.CheckpointHeader
	skipped []checkpoint.ReadFinding
}

func (f fakeRepo) StoreHealth(context.Context) (checkpoint.CheckpointStoreHealth, error) {
	return checkpoint.NewCheckpointStoreHealth(f.list, f.skipped), nil
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
	// StoreHealth sorts headers newest-first, so give each a distinct creation time
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
	svc := New(fakeRepo{list: checkpoints(t, 25)})
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

func TestRunAll(t *testing.T) {
	svc := New(fakeRepo{list: checkpoints(t, 25)})
	res, _ := svc.Run(context.Background(), Request{All: true})
	if len(res.Entries) != 25 {
		t.Fatalf("entries = %d, want 25", len(res.Entries))
	}
}

func TestRunLatest(t *testing.T) {
	all := checkpoints(t, 5)
	svc := New(fakeRepo{list: all})
	res, _ := svc.Run(context.Background(), Request{Latest: true})
	if len(res.Entries) != 1 || res.Entries[0].ID != all[0].ID {
		t.Fatalf("latest selection wrong: %+v", res.Entries)
	}
}

func TestRunExplicitLimit(t *testing.T) {
	svc := New(fakeRepo{list: checkpoints(t, 5)})
	res, _ := svc.Run(context.Background(), Request{Limit: 2})
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(res.Entries))
	}
}
