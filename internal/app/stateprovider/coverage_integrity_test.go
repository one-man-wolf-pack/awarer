package stateprovider_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/provider"
)

func removeFrontier(t *testing.T, layout paths.Layout, id checkpoint.CheckpointID) {
	t.Helper()
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "frontier.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestMissingRequiredFrontierIsUnavailableWithoutAMerge(t *testing.T) {
	a, repo, layout := newAssessor(t)
	damaged := putCP(t, repo, 0x11, hexA, "a")
	otherPolicy := putCP(t, repo, 0x22, hexA, "b")
	healthy := putCP(t, repo, 0x33, hexB, "a")
	removeFrontier(t, layout, damaged)

	res, err := a.Resolve(context.Background(), idRef(damaged), damaged.String(), state.NowContext{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome() != provider.OutcomeUnavailable {
		t.Fatalf("resolve outcome = %v, want unavailable", res.Outcome())
	}
	if r, _ := res.Reason(); r != provider.ReasonMetadataCorrupt {
		t.Errorf("resolve reason = %v, want metadata-corrupt", r)
	}

	for name, right := range map[string]checkpoint.CheckpointID{
		"same record":         damaged,
		"incomparable policy": otherPolicy,
		"needs the merge":     healthy,
	} {
		cmp, err := a.Compare(context.Background(), state.Range{Left: idRef(damaged), Right: idRef(right)}, state.NowContext{})
		if err != nil {
			t.Fatalf("%s: Compare fault: %v", name, err)
		}
		if cmp.Relationship() != provider.RelUnavailable {
			t.Errorf("%s: relationship = %v, want unavailable", name, cmp.Relationship())
		}
		if r, _ := cmp.Reason(); r != provider.ReasonMetadataCorrupt {
			t.Errorf("%s: reason = %v, want metadata-corrupt", name, r)
		}
		if _, ok := cmp.Left(); ok {
			t.Errorf("%s: published the operand whose proof is missing", name)
		}
	}
}

func TestFrontierVerificationCancellationIsAFault(t *testing.T) {
	a, repo, _ := newAssessor(t)
	id := putCP(t, repo, 0x11, hexA, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Resolve(ctx, idRef(id), id.String(), state.NowContext{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve under cancellation = %v, want context.Canceled", err)
	}
}
