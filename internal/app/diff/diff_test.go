package diff_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appdiff "awarer/internal/app/diff"
	"awarer/internal/app/state"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/compare"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/scantest"
)

// fixtureRoot is an absolute, cleaned path on whichever host runs the test. A checkpoint
// records the project root it was captured from and Checkpoint.Validate requires exactly
// that shape, so a fixture spelling a POSIX literal like "/abs/project" is not cosmetic:
// filepath.IsAbs rejects a volume-less path on Windows, which would fail every checkpoint
// built here and confine this package's coverage to unix.
func fixtureRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("abs", "project"))
	if err != nil {
		t.Fatalf("resolving a fixture root: %v", err)
	}
	return root
}

type fixture struct {
	repo   *checkpointjson.Repo
	store  blob.Store
	hasher hashing.Hasher
	res    *state.Resolver
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	layout := paths.New(t.TempDir())
	hasher := blake3hash.New()
	repo := checkpointjson.NewRepo(layout)
	store := blobstore.New(layout, hasher)
	res := state.NewResolver(state.Deps{Checkpoints: repo, Blobs: store, Hasher: hasher})
	return fixture{repo: repo, store: store, hasher: hasher, res: res}
}

// hashOf returns the content hash of data under the fixture hasher.
func (f fixture) hashOf(t *testing.T, data []byte) hashing.ContentHash {
	t.Helper()
	h, err := f.hasher.HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	return h
}

// writeBlob stores data and returns its content hash.
func (f fixture) writeBlob(t *testing.T, data []byte) hashing.ContentHash {
	t.Helper()
	h := f.hashOf(t, data)
	_, _, err := f.store.Materialize(h, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return h
}

func rel(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	return p
}

// putCheckpoint stores a checkpoint of a single regular entry at path with the given
// content hash and storage intent.
func (f fixture) putCheckpoint(t *testing.T, idByte byte, created time.Time, path string, content hashing.ContentHash, storage worktree.ContentStorageIntent, mode uint32) {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{idByte}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	entry, err := worktree.NewRegularEntry(rel(t, path), content, storage,
		worktree.StatSignature{Size: 10, MtimeNs: 5, Mode: mode,
			Omitted: worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)},
		worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewRegularEntry: %v", err)
	}
	th, _ := hashing.ParseTreeHash("blake3:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	build := checkpoint.CheckpointBuild{
		ID:                   id,
		CreatedAt:            created,
		Root:                 fixtureRoot(t),
		CommandCwd:           ".",
		AwaVersion:           "0.0.0-dev",
		ScanConfigHash:       hashing.ConfigHashFromTree(th),
		CheckpointPolicyHash: hashing.ConfigHashFromTree(th),
		TrustMode:            config.TrustNormal,
		OmittedStatFields:    worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink),
	}
	// The store derives the tree hash and stats from the records it streams.
	if _, err := f.repo.PutManifest(context.Background(), build, scantest.CanonicalStream([]worktree.Entry{entry}, nil)); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
}

func mustRange(t *testing.T, tok string) state.Range {
	t.Helper()
	r, err := state.ParseRange(tok)
	if err != nil {
		t.Fatalf("ParseRange(%q): %v", tok, err)
	}
	return r
}

// drainDiffStream drains a diff StreamResult into the file-diff slice these tests
// assert on. It is the test-side materializer for the streaming diff service (the
// production path renders one file at a time and never holds the whole set); the
// returned error is the cursor's sticky error, so a missing/corrupt blob surfaces here
// exactly as the streamed CLI path would see it mid-stream.
func drainDiffStream(sr appdiff.StreamResult) ([]appdiff.FileDiff, error) {
	var files []appdiff.FileDiff
	for sr.Files.Next() {
		files = append(files, sr.Files.FileDiff())
	}
	return files, sr.Files.Err()
}

func TestDiffCheckpointToCheckpointText(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	oldHash := f.writeBlob(t, []byte("line1\nline2\nline3\n"))
	newHash := f.writeBlob(t, []byte("line1\nCHANGED\nline3\n"))
	f.putCheckpoint(t, 0x11, base, "calc.go", oldHash, worktree.StorageBlob, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "calc.go", newHash, worktree.StorageBlob, 0o644)

	svc := appdiff.New(f.res)
	sr, err := svc.Stream(context.Background(), appdiff.Request{Range: mustRange(t, "@-2..@-1"), Context: 3})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = sr.Close() }()
	files, err := drainDiffStream(sr)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file diff, got %d", len(files))
	}
	fd := files[0]
	if fd.Availability != appdiff.Text {
		t.Fatalf("availability = %v, want Text; reason=%q", fd.Availability, fd.Reason)
	}
	if !strings.Contains(fd.Text, "-line2") || !strings.Contains(fd.Text, "+CHANGED") {
		t.Errorf("diff text missing change:\n%s", fd.Text)
	}
}

func TestDiffHistogramAlgorithmRoutes(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	oldHash := f.writeBlob(t, []byte("line1\nline2\nline3\n"))
	newHash := f.writeBlob(t, []byte("line1\nCHANGED\nline3\n"))
	f.putCheckpoint(t, 0x11, base, "calc.go", oldHash, worktree.StorageBlob, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "calc.go", newHash, worktree.StorageBlob, 0o644)

	svc := appdiff.New(f.res)
	sr, err := svc.Stream(context.Background(), appdiff.Request{
		Range:     mustRange(t, "@-2..@-1"),
		Context:   3,
		Algorithm: config.DiffHistogram,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = sr.Close() }()
	files, err := drainDiffStream(sr)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file diff, got %d", len(files))
	}
	fd := files[0]
	if fd.Availability != appdiff.Text {
		t.Fatalf("availability = %v, want Text; reason=%q", fd.Availability, fd.Reason)
	}
	if !strings.Contains(fd.Text, "-line2") || !strings.Contains(fd.Text, "+CHANGED") {
		t.Errorf("histogram diff text missing change:\n%s", fd.Text)
	}
}

