package runstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/runstore"
	"awarer/internal/scantest"
)

// listEntries enumerates every stored run through the production streaming surface —
// ListRefs for the ids, Get for each entry — so a test that needs the whole (small)
// fixture history reads it the way production does, one decoded entry at a time
// rather than through a store method that materializes them all.
func listEntries(r *runstore.Repo) ([]runcache.RunEntry, error) {
	ids, err := r.ListRefs(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]runcache.RunEntry, 0, len(ids))
	for _, id := range ids {
		entry, err := r.Get(id)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// manifestTreeHash returns the reduced tree hash of a slice manifest, the value a
// recorded run's observation manifest reduces to.
func manifestTreeHash(h hashing.Hasher, entries []worktree.Entry, skipped []worktree.SkippedInput) hashing.TreeHash {
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(entries, skipped))
	if err != nil {
		panic(err)
	}
	return red.Hash
}

// emptyTreeHash is the tree hash of an empty manifest, the InputTreeHash test key
// inputs use so the before-observation (an empty manifest) reproduces it.
func emptyTreeHash(h hashing.Hasher) hashing.TreeHash {
	return manifestTreeHash(h, nil, nil)
}

// testScanCfg is a fixed, valid scan_config_hash, so a stored observation carries
// the scan-policy identity its manifest tree hash needs to be resolvable.
func testScanCfg() hashing.ConfigHash {
	ch, err := hashing.ParseConfigHash("blake3:" + strings.Repeat("a", 64))
	if err != nil {
		panic(err)
	}
	return ch
}

// unchangedObs builds observations whose before and after are both the empty
// manifest (with one shared scan policy identity), so a recorded run is unchanged
// (and therefore reusable).
func unchangedObs() runcache.RunObservations {
	cfg := testScanCfg()
	return runcache.RunObservations{
		Before:               scantest.CanonicalStream(nil, nil),
		After:                scantest.CanonicalStream(nil, nil),
		BeforeScanConfigHash: cfg,
		AfterScanConfigHash:  cfg,
	}
}

// mustUnchanged returns the unchanged mutation status, the outcome of a reusable
// recorded run.
func mustUnchanged() runcache.MutationStatus {
	st, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		panic(err)
	}
	return st
}

// mustUnchangedEffect returns the unchanged effect guard, the effect-side companion
// of an unchanged mutation for a run whose watched roots came back identical.
func mustUnchangedEffect() runcache.EffectGuardStatus {
	g, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		panic(err)
	}
	return g
}

// mustObservedEffect returns the effect identity every execution keys on: production
// always observes the non-empty built-in watch set, so no stored key input carries
// anything else.
func mustObservedEffect(h hashing.Hasher) runcache.EffectObservation {
	o, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		panic(err)
	}
	return o
}

func newStore(t testing.TB) (*runstore.Repo, paths.Layout, hashing.Hasher) {
	t.Helper()
	root := t.TempDir()
	layout := paths.New(root)
	for _, d := range layout.RequiredDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	h := blake3hash.New()
	return runstore.New(layout, h), layout, h
}

// keyInputFor builds a KeyInput discriminated by disc (via the command argv), so
// distinct discriminators yield distinct keys and the same discriminator yields
// the same key. The store enforces entry.Key == KeyInput.Compute, so test entries
// must be built from a real KeyInput rather than an arbitrary key.
func keyInputFor(h hashing.Hasher, disc string) runcache.KeyInput {
	return keyInputForTree(h, disc, emptyTreeHash(h))
}

// keyInputForTree is keyInputFor over a chosen input tree hash. The store requires a
// published run's before-observation to reduce to exactly the keyed input tree hash,
// so a fixture with a non-empty observation has to key on that observation's hash.
func keyInputForTree(h hashing.Hasher, disc string, tree hashing.TreeHash) runcache.KeyInput {
	cwd, _ := runcache.NewExecutionCWD(".")
	return runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"echo", disc}, RawExecutable: "echo"},
		CWD:                cwd,
		InputTreeHash:      tree,
		Effect:             mustObservedEffect(h),
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(h.HashBytes([]byte("cfg"))),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
}

// keyFor returns the run key a discriminator resolves to, without storing anything.
func keyFor(h hashing.Hasher, disc string) runcache.RunKey {
	return keyInputFor(h, disc).Compute(h)
}

// storeRunWithObs is storeRun with caller-supplied observations, for tests about
// what the observation manifests themselves persist.
func storeRunWithObs(t testing.TB, r *runstore.Repo, h hashing.Hasher, disc string, obs runcache.RunObservations, inputTree hashing.TreeHash) runcache.RunEntry {
	t.Helper()
	return storeRunObs(t, r, h, disc, "out", "", 1<<20, obs, inputTree)
}

