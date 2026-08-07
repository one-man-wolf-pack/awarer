package checkpointjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/manifestjsonl"
)

// drainManifest pulls every record from a checkpoint's manifest stream into ordered
// path strings, returning the cursor's terminal error.
func drainManifest(t *testing.T, repo *Repo, id checkpoint.CheckpointID) ([]string, error) {
	t.Helper()
	stream, err := repo.OpenManifest(id)
	if err != nil {
		return nil, err
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()
	var paths []string
	for cur.Next() {
		paths = append(paths, cur.Record().Path().String())
	}
	return paths, cur.Err()
}

// TestCheckpointPutHeaderAndManifest proves a Put writes a separated header and
// manifest that Header reads without the manifest and OpenManifest streams in
// canonical order.
func TestCheckpointPutHeaderAndManifest(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x11), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)

	// The on-disk layout is the separated header-and-manifest form.
	if _, err := os.Stat(filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)); err != nil {
		t.Fatalf("header.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)); err != nil {
		t.Fatalf("manifest.jsonl missing: %v", err)
	}

	h, err := repo.Header(s.id())
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if want := s.derivedHeader(t); h.TreeHash != want.TreeHash || h.RecordCount != want.RecordCount {
		t.Fatalf("stored header %+v does not match the records it was written from %+v", h, want)
	}

	// Header must not depend on the manifest: removing it leaves Header working.
	manifestPath := filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)
	if err := os.Chmod(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Header(s.id()); err != nil {
		t.Fatalf("Header after manifest removal: %v", err)
	}
}

// TestCheckpointManifestStreamOrder proves the manifest streams entries and skips
// merged in canonical path order, and that a full read of the stored checkpoint —
// header plus a complete manifest drain — succeeds over the same records.
func TestCheckpointManifestStreamOrder(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x12), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	got, err := drainManifest(t, repo, s.id())
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"a.txt", "big.bin", "dev/null", "loop", "secret", "sub", "sub/link"}
	if len(got) != len(want) {
		t.Fatalf("record count %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d = %q, want %q (%v)", i, got[i], want[i], got)
		}
	}

	if err := readCheckpoint(repo, s.id()); err != nil {
		t.Fatalf("full read of the stored checkpoint: %v", err)
	}
}

// TestCheckpointCorruptHeader proves a malformed header.json is corrupt store, not an
// ordinary failure.
func TestCheckpointCorruptHeader(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x13), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	hp := filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)
	if err := os.Chmod(filepath.Dir(hp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, []byte("{not json"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Header(s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Header on corrupt header err = %v, want ErrCorruptStore", err)
	}
}

// TestCorruptHeaderDiagnosticNamesTheCause pins that the corrupt branch forwards why
// the document did not parse. A truncated header is the likely outcome of a partial
// write, and it reaches the same branch as a well-formed document of the wrong shape;
// a diagnostic that answered "not a schema-carrying document" for both would send the
// reader looking for the wrong problem in the one place they go for answers.
func TestCorruptHeaderDiagnosticNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bytes string
		wants string
	}{
		{"truncated", `{"schema_version": 1, "id": "`, "unexpected end of JSON input"},
		{"not an object", `["schema_version"]`, "cannot unmarshal"},
		{"no schema_version", `{"id": "x"}`, "no schema_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeHeader([]byte(tc.bytes))
			if err == nil {
				t.Fatalf("decodeHeader accepted %q", tc.bytes)
			}
			if errors.Is(err, checkpoint.ErrIncompatibleFormat) {
				t.Fatalf("unparseable data classified incompatible, want corruption: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not name the cause %q", err, tc.wants)
			}
		})
	}
}

// TestCheckpointCorruptManifestRecord proves a malformed manifest line fails the read with
// corrupt store and a line location.
func TestCheckpointCorruptManifestRecord(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x14), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	mp := filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)
	if err := os.Chmod(filepath.Dir(mp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, []byte("{\"entry\":{\"path\":\"x\"}}\n{garbage}\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := readCheckpoint(repo, s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Get on corrupt manifest err = %v, want ErrCorruptStore", err)
	}
}

