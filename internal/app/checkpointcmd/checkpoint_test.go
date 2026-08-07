package checkpointcmd

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/initcmd"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

type fakeGit struct{}

func (fakeGit) Capture(context.Context) (*checkpoint.GitMetadata, bool, error) {
	return nil, false, nil
}

type harness struct {
	svc         *Service
	deps        Deps
	project     projfs.Project
	root        string
	blobs       *blobstore.FS
	checkpoints *checkpointjson.Repo
}

func setup(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	project, err := projfs.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	layout, _ := project.Paths()

	hasher := blake3hash.New()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	blobs := blobstore.New(layout, hasher)
	checkpoints := checkpointjson.NewRepo(layout)
	deps := Deps{
		Scanner:     scanner.New(worktreefs.New(), hasher, idx),
		Index:       idx,
		Hasher:      hasher,
		Blobs:       blobs,
		Checkpoints: checkpoints,
		Git:         fakeGit{},
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Rand:        rand.Reader,
		Version:     "test",
	}
	return &harness{svc: New(deps), deps: deps, project: project, root: root, blobs: blobs, checkpoints: checkpoints}
}

func (h *harness) writeFile(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(h.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) run(t *testing.T, req Request) Result {
	t.Helper()
	req.Project = h.project
	if req.CommandCwd == "" {
		req.CommandCwd = h.root
	}
	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// manifestRecords is a stored checkpoint's manifest drained into slices. It is a
// test-local view, not a stored shape: production reads the header and streams the
// records separately, and these tests need the whole (small) fixture manifest in hand
// to assert per-entry storage, content, and skip facts.
type manifestRecords struct {
	entries []worktree.Entry
	skipped []worktree.SkippedInput
}

// loaded drains a checkpoint's persisted manifest back through the repository's
// record stream — the same reader production uses.
func (h *harness) loaded(t *testing.T, id checkpoint.CheckpointID) manifestRecords {
	t.Helper()
	stream, err := h.checkpoints.OpenManifest(id)
	if err != nil {
		t.Fatalf("open manifest %s: %v", id.Short(), err)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest cursor %s: %v", id.Short(), err)
	}
	defer func() { _ = cur.Close() }()
	var out manifestRecords
	for cur.Next() {
		rec := cur.Record()
		if e, ok := rec.Entry(); ok {
			out.entries = append(out.entries, e)
		} else if sk, ok := rec.Skipped(); ok {
			out.skipped = append(out.skipped, sk)
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain manifest %s: %v", id.Short(), err)
	}
	return out
}

func defaultsStoring() config.Config { return config.Defaults() }

func TestCheckpointCreatesCheckpointWithBlob(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "calc.go", "package calc")

	res := h.run(t, Request{Config: defaultsStoring()})
	if res.BlobsWritten != 1 {
		t.Fatalf("BlobsWritten = %d, want 1", res.BlobsWritten)
	}

	got, ok, err := h.checkpoints.LatestHeader(context.Background())
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%v err=%v", ok, err)
	}
	if got.ID != res.Header.ID {
		t.Fatalf("persisted id %s != returned %s", got.ID, res.Header.ID)
	}
	// The blob for calc.go exists.
	var found bool
	for _, e := range h.loaded(t, got.ID).entries {
		if e.Path.String() == "calc.go" {
			found = true
			if e.Storage != worktree.StorageBlob {
				t.Fatalf("calc.go storage = %v, want blob", e.Storage)
			}
			if has, _ := h.blobs.Has(e.Content); !has {
				t.Fatal("blob for calc.go not stored")
			}
		}
	}
	if !found {
		t.Fatal("calc.go missing from manifest")
	}
}

func TestStrictTrustModeRecorded(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "hi")
	cfg := defaultsStoring()
	cfg.Hashing.TrustMode = config.TrustStrict
	res := h.run(t, Request{Config: cfg})
	if res.Header.TrustMode != config.TrustStrict {
		t.Fatalf("trust mode = %v, want strict", res.Header.TrustMode)
	}
}