// storeRun publishes a run discriminated by disc, returning the published entry.
// Its key is computed from the entry's own KeyInput, satisfying the store's
// self-consistency invariant.
func storeRun(t testing.TB, r *runstore.Repo, h hashing.Hasher, disc, stdout, stderr string, limit int64) runcache.RunEntry {
	t.Helper()
	return storeRunObs(t, r, h, disc, stdout, stderr, limit, unchangedObs(), emptyTreeHash(h))
}

// storeRunObs publishes one run with explicit observations. It is the single commit
// path the fixtures share, so a test about observation contents and a test about
// metadata cannot drift on how a run is stored.
func storeRunObs(t testing.TB, r *runstore.Repo, h hashing.Hasher, disc, stdout, stderr string, limit int64, obs runcache.RunObservations, inputTree hashing.TreeHash) runcache.RunEntry {
	t.Helper()
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: limit, MaxStderr: limit})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := io.WriteString(pending.Stdout(), stdout); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := io.WriteString(pending.Stderr(), stderr); err != nil {
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
	keyInput := keyInputForTree(h, disc, inputTree)
	start := time.Now()
	entry := runcache.RunEntry{
		ID:          id,
		Key:         keyInput.Compute(h),
		KeyInput:    keyInput,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      so,
		Stderr:      se,
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    mustUnchanged(),
		EffectGuard: mustUnchangedEffect(),
	}
	if err := pending.Commit(context.Background(), entry, obs); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return entry
}

func TestStoreLookupRoundTrip(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "k1", "hello stdout", "hello stderr", 1<<20)

	ce, ok, err := r.Lookup(entry.Key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("expected a hit")
	}
	if got := ce.Entry(); got.ID != entry.ID {
		t.Errorf("id = %s, want %s", got.ID, entry.ID)
	}

	so, err := r.OpenStdout(entry.ID)
	if err != nil {
		t.Fatalf("OpenStdout: %v", err)
	}
	defer so.Close()
	b, _ := io.ReadAll(so)
	if string(b) != "hello stdout" {
		t.Errorf("stdout = %q", b)
	}
}

func TestLookupMissReturnsNoEntry(t *testing.T) {
	r, _, h := newStore(t)
	_, ok, err := r.Lookup(keyFor(h, "absent"))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok {
		t.Error("expected a miss for an unknown key")
	}
}

func TestTruncationHeadOnly(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "trunc", "0123456789ABCDEFGHIJ", "", 5)

	if !entry.Stdout.Truncated {
		t.Fatal("stdout should be truncated")
	}
	if entry.Stdout.OriginalBytes != 20 {
		t.Errorf("original = %d, want 20", entry.Stdout.OriginalBytes)
	}
	if entry.Stdout.OmittedBytes != 15 {
		t.Errorf("omitted = %d, want 15", entry.Stdout.OmittedBytes)
	}
	so, err := r.OpenStdout(entry.ID)
	if err != nil {
		t.Fatalf("OpenStdout: %v", err)
	}
	defer so.Close()
	b, _ := io.ReadAll(so)
	if !strings.HasPrefix(string(b), "01234") {
		t.Errorf("stored head = %q, want prefix 01234", b)
	}
	if !strings.Contains(string(b), "15 bytes omitted") {
		t.Errorf("stored output missing truncation marker: %q", b)
	}
}

func TestPrefixResolution(t *testing.T) {
	r, _, h := newStore(t)
	e1 := storeRun(t, r, h, "a", "x", "", 1<<20)

	id, err := r.Resolve(context.Background(), e1.ID.String()[:8])
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if id != e1.ID {
		t.Errorf("resolved %s, want %s", id, e1.ID)
	}

	if _, err := r.Resolve(context.Background(), "ffffffffffff"); !errors.Is(err, runcache.ErrNotFound) {
		t.Errorf("unknown prefix err = %v, want ErrNotFound", err)
	}
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Error("empty prefix should be rejected")
	}
}

func TestDeleteRemovesEntryAndPointer(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRun(t, r, h, "del", "x", "", 1<<20)

	if err := r.Delete(entry.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrNotFound) {
		t.Errorf("Get after delete err = %v, want ErrNotFound", err)
	}
	_, ok, err := r.Lookup(entry.Key)
	if err != nil {
		t.Fatalf("Lookup after delete: %v", err)
	}
	if ok {
		t.Error("key pointer should be gone after delete")
	}
}

