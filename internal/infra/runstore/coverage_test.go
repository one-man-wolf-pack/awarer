package runstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
)

var observationFrontier = regexp.MustCompile(`,\s*"frontier": \{[^}]*\}`)

func editMeta(t *testing.T, meta string, edit func(string) string) {
	t.Helper()
	data, err := os.ReadFile(meta) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meta, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(edit(string(data))), 0o600); err != nil {
		t.Fatal(err)
	}
}

func drainCoverage(t *testing.T, c worktree.Coverage) error {
	t.Helper()
	stream, _, ok := c.Frontier()
	if !ok {
		t.Fatalf("coverage %v carries no frontier", c.Kind())
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
	}
	return cur.Err()
}

func TestRunObservationsCarryTheirOwnFrontiers(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "cov", "out", "", 1<<20)
	for _, after := range []bool{false, true} {
		read, err := r.Observation(entry.ID, after)
		if err != nil {
			t.Fatal(err)
		}
		if read.Coverage().Kind() != worktree.CoverageRecorded {
			t.Fatalf("observation after=%v coverage = %v, want recorded", after, read.Coverage().Kind())
		}
		if err := drainCoverage(t, read.Coverage()); err != nil {
			t.Fatalf("observation after=%v frontier: %v", after, err)
		}
	}
}

func TestPredecessorRunReadsWithUnknownCoverage(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "legacy", "out", "", 1<<20)
	meta := metaPathOf(t, layout.RunsDir(), entry.ID)
	editMeta(t, meta, func(d string) string {
		d = regexp.MustCompile(`"schema_version": [0-9]+`).ReplaceAllString(d, `"schema_version": 1`)
		return observationFrontier.ReplaceAllString(d, "")
	})
	for _, name := range []string{"before.frontier.jsonl", "after.frontier.jsonl"} {
		if err := os.Remove(filepath.Join(filepath.Dir(meta), name)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.Get(entry.ID)
	if err != nil {
		t.Fatalf("a predecessor run is unreadable: %v", err)
	}
	if got.Before.Frontier != nil || got.After.Frontier != nil {
		t.Fatal("a predecessor run claims frontiers")
	}
	read, err := r.Observation(entry.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if read.Coverage().Kind().Known() {
		t.Fatalf("predecessor coverage = %v, want unknown", read.Coverage().Kind())
	}
}

func TestProofBearingRunRequiresAndVerifiesItsFrontiers(t *testing.T) {
	r, layout, h := newStore(t)
	stripped := storeRun(t, r, h, "stripped", "out", "", 1<<20)
	editMeta(t, metaPathOf(t, layout.RunsDir(), stripped.ID), func(d string) string {
		return observationFrontier.ReplaceAllString(d, "")
	})
	if _, err := r.Get(stripped.ID); !errors.Is(err, runcache.ErrCorruptStore) || errors.Is(err, runcache.ErrIncompatibleEntry) {
		t.Fatalf("a schema-2 run without frontiers = %v, want corruption", err)
	}

	tampered := storeRun(t, r, h, "tampered", "out", "", 1<<20)
	frontier := filepath.Join(filepath.Dir(metaPathOf(t, layout.RunsDir(), tampered.ID)), "before.frontier.jsonl")
	if err := os.Chmod(frontier, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(frontier, []byte(`{"path":"forged","limit":"node"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := r.Observation(tampered.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := drainCoverage(t, read.Coverage()); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Fatalf("a tampered frontier = %v, want corruption", err)
	}
}

func TestCommitRefusesAnObservationWithoutAFrontier(t *testing.T) {
	r, _, h := newStore(t)
	obs := unchangedObs()
	obs.AfterFrontier = nil
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: 1, MaxStderr: 1})
	if err != nil {
		t.Fatal(err)
	}
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	entry := runEntryFor(t, h, "nofrontier", so, se)
	if err := pending.Commit(context.Background(), entry, obs); err == nil {
		t.Fatal("published a run observation without coverage evidence")
	}
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrNotFound) {
		t.Fatalf("Get after a refused commit = %v, want not found", err)
	}
}

func runEntryFor(t *testing.T, h hashing.Hasher, disc string, so, se runcache.OutputCapture) runcache.RunEntry {
	t.Helper()
	id, err := runcache.NewRunID(time.Now().UnixNano(), strings.NewReader("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	keyInput := keyInputFor(h, disc)
	start := time.Now()
	return runcache.RunEntry{
		ID: id, Key: keyInput.Compute(h), KeyInput: keyInput,
		StartedAt: start, FinishedAt: start.Add(time.Second),
		Exit:   runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout: so, Stderr: se,
		Decision: runcache.CacheDecision{Cacheable: true}, Reuse: runcache.ReusableCacheEntry(),
		Mutation: mustUnchanged(), EffectGuard: mustUnchangedEffect(),
	}
}