func TestStoreFileContentsFalseProducesHashOnly(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "secret-ish")
	cfg := defaultsStoring()
	cfg.Checkpoint.StoreFileContents = false
	res := h.run(t, Request{Config: cfg})
	if res.BlobsWritten != 0 {
		t.Fatalf("BlobsWritten = %d, want 0 when not storing contents", res.BlobsWritten)
	}
	for _, e := range h.loaded(t, res.Header.ID).entries {
		if e.Path.String() == "a.txt" {
			if e.Storage != worktree.StorageHashOnly {
				t.Fatalf("a.txt storage = %v, want hash-only", e.Storage)
			}
			if e.Content.IsZero() {
				t.Fatal("hash-only entry must still carry a content hash")
			}
			if has, _ := h.blobs.Has(e.Content); has {
				t.Fatal("no blob should be stored when store_file_contents=false")
			}
		}
	}
	// The recorded tree hash must be reproducible from the final manifest.
	records := h.loaded(t, res.Header.ID)
	red, err := worktree.ReduceCursor(blake3hash.New(), scantest.CanonicalCursor(records.entries, records.skipped))
	if err != nil {
		t.Fatalf("reduce manifest: %v", err)
	}
	if red.Hash != res.Header.TreeHash {
		t.Fatal("checkpoint tree hash not reproducible from manifest after hash-only downgrade")
	}
}

func TestStoreFileContentsToggleKeepsTreeHashChangesPolicyHash(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "stable content")

	stored := h.run(t, Request{Config: defaultsStoring()})

	cfgOff := defaultsStoring()
	cfgOff.Checkpoint.StoreFileContents = false
	hashOnly := h.run(t, Request{Config: cfgOff})

	// The observed worktree is unchanged, so its tree hash and scan config hash
	// must be identical; only the persistence policy differs.
	if stored.Header.TreeHash != hashOnly.Header.TreeHash {
		t.Errorf("tree hash changed across store_file_contents toggle: %s vs %s", stored.Header.TreeHash, hashOnly.Header.TreeHash)
	}
	if stored.Header.ScanConfigHash != hashOnly.Header.ScanConfigHash {
		t.Errorf("scan config hash changed across store toggle: %s vs %s", stored.Header.ScanConfigHash, hashOnly.Header.ScanConfigHash)
	}
	if stored.Header.CheckpointPolicyHash == hashOnly.Header.CheckpointPolicyHash {
		t.Errorf("checkpoint policy hash unchanged across store toggle: %s", stored.Header.CheckpointPolicyHash)
	}

	// The first run materializes a blob; the second records hash-only with none.
	if stored.BlobsWritten < 1 {
		t.Errorf("store=true run wrote %d blobs, want >=1", stored.BlobsWritten)
	}
	if hashOnly.BlobsWritten != 0 {
		t.Errorf("store=false run wrote %d blobs, want 0", hashOnly.BlobsWritten)
	}
	for _, e := range h.loaded(t, hashOnly.Header.ID).entries {
		if e.Path.String() == "a.txt" && e.Storage != worktree.StorageHashOnly {
			t.Errorf("a.txt storage = %v, want hash-only", e.Storage)
		}
	}
}

func TestLargeFileSkipRecordedAsSkipped(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "small.txt", "x")
	h.writeFile(t, "big.bin", "this is over the tiny limit")
	cfg := defaultsStoring()
	cfg.Hashing.MaxFileSize = config.ByteSize(4)
	cfg.Hashing.LargeFilePolicy = config.LargeFileSkip
	res := h.run(t, Request{Config: cfg})

	var skipped bool
	for _, s := range h.loaded(t, res.Header.ID).skipped {
		if s.Path.String() == "big.bin" && s.Reason == worktree.ReasonLargeFileSkipPolicy {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("big.bin not recorded as skipped: %+v", h.loaded(t, res.Header.ID).skipped)
	}
	if res.Header.Stats.Skipped < 1 {
		t.Fatalf("stats skipped = %d, want >=1", res.Header.Stats.Skipped)
	}
}