func TestDeleteCleansUpCorruptPointer(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "delcorrupt", "x", "", 1<<20)

	// Corrupt the key pointer so it cannot be decoded. Delete must still remove it
	// along with the entry: leaving an undecodable pointer behind would turn the
	// next Lookup of this key into store corruption instead of a clean miss.
	ptr := filepath.Join(layout.RunsDir(), "keys", "blake3", entry.Key.Hex()[:2], entry.Key.Hex()+".json")
	if err := os.WriteFile(ptr, []byte("not json"), 0o644); err != nil {
		t.Fatalf("corrupt pointer: %v", err)
	}

	if err := r.Delete(entry.ID); err != nil {
		t.Fatalf("Delete with corrupt pointer: %v", err)
	}
	if _, err := os.Stat(ptr); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("corrupt pointer still present after delete (stat err = %v)", err)
	}
	if _, ok, err := r.Lookup(entry.Key); err != nil || ok {
		t.Errorf("Lookup after delete = (ok %v, err %v), want a clean miss", ok, err)
	}
}

func TestDeleteRemovesCrossKeyCorruptPointer(t *testing.T) {
	r, layout, h := newStore(t)
	entryA := storeRun(t, r, h, "A", "out-A", "", 1<<20)
	entryB := storeRun(t, r, h, "B", "out-B", "", 1<<20)

	// Corrupt key A's pointer so it decodes to run B, which is valid but carries a
	// different key. This is not a refresh (a refresh keeps the same key), so deleting
	// run A must remove this stale cross-key pointer too — otherwise a later Lookup of
	// key A would resolve to B and fail as corruption instead of a clean miss.
	ptr := filepath.Join(layout.RunsDir(), "keys", "blake3", entryA.Key.Hex()[:2], entryA.Key.Hex()+".json")
	doc := []byte(`{"schema_version":1,"run_id":"` + entryB.ID.String() + `"}`)
	if err := os.WriteFile(ptr, doc, 0o644); err != nil {
		t.Fatalf("rewrite pointer: %v", err)
	}

	if err := r.Delete(entryA.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(ptr); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cross-key pointer still present after delete (stat err = %v)", err)
	}
	if _, ok, err := r.Lookup(entryA.Key); err != nil || ok {
		t.Errorf("Lookup(key A) after delete = (ok %v, err %v), want a clean miss", ok, err)
	}
	// Run B and its own pointer are untouched.
	if _, err := r.Get(entryB.ID); err != nil {
		t.Errorf("run B should be untouched: %v", err)
	}
	if _, ok, err := r.Lookup(entryB.Key); err != nil || !ok {
		t.Errorf("Lookup(key B) = (ok %v, err %v), want a hit", ok, err)
	}
}

func TestDeleteCorruptMetadataByID(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "corruptmeta", "out", "", 1<<20)

	// Corrupt the metadata so Get rejects the entry. Delete must still remove it by
	// id (the key cannot be derived from the bad document), clearing the entry and
	// its key pointer so the wedged run is gone.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Fatalf("Get should report corruption, got %v", err)
	}

	if err := r.Delete(entry.ID); err != nil {
		t.Fatalf("Delete of a corrupt-metadata run: %v", err)
	}
	entryDir := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String())
	if _, err := os.Stat(entryDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("entry directory still present after delete (stat err = %v)", err)
	}
	ptr := filepath.Join(layout.RunsDir(), "keys", "blake3", entry.Key.Hex()[:2], entry.Key.Hex()+".json")
	if _, err := os.Stat(ptr); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key pointer still present after delete (stat err = %v)", err)
	}
}

func TestIncompleteEntryIsNotAHit(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "incomplete", "x", "y", 1<<20)

	// Simulate an interrupted write: remove the stdout payload from the published
	// entry. A lookup must not replay a partial entry — it is corruption.
	shard := entry.ID.String()[:2]
	payload := filepath.Join(layout.RunsDir(), "entries", shard, entry.ID.String(), "stdout.log")
	if err := os.Remove(payload); err != nil {
		t.Fatalf("remove payload: %v", err)
	}

	// Lookup reads only metadata and the key pointer, so it still resolves the
	// entry; opening the missing payload is where corruption surfaces (the service
	// verifies payloads on the hit path via this same open).
	if _, ok, err := r.Lookup(entry.Key); err != nil || !ok {
		t.Fatalf("Lookup metadata still ok: err=%v ok=%v", err, ok)
	}
	if _, err := r.OpenStdout(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("OpenStdout on missing payload err = %v, want ErrCorruptStore", err)
	}
}

func TestCorruptPayloadDetected(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "corrupt", "original", "", 1<<20)

	shard := entry.ID.String()[:2]
	payload := filepath.Join(layout.RunsDir(), "entries", shard, entry.ID.String(), "stdout.log")
	// Published payloads are read-only; make it writable to simulate tampering.
	if err := os.Chmod(payload, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(payload, []byte("tampered!"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := r.OpenStdout(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("OpenStdout on tampered payload err = %v, want ErrCorruptStore", err)
	}
}

// beginFinalized starts a capture, writes the given output, and finalizes it,
// returning the pending run plus the captures Finalize produced.
func beginFinalized(t *testing.T, r *runstore.Repo, stdout, stderr string) (runcache.PendingRun, runcache.OutputCapture, runcache.OutputCapture) {
	t.Helper()
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: 1 << 20, MaxStderr: 1 << 20})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := io.WriteString(pending.Stdout(), stdout); err != nil {
		t.Fatalf("write stdout: %v", err)
	}
	if _, err := io.WriteString(pending.Stderr(), stderr); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return pending, so, se
}

