package checkpointcmd

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
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
	content     *recordingContent
}

// recordingContent is the production direct-content reader with a log in front of it.
// It owns the one fact no production instrumentation should have to expose: which
// entries were read by reopening a recorded path. Delegating rather than faking keeps
// the verification real, so a test that asserts a rejection still asserts the shared
// reader's rejection.
//
// Absence from the log means only "not read through this reader"; reading it as "went
// through the retained opener instead" requires the precondition requireNoStoredBlobs
// documents and asserts.
type recordingContent struct {
	inner *worktreefs.ContentReader
	paths []string
}

func (c *recordingContent) Open(path worktree.RelPath, observed worktree.StatSignature) (io.ReadCloser, error) {
	c.paths = append(c.paths, path.String())
	return c.inner.Open(path, observed)
}

// asked reports whether the direct reader was consulted for path.
func (c *recordingContent) asked(path string) bool {
	for _, p := range c.paths {
		if p == path {
			return true
		}
	}
	return false
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
	content := &recordingContent{inner: worktreefs.NewContentReader(layout.Root())}
	deps := Deps{
		Scanner:     scanner.New(worktreefs.New(), hasher, idx),
		Index:       idx,
		Hasher:      hasher,
		Blobs:       blobs,
		Checkpoints: checkpoints,
		Git:         fakeGit{},
		Content:     content,
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Rand:        rand.Reader,
		Version:     "test",
	}
	return &harness{svc: New(deps), deps: deps, project: project, root: root, blobs: blobs, checkpoints: checkpoints, content: content}
}