func TestDiffUnknownAlgorithmFailsLoud(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	// Identical content on both sides: there is no text diff to render, so the
	// algorithm is validated at the Stream boundary rather than at render time. An
	// out-of-range algorithm must still fail loud — the engine is part of the
	// diff's evidence, so a wiring bug must not pass silently.
	h := f.writeBlob(t, []byte("line1\nline2\n"))
	f.putCheckpoint(t, 0x11, base, "calc.go", h, worktree.StorageBlob, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "calc.go", h, worktree.StorageBlob, 0o644)

	svc := appdiff.New(f.res)
	_, err := svc.Stream(context.Background(), appdiff.Request{
		Range:     mustRange(t, "@-2..@-1"),
		Context:   3,
		Algorithm: config.DiffAlgorithm(99),
	})
	if err == nil {
		t.Fatal("Stream with an unknown algorithm should fail, got nil error")
	}
}

func TestDiffMissingBlobIsCorruption(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	// Two distinct content hashes, but the blobs are never written to the store.
	oldHash := f.hashOf(t, []byte("old\n"))
	newHash := f.hashOf(t, []byte("new\n"))
	f.putCheckpoint(t, 0x11, base, "calc.go", oldHash, worktree.StorageBlob, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "calc.go", newHash, worktree.StorageBlob, 0o644)

	svc := appdiff.New(f.res)
	sr, err := svc.Stream(context.Background(), appdiff.Request{Range: mustRange(t, "@-2..@-1"), Context: 3})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = sr.Close() }()
	// The missing blob surfaces while the cursor renders the file's content diff, so it
	// arrives through the drain error, not the Stream setup error.
	if _, err := drainDiffStream(sr); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("want ErrCorruptStore, got %v", err)
	}
}

// TestStreamChangesSkipsContent proves the --stat path is content-free: over the same
// missing-blob store that makes the content diff path fail with corruption, StreamChanges
// succeeds and counts the change, because it reads no blob and runs no text diff engine.
// A regression that routed --stat back through the per-file diff cursor would fail here.
func TestStreamChangesSkipsContent(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	oldHash := f.hashOf(t, []byte("old\n"))
	newHash := f.hashOf(t, []byte("new\n"))
	f.putCheckpoint(t, 0x11, base, "calc.go", oldHash, worktree.StorageBlob, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "calc.go", newHash, worktree.StorageBlob, 0o644)

	svc := appdiff.New(f.res)
	// No Algorithm set: the change-only path needs no text engine, so it must not require
	// a valid one either.
	csr, err := svc.StreamChanges(context.Background(), appdiff.Request{Range: mustRange(t, "@-2..@-1")})
	if err != nil {
		t.Fatalf("StreamChanges over a missing-blob store: %v (it must not read content)", err)
	}
	defer func() { _ = csr.Close() }()

	var sum compare.Summary
	for csr.Changes.Next() {
		sum.Add(csr.Changes.Change())
	}
	if err := csr.Changes.Err(); err != nil {
		t.Fatalf("change cursor err: %v (the change-only path must not touch content)", err)
	}
	if sum.Modified != 1 || sum.Total() != 1 {
		t.Fatalf("summary = %+v, want exactly one modify", sum)
	}
}

func TestDiffModeOnlyChangeIsMetadata(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	// Same content hash on both sides, different mode bits: a metadata-only change.
	h := f.hashOf(t, []byte("#!/bin/sh\necho hi\n"))
	f.putCheckpoint(t, 0x11, base, "run.sh", h, worktree.StorageBlob, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "run.sh", h, worktree.StorageBlob, 0o755)

	svc := appdiff.New(f.res)
	sr, err := svc.Stream(context.Background(), appdiff.Request{Range: mustRange(t, "@-2..@-1"), Context: 3})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = sr.Close() }()
	files, err := drainDiffStream(sr)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file diff, got %d", len(files))
	}
	fd := files[0]
	// It must be reported as a metadata change with a mode reason, never an empty
	// text diff over identical bytes.
	if fd.Availability != appdiff.Metadata {
		t.Fatalf("availability = %v, want Metadata", fd.Availability)
	}
	if !strings.Contains(fd.Reason, "mode") {
		t.Errorf("reason = %q, want it to mention the mode change", fd.Reason)
	}
	if fd.Text != "" {
		t.Errorf("metadata change must carry no diff text, got %q", fd.Text)
	}
}

func TestDiffHashOnlyChangeHasNoContentDiff(t *testing.T) {
	f := newFixture(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	// Distinct content hashes, stored hash-only, so no blobs and no content diff.
	oldHash := f.hashOf(t, []byte("aaaa\n"))
	newHash := f.hashOf(t, []byte("bbbb\n"))
	f.putCheckpoint(t, 0x11, base, "big.bin", oldHash, worktree.StorageHashOnly, 0o644)
	f.putCheckpoint(t, 0x22, base.Add(time.Hour), "big.bin", newHash, worktree.StorageHashOnly, 0o644)

	svc := appdiff.New(f.res)
	sr, err := svc.Stream(context.Background(), appdiff.Request{Range: mustRange(t, "@-2..@-1"), Context: 3})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = sr.Close() }()
	files, err := drainDiffStream(sr)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(files) != 1 || files[0].Availability != appdiff.HashOnly {
		t.Fatalf("want one HashOnly file, got %+v", files)
	}
}