func newRunID(t *testing.T) runcache.RunID {
	t.Helper()
	id, err := runcache.NewRunID(time.Now().UnixNano(), strings.NewReader("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	return id
}

func TestCommitRejectsMismatchedKey(t *testing.T) {
	r, _, h := newStore(t)
	pending, so, se := beginFinalized(t, r, "out", "")

	ki := keyInputFor(h, "x")
	start := time.Now()
	entry := runcache.RunEntry{
		ID:         newRunID(t),
		Key:        keyFor(h, "different"), // valid key, but not the one ki computes
		KeyInput:   ki,
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
		Exit:       runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:     so,
		Stderr:     se,
		Decision:   runcache.CacheDecision{Cacheable: true},
	}
	if err := pending.Commit(context.Background(), entry, runcache.RunObservations{}); err == nil {
		t.Fatal("Commit must reject a key that does not match its inputs")
	}
	// Nothing was published under either the wrong or the real key.
	assertNothingPublished(t, r, keyFor(h, "different"), ki.Compute(h))
}

func TestCommitRejectsMismatchedCaptures(t *testing.T) {
	r, _, h := newStore(t)
	pending, _, se := beginFinalized(t, r, "real-output", "")

	// A valid-looking capture that does not match the finalized stdout payload.
	wrongHash, err := h.HashReader(strings.NewReader("not the stored bytes"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := runcache.OutputCapture{
		OriginalBytes:    5,
		StoredBytes:      5,
		TruncationPolicy: runcache.TruncationNone,
		Hash:             wrongHash,
		File:             "stdout.log",
	}
	ki := keyInputFor(h, "y")
	start := time.Now()
	entry := runcache.RunEntry{
		ID:         newRunID(t),
		Key:        ki.Compute(h),
		KeyInput:   ki,
		StartedAt:  start,
		FinishedAt: start.Add(time.Second),
		Exit:       runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:     wrong,
		Stderr:     se,
		Decision:   runcache.CacheDecision{Cacheable: true},
	}
	if err := pending.Commit(context.Background(), entry, unchangedObs()); err == nil {
		t.Fatal("Commit must reject captures that do not match the finalized payloads")
	}
	assertNothingPublished(t, r, ki.Compute(h))
}

func TestWriteAfterFinalizeRejected(t *testing.T) {
	r, _, h := newStore(t)
	pending, so, se := beginFinalized(t, r, "out", "")

	// A late write must be rejected: the payload hash is already recorded, so
	// changing the bytes now would make the entry self-corrupt.
	if _, err := pending.Stdout().Write([]byte("extra")); err == nil {
		t.Fatal("write after Finalize should be rejected")
	}

	// Committing the finalized captures still yields a consistent entry: the
	// rejected write never touched the payload.
	ki := keyInputFor(h, "seal")
	id := newRunID(t)
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
		Mutation:    mustUnchanged(),
		EffectGuard: mustUnchangedEffect(),
	}
	if err := pending.Commit(context.Background(), entry, unchangedObs()); err != nil {
		t.Fatalf("Commit of a consistent entry: %v", err)
	}
	rc, err := r.OpenStdout(id)
	if err != nil {
		t.Fatalf("OpenStdout: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "out" {
		t.Errorf("stored stdout = %q, want %q (late write must not have landed)", b, "out")
	}
}

func TestFinalizeIdempotent(t *testing.T) {
	r, _, _ := newStore(t)
	pending, so, se := beginFinalized(t, r, "some output", "errs")
	so2, se2, err := pending.Finalize()
	if err != nil {
		t.Fatalf("second Finalize: %v", err)
	}
	if so2 != so || se2 != se {
		t.Error("repeat Finalize must return the same captures without mutating payloads")
	}
}

func TestCommitRollsBackOnPointerFailure(t *testing.T) {
	r, layout, h := newStore(t)
	ki := keyInputFor(h, "ptrfail")
	key := ki.Compute(h)

	// Block the key pointer write by planting a regular file where its shard
	// directory must be created, so ReplaceBytesNoFollow fails.
	shardParent := filepath.Join(layout.RunsDir(), "keys", "blake3")
	if err := os.MkdirAll(shardParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shardParent, key.Hex()[:2]), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	pending, so, se := beginFinalized(t, r, "out", "")
	id := newRunID(t)
	start := time.Now()
	entry := runcache.RunEntry{
		ID:          id,
		Key:         key,
		KeyInput:    ki,
		StartedAt:   start,
		FinishedAt:  start.Add(time.Second),
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      so,
		Stderr:      se,
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    mustUnchanged(),
		EffectGuard: mustUnchangedEffect(),
	}
	err := pending.Commit(context.Background(), entry, unchangedObs())
	if err == nil {
		t.Fatal("Commit should fail when the key pointer cannot be written")
	}
	// The pointer-write cause is preserved through the rollback error join.
	if !errors.Is(err, fsx.ErrNotDirectory) {
		t.Errorf("Commit error = %v, want it to wrap the pointer-write failure", err)
	}
	// The published entry must have been rolled back, not left visible to List/Get
	// without a resolvable pointer.
	if _, err := r.Get(id); !errors.Is(err, runcache.ErrNotFound) {
		t.Errorf("Get after rolled-back commit err = %v, want ErrNotFound", err)
	}
	entries, err := listEntries(r)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("rolled-back commit left %d entries, want 0", len(entries))
	}
}

// assertNothingPublished checks no key pointer resolves and no entry was stored.
func assertNothingPublished(t *testing.T, r *runstore.Repo, keys ...runcache.RunKey) {
	t.Helper()
	for _, k := range keys {
		if _, ok, err := r.Lookup(k); err != nil || ok {
			t.Errorf("rejected commit left a hit: ok=%v err=%v", ok, err)
		}
	}
	entries, err := listEntries(r)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("rejected commit published %d entries, want 0", len(entries))
	}
}

func TestGetRejectsDriftedKeyInput(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "drift", "out", "", 1<<20)

	// Hand-edit the command in key_input while leaving the recorded key untouched:
	// the metadata now lies about what produced the result. Get must recompute the
	// key from the inputs and reject the mismatch rather than trust the record.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"drift"`, `"tampered-command"`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the command token to tamper")
	}
	// Published entries are read-only; make it writable to simulate tampering.
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on drifted key_input err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsSwappedPayloadName(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "swap", "out", "err", 1<<20)

	// Repoint stdout at the stderr payload file. With a matching hash this would let
	// OpenStdout replay stderr's bytes as stdout; the layout fixes each stream's file
	// name, so Get must reject the swap rather than trust the recorded name.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"stdout.log"`, `"stderr.log"`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the stdout payload name to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on swapped payload name err = %v, want ErrCorruptStore", err)
	}
}

