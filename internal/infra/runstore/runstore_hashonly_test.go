package runstore_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// A run observation records what the worktree looked like before and after a
// command. It publishes no content: no blob is written, no bytes are copied
// anywhere. The scanner's storage field is an intent — "this content would be
// stored" — and honouring that intent is the checkpoint's job, not a run's.
//
// These tests pin the consequence. Persisting the intent verbatim made a record
// advertise a capability the store never had, and a consumer could only find out by
// asking for the bytes and being told the blob was missing — which says content was
// reclaimed, not that the promise was never real.

// observationWithFile builds observations whose before and after manifests hold one
// regular entry (scanned as blob-backed, which is the default intent) and one
// symlink, so normalization has something to change and something it must leave
// alone.
func observationWithFile(t *testing.T, h hashing.Hasher) (runcache.RunObservations, []worktree.Entry) {
	t.Helper()
	file, err := worktree.NewRegularEntry(mustRel(t, "a.txt"), contentHash(t, h, "one"),
		worktree.StorageBlob, worktree.StatSignature{Size: 3, Mode: 0o644}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("regular entry: %v", err)
	}
	target, err := worktree.NewSymlinkTarget("a.txt")
	if err != nil {
		t.Fatalf("symlink target: %v", err)
	}
	link, err := worktree.NewSymlinkEntry(mustRel(t, "link"), target,
		worktree.StatSignature{Size: 5, Mode: 0o777}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("symlink entry: %v", err)
	}
	entries := []worktree.Entry{file, link}
	cfg := testScanCfg()
	return runcache.RunObservations{
		Before:               scantest.CanonicalStream(entries, nil),
		After:                scantest.CanonicalStream(entries, nil),
		BeforeScanConfigHash: cfg,
		AfterScanConfigHash:  cfg,
	}, entries
}

func mustRel(t *testing.T, p string) worktree.RelPath {
	t.Helper()
	rp, err := worktree.ParseRelPath(p)
	if err != nil {
		t.Fatalf("rel path %q: %v", p, err)
	}
	return rp
}

func contentHash(t *testing.T, h hashing.Hasher, s string) hashing.ContentHash {
	t.Helper()
	c, err := h.HashReader(strings.NewReader(s))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return c
}

// drainObservation reads one stored observation's records back through the repo's
// own verified stream, which is the path every consumer uses.
func drainObservation(t *testing.T, r observationOpener, id runcache.RunID, after bool) []worktree.Entry {
	t.Helper()
	read, err := r.Observation(id, after)
	if err != nil {
		t.Fatalf("Observation(after=%v): %v", after, err)
	}
	cur, err := read.Manifest().Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var out []worktree.Entry
	for cur.Next() {
		if e, ok := cur.Record().Entry(); ok {
			out = append(out, e)
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain manifest: %v", err)
	}
	return out
}

type observationOpener interface {
	Observation(id runcache.RunID, after bool) (runcache.RunObservationRead, error)
}

// TestObservationRegularEntriesPersistAsHashOnly is the central round-trip: what
// goes in as the scanner's blob intent comes back as hash-only, on both the before
// and the after observation, while the symlink keeps the inline target that really
// is inside the record.
func TestObservationRegularEntriesPersistAsHashOnly(t *testing.T) {
	r, _, h := newStore(t)
	obs, entries := observationWithFile(t, h)
	entry := storeRunWithObs(t, r, h, "hash-only", obs, manifestTreeHash(h, entries, nil))

	for _, after := range []bool{false, true} {
		got := drainObservation(t, r, entry.ID, after)
		if len(got) != 2 {
			t.Fatalf("after=%v: %d entries, want 2", after, len(got))
		}
		file, link := got[0], got[1]
		if file.Path.String() != "a.txt" || link.Path.String() != "link" {
			t.Fatalf("after=%v: unexpected paths %q %q", after, file.Path, link.Path)
		}
		if file.Storage != worktree.StorageHashOnly {
			t.Errorf("after=%v: regular entry storage = %v, want hash-only", after, file.Storage)
		}
		if link.Storage != worktree.StorageInlineSymlinkTarget {
			t.Errorf("after=%v: symlink storage = %v, want inline-symlink-target", after, link.Storage)
		}
		if link.Symlink.String() != "a.txt" {
			t.Errorf("after=%v: symlink target = %q, want a.txt", after, link.Symlink)
		}
	}
}

// TestNormalizationDoesNotMoveTheTreeHash is what makes the normalization a
// persistence detail rather than a change of what the observation means: a regular
// entry's identity is its path, content hash, and permission bits, and the storage
// class is deliberately not folded in. If that ever changed, every recorded run's
// identity would shift silently and the run cache would stop matching.
func TestNormalizationDoesNotMoveTheTreeHash(t *testing.T) {
	r, _, h := newStore(t)
	obs, entries := observationWithFile(t, h)

	// The hash of the manifest as SCANNED (blob intent), computed independently of
	// the store.
	asScanned := manifestTreeHash(h, entries, nil)

	entry := storeRunWithObs(t, r, h, "stable-hash", obs, asScanned)
	stored, err := r.Get(entry.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Before.Manifest.TreeHash != asScanned {
		t.Errorf("stored before tree hash = %v, want the as-scanned %v: normalization must not move identity",
			stored.Before.Manifest.TreeHash, asScanned)
	}
	if stored.After.Manifest.TreeHash != asScanned {
		t.Errorf("stored after tree hash = %v, want the as-scanned %v", stored.After.Manifest.TreeHash, asScanned)
	}
	// And the recorded hash still verifies against the persisted bytes, which is the
	// check the reader makes on every full drain.
	if got := drainObservation(t, r, entry.ID, false); len(got) != 2 {
		t.Fatalf("drained %d entries, want 2", len(got))
	}
}

// TestForgedBlobClaimInAnObservationIsRefused covers the read boundary. runstore
// writes nothing but hash-only, so a regular entry advertising blob storage is a
// hand-edited or forged manifest. It must fail loud as corruption rather than
// travelling on to a consumer, where asking for the bytes would produce an ordinary
// "the blob is missing" answer and read as reclaimed content.
func TestForgedBlobClaimInAnObservationIsRefused(t *testing.T) {
	r, layout, h := newStore(t)
	obs, entries := observationWithFile(t, h)
	entry := storeRunWithObs(t, r, h, "forged-blob", obs, manifestTreeHash(h, entries, nil))

	id := entry.ID.String()
	manifest := filepath.Join(layout.RunsDir(), "entries", id[:2], id, "before.manifest.jsonl")
	data, err := os.ReadFile(manifest) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	forged := strings.Replace(string(data), `"content_storage":"hash-only"`, `"content_storage":"blob"`, 1)
	if forged == string(data) {
		// The persisted token spelling is the store's, not this test's, so locate it
		// rather than assuming it.
		t.Fatalf("expected a hash-only storage token to forge in:\n%s", firstLine(string(data)))
	}
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(forged), 0o444); err != nil {
		t.Fatal(err)
	}

	read, err := r.Observation(entry.ID, false)
	if err != nil {
		t.Fatalf("Observation: %v", err)
	}
	cur, err := read.Manifest().Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() { //nolint:revive // draining is the point: the refusal surfaces in Err
	}
	if !errors.Is(cur.Err(), runcache.ErrCorruptStore) {
		t.Fatalf("draining a forged blob claim = %v, want ErrCorruptStore", cur.Err())
	}
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}