func TestReusedHashMaterializesMissingBlob(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "reuse me")

	// First checkpoint stores the blob and indexes the file (normal mode).
	first := h.run(t, Request{Config: defaultsStoring()})
	var content = h.loaded(t, first.Header.ID).entries[0].Content
	if has, _ := h.blobs.Has(content); !has {
		t.Fatal("blob not stored on first checkpoint")
	}
	// Delete the blob to simulate a missing-but-indexed file.
	rc, _ := h.blobs.Open(content)
	_ = rc.Close()
	bpath := blobPath(t, h.root, content)
	if err := os.Remove(bpath); err != nil {
		t.Fatalf("removing blob: %v", err)
	}

	// Second checkpoint: normal mode reuses the indexed hash (file unchanged) but
	// must still re-materialize the missing blob, verifying the bytes.
	second := h.run(t, Request{Config: defaultsStoring()})
	if second.BlobsWritten != 1 {
		t.Fatalf("BlobsWritten = %d, want 1 (re-materialized)", second.BlobsWritten)
	}
	if has, _ := h.blobs.Has(content); !has {
		t.Fatal("missing blob not re-materialized")
	}
}

func TestMissingBlobChangedBytesAborts(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "original")
	apath := filepath.Join(h.root, "a.txt")
	info, _ := os.Stat(apath)
	mtime := info.ModTime()

	cfg := defaultsStoring()
	cfg.Hashing.TrustMode = config.TrustFast // fast mode reuses on size+mtime only
	first := h.run(t, Request{Config: cfg})
	content := h.loaded(t, first.Header.ID).entries[0].Content

	// Delete the blob, then change the bytes while preserving size and mtime so
	// fast mode reuses the stale hash.
	if err := os.Remove(blobPath(t, h.root, content)); err != nil {
		t.Fatalf("removing blob: %v", err)
	}
	if err := os.WriteFile(apath, []byte("modified"), 0o644); err != nil { // same length as "original"
		t.Fatal(err)
	}
	if err := os.Chtimes(apath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	_, err := h.svc.Run(context.Background(), Request{Project: h.project, Config: cfg, CommandCwd: h.root})
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}
	// Only the first checkpoint exists; the aborted one published nothing new.
	list, _ := h.checkpoints.ListHeaders(context.Background())
	if len(list) != 1 {
		t.Fatalf("checkpoint count = %d, want 1 (aborted checkpoint must not publish)", len(list))
	}

	// The abort must have invalidated the stale index entry, so a retry re-hashes
	// the changed file (now content "modified") and succeeds rather than reusing
	// the bad mapping and aborting forever.
	retry := h.run(t, Request{Config: cfg})
	if retry.BlobsWritten != 1 {
		t.Fatalf("retry BlobsWritten = %d, want 1 (re-hashed after invalidation)", retry.BlobsWritten)
	}
	if list, _ := h.checkpoints.ListHeaders(context.Background()); len(list) != 2 {
		t.Fatalf("after successful retry: %d checkpoints, want 2", len(list))
	}
}