func TestPublishedPayloadsAreReadOnly(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "ro", "out", "err", 1<<20)

	// A completed entry is immutable: its payloads, like its metadata, carry no
	// write bits so the stored bytes cannot drift from the recorded hashes.
	dir := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String())
	for _, name := range []string{"stdout.log", "stderr.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s mode = %v, want no write bits", name, info.Mode().Perm())
		}
	}
}

func TestLookupRejectsPointerToWrongEntry(t *testing.T) {
	r, layout, h := newStore(t)
	entryA := storeRun(t, r, h, "A", "output-A", "", 1<<20)
	entryB := storeRun(t, r, h, "B", "output-B", "", 1<<20)

	// Corrupt key A's pointer so it references run B (a valid but different entry).
	ptr := filepath.Join(layout.RunsDir(), "keys", "blake3", entryA.Key.Hex()[:2], entryA.Key.Hex()+".json")
	doc := []byte(`{"schema_version":1,"run_id":"` + entryB.ID.String() + `"}`)
	if err := os.WriteFile(ptr, doc, 0o644); err != nil {
		t.Fatalf("rewrite pointer: %v", err)
	}

	// Looking up key A must not return run B's result as a hit.
	if _, _, err := r.Lookup(entryA.Key); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Lookup with mispointed key err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsUnknownMetaField(t *testing.T) {
	for _, tc := range []struct {
		name string
		find string
		repl string
	}{
		{"top level", "{", `{"bogus_top":true,`},
		{"nested", `"cache_decision": {`, `"cache_decision": {"bogus_nested": 1,`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, layout, h := newStore(t)
			entry := storeRun(t, r, h, "unknownfield", "out", "", 1<<20)

			meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
			data, err := os.ReadFile(meta)
			if err != nil {
				t.Fatal(err)
			}
			tampered := strings.Replace(string(data), tc.find, tc.repl, 1)
			if tampered == string(data) {
				t.Fatalf("expected to find %q to inject an unknown field", tc.find)
			}
			if err := os.Chmod(meta, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
				t.Errorf("Get on meta with an unknown field err = %v, want ErrCorruptStore", err)
			}
		})
	}
}