// TestCheckpointTruncatedManifest proves a manifest with fewer records than the header
// declares is rejected, so a truncated manifest is not silently shortened.
func TestCheckpointTruncatedManifest(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x15), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	mp := filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)
	if err := os.Chmod(filepath.Dir(mp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mp, 0o644); err != nil {
		t.Fatal(err)
	}
	// Keep only the first record line.
	data, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	first := bytes.SplitN(data, []byte("\n"), 2)[0]
	if err := os.WriteFile(mp, append(first, '\n'), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := drainManifest(t, repo, s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("drain truncated manifest err = %v, want ErrCorruptStore", err)
	}
}

// TestCheckpointManifestLineTrailingData proves a manifest line that is one valid object
// followed by trailing structural bytes (here "]") is rejected as corrupt, so the
// hostile-input boundary is a strict one-object-per-line check, not a lenient one.
func TestCheckpointManifestLineTrailingData(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x1a), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	mp := filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)
	if err := os.Chmod(filepath.Dir(mp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mp, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitN(data, []byte("\n"), 2)
	// Append a stray "]" to the first (valid) record line; keep the rest intact.
	corrupted := append(append(lines[0], ']'), '\n')
	if len(lines) > 1 {
		corrupted = append(corrupted, lines[1]...)
	}
	if err := os.WriteFile(mp, corrupted, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := drainManifest(t, repo, s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("trailing-data manifest line err = %v, want ErrCorruptStore", err)
	}
}

// TestCheckpointPutManifestDerivesHeader proves the streaming write contract derives
// the tree hash, stats, and record count from the manifest stream itself, persists a
// readable checkpoint, and rejects an id collision. The caller supplies only the
// non-derived build metadata and the records, so a header can never carry a derived
// fact the manifest does not support.
func TestCheckpointPutManifestDerivesHeader(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x1e), time.Unix(1, 0).UTC(), false)

	header, err := repo.PutManifest(context.Background(), s.build, s.stream())
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	// The oracle is an independent reduction of the same records, not a fact the
	// caller handed in: nothing but the manifest can make the header agree.
	want := s.derivedHeader(t)
	if header.TreeHash != want.TreeHash {
		t.Fatalf("derived tree hash %s != %s", header.TreeHash, want.TreeHash)
	}
	if header.Stats != want.Stats {
		t.Fatalf("derived stats %+v != %+v", header.Stats, want.Stats)
	}
	if header.RecordCount != s.records() {
		t.Fatalf("derived record count %d != %d", header.RecordCount, s.records())
	}

	if err := readCheckpoint(repo, s.id()); err != nil {
		t.Fatalf("full read of the stored checkpoint: %v", err)
	}

	if _, err := repo.PutManifest(context.Background(), s.build, s.stream()); !errors.Is(err, checkpoint.ErrIDCollision) {
		t.Fatalf("second PutManifest err = %v, want ErrIDCollision", err)
	}
}

// TestCheckpointHeaderRejectsImpossibleStats proves a header whose stats are mechanically
// impossible (here blobs + hash-only no longer account for the files) is rejected
// on read, so a header is a hostile-input boundary even without the manifest.
func TestCheckpointHeaderRejectsImpossibleStats(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x1b), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	h := s.derivedHeader(t)
	h.Stats.Blobs++ // blobs + hash-only != files
	bad, err := encodeHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)
	if err := os.Chmod(filepath.Dir(hp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, bad, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Header(s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Header with impossible stats err = %v, want ErrCorruptStore", err)
	}
}

// TestCheckpointHeaderRejectsUnknownField proves the header read is strict: an extra
// field (a hand-edit or a typo) in a document that still claims the current schema is
// corruption — not silently ignored, and not softened into the milder incompatible
// verdict, which belongs only to a document declaring a schema this build has no
// reader for. The field is arbitrary on purpose: the rule is that this build's header
// has exactly the keys it writes, so any key beside them condemns the document
// without anything having to recognize the key itself.
func TestCheckpointHeaderRejectsUnknownField(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x1c), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	hp := filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)
	data, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["bogus_field"] = 1
	bad, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(hp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, bad, 0o444); err != nil {
		t.Fatal(err)
	}
	_, err = repo.Header(s.id())
	if !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Header with unknown field err = %v, want ErrCorruptStore", err)
	}
	if errors.Is(err, checkpoint.ErrIncompatibleFormat) {
		t.Errorf("unknown field classified incompatible, want corruption: %v", err)
	}
}