// scan runs one scan under exactly the retention mode production derives from cfg, so
// an assertion about what a checkpoint retains is an assertion about the mapping the
// service actually uses.
func (h *harness) scan(t *testing.T, cfg config.Config) scanner.Result {
	t.Helper()
	res, err := h.deps.Scanner.Scan(context.Background(), h.project, cfg, cfg.HistoryScanScope(), scanner.Options{
		Now:     h.deps.Now(),
		Sources: sourceRetention(cfg),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return res
}

// scannedEntries drains a scan's manifest and returns its entries, so an assertion can
// ask about every path the scan actually recorded rather than a hand-listed subset.
func scannedEntries(t *testing.T, scan scanner.Result) []worktree.Entry {
	t.Helper()
	cur, err := scan.Manifest().Open(context.Background())
	if err != nil {
		t.Fatalf("open scan manifest: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var out []worktree.Entry
	for cur.Next() {
		if e, ok := cur.Record().Entry(); ok {
			out = append(out, e)
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain scan manifest: %v", err)
	}
	return out
}

// requireNoStoredBlobs pins the precondition every post-scan rejection test depends on.
// The blob store is content-addressed and idempotent: a blob already present is verified
// against its own address and reused, and the entry's content source is never opened at
// all. Only an absent blob makes materialization actually reach for source bytes, which
// is the only situation in which a post-scan substitution can be detected. Asserting it
// keeps that dependency executable rather than a claim in a comment.
//
// This covers blobs stored before the test runs. The other half of the precondition —
// that no earlier entry in the same pass publishes the same content — belongs to the
// fixture; see followFixture.
func (h *harness) requireNoStoredBlobs(t *testing.T) {
	t.Helper()
	stored, err := h.blobs.List()
	if err != nil {
		t.Fatalf("list blobs: %v", err)
	}
	if len(stored) != 0 {
		t.Fatalf("precondition failed: %d blobs already stored, so materialization would reuse them without opening any source", len(stored))
	}
}

// materialize drives the streaming manifest source to completion and returns its
// terminal error, if any. Materialization happens as records are pulled, so this is how
// a per-entry rejection surfaces.
func (h *harness) materialize(t *testing.T, cfg config.Config, scan scanner.Result) error {
	t.Helper()
	src := &manifestSource{svc: h.svc, cfg: cfg, scan: scan}
	cur, err := src.Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest source: %v", err)
	}
	for cur.Next() {
	}
	cerr := cur.Err()
	_ = cur.Close()
	return cerr
}

// followFixture writes the worktree every followed-provenance test needs: two ordinary
// files, one file reached through a file symlink, and one reached below a followed
// directory symlink. hidden/equal.txt and hidden/dir2/inner.txt hold bytes identical to
// the terminals the links point at, so a repoint substitutes content that would satisfy
// any hash check and must be rejected on provenance alone.
//
// The terminals live under an excluded directory deliberately: it is requireNoStoredBlobs'
// precondition carried into the fixture, since scanning them as entries in their own
// right would publish the same bytes earlier in the pass and leave nothing to read.
func (h *harness) followFixture(t *testing.T) config.Config {
	t.Helper()
	scantest.RequireSymlinks(t)
	h.writeFile(t, "plain.txt", "plain")
	h.writeFile(t, "pkg/a.txt", "alpha")
	h.writeFile(t, "hidden/real.txt", "followed-bytes")
	h.writeFile(t, "hidden/equal.txt", "followed-bytes")
	h.writeFile(t, "hidden/dir/inner.txt", "descendant-bytes")
	h.writeFile(t, "hidden/dir2/inner.txt", "descendant-bytes")
	h.symlink(t, "link.txt", "hidden/real.txt")
	h.symlink(t, "dirlink", "hidden/dir")

	cfg := defaultsStoring()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.ExtraExcludes = []string{"hidden"}
	return cfg
}

// symlink creates one fixture link. Its callers gate on scantest.RequireSymlinks
// first, so a failure here is a broken fixture, not an unsupported platform, and must
// fail rather than skip — a skip would quietly retire the whole followed-provenance
// suite.
func (h *harness) symlink(t *testing.T, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(h.root, filepath.FromSlash(name))); err != nil {
		t.Fatalf("linking %s -> %s: %v", name, target, err)
	}
}

// repoint replaces an existing symlink with one pointing somewhere else.
func (h *harness) repoint(t *testing.T, name, target string) {
	t.Helper()
	p := filepath.Join(h.root, filepath.FromSlash(name))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
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

	health, err := h.checkpoints.StoreHealthNewest(context.Background(), 1)
	if err != nil {
		t.Fatalf("store health: %v", err)
	}
	got, ok := health.Latest()
	if !ok {
		t.Fatal("Latest: no checkpoint recorded")
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
	health, _ := h.checkpoints.StoreHealthAll(context.Background())
	if health.Recorded() != 1 {
		t.Fatalf("checkpoint count = %d, want 1 (aborted checkpoint must not publish)", health.Recorded())
	}

	// The abort must have invalidated the stale index entry, so a retry re-hashes
	// the changed file (now content "modified") and succeeds rather than reusing
	// the bad mapping and aborting forever.
	retry := h.run(t, Request{Config: cfg})
	if retry.BlobsWritten != 1 {
		t.Fatalf("retry BlobsWritten = %d, want 1 (re-hashed after invalidation)", retry.BlobsWritten)
	}
	if health, _ := h.checkpoints.StoreHealthAll(context.Background()); health.Recorded() != 2 {
		t.Fatalf("after successful retry: %d checkpoints, want 2", health.Recorded())
	}
}

// TestMaterializeRejectsPostScanShapeSwap proves an ordinary entry is reopened through
// the shared verified reader rather than by a plain path open: replacing a scanned
// regular file with a symlink to an out-of-root file holding the same bytes (same
// content hash) is rejected, even though a plain os.Open would follow it and the hash
// would match. The scan retained no source for a.txt, so the rejection is the reader's
// alone.
func TestMaterializeRejectsPostScanShapeSwap(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "secret")

	cfg := defaultsStoring()
	scan := h.scan(t, cfg)
	h.requireNoStoredBlobs(t)
	p, _ := worktree.ParseRelPath("a.txt")
	if _, ok := scan.Source(p); ok {
		t.Fatal("a content-enabled checkpoint scan must retain no source for a directly-reached file")
	}

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

	if err := h.materialize(t, cfg, scan); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("materialize after shape swap err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestMaterializeRejectsPostScanStatSubstitution proves the shared reader also checks
// the node it opened against the stat the entry recorded: with the blob absent so the
// source really is read, a file replaced after the scan by a different regular file at
// the same path is refused on the descriptor, before any hashing, so the failure is an
// observation change rather than a hash mismatch discovered afterwards.
func TestMaterializeRejectsPostScanStatSubstitution(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "secret")

	cfg := defaultsStoring()
	scan := h.scan(t, cfg)
	h.requireNoStoredBlobs(t)

	apath := filepath.Join(h.root, "a.txt")
	if err := os.Remove(apath); err != nil {
		t.Fatal(err)
	}
	h.writeFile(t, "a.txt", "a decidedly longer replacement body")

	err := h.materialize(t, cfg, scan)
	if err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("materialize after stat substitution err = %v, want a \"changed during scan\" rejection", err)
	}
	if errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("substitution should be refused at the open, not after hashing: %v", err)
	}
}

// TestContentDisabledRetainsNoSourceAndNeverOpensContent proves a checkpoint that
// stores no file contents asks neither source owner for bytes: the scan keeps no
// content source at all, and the direct reader is never consulted during
// materialization. Hashing during the scan itself is untouched and is not the claim
// here — the entries still carry their content hashes.
func TestContentDisabledRetainsNoSourceAndNeverOpensContent(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "alpha")
	h.writeFile(t, "pkg/b.txt", "beta")

	cfg := defaultsStoring()
	cfg.Checkpoint.StoreFileContents = false

	// The scan production would take for this configuration retains nothing.
	scan := h.scan(t, cfg)
	for _, e := range scannedEntries(t, scan) {
		if _, ok := scan.Source(e.Path); ok {
			t.Fatalf("content-disabled checkpoint scan retained a source for %s", e.Path)
		}
	}

	res := h.run(t, Request{Config: cfg})
	if len(h.content.paths) != 0 {
		t.Fatalf("content-disabled checkpoint opened %v through the direct reader", h.content.paths)
	}
	if res.BlobsWritten != 0 || res.BlobsReused != 0 {
		t.Fatalf("blobs written=%d reused=%d, want none when contents are disabled", res.BlobsWritten, res.BlobsReused)
	}
	for _, e := range h.loaded(t, res.Header.ID).entries {
		if e.Kind != worktree.KindRegular {
			continue
		}
		if e.Storage != worktree.StorageHashOnly {
			t.Errorf("%s storage = %v, want hash-only", e.Path, e.Storage)
		}
		if e.Content.IsZero() {
			t.Errorf("%s lost its content hash; the scan must still hash what it reads", e.Path)
		}
	}
}

// TestOrdinaryMaterializationNeedsNoRetainedSource proves the ordinary path works with
// nothing retained: a content-enabled checkpoint of directly-reached files keeps no
// source and writes every blob through the shared reader.
func TestOrdinaryMaterializationNeedsNoRetainedSource(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.txt", "alpha")
	h.writeFile(t, "pkg/b.txt", "beta")

	cfg := defaultsStoring()
	scan := h.scan(t, cfg)
	h.requireNoStoredBlobs(t)
	for _, e := range scannedEntries(t, scan) {
		if _, ok := scan.Source(e.Path); ok {
			t.Fatalf("a worktree with no followed entry retained a source for %s", e.Path)
		}
	}

	res := h.run(t, Request{Config: cfg})
	if res.BlobsWritten != 2 {
		t.Fatalf("BlobsWritten = %d, want 2", res.BlobsWritten)
	}
	for _, want := range []string{"a.txt", "pkg/b.txt"} {
		if !h.content.asked(want) {
			t.Errorf("%s was not read through the direct content reader (asked: %v)", want, h.content.paths)
		}
	}
	for _, e := range h.loaded(t, res.Header.ID).entries {
		if e.Kind != worktree.KindRegular {
			continue
		}
		if e.Storage != worktree.StorageBlob {
			t.Errorf("%s storage = %v, want blob", e.Path, e.Storage)
			continue
		}
		if has, _ := h.blobs.Has(e.Content); !has {
			t.Errorf("%s published a blob reference the store does not hold", e.Path)
		}
	}
}

// TestFollowedEntriesUseTheRetainedOpener proves the split holds on a worktree that has
// both kinds: the direct reader's log holds the directly-reached files only. Routing a
// followed entry through it would put that virtual path in the log and fail here.
func TestFollowedEntriesUseTheRetainedOpener(t *testing.T) {
	h := setup(t)
	cfg := h.followFixture(t)

	scan := h.scan(t, cfg)
	h.requireNoStoredBlobs(t)
	for _, followed := range []string{"link.txt", "dirlink/inner.txt"} {
		p, _ := worktree.ParseRelPath(followed)
		if _, ok := scan.Source(p); !ok {
			t.Fatalf("followed entry %s has no retained source", followed)
		}
	}

	res := h.run(t, Request{Config: cfg})
	// Four distinct contents, so four blobs are written. Without this the assertions
	// below would also hold for entries that were never materialized at all.
	if res.BlobsWritten != 4 {
		t.Fatalf("BlobsWritten = %d, want 4", res.BlobsWritten)
	}
	for _, followed := range []string{"link.txt", "dirlink/inner.txt"} {
		if h.content.asked(followed) {
			t.Errorf("followed entry %s was read through the direct content reader", followed)
		}
	}
	for _, direct := range []string{"plain.txt", "pkg/a.txt"} {
		if !h.content.asked(direct) {
			t.Errorf("ordinary entry %s was not read through the direct content reader (asked: %v)", direct, h.content.paths)
		}
	}
}

// TestFollowedFileLinkRepointRejected proves a followed file entry is validated against
// the chain the walk observed. The link is repointed after the scan to a terminal whose
// bytes are identical, so nothing about the content would betray the substitution — and
// the direct reader, which would resolve the new target's path just as happily, is never
// asked.
func TestFollowedFileLinkRepointRejected(t *testing.T) {
	h := setup(t)
	cfg := h.followFixture(t)
	scan := h.scan(t, cfg)
	h.requireNoStoredBlobs(t)

	h.repoint(t, "link.txt", "hidden/equal.txt")

	err := h.materialize(t, cfg, scan)
	if err == nil || !strings.Contains(err.Error(), "changed during scan") || !strings.Contains(err.Error(), "link.txt") {
		t.Fatalf("materialize after followed-link repoint err = %v, want link.txt rejected as \"changed during scan\"", err)
	}
	if h.content.asked("link.txt") {
		t.Errorf("the repointed followed entry was consulted through the direct content reader: %v", h.content.paths)
	}
}

// TestFollowedDirectoryAncestorRepointRejected proves the same for an entry whose
// followed provenance is an ancestor, not its own final component: repointing the
// directory symlink it was reached through to a directory holding a byte-identical file
// at the same name must be rejected, and again without ever consulting the direct reader
// for that virtual path.
func TestFollowedDirectoryAncestorRepointRejected(t *testing.T) {
	h := setup(t)
	cfg := h.followFixture(t)
	scan := h.scan(t, cfg)
	h.requireNoStoredBlobs(t)

	h.repoint(t, "dirlink", "hidden/dir2")

	err := h.materialize(t, cfg, scan)
	if err == nil || !strings.Contains(err.Error(), "changed during scan") || !strings.Contains(err.Error(), "dirlink/inner.txt") {
		t.Fatalf("materialize after followed-ancestor repoint err = %v, want dirlink/inner.txt rejected as \"changed during scan\"", err)
	}
	if h.content.asked("dirlink/inner.txt") {
		t.Errorf("the entry below the repointed directory link was consulted through the direct content reader: %v", h.content.paths)
	}
}

// TestMissingFollowedSourceFailsLoudly proves the absence of a followed entry's
// retained opener is treated as the wiring fault it is, and that the fault is raised
// before the blob store is asked to do anything. There is no fallback: the virtual path
// still resolves to a readable regular file with the recorded bytes, and the direct
// reader is still refused.
//
// The sharp case is "blob already stored": a check that lived inside the store, or that
// only ran when bytes were needed, would let a mis-wired retention mode pass silently on
// every checkpoint after the first. Both cases must fail identically.
func TestMissingFollowedSourceFailsLoudly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prestore bool
	}{
		{name: "blob absent", prestore: false},
		{name: "blob already stored and verified", prestore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			cfg := h.followFixture(t)
			if tc.prestore {
				// One correctly-wired checkpoint first, so every blob this manifest
				// references is already present.
				h.run(t, Request{Config: cfg})
				h.content.paths = nil
			}
			stored, err := h.blobs.List()
			if err != nil {
				t.Fatalf("list blobs: %v", err)
			}
			if (len(stored) > 0) != tc.prestore {
				t.Fatalf("precondition failed: %d blobs stored, prestore=%v", len(stored), tc.prestore)
			}

			// A scan that kept nothing, paired with a policy that stores contents — the
			// state a mis-wired retention mode would produce.
			scan, err := h.deps.Scanner.Scan(context.Background(), h.project, cfg, cfg.HistoryScanScope(), scanner.Options{
				Now:     h.deps.Now(),
				Sources: scanner.RetainNoSources,
			})
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			defer func() { _ = scan.Close() }()

			merr := h.materialize(t, cfg, scan)
			if merr == nil || !strings.Contains(merr.Error(), "no retained traversal-verified content source") {
				t.Fatalf("materialize without a followed source err = %v, want a loud wiring failure", merr)
			}
			if h.content.asked("link.txt") || h.content.asked("dirlink/inner.txt") {
				t.Errorf("a followed entry fell back to the direct content reader: %v", h.content.paths)
			}
		})
	}
}

// TestSourceRetentionFollowsCheckpointPolicy pins the mapping the service derives from
// the effective configuration, so a change to it is a change to this oracle rather than
// a silent widening of what every checkpoint retains.
func TestSourceRetentionFollowsCheckpointPolicy(t *testing.T) {
	storing := defaultsStoring()
	if got := sourceRetention(storing); got != scanner.RetainFollowedSources {
		t.Errorf("store_file_contents=true retention = %v, want followed-only", got)
	}
	notStoring := defaultsStoring()
	notStoring.Checkpoint.StoreFileContents = false
	if got := sourceRetention(notStoring); got != scanner.RetainNoSources {
		t.Errorf("store_file_contents=false retention = %v, want none", got)
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