func TestGetRejectsTrailingData(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trailer string
	}{
		{"extra brace", "}"},
		{"extra bracket", "]"},
		{"second object", `{"schema_version":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, layout, h := newStore(t)
			entry := storeRun(t, r, h, "trailing", "out", "", 1<<20)

			meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
			data, err := os.ReadFile(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(meta, 0o644); err != nil {
				t.Fatal(err)
			}
			// Append trailing bytes after the otherwise-valid document.
			if err := os.WriteFile(meta, append(data, []byte("\n"+tc.trailer)...), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
				t.Errorf("Get on meta with trailing %q err = %v, want ErrCorruptStore", tc.trailer, err)
			}
		})
	}
}

func TestGetRejectsInconsistentDuration(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "duration", "out", "", 1<<20)

	// duration_ms is derived from the started/finished span; a value that disagrees
	// is a hand-edited or corrupt record, not something to recompute over silently.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := regexp.MustCompile(`"duration_ms": \d+`).ReplaceAllString(string(data), `"duration_ms": 999999`)
	if tampered == string(data) {
		t.Fatal("expected to find duration_ms to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on inconsistent duration_ms err = %v, want ErrCorruptStore", err)
	}
}

func TestOpenStdoutRejectsWrongStoredBytes(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "storedbytes", "out", "", 1<<20)

	// Change both byte counters so the capture stays internally consistent (an
	// untruncated stream keeps stored==original) and the hash still matches the
	// untouched 3-byte payload — but the recorded size no longer matches the file.
	// openVerified must catch the size lie even though the hash is correct.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"original_bytes": 3`, `"original_bytes": 4`, 1)
	tampered = strings.Replace(tampered, `"stored_bytes": 3`, `"stored_bytes": 4`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the byte counters to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.OpenStdout(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("OpenStdout on mismatched stored_bytes err = %v, want ErrCorruptStore", err)
	}
}

func TestBeginRejectsNonPositiveLimits(t *testing.T) {
	r, _, _ := newStore(t)
	for _, limits := range []runcache.CaptureLimits{
		{MaxStdout: 0, MaxStderr: 1 << 20},
		{MaxStdout: 1 << 20, MaxStderr: -1},
	} {
		if _, err := r.Begin(limits); err == nil {
			t.Errorf("Begin(%+v) = nil, want an error for a non-positive limit", limits)
		}
	}
}

func TestListRejectsMisplacedEntry(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "misplaced", "out", "", 1<<20)
	id := entry.ID.String()

	// Plant an entry directory in the wrong shard. A valid id outside its own shard
	// is layout corruption that would otherwise read as a not-found (List sees it,
	// Get looks in the canonical shard) or, if duplicated, a false ambiguous prefix.
	wrongShard := "00"
	if id[:2] == wrongShard {
		wrongShard = "ff"
	}
	if err := os.MkdirAll(filepath.Join(layout.RunsDir(), "entries", wrongShard, id), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := listEntries(r); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("enumeration with a misplaced entry err = %v, want ErrCorruptStore", err)
	}
	if _, err := r.Resolve(context.Background(), id[:12]); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Resolve with a misplaced entry err = %v, want ErrCorruptStore", err)
	}
}