// TestCheckpointHeaderRejectsForeignDigestPrefix proves the persisted boundary
// enforces the fixed local identity: a header whose tree hash names any primitive
// other than BLAKE3 — pointedly the sha256: that external release and checksum
// contracts speak — is a foreign digest namespace and fails through the owning typed
// validation rather than being reinterpreted as BLAKE3.
func TestCheckpointHeaderRejectsForeignDigestPrefix(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x1d), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	hp := filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)
	data, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	tree, ok := m["tree_hash"].(string)
	if !ok || !strings.HasPrefix(tree, "blake3:") {
		t.Fatalf("tree_hash = %v, want a blake3: digest to rewrite", m["tree_hash"])
	}
	m["tree_hash"] = "sha256:" + strings.TrimPrefix(tree, "blake3:")
	bad, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(hp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, bad, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Header(s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Header with a sha256: tree hash err = %v, want ErrCorruptStore", err)
	}
}

// TestCheckpointRejectsSymlinkAtDirAddress proves a symlink occupying a checkpoint's
// directory address surfaces as corruption in discovery paths (List,
// ResolvePrefix) instead of silently disappearing from listings.
func TestCheckpointRejectsSymlinkAtDirAddress(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	if err := os.MkdirAll(layout.CheckpointsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	id := idFrom(t, 0x20)
	link := filepath.Join(layout.CheckpointsDir(), id.String())
	mustSymlink(t, t.TempDir(), link)
	if _, err := repo.ListHeaders(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("List with symlink at dir address err = %v, want ErrCorruptStore", err)
	}
	if _, err := repo.ResolvePrefix(context.Background(), id.String()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("ResolvePrefix with symlink at dir address err = %v, want ErrCorruptStore", err)
	}
}

// TestStoredManifestReproducesItsHeader is the decode-fidelity oracle for the full
// schema. buildCheckpoint is the only fixture in the repository that carries every
// record shape — a blob entry, a hash-only entry with omitted stat fields, a
// directory, a symlink reached by following another symlink, and skips for a special
// file, a read error, and a symlink cycle — and the write derives the header from
// those records while they are still in memory. Draining paths therefore proves
// nothing about the fields the encoder wrote: a decoder that dropped a symlink
// target, a traversal source, or a storage intent would keep every path and every
// other test in this package green.
//
// Folding the DECODED records back through the same reducer the write used closes
// that loop. The tree hash covers content, mode, symlink target, and traversal
// provenance; the stats cover the per-kind counts, the blob/hash-only split, and the
// total size; the count covers the records themselves. This is a test oracle, not a
// read-path check — checkpointjson deliberately does not verify derived facts on
// read; internal/app/state owns that.
func TestStoredManifestReproducesItsHeader(t *testing.T) {
	repo := NewRepo(paths.New(t.TempDir()))
	s := buildCheckpoint(t, idFrom(t, 0x1e), time.Unix(1, 0).UTC(), true)
	stored := s.put(t, repo)

	stream, err := repo.OpenManifest(s.id())
	if err != nil {
		t.Fatalf("OpenManifest: %v", err)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest cursor: %v", err)
	}
	defer func() { _ = cur.Close() }()
	red, err := worktree.ReduceCursor(blake3hash.New(), cur)
	if err != nil {
		t.Fatalf("reducing the decoded manifest: %v", err)
	}

	if red.Hash != stored.TreeHash {
		t.Errorf("decoded manifest hashes to %s, header records %s", red.Hash, stored.TreeHash)
	}
	if got := checkpoint.StatsFromReduced(red.Stats); got != stored.Stats {
		t.Errorf("decoded manifest stats %+v, header records %+v", got, stored.Stats)
	}
	if red.Count != stored.RecordCount {
		t.Errorf("decoded manifest holds %d records, header records %d", red.Count, stored.RecordCount)
	}
}

// TestCheckpointMissingManifestIsCorrupt proves that once a header is committed, a missing
// manifest is corrupt durable state — surfaced by both Get and OpenManifest as
// ErrCorruptStore, not a bare not-found.
func TestCheckpointMissingManifestIsCorrupt(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x1d), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	mp := filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)
	if err := os.Chmod(filepath.Dir(mp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mp); err != nil {
		t.Fatal(err)
	}

	if err := readCheckpoint(repo, s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Get with missing manifest err = %v, want ErrCorruptStore", err)
	}
	stream, err := repo.OpenManifest(s.id())
	if err != nil {
		t.Fatalf("OpenManifest: %v", err)
	}
	if _, err := stream.Open(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("manifest Open with missing manifest err = %v, want ErrCorruptStore", err)
	}
}

