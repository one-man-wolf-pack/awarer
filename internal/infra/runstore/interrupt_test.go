package runstore

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

// internalEmptyTreeHash is the tree hash of an empty manifest.
func internalEmptyTreeHash(h hashing.Hasher) hashing.TreeHash {
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(nil, nil))
	if err != nil {
		panic(err)
	}
	return red.Hash
}

// internalTestScanCfg is a fixed, valid scan_config_hash for the internal test
// observations, which carry the empty tree hash.
func internalTestScanCfg() hashing.ConfigHash {
	ch, err := hashing.ParseConfigHash("blake3:" + strings.Repeat("a", 64))
	if err != nil {
		panic(err)
	}
	return ch
}

// internalUnchangedObs builds observations whose before/after are both empty (with
// one shared scan policy identity), so a recorded run is unchanged and reusable.
func internalUnchangedObs() runcache.RunObservations {
	cfg := internalTestScanCfg()
	return runcache.RunObservations{
		Before:               scantest.CanonicalStream(nil, nil),
		After:                scantest.CanonicalStream(nil, nil),
		BeforeScanConfigHash: cfg,
		AfterScanConfigHash:  cfg,
	}
}

// internalUnchangedEffect is the unchanged effect guard.
func internalUnchangedEffect() runcache.EffectGuardStatus {
	g, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		panic(err)
	}
	return g
}

// internalObservedEffect is the effect identity every execution keys on: production
// always observes the non-empty built-in watch set.
func internalObservedEffect(h hashing.Hasher) runcache.EffectObservation {
	o, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		panic(err)
	}
	return o
}

// internalUnchanged is the unchanged mutation status.
func internalUnchanged() runcache.MutationStatus {
	st, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		panic(err)
	}
	return st
}

// These tests live in package runstore (not runstore_test) so they can set the
// unexported fault-injection seam (Repo.fail).

func newInternalStore(t *testing.T) (*Repo, paths.Layout, hashing.Hasher) {
	t.Helper()
	root := t.TempDir()
	layout := paths.New(root)
	for _, d := range layout.RequiredDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	h := blake3hash.New()
	return New(layout, h), layout, h
}

func internalKeyInput(h hashing.Hasher, disc string) runcache.KeyInput {
	cwd, _ := runcache.NewExecutionCWD(".")
	return runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"echo", disc}, RawExecutable: "echo"},
		CWD:                cwd,
		InputTreeHash:      internalEmptyTreeHash(h),
		Effect:             internalObservedEffect(h),
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(h.HashBytes([]byte("cfg"))),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
}

// commitOne builds and commits one run discriminated by disc through r, returning
// the entry it tried to publish (whether or not Commit succeeded).
func commitOne(t *testing.T, r *Repo, h hashing.Hasher, disc string) (runcache.RunEntry, error) {
	t.Helper()
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: 1 << 20, MaxStderr: 1 << 20})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := io.WriteString(pending.Stdout(), "out-"+disc); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := io.WriteString(pending.Stderr(), "err-"+disc); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	id, err := runcache.NewRunID(time.Now().UnixNano(), strings.NewReader("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	ki := internalKeyInput(h, disc)
	start := time.Now()
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
	return entry, pending.Commit(context.Background(), entry, internalUnchangedObs())
}

// TestCommitCrashAfterMetaBeforePointerOrphanNotHit proves a crash after the entry
// is durable but before its key pointer is written leaves an orphan entry that
// Lookup reports as a clean miss (never a partial hit), recoverable by gc/doctor.
func TestCommitCrashAfterMetaBeforePointerOrphanNotHit(t *testing.T) {
	r, layout, h := newInternalStore(t)
	r.fail = func(stage failStage) error {
		if stage == failAfterMetaBeforePointer {
			return errCrash
		}
		return nil
	}
	entry, err := commitOne(t, r, h, "orphan")
	if err == nil {
		t.Fatalf("expected commit to abort at the crash point")
	}

	// A fresh repo over the same store: the entry exists but resolves to no hit.
	fresh := New(layout, h)
	if _, ok, lerr := fresh.Lookup(entry.Key); lerr != nil || ok {
		t.Fatalf("Lookup of orphan = (ok %v, err %v), want a clean miss", ok, lerr)
	}
	if _, gerr := fresh.Get(entry.ID); gerr != nil {
		t.Fatalf("Get(orphan id) should succeed (entry is durable): %v", gerr)
	}
	refs, rerr := fresh.ListRefs(context.Background())
	if rerr != nil {
		t.Fatalf("ListRefs: %v", rerr)
	}
	found := false
	for _, id := range refs {
		if id == entry.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("orphan entry should be listed for reclamation")
	}
}

// TestCommitLiveErrorAfterMetaRollsBack proves a plain (non-crash) error in the
// same window rolls the entry back: nothing is left for Lookup, Get, or ListRefs.
func TestCommitLiveErrorAfterMetaRollsBack(t *testing.T) {
	r, layout, h := newInternalStore(t)
	r.fail = func(stage failStage) error {
		if stage == failAfterMetaBeforePointer {
			return errors.New("boom after meta")
		}
		return nil
	}
	entry, err := commitOne(t, r, h, "rollback")
	if err == nil {
		t.Fatalf("expected commit to abort with a live error")
	}

	fresh := New(layout, h)
	if _, ok, lerr := fresh.Lookup(entry.Key); lerr != nil || ok {
		t.Fatalf("Lookup after rollback = (ok %v, err %v), want a clean miss", ok, lerr)
	}
	if _, gerr := fresh.Get(entry.ID); gerr == nil {
		t.Fatalf("Get after rollback should fail; the entry must be gone")
	}
}

// TestCommitCrashAfterPayloadBeforeMetaLeavesNoEntry proves a crash before the
// staged entry is renamed into the shard never publishes it: a fresh process sees
// no entry (only a reclaimable temp leftover).
func TestCommitCrashAfterPayloadBeforeMetaLeavesNoEntry(t *testing.T) {
	r, layout, h := newInternalStore(t)
	r.fail = func(stage failStage) error {
		if stage == failAfterPayloadBeforeMeta {
			return errCrash
		}
		return nil
	}
	entry, err := commitOne(t, r, h, "staged")
	if err == nil {
		t.Fatalf("expected commit to abort at the crash point")
	}

	fresh := New(layout, h)
	if _, ok, lerr := fresh.Lookup(entry.Key); lerr != nil || ok {
		t.Fatalf("Lookup = (ok %v, err %v), want a clean miss", ok, lerr)
	}
	if _, gerr := fresh.Get(entry.ID); gerr == nil {
		t.Fatalf("Get should fail; the entry was never published")
	}
}

// TestCommitReRunAfterCrashSucceeds proves a clean re-run after an orphaning crash
// publishes a resolvable entry with no manual cleanup.
func TestCommitReRunAfterCrashSucceeds(t *testing.T) {
	r, layout, h := newInternalStore(t)
	r.fail = func(stage failStage) error {
		if stage == failAfterMetaBeforePointer {
			return errCrash
		}
		return nil
	}
	if _, err := commitOne(t, r, h, "k"); err == nil {
		t.Fatalf("expected first commit to abort")
	}

	r.fail = nil
	entry, err := commitOne(t, r, h, "k")
	if err != nil {
		t.Fatalf("re-run commit: %v", err)
	}
	fresh := New(layout, h)
	if _, ok, lerr := fresh.Lookup(entry.Key); lerr != nil || !ok {
		t.Fatalf("Lookup after recovery = (ok %v, err %v), want a hit", ok, lerr)
	}
}