func TestOpenStdoutRejectsForgedTruncationMarker(t *testing.T) {
	r, layout, h := newStore(t)
	// A tiny limit forces a head-truncated stdout capture ending in a marker.
	entry := storeRun(t, r, h, "forgemarker", strings.Repeat("A", 100), "", 10)
	if !entry.Stdout.Truncated {
		t.Fatal("expected the stdout capture to be truncated")
	}
	head := entry.Stdout.OriginalBytes - entry.Stdout.OmittedBytes
	markerLen := int(entry.Stdout.StoredBytes - head)

	dir := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String())
	payloadPath := filepath.Join(dir, "stdout.log")
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the trailing marker with same-length non-marker bytes, then recompute
	// the hash so size and hash stay consistent — only the marker suffix is wrong.
	forged := append(append([]byte{}, payload[:head]...), []byte(strings.Repeat("Z", markerLen))...)
	newHash, err := h.HashReader(strings.NewReader(string(forged)))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(payloadPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadPath, forged, 0o444); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), entry.Stdout.Hash.String(), newHash.String(), 1)
	if tampered == string(data) {
		t.Fatal("expected to find the stdout hash to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.OpenStdout(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("OpenStdout on a forged truncation marker err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsNegativeExitCode(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "negexit", "out", "", 1<<20)

	// A negative exit code is impossible for a real process and would drive awa's own
	// exit status on a hit, so the read path must reject it.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"code": 0`, `"code": -1`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the exit code to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on negative exit code err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsExitCodeAboveUnixMax(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "bigexit", "out", "", 1<<20) // platform GOOS is linux

	// 999 is impossible for an 8-bit Unix exit status; returned to os.Exit it would
	// truncate, breaking miss/hit parity, so the read path must reject it.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"code": 0`, `"code": 999`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the exit code to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on out-of-range exit code err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsSignaledBelowFloor(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "sigfloor", "out", "", 1<<20)

	// Rewrite the normal exit into a signaled one with code 0. A signaled process is
	// always recorded as 128+signal, so code 0 is impossible; otherwise a hit would
	// replay as success while the metadata says the process was killed.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"kind": "normal"`, `"kind": "signaled"`, 1)
	tampered = strings.Replace(tampered, `"code": 0`, `"code": 0, "signal": "killed"`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the exit status to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on signaled exit below floor err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsTruncatedWithoutMarker(t *testing.T) {
	r, layout, h := newStore(t)
	// Force truncation with a tiny limit so the stdout capture is truncated=true.
	entry := storeRun(t, r, h, "nomarker", strings.Repeat("A", 100), "", 10)
	if !entry.Stdout.Truncated {
		t.Fatal("expected the stdout capture to be truncated")
	}

	// Shrink stored_bytes to exactly the retained head, as if no truncation marker
	// were stored. A truncated payload with no marker would replay as a silent,
	// unmarked cut, so validation must reject it.
	head := entry.Stdout.OriginalBytes - entry.Stdout.OmittedBytes
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data),
		fmt.Sprintf(`"stored_bytes": %d`, entry.Stdout.StoredBytes),
		fmt.Sprintf(`"stored_bytes": %d`, head), 1)
	if tampered == string(data) {
		t.Fatal("expected to find stored_bytes to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on truncated capture without marker err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsEmptyAwaVersion(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "noversion", "out", "", 1<<20)

	// Blank the awa version. It is part of the key, so a real run always records one;
	// an empty version is a miswired write the read path must reject.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"awa_version": "test"`, `"awa_version": ""`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find awa_version to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on empty awa_version err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsSkippedPolicyMismatch(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "skippol", "out", "", 1<<20)

	// Flip skipped.allowed without touching the key_input's allow_skipped_inputs. The
	// write path fills both from one flag, so they must agree; a document where they
	// disagree would replay as a hit while show/JSON disagreed about the policy.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"allowed": false`, `"allowed": true`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the skipped policy to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on skipped-policy mismatch err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsNonCacheableStoredEntry(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "noncacheable", "out", "", 1<<20)

	// Flip the stored decision to a self-consistent but impossible non-cacheable
	// state. A persisted entry is always cacheable; otherwise it would replay as a
	// hit while reporting cacheable=false, so Get must reject it.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"cacheable": true`, `"cacheable": false, "reason": "no-cache"`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find the cache decision to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on non-cacheable stored entry err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsUnsupportedCacheSchema(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "schema", "out", "", 1<<20)

	// Bump the key's cache schema version to a future value. Even if a tamperer
	// recomputed the key to match, decode validates the key_input before trusting
	// it, so an unsupported schema version is rejected as corruption.
	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data),
		fmt.Sprintf(`"cache_schema_version": %d`, runcache.CacheSchemaVersion),
		fmt.Sprintf(`"cache_schema_version": %d`, runcache.CacheSchemaVersion+8), 1)
	if tampered == string(data) {
		t.Fatal("expected to find cache_schema_version to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on unsupported cache schema err = %v, want ErrCorruptStore", err)
	}
}

func TestGetRejectsMalformedScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		find string
		repl string
	}{
		{"absolute include", `"include_scope": [`, `"include_scope": ["/etc",`},
		{"empty include element", `"include_scope": [`, `"include_scope": ["",`},
		{"empty exclude element", `"exclude_scope": null`, `"exclude_scope": [""]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, layout, h := newStore(t)
			entry := storeRun(t, r, h, "badscope", "out", "", 1<<20)

			meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
			data, err := os.ReadFile(meta)
			if err != nil {
				t.Fatal(err)
			}
			tampered := strings.Replace(string(data), tc.find, tc.repl, 1)
			if tampered == string(data) {
				t.Fatalf("expected to find %q to tamper", tc.find)
			}
			if err := os.Chmod(meta, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
				t.Fatal(err)
			}

			if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
				t.Errorf("Get on malformed scope err = %v, want ErrCorruptStore", err)
			}
		})
	}
}

func TestGetRejectsMalformedEnvCheckpoint(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "badenv", "out", "", 1<<20)

	meta := filepath.Join(layout.RunsDir(), "entries", entry.ID.String()[:2], entry.ID.String(), "meta.json")
	data, err := os.ReadFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"env": null`, `"env": [{"name":"CI","presence":"bogus"}]`, 1)
	if tampered == string(data) {
		t.Fatal("expected to find env checkpoint to tamper")
	}
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Get(entry.ID); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Get on malformed env checkpoint err = %v, want ErrCorruptStore", err)
	}
}