// TestMaterializeRejectsPostScanShapeSwap proves materialization reads through the
// scan's verified content source, not a plain re-open by path: replacing a scanned
// regular file with a symlink to an out-of-root file holding the same bytes (same
// content hash) is rejected, even though a plain os.Open would follow it and the
// hash would match.
func TestMaterializeRejectsPostScanShapeSwap(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "secret")

	// Scan first so the verified opener for a.txt is captured.
	scanCfg := defaultsStoring()
	scan, err := h.deps.Scanner.Scan(context.Background(), h.project, scanCfg, scanCfg.HistoryScanScope(), scanner.Options{Now: h.deps.Now(), NeedContentSources: true})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer func() { _ = scan.Close() }()

	// Swap a.txt for a symlink to an out-of-root file with identical bytes.
	outside := t.TempDir()
	outFile := filepath.Join(outside, "copy")
	if err := os.WriteFile(outFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	apath := filepath.Join(h.root, "a.txt")
	if err := os.Remove(apath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outFile, apath); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// Drive the streaming manifest source: materialization happens as records are
	// pulled, so the shape swap surfaces as the cursor's terminal error.
	src := &manifestSource{svc: h.svc, cfg: defaultsStoring(), scan: scan}
	cur, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest source: %v", err)
	}
	for cur.Next() {
	}
	err = cur.Err()
	_ = cur.Close()
	if err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("materialize after shape swap err = %v, want a \"changed during scan\" rejection", err)
	}
}

// recordingInvalidator records which paths were invalidated.
type recordingInvalidator struct{ paths []string }

func (r *recordingInvalidator) Invalidate(_ context.Context, p worktree.RelPath) error {
	r.paths = append(r.paths, p.String())
	return nil
}

func TestUnreadableSourceInvalidatesIndex(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based unreadable test is meaningless as root")
	}
	h := setup(t)
	h.writeFile(t, "a.txt", "secret")
	apath := filepath.Join(h.root, "a.txt")
	info, _ := os.Stat(apath)
	mtime := info.ModTime()

	cfg := defaultsStoring()
	cfg.Hashing.TrustMode = config.TrustFast // reuse on size+mtime, so the scan does not open the file
	first := h.run(t, Request{Config: cfg})
	content := h.loaded(t, first.Header.ID).entries[0].Content

	// Remove the blob and make the source unreadable while keeping size+mtime, so
	// the scan reuses the indexed hash but materialization fails opening the file —
	// a non-mismatch failure that must still drop the stale index entry.
	if err := os.Remove(blobPath(t, h.root, content)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(apath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(apath, 0o644) })
	if err := os.Chtimes(apath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	rec := &recordingInvalidator{}
	deps := h.deps
	deps.Index = rec
	svc := New(deps)

	_, err := svc.Run(context.Background(), Request{Project: h.project, Config: cfg, CommandCwd: h.root})
	if err == nil {
		t.Fatal("expected a materialization failure for the unreadable source")
	}
	if errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("failure should be an open error, not a hash mismatch: %v", err)
	}
	if len(rec.paths) != 1 || rec.paths[0] != "a.txt" {
		t.Fatalf("expected a.txt to be invalidated, got %v", rec.paths)
	}
}

// failingInvalidator models a worktree index whose invalidation fails.
type failingInvalidator struct{}

func (failingInvalidator) Invalidate(context.Context, worktree.RelPath) error {
	return errors.New("index delete failed")
}

func TestMismatchSurfacesInvalidationFailure(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "original")
	apath := filepath.Join(h.root, "a.txt")
	info, _ := os.Stat(apath)
	mtime := info.ModTime()

	cfg := defaultsStoring()
	cfg.Hashing.TrustMode = config.TrustFast
	first := h.run(t, Request{Config: cfg})
	content := h.loaded(t, first.Header.ID).entries[0].Content

	if err := os.Remove(blobPath(t, h.root, content)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apath, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(apath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	// Same scanner/index (so the stale hash is still reused), but invalidation fails.
	deps := h.deps
	deps.Index = failingInvalidator{}
	svc := New(deps)

	_, err := svc.Run(context.Background(), Request{Project: h.project, Config: cfg, CommandCwd: h.root})
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("err should still wrap ErrHashMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalidate") {
		t.Fatalf("err should also report the failed invalidation, got %v", err)
	}
}

func blobPath(t *testing.T, root string, content hashing.ContentHash) string {
	t.Helper()
	bp, err := blob.PathFor(content)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, ".awa", "store", "blobs", filepath.FromSlash(bp.Rel()))
}