// TestCheckpointDeleteRemovesDir proves Delete removes the whole checkpoint directory and is
// idempotent.
func TestCheckpointDeleteRemovesDir(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x18), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	if err := repo.Delete(s.id()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(layout.CheckpointsDir(), s.id().String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint dir still present after Delete: %v", err)
	}
	if err := repo.Delete(s.id()); err != nil {
		t.Fatalf("second Delete not idempotent: %v", err)
	}
}

// compile-time assertion that the repo satisfies the extended interface.
var _ checkpoint.Repository = (*Repo)(nil)

// compile-time assertion that the shared manifest stream satisfies the port.
var _ worktree.ManifestStream = manifestjsonl.Stream{}

// TestCheckpointHeaderIDMustMatchItsAddress proves a record must agree with the
// address it was read from: a header whose declared id is not the directory it sits
// in is corruption, not a checkpoint under either id.
//
// The address is the store's only index, so a header that disagrees with it would
// otherwise let one record answer for another — the read would return a checkpoint
// the caller did not ask for, with a tree hash and manifest belonging to something
// else. Nothing upstream can catch that: the id it asked for came back "found".
func TestCheckpointHeaderIDMustMatchItsAddress(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)

	// Publish a real checkpoint, then rewrite its header to claim a different id.
	// The bytes stay a valid, current-schema header; only the id disagrees with the
	// directory holding it.
	s := buildCheckpoint(t, idFrom(t, 0x5a), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	other := idFrom(t, 0x5b)

	headerPath := filepath.Join(layout.CheckpointsDir(), s.id().String(), headerName)
	data, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	swapped := bytes.Replace(data, []byte(s.id().String()), []byte(other.String()), 1)
	if bytes.Equal(swapped, data) {
		t.Fatalf("fixture header does not carry its own id, so the swap proved nothing")
	}
	// The store writes headers read-only, so replace rather than overwrite in place.
	if err := os.Remove(headerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, swapped, 0o444); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Header(s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Header at a disagreeing address err = %v, want ErrCorruptStore", err)
	}
	// And it does not become readable under the id it claims: that address holds
	// nothing, so the claimed id is absent rather than corrupt. Asserted positively —
	// a read that succeeded here would be the very substitution this test exists to
	// catch, and merely "not corrupt" would let it pass.
	if _, err := repo.Header(other); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Header at the claimed id err = %v, want ErrNotFound", err)
	}
}