func TestLookupRejectsUnknownPointerField(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "ptrfield", "out", "", 1<<20)

	// A key pointer with an extra field is not the exact schema shape; Lookup must
	// surface it as corruption rather than silently ignore the field.
	ptr := filepath.Join(layout.RunsDir(), "keys", "blake3", entry.Key.Hex()[:2], entry.Key.Hex()+".json")
	doc := []byte(`{"schema_version":1,"run_id":"` + entry.ID.String() + `","bogus":1}`)
	if err := os.WriteFile(ptr, doc, 0o644); err != nil {
		t.Fatalf("rewrite pointer: %v", err)
	}

	if _, _, err := r.Lookup(entry.Key); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("Lookup with an unknown pointer field err = %v, want ErrCorruptStore", err)
	}
}

func TestRefreshRepointsKey(t *testing.T) {
	r, _, h := newStore(t)
	first := storeRun(t, r, h, "refresh", "v1", "", 1<<20)
	second := storeRun(t, r, h, "refresh", "v2", "", 1<<20)
	if first.Key != second.Key {
		t.Fatal("same discriminator must yield the same key")
	}

	ce, ok, err := r.Lookup(first.Key)
	if err != nil || !ok {
		t.Fatalf("Lookup: %v ok=%v", err, ok)
	}
	if got := ce.Entry(); got.ID != second.ID {
		t.Errorf("key resolves to %s, want the refreshed run %s", got.ID, second.ID)
	}
	// The first run still exists as an entry until explicitly removed.
	if _, err := r.Get(first.ID); err != nil {
		t.Errorf("first run should still be retrievable: %v", err)
	}
}

func TestInspectKeyPointersHealthy(t *testing.T) {
	r, _, h := newStore(t)
	storeRun(t, r, h, "ok1", "x", "", 1<<20)
	storeRun(t, r, h, "ok2", "y", "", 1<<20)

	problems, err := r.InspectKeyPointers()
	if err != nil {
		t.Fatalf("InspectKeyPointers: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("want no problems, got %+v", problems)
	}
}

func TestInspectKeyPointersUndecodable(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "bad", "x", "", 1<<20)

	ptr := filepath.Join(layout.RunsDir(), "keys", "blake3", entry.Key.Hex()[:2], entry.Key.Hex()+".json")
	if err := os.WriteFile(ptr, []byte("not json"), 0o644); err != nil {
		t.Fatalf("corrupt pointer: %v", err)
	}

	problems, err := r.InspectKeyPointers()
	if err != nil {
		t.Fatalf("InspectKeyPointers: %v", err)
	}
	if len(problems) != 1 || problems[0].Path != ptr {
		t.Fatalf("want one problem at %s, got %+v", ptr, problems)
	}
}

func TestInspectKeyPointersDangling(t *testing.T) {
	r, layout, h := newStore(t)
	entry := storeRun(t, r, h, "dangle", "x", "", 1<<20)

	// Remove the entry directory but leave its key pointer: the pointer now dangles.
	shard := entry.ID.String()[:2]
	if err := os.RemoveAll(filepath.Join(layout.RunsDir(), "entries", shard, entry.ID.String())); err != nil {
		t.Fatalf("remove entry: %v", err)
	}

	problems, err := r.InspectKeyPointers()
	if err != nil {
		t.Fatalf("InspectKeyPointers: %v", err)
	}
	if len(problems) != 1 {
		t.Fatalf("want one dangling-pointer problem, got %+v", problems)
	}
}

func TestInspectKeyPointersBadShapes(t *testing.T) {
	t.Run("algo level is a file", func(t *testing.T) {
		r, layout, _ := newStore(t)
		keys := filepath.Join(layout.RunsDir(), "keys")
		if err := os.MkdirAll(keys, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(keys, "blake3"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		problems, err := r.InspectKeyPointers()
		if err != nil {
			t.Fatalf("InspectKeyPointers: %v", err)
		}
		if len(problems) != 1 {
			t.Fatalf("want one structural problem, got %+v", problems)
		}
	})

	t.Run("shard level is a file", func(t *testing.T) {
		r, layout, _ := newStore(t)
		algo := filepath.Join(layout.RunsDir(), "keys", "blake3")
		if err := os.MkdirAll(algo, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(algo, "ab"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		problems, err := r.InspectKeyPointers()
		if err != nil {
			t.Fatalf("InspectKeyPointers: %v", err)
		}
		if len(problems) != 1 {
			t.Fatalf("want one structural problem, got %+v", problems)
		}
	})

	t.Run("pointer leaf is a directory", func(t *testing.T) {
		r, layout, _ := newStore(t)
		shard := filepath.Join(layout.RunsDir(), "keys", "blake3", "ab")
		if err := os.MkdirAll(filepath.Join(shard, "abdeadbeef.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		problems, err := r.InspectKeyPointers()
		if err != nil {
			t.Fatalf("InspectKeyPointers: %v", err)
		}
		if len(problems) != 1 {
			t.Fatalf("want one structural problem, got %+v", problems)
		}
	})
}
