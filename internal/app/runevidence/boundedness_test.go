package runevidence

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"awarer/internal/app/state"
	"awarer/internal/app/stateprovider"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

// genCursor yields n manifest records lazily, retaining none: Next advances a counter and
// Record returns a fresh zero record. A streaming consumer (the production verify loop,
// which drives Next and discards) holds O(1); a materializing consumer that collected the
// records would retain O(n), which the peak-memory oracle below detects.
type genCursor struct{ remaining int }

func (c *genCursor) Next() bool {
	if c.remaining <= 0 {
		return false
	}
	c.remaining--
	return true
}
func (c *genCursor) Record() worktree.ManifestRecord { return worktree.ManifestRecord{} }
func (c *genCursor) Err() error                      { return nil }
func (c *genCursor) Close() error                    { return nil }

type genStream struct{ n int }

func (s genStream) Open(context.Context) (worktree.ManifestCursor, error) {
	return &genCursor{remaining: s.n}, nil
}

// genStubRuns is the resolver's run-observation port: it hands back a read whose manifest
// yields n records, so the verify drain processes n records per side.
type genStubRuns struct {
	id   runcache.RunID
	tree hashing.TreeHash
	cfg  hashing.ConfigHash
	n    int
}

func (s genStubRuns) Resolve(context.Context, string) (runcache.RunID, error) { return s.id, nil }

func (s genStubRuns) Observation(runcache.RunID, bool) (runcache.RunObservationRead, error) {
	return runcache.NewRunObservationRead(genStream{n: s.n}, s.tree, s.cfg, runcache.ReusableCacheEntry())
}

// genStubStore is the builder's RunStore port. It exposes no payload opener at all, so the
// builder is structurally unable to read output bytes on the default path.
type genStubStore struct{ entry runcache.RunEntry }

func (s genStubStore) Get(runcache.RunID) (runcache.RunEntry, error) { return s.entry, nil }

func (s genStubStore) OutputInspectability(runcache.RunID) (stdout, stderr runevidence.OutputInspectability, err error) {
	p, err := runevidence.NewOutputInspectability(runevidence.PresencePresent)
	return p, p, err
}

// TestBuildDoesNotMaterializeManifests proves the default JSON build path is bounded: it
// stream-verifies the before/after observation manifests (draining each once) and never
// retains their records, so its peak live memory does not scale with manifest cardinality.
// The oracle compares peak live heap across a 10x cardinality increase: a materializing
// build (one that collected the drained records) would retain O(n) records and blow the
// band, while the streaming production path stays flat. It complements the structural fact
// that the builder's output types (Evidence, evidenceDoc) have no field able to hold a
// manifest record.
func TestBuildDoesNotMaterializeManifests(t *testing.T) {
	// A materialized 100k-record slice is many MiB live; the streaming drain holds only
	// bounded decode/GC-lag noise. 8 MiB sits above that noise yet well under a
	// materialization regression, mirroring the compare path's boundedness oracle.
	const band = 8 << 20 // 8 MiB
	small := buildPeakLiveBytes(t, 10_000)
	large := buildPeakLiveBytes(t, 100_000)
	t.Logf("peak live heap: n=10000 -> %d bytes, n=100000 -> %d bytes (delta %d)", small, large, large-small)
	if large-small > band {
		t.Errorf("peak live heap grew by %d bytes across a 10x cardinality increase (band %d): the evidence build materializes manifest records, it does not stream", large-small, band)
	}
}

// buildPeakLiveBytes returns the peak live heap growth during a build whose observation
// manifests each yield n records.
func buildPeakLiveBytes(t *testing.T, n int) int64 {
	t.Helper()
	id, tree, cfg := boundIdentity(t)
	resolver := state.NewResolver(state.Deps{
		Runs:   genStubRuns{id: id, tree: tree, cfg: cfg, n: n},
		Hasher: blake3hash.New(),
	})
	svc := NewService(genStubStore{entry: boundRecord(t, id, tree, cfg)}, stateprovider.NewAssessor(resolver))

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	var peak atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if live := int64(ms.HeapAlloc) - int64(base.HeapAlloc); live > peak.Load() {
				peak.Store(live)
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	_, err := svc.Build(context.Background(), id)
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("n=%d: Build: %v", n, err)
	}
	return peak.Load()
}

// boundIdentity returns a run id and the tree/scan-config identity shared by the record's
// persisted observations and the resolver's observation read, so the aggregate's
// resolved-vs-persisted cross-check is satisfied and the resolved path is exercised.
func boundIdentity(t *testing.T) (runcache.RunID, hashing.TreeHash, hashing.ConfigHash) {
	t.Helper()
	id, err := runcache.NewRunID(1, strings.NewReader("abcdefghijklmnop"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	h := blake3hash.New()
	tree := h.HashBytes([]byte("tree"))
	return id, tree, hashing.ConfigHashFromTree(h.HashBytes([]byte("cfg")))
}

func boundRecord(t *testing.T, id runcache.RunID, tree hashing.TreeHash, cfg hashing.ConfigHash) runcache.RunEntry {
	t.Helper()
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatalf("NewExecutionCWD: %v", err)
	}
	mutation, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		t.Fatalf("NewMutationStatus: %v", err)
	}
	guard, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		t.Fatalf("NewEffectGuardStatus: %v", err)
	}
	h := blake3hash.New()
	emptyHash, err := h.HashReader(strings.NewReader(""))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	effect, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		t.Fatalf("ObservedEffect: %v", err)
	}
	start := time.Unix(1000, 0)
	ki := runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"go", "test"}, RawExecutable: "go"},
		CWD:                cwd,
		InputTreeHash:      tree,
		Effect:             effect,
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(tree),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
	before := runcache.Observation{Manifest: runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: tree, RecordCount: 1}, ScanConfigHash: cfg}
	after := runcache.Observation{Manifest: runcache.ManifestRef{File: "after.manifest.jsonl", TreeHash: tree, RecordCount: 1}, ScanConfigHash: cfg}
	e := runcache.RunEntry{
		ID:          id,
		Key:         ki.Compute(h),
		KeyInput:    ki,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      runcache.OutputCapture{File: "stdout.log", Hash: emptyHash, TruncationPolicy: runcache.TruncationNone},
		Stderr:      runcache.OutputCapture{File: "stderr.log", Hash: emptyHash, TruncationPolicy: runcache.TruncationNone},
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    mutation,
		EffectGuard: guard,
		Before:      &before,
		After:       &after,
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("boundRecord invalid: %v", err)
	}
	return e
}
