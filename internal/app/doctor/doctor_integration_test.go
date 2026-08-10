package doctor

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver for the stale-index test

	"awarer/internal/app/checkpointcmd"
	"awarer/internal/app/initcmd"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	dom "awarer/internal/domain/doctor"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
)

type fakeTracker struct {
	tr  gitmeta.Tracking
	err error
}

func (f fakeTracker) AwaTracking(context.Context) (gitmeta.Tracking, error) { return f.tr, f.err }

type env struct {
	project projfs.Project
	root    string
	cfg     config.Config
}

func newEnv(t testing.TB) *env {
	t.Helper()
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	p, err := projfs.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	return &env{project: p, root: root, cfg: config.Defaults()}
}

// checkpointWith writes files and records a checkpoint, so the project has a real
// checkpoint with blobs to inspect or corrupt.
func (e *env) checkpointWith(t testing.TB, cfg config.Config, files map[string]string) checkpoint.CheckpointHeader {
	t.Helper()
	layout, _ := e.project.Paths()
	hasher := blake3hash.New()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	defer func() { _ = idx.Close() }()

	for name, content := range files {
		path := filepath.Join(e.root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	svc := checkpointcmd.New(checkpointcmd.Deps{
		Scanner:     scanner.New(worktreefs.New(), hasher, idx),
		Index:       idx,
		Hasher:      hasher,
		Blobs:       blobstore.New(layout, hasher),
		Checkpoints: checkpointjson.NewRepo(layout),
		Git:         fakeGit{},
		Content:     worktreefs.NewContentReader(layout.Root()),
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		Rand:        rand.Reader,
		Version:     "test",
	})
	res, err := svc.Run(context.Background(), checkpointcmd.Request{Project: e.project, Config: cfg, CommandCwd: e.root})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return res.Header
}

type fakeGit struct{}

func (fakeGit) Capture(context.Context) (*checkpoint.GitMetadata, bool, error) {
	return nil, false, nil
}

// resolvedConfig wraps a plain config in a ResolvedConfig for doctor tests that
// exercise value checks rather than layer attribution. Any value that differs from
// the product default is honestly attributed to a local config file (so the
// constructor's value/origin agreement holds); a default value stays default and
// carries no source path.
func resolvedConfig(cfg config.Config) config.ResolvedConfig {
	origins := config.OriginsFrom(cfg, config.LayerLocal)
	local, err := config.NewLayerFact(config.LayerLocal, "/test/.awa/config.toml", true)
	if err != nil {
		panic("doctor test: layer fact: " + err.Error())
	}
	r, err := config.NewResolvedConfig(cfg, origins, []config.LayerFact{local})
	if err != nil {
		panic("doctor test: resolvedConfig: " + err.Error())
	}
	return r
}

func (e *env) run(t testing.TB, d Deps, req Request) dom.DoctorResult {
	t.Helper()
	req.Project = e.project
	// A zero Resolved has no scope include; the real default config always sets one.
	if len(req.Resolved.Config().Scope.Include) == 0 {
		req.Resolved = resolvedConfig(e.cfg)
	}
	res, err := New(d).Run(context.Background(), req)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return res
}

// noGit keeps every test deterministic and offline: the git checker never shells
// out unless a test deliberately injects a tracker.
func noGit() Deps { return Deps{Git: fakeTracker{tr: gitmeta.Tracking{InRepo: false}}} }

// overwrite replaces a published, read-only state file: such files are written
// 0o444, so a test that corrupts one must remove it first rather than write over it.
func overwrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findingsByCode(res dom.DoctorResult, code dom.FindingCode) []dom.Finding {
	var out []dom.Finding
	for _, f := range res.Findings() {
		if f.Code() == code {
			out = append(out, f)
		}
	}
	return out
}

func TestHealthyProject(t *testing.T) {
	e := newEnv(t)
	e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})

	res := e.run(t, noGit(), Request{})
	if res.Health() != dom.HealthOK {
		t.Fatalf("health = %v, want ok; findings=%+v", res.Health(), res.Findings())
	}
	if res.NeedsAttention() {
		t.Fatalf("healthy project should not need attention")
	}
	if len(res.Findings()) != 0 {
		t.Fatalf("expected no findings, got %+v", res.Findings())
	}
}

func TestMissingRequiredDir(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	if err := os.RemoveAll(layout.LocksDir()); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeRequiredDirMissing); len(got) != 1 {
		t.Fatalf("want one required-dir-missing, got %+v", res.Findings())
	}
	if res.Health() != dom.HealthFailed {
		t.Fatalf("health = %v, want failed", res.Health())
	}
}

func TestRequiredDirReplacedByFile(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	if err := os.RemoveAll(layout.LocksDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.LocksDir(), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeRequiredDirNotDir); len(got) != 1 {
		t.Fatalf("want one required-dir-not-directory, got %+v", res.Findings())
	}
}

func TestMissingBlobReported(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()

	// Delete the blob the checkpoint's calc.go entry references.
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), h.ID, "calc.go")
	if err := os.Remove(blobPathFor(t, layout, ent.Content)); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeCheckpointMissingBlob); len(got) != 1 {
		t.Fatalf("want one checkpoint-missing-blob, got %+v", res.Findings())
	}
	if !res.NeedsAttention() || res.Health() != dom.HealthFailed {
		t.Fatalf("missing blob should fail health and need attention")
	}
}

// manifestEntries drains a stored checkpoint's entries through the repository's
// record stream, so a test that has to reach a referenced blob reads the manifest the
// same way production does.
func manifestEntries(t *testing.T, repo *checkpointjson.Repo, id checkpoint.CheckpointID) []worktree.Entry {
	t.Helper()
	stream, err := repo.OpenManifest(id)
	if err != nil {
		t.Fatalf("open manifest %s: %v", id.Short(), err)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest cursor %s: %v", id.Short(), err)
	}
	defer func() { _ = cur.Close() }()
	var entries []worktree.Entry
	for cur.Next() {
		if e, ok := cur.Record().Entry(); ok {
			entries = append(entries, e)
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain manifest %s: %v", id.Short(), err)
	}
	return entries
}

func TestStrictDetectsTamperedBlob(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()

	// Overwrite a referenced blob in place: it is present but no longer hashes to
	// its address. Default doctor only checks presence, so it passes; --strict
	// recomputes the hash and catches the tampering.
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), h.ID, "calc.go")
	overwrite(t, blobPathFor(t, layout, ent.Content), "tampered bytes")

	if res := e.run(t, noGit(), Request{}); res.Health() != dom.HealthOK {
		t.Fatalf("non-strict doctor should pass on a present (if tampered) blob, got %v", res.Health())
	}
	res := e.run(t, noGit(), Request{Strict: true})
	if got := findingsByCode(res, dom.CodeCheckpointMissingBlob); len(got) != 1 {
		t.Fatalf("strict doctor should flag the tampered blob, got %+v", res.Findings())
	}
}

// blobPathFor locates the stored file backing a content hash, so a test can remove or
// rewrite exactly the blob a checkpoint entry references.
func blobPathFor(t *testing.T, layout paths.Layout, h hashing.ContentHash) string {
	t.Helper()
	bp, err := blob.PathFor(h)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(layout.BlobsDir(), filepath.FromSlash(bp.Rel()))
}

// blobEntryFor returns the manifest entry a checkpoint recorded for one worktree path.
func blobEntryFor(t *testing.T, repo *checkpointjson.Repo, id checkpoint.CheckpointID, path string) worktree.Entry {
	t.Helper()
	for _, ent := range manifestEntries(t, repo, id) {
		if ent.Storage == worktree.StorageBlob && ent.Path.String() == path {
			return ent
		}
	}
	t.Fatalf("checkpoint %s has no blob entry for %s", id.Short(), path)
	return worktree.Entry{}
}

// TestVerifiedBlobSkipsRepeatedIO pins the invocation-local reuse: once a hash has
// passed its presence check, another reference to the same hash must not read the store
// again. The oracle is real store state — the blob is deleted between the two
// verifications, so a repeated read could only report it missing.
func TestVerifiedBlobSkipsRepeatedIO(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), h.ID, "calc.go")

	v := newCheckpointBlobVerifier(layout, false)
	rep := &report{}
	v.verify(ent, "checkpoint", rep)
	if len(rep.findings) != 0 {
		t.Fatalf("a present blob must verify cleanly, got %+v", rep.findings)
	}

	if err := os.Remove(blobPathFor(t, layout, ent.Content)); err != nil {
		t.Fatal(err)
	}

	v.verify(ent, "checkpoint", rep)
	if len(rep.findings) != 0 {
		t.Fatalf("an already verified hash must not be read again, got %+v", rep.findings)
	}

	// Control: the deletion really is observable, so the assertion above is about reuse
	// rather than about a check that cannot fail.
	fresh := &report{}
	newCheckpointBlobVerifier(layout, false).verify(ent, "checkpoint", fresh)
	if len(fresh.findings) != 1 {
		t.Fatalf("a verifier that has not seen the hash must report the deleted blob, got %+v", fresh.findings)
	}
}

// TestStrictVerifiedBlobSkipsRepeatedIO is the strict counterpart: a hash whose content
// has been rehashed and matched is not opened or rehashed again. The blob is tampered
// between the two verifications, so a repeated read could only report a mismatch.
func TestStrictVerifiedBlobSkipsRepeatedIO(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), h.ID, "calc.go")

	v := newCheckpointBlobVerifier(layout, true)
	rep := &report{}
	v.verify(ent, "checkpoint", rep)
	if len(rep.findings) != 0 {
		t.Fatalf("an intact blob must verify cleanly in strict mode, got %+v", rep.findings)
	}

	overwrite(t, blobPathFor(t, layout, ent.Content), "tampered bytes")

	v.verify(ent, "checkpoint", rep)
	if len(rep.findings) != 0 {
		t.Fatalf("an already rehashed hash must not be read again, got %+v", rep.findings)
	}

	fresh := &report{}
	newCheckpointBlobVerifier(layout, true).verify(ent, "checkpoint", fresh)
	if len(fresh.findings) != 1 {
		t.Fatalf("a verifier that has not seen the hash must report the tampered blob, got %+v", fresh.findings)
	}
}

// TestStrictMissingBlobKeepsMissingMessage pins strict classification of absent
// content: the single strict open replaces the presence check, so a blob that is simply
// gone must still be diagnosed as missing rather than as an unreadable one.
func TestStrictMissingBlobKeepsMissingMessage(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), h.ID, "calc.go")
	if err := os.Remove(blobPathFor(t, layout, ent.Content)); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, noGit(), Request{Strict: true})
	got := findingsByCode(res, dom.CodeCheckpointMissingBlob)
	if len(got) != 1 {
		t.Fatalf("want one missing-blob finding, got %+v", got)
	}
	if !strings.Contains(got[0].Message(), "references a missing blob") {
		t.Fatalf("want the missing-blob message, got %q", got[0].Message())
	}
}

// TestUnreadableBlobIsClassifiedInBothModes pins the other side of that classification:
// a blob the store rejects for a reason other than absence (here its path is no longer a
// regular file) is unreadable rather than missing, and says so identically in both modes
// — strict mode reads it through the open that replaced its presence preflight, which is
// an implementation detail the diagnosis must not expose. Both carry the store's own
// rejection reason.
func TestUnreadableBlobIsClassifiedInBothModes(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), h.ID, "calc.go")
	path := blobPathFor(t, layout, ent.Content)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	message := map[string]string{}
	for _, tc := range []struct {
		name   string
		strict bool
	}{
		{name: "default", strict: false},
		{name: "strict", strict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := e.run(t, noGit(), Request{Strict: tc.strict})
			got := findingsByCode(res, dom.CodeCheckpointMissingBlob)
			if len(got) != 1 {
				t.Fatalf("want one unreadable-blob finding, got %+v", got)
			}
			if !strings.Contains(got[0].Message(), "cannot read referenced blob") {
				t.Fatalf("want the unreadable-blob message, got %q", got[0].Message())
			}
			if !strings.Contains(got[0].Message(), "not a regular file") {
				t.Fatalf("want the store's own rejection reason, got %q", got[0].Message())
			}
			message[tc.name] = got[0].Message()
		})
	}

	// Each mode builds this message on its own path, so equality — not a shared prefix —
	// is what keeps the removed two-step implementation from reappearing as a second
	// wording.
	if message["default"] != message["strict"] {
		t.Fatalf("the modes must report the same damage identically:\n default: %q\n strict:  %q",
			message["default"], message["strict"])
	}
}

// checkpointsSharingBlob records two checkpoints that both reference one piece of
// content through two entry paths, so a single blob backs four manifest references
// spread over two checkpoints.
func (e *env) checkpointsSharingBlob(t *testing.T) (first, second checkpoint.CheckpointHeader) {
	t.Helper()
	first = e.checkpointWith(t, e.cfg, map[string]string{"one.go": "package shared", "two.go": "package shared"})
	second = e.checkpointWith(t, e.cfg, map[string]string{"later.go": "package later"})
	return first, second
}

// assertSharedBlobAttribution requires exactly one finding per referencing manifest
// entry: both entry paths, in both checkpoints, each reported once at its own
// checkpoint path. Deduplicated output — or a hash recorded before it was verified —
// collapses this set.
func assertSharedBlobAttribution(t *testing.T, got []dom.Finding, layout paths.Layout, ids ...checkpoint.CheckpointID) {
	t.Helper()
	seen := make(map[[2]string]int, len(got))
	for _, f := range got {
		seen[[2]string{f.Path(), f.Subject()}]++
	}
	for _, id := range ids {
		for _, entry := range []string{"one.go", "two.go"} {
			at := [2]string{filepath.Join(layout.CheckpointsDir(), id.String()), entry}
			if seen[at] != 1 {
				t.Fatalf("want exactly one finding for %v, got %d (all: %+v)", at, seen[at], got)
			}
		}
	}
	if len(seen) != 2*len(ids) {
		t.Fatalf("unexpected finding locations: %+v", seen)
	}
}

// TestReusedBlobStillStreamsEveryCheckpoint pins the two halves of the reuse contract
// that only show up across checkpoints: one verifier serves the whole walk, and a reused
// hash stops the blob read only — the second checkpoint's manifest is still opened,
// streamed, and counted as a passed check. The blob is deleted between the two
// checkpoints, so nothing but the first checkpoint's success can keep the second clean.
func TestReusedBlobStillStreamsEveryCheckpoint(t *testing.T) {
	e := newEnv(t)
	first, second := e.checkpointsSharingBlob(t)
	layout, _ := e.project.Paths()
	repo := checkpointjson.NewRepo(layout)
	ent := blobEntryFor(t, repo, first.ID, "one.go")

	s := New(noGit())
	v := newCheckpointBlobVerifier(layout, false)
	rep := &report{}
	s.checkOneCheckpoint(context.Background(), layout, repo, first.ID, v, rep)
	if len(rep.findings) != 0 || rep.passed != 1 {
		t.Fatalf("the first checkpoint must pass cleanly, got %d passed and %+v", rep.passed, rep.findings)
	}

	if err := os.Remove(blobPathFor(t, layout, ent.Content)); err != nil {
		t.Fatal(err)
	}

	s.checkOneCheckpoint(context.Background(), layout, repo, second.ID, v, rep)
	if len(rep.findings) != 0 {
		t.Fatalf("the shared hash was already verified, so it must not be read again: %+v", rep.findings)
	}
	if rep.passed != 2 {
		t.Fatalf("the second checkpoint must still be streamed and counted, got %d passed", rep.passed)
	}

	// Control: without that earlier success the same checkpoint reports both of its
	// references to the deleted blob, so the assertions above are about reuse.
	fresh := &report{}
	s.checkOneCheckpoint(context.Background(), layout, repo, second.ID, newCheckpointBlobVerifier(layout, false), fresh)
	if len(fresh.findings) != 2 {
		t.Fatalf("want both references to the deleted blob reported, got %+v", fresh.findings)
	}
}

// TestMissingBlobReportedForEveryDuplicateReference proves reuse never suppresses a
// damaged location: an absent blob referenced four times is reported four times, each
// at its own checkpoint and entry path.
func TestMissingBlobReportedForEveryDuplicateReference(t *testing.T) {
	e := newEnv(t)
	first, second := e.checkpointsSharingBlob(t)
	layout, _ := e.project.Paths()
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), first.ID, "one.go")
	if err := os.Remove(blobPathFor(t, layout, ent.Content)); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeCheckpointMissingBlob)
	if len(got) != 4 {
		t.Fatalf("want one missing-blob finding per reference (4), got %d: %+v", len(got), got)
	}
	assertSharedBlobAttribution(t, got, layout, first.ID, second.ID)
	if !res.NeedsAttention() || res.Health() != dom.HealthFailed {
		t.Fatalf("a missing blob must still fail health, got %v", res.Health())
	}
}

// TestStrictMismatchedBlobReportedForEveryDuplicateReference is the strict counterpart:
// a present blob whose bytes no longer match its address is not a strict success, so it
// stays retryable and every reference to it is diagnosed.
func TestStrictMismatchedBlobReportedForEveryDuplicateReference(t *testing.T) {
	e := newEnv(t)
	first, second := e.checkpointsSharingBlob(t)
	layout, _ := e.project.Paths()
	ent := blobEntryFor(t, checkpointjson.NewRepo(layout), first.ID, "one.go")
	overwrite(t, blobPathFor(t, layout, ent.Content), "tampered bytes")

	if res := e.run(t, noGit(), Request{}); res.Health() != dom.HealthOK {
		t.Fatalf("non-strict doctor only checks presence, got %v", res.Health())
	}

	res := e.run(t, noGit(), Request{Strict: true})
	got := findingsByCode(res, dom.CodeCheckpointMissingBlob)
	if len(got) != 4 {
		t.Fatalf("want one mismatch finding per reference (4), got %d: %+v", len(got), got)
	}
	for _, f := range got {
		if !strings.Contains(f.Message(), "does not match its address") {
			t.Fatalf("want an address-mismatch message, got %q", f.Message())
		}
	}
	assertSharedBlobAttribution(t, got, layout, first.ID, second.ID)
}

func TestHashOnlyEntryStaysHealthy(t *testing.T) {
	e := newEnv(t)
	cfg := config.Defaults()
	cfg.Checkpoint.StoreFileContents = false
	e.checkpointWith(t, cfg, map[string]string{"a.txt": "secret-ish"})

	res := e.run(t, noGit(), Request{Resolved: resolvedConfig(cfg)})
	if got := findingsByCode(res, dom.CodeCheckpointMissingBlob); len(got) != 0 {
		t.Fatalf("hash-only entry must not be reported as a missing blob: %+v", got)
	}
	if res.Health() != dom.HealthOK {
		t.Fatalf("health = %v, want ok", res.Health())
	}
}

func TestCorruptCheckpointHeader(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()

	headerPath := filepath.Join(layout.CheckpointsDir(), h.ID.String(), "header.json")
	overwrite(t, headerPath, "{ not valid json")
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeCheckpointCorrupt); len(got) != 1 {
		t.Fatalf("want one checkpoint-corrupt, got %+v", res.Findings())
	}
}

// TestIncompatibleCheckpointFormatReported proves a header declaring a schema this
// build cannot read is reported under its own stable code, distinct from corruption,
// is not repairable, and survives a repair pass unchanged — doctor diagnoses an
// incompatible schema, it does not migrate, quarantine, or delete it.
func TestIncompatibleCheckpointFormatReported(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()

	headerPath := filepath.Join(layout.CheckpointsDir(), h.ID.String(), "header.json")
	restampToNonCurrentSchema(t, headerPath)

	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeCheckpointIncompatibleFormat)
	if len(got) != 1 {
		t.Fatalf("want one checkpoint-incompatible-format, got %+v", res.Findings())
	}
	if got[0].Severity() != dom.SeverityError || got[0].Repairable() {
		t.Fatalf("incompatible finding severity=%v repairable=%v, want error/non-repairable", got[0].Severity(), got[0].Repairable())
	}
	// It must NOT also be reported as generic corruption.
	if c := findingsByCode(res, dom.CodeCheckpointCorrupt); len(c) != 0 {
		t.Fatalf("incompatible format also reported as corrupt: %+v", c)
	}
	if res.Health() != dom.HealthFailed {
		t.Fatalf("health = %v, want failed", res.Health())
	}

	// A repair pass must not migrate or quarantine the incompatible checkpoint: it stays on
	// disk and stays incompatible.
	res = e.run(t, noGit(), Request{Repair: true})
	if len(findingsByCode(res, dom.CodeCheckpointIncompatibleFormat)) != 1 {
		t.Fatalf("incompatible finding gone after --repair: %+v", res.Findings())
	}
	if _, err := os.Stat(headerPath); err != nil {
		t.Fatalf("incompatible checkpoint header removed by --repair: %v", err)
	}
}

// restampToNonCurrentSchema rewrites a persisted record's declared schema_version to
// one this build does not speak, and returns the number it wrote.
//
// The replacement is derived from the document awa itself just produced rather than
// written as a literal. A literal would have to be a real retired generation to look
// plausible, and keeping those numbers alive in ordinary tests is exactly the
// archaeology the reset removed from production: the classifier keys on "not the
// current version", so that is what the fixture must express.
func restampToNonCurrentSchema(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	current, err := strconv.Atoi(string(m["schema_version"]))
	if err != nil {
		t.Fatalf("%s carries no numeric schema_version: %v", path, err)
	}
	next := current + 1
	m["schema_version"] = json.RawMessage(strconv.Itoa(next))
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	overwrite(t, path, string(out))
	return next
}

func TestCorruptManifestRecordReportsLocation(t *testing.T) {
	e := newEnv(t)
	h := e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()

	manifestPath := filepath.Join(layout.CheckpointsDir(), h.ID.String(), "manifest.jsonl")
	overwrite(t, manifestPath, "{\"entry\": broken}\n")
	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeCheckpointManifestCorrupt)
	if len(got) != 1 {
		t.Fatalf("want one checkpoint-manifest-corrupt, got %+v", res.Findings())
	}
}

func TestInvalidIndexReported(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	dbPath := filepath.Join(layout.IndexesDir(), sqliteindex.FileName)
	if err := os.WriteFile(dbPath, []byte("this is not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeIndexUnreadable); len(got) != 1 {
		t.Fatalf("want one index-unreadable, got %+v", res.Findings())
	}
}

func TestOrphanTempRepairAndIdempotent(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	orphan := filepath.Join(layout.TmpDir(), ".orphan-123")
	if err := os.WriteFile(orphan, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate it well past the stale threshold so it is provably a crash leftover,
	// not an active operation, and therefore safely repairable.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	// Without --repair: reported and unrepaired.
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeOrphanTemp); len(got) != 1 || got[0].Repaired() {
		t.Fatalf("want one unrepaired orphan-temp, got %+v", res.Findings())
	}
	if !res.NeedsAttention() {
		t.Fatalf("unrepaired orphan temp should need attention")
	}

	// With --repair: removed and marked repaired.
	res = e.run(t, noGit(), Request{Repair: true})
	got := findingsByCode(res, dom.CodeOrphanTemp)
	if len(got) != 1 || !got[0].Repaired() {
		t.Fatalf("want one repaired orphan-temp, got %+v", res.Findings())
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan temp still present after repair: %v", err)
	}
	if res.NeedsAttention() {
		t.Fatalf("after repair, nothing should need attention")
	}

	// Idempotent: a second --repair run is clean.
	res = e.run(t, noGit(), Request{Repair: true})
	if len(findingsByCode(res, dom.CodeOrphanTemp)) != 0 {
		t.Fatalf("second repair should find no orphan temp, got %+v", res.Findings())
	}
}

func TestFreshTempNotRepaired(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// A just-created temp artifact may belong to an active operation, so it is a
	// non-repairable warning and --repair must not delete it.
	fresh := filepath.Join(layout.TmpDir(), ".active-xyz")
	if err := os.WriteFile(fresh, []byte("in progress"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{Repair: true})
	got := findingsByCode(res, dom.CodeOrphanTemp)
	if len(got) != 1 || got[0].Repairable() {
		t.Fatalf("want one non-repairable orphan-temp for a fresh artifact, got %+v", res.Findings())
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a fresh temp artifact must not be removed: %v", err)
	}
}

func TestUnknownLockNotRemoved(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	lockPath := filepath.Join(layout.LocksDir(), "weird.lock")
	if err := os.WriteFile(lockPath, []byte("garbage not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeLockUnknown); len(got) != 1 || got[0].Repairable() {
		t.Fatalf("want one non-repairable lock-unknown, got %+v", res.Findings())
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("unknown lock must not be removed: %v", err)
	}
}

func TestStaleLockRepaired(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	rec := lockfile.Record{Operation: "run", PID: 424242, Hostname: "thishost", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
	data, err := lockfile.Encode(rec)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(layout.LocksDir(), "run.lock")
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	d := Deps{
		Git:          fakeTracker{tr: gitmeta.Tracking{InRepo: false}},
		Hostname:     "thishost",
		ProcessAlive: func(int) bool { return false }, // owner provably gone
		Now:          func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
	}
	res := e.run(t, d, Request{Repair: true})
	got := findingsByCode(res, dom.CodeLockStale)
	if len(got) != 1 || !got[0].Repaired() {
		t.Fatalf("want one repaired lock-stale, got %+v", res.Findings())
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock should be removed: %v", err)
	}
}

// TestStaleExclusiveLocksNotRepaired covers every exclusive lock, not just the
// collector's. An exclusive lock file is owned by the acquire path: under the flock
// backend it is deliberately left on disk after release, because unlinking it would
// break flock for a waiter. Doctor reading that as a repairable stale lock would
// make an ordinary "awa restore --apply" leave the project reporting a finding, and
// the repair pass could delete the very lock a live holder is standing on.
//
// The names come from lockfile so the classification stays semantic; a new
// exclusive lock added there without doctor's exemption is what this must catch.
func TestStaleExclusiveLocksNotRepaired(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   lockfile.Operation
	}{
		{name: lockfile.CollectorLockName, op: lockfile.OpGC},
		{name: lockfile.RestoreLockName, op: lockfile.OpRestore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !lockfile.IsExclusiveLockName(tc.name) {
				t.Fatalf("%q is not classified as an exclusive lock name", tc.name)
			}
			e := newEnv(t)
			layout, _ := e.project.Paths()
			rec := lockfile.Record{Operation: string(tc.op), PID: 424242, Hostname: "thishost", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}
			data, err := lockfile.Encode(rec)
			if err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(layout.LocksDir(), tc.name)
			if err := os.WriteFile(lockPath, data, 0o644); err != nil {
				t.Fatal(err)
			}

			d := Deps{
				Git:          fakeTracker{tr: gitmeta.Tracking{InRepo: false}},
				Hostname:     "thishost",
				ProcessAlive: func(int) bool { return false }, // owner provably gone (stale)
				Now:          func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
			}
			res := e.run(t, d, Request{Repair: true})
			if got := findingsByCode(res, dom.CodeLockStale); len(got) != 0 {
				t.Fatalf("an exclusive lock must not be a stale-lock finding, got %+v", got)
			}
			if _, err := os.Stat(lockPath); err != nil {
				t.Fatalf("doctor must not remove an exclusive lock: %v", err)
			}
		})
	}
}

func TestActiveLockKept(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	rec := lockfile.Record{Operation: "run", PID: 4242, Hostname: "thishost", CreatedAt: time.Unix(1_700_000_090, 0).UTC()}
	data, _ := lockfile.Encode(rec)
	lockPath := filepath.Join(layout.LocksDir(), "run.lock")
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	d := Deps{
		Git:          fakeTracker{tr: gitmeta.Tracking{InRepo: false}},
		Hostname:     "thishost",
		ProcessAlive: func(int) bool { return true }, // owner alive
		Now:          func() time.Time { return time.Unix(1_700_000_100, 0).UTC() },
	}
	res := e.run(t, d, Request{Repair: true})
	if len(findingsByCode(res, dom.CodeLockStale)) != 0 {
		t.Fatalf("active lock must not be reported stale: %+v", res.Findings())
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("active lock must be kept: %v", err)
	}
}

func TestStaleIndexRepairedByRebuild(t *testing.T) {
	e := newEnv(t)
	// Build the index (lazily created on the first scan via checkpoint).
	e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	dbPath := filepath.Join(layout.IndexesDir(), sqliteindex.FileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("index file not created (lazy): %v", err)
	}

	// Stamp an older schema version: doctor must report it as repairable stale, not ok.
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE schema_meta SET version = 0"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	// Leave WAL sidecars next to the database; a rebuild must remove the whole owned
	// file group, not just the main file.
	walPath := dbPath + "-wal"
	shmPath := dbPath + "-shm"
	for _, p := range []string{walPath, shmPath} {
		if err := os.WriteFile(p, []byte("sidecar"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeIndexStale)
	if len(got) != 1 || !got[0].Repairable() {
		t.Fatalf("want one repairable index-stale, got %+v", res.Findings())
	}

	// --repair rebuilds (removes) the index so the next scan recreates it.
	res = e.run(t, noGit(), Request{Repair: true})
	if g := findingsByCode(res, dom.CodeIndexStale); len(g) != 1 || !g[0].Repaired() {
		t.Fatalf("want index-stale repaired, got %+v", res.Findings())
	}
	for _, p := range []string{dbPath, walPath, shmPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("rebuild repair should remove %s: %v", filepath.Base(p), err)
		}
	}
}

func TestUnavailableIndexIsNonRepairableWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	e := newEnv(t)
	e.checkpointWith(t, e.cfg, map[string]string{"calc.go": "package calc"})
	layout, _ := e.project.Paths()
	dbPath := filepath.Join(layout.IndexesDir(), sqliteindex.FileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Skipf("index file not created (lazy): %v", err)
	}
	if err := os.Chmod(dbPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })

	// An unreadable index (permission) is not corruption: doctor must not offer to
	// delete it. It is a non-repairable warning, so the run still exits 0.
	res := e.run(t, noGit(), Request{Repair: true})
	got := findingsByCode(res, dom.CodeIndexUnreadable)
	if len(got) != 1 || got[0].Repairable() || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one non-repairable index-unreadable warning, got %+v", res.Findings())
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("unavailable index must not be removed: %v", err)
	}
}

func TestFailedRepairRecordsRepairFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// An old (repairable) orphan whose parent dir is read-only: the delete fails, so
	// doctor must record an explicit repair-failed finding and leave the orphan.
	orphan := filepath.Join(layout.TmpDir(), ".orphan-stuck")
	if err := os.WriteFile(orphan, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(layout.TmpDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(layout.TmpDir(), 0o755) })

	res := e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeRepairFailed); len(got) != 1 || got[0].Repairable() {
		t.Fatalf("want one non-repairable repair-failed, got %+v", res.Findings())
	}
	if !res.NeedsAttention() {
		t.Fatalf("a failed repair must keep the run needing attention")
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan must remain when its repair failed: %v", err)
	}
}

func TestGitCheckFailureWarning(t *testing.T) {
	e := newEnv(t)
	d := Deps{Git: fakeTracker{err: errors.New("git exploded")}}
	res := e.run(t, d, Request{})
	got := findingsByCode(res, dom.CodeGitCheckFailed)
	if len(got) != 1 || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one git-check-failed warning, got %+v", res.Findings())
	}
	if res.NeedsAttention() {
		t.Fatalf("a git-check warning should not need attention")
	}
}

func TestSymlinkedRequiredDirReported(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// Replace .awa/locks with a symlink to a real directory elsewhere: stat would
	// follow it and call it a valid dir, but no-follow detection must flag it.
	target := t.TempDir()
	if err := os.RemoveAll(layout.LocksDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, layout.LocksDir()); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeRequiredDirNotDir); len(got) != 1 {
		t.Fatalf("want one required-dir-not-directory for symlinked locks, got %+v", res.Findings())
	}
}

func TestOversizedLockIsUnknownNotRemoved(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	lockPath := filepath.Join(layout.LocksDir(), "big.lock")
	// Valid lock JSON in the first bytes, then padding past the read cap.
	big := make([]byte, 70*1024)
	for i := range big {
		big[i] = ' '
	}
	copy(big, []byte(`{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}`))
	if err := os.WriteFile(lockPath, big, 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeLockUnknown); len(got) != 1 || got[0].Repairable() {
		t.Fatalf("want one non-repairable lock-unknown for oversized lock, got %+v", res.Findings())
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("oversized lock must not be removed: %v", err)
	}
}

func TestDanglingRunPointerReported(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// A key pointer whose target run does not exist would turn a real Lookup into
	// corruption; doctor must surface it as run-pointer-corrupt.
	hex := strings.Repeat("ab", 32)
	ptrDir := filepath.Join(layout.RunsDir(), "keys", "blake3", hex[:2])
	if err := os.MkdirAll(ptrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"schema_version":1,"run_id":"0123456789abcdef0123456789abcdef"}`
	if err := os.WriteFile(filepath.Join(ptrDir, hex+".json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeRunPointerCorrupt); len(got) != 1 {
		t.Fatalf("want one run-pointer-corrupt, got %+v", res.Findings())
	}
}

// TestIncompatibleRunEntryReportedWithItsSchemaVersion pins the incompatible-run
// diagnostic: a run declaring a schema this build cannot read is a warning distinct
// from corruption, and the message names the version actually persisted instead of a
// class label that goes stale on every schema bump.
func TestIncompatibleRunEntryReportedWithItsSchemaVersion(t *testing.T) {
	// An obviously synthetic version, not a generation this project ever shipped. The
	// schema probe runs before any field validation, so this number alone decides the
	// verdict — which is why the fixture can stay a two-line document instead of a
	// published run, and why nothing here needs to know what the current version is.
	const nonCurrentSchema = 99

	e := newEnv(t)
	layout, _ := e.project.Paths()
	id := strings.Repeat("0a", 16)
	dir := filepath.Join(layout.RunsDir(), "entries", id[:2], id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"schema_version":%d}`, nonCurrentSchema)
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeRunIncompatibleFormat)
	if len(got) != 1 || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one incompatible-run warning, got %+v", res.Findings())
	}
	if len(findingsByCode(res, dom.CodeRunCorrupt)) != 0 {
		t.Fatalf("an incompatible schema is not corruption, got %+v", res.Findings())
	}
	if msg := got[0].Message(); !strings.Contains(msg, fmt.Sprintf("schema version %d", nonCurrentSchema)) {
		t.Errorf("message %q must name the persisted schema version %d", msg, nonCurrentSchema)
	}
}

func TestSymlinkedRunsTmpReported(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// runs/tmp is not a required dir, so checkLayout does not cover it; the temp
	// checker must report it when it exists but is not a usable directory.
	runsTmp := filepath.Join(layout.RunsDir(), "tmp")
	if err := os.Symlink(t.TempDir(), runsTmp); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeTempUnreadable); len(got) != 1 || got[0].Repairable() {
		t.Fatalf("want one non-repairable temp-unreadable for symlinked runs/tmp, got %+v", res.Findings())
	}
}

func TestGitTrackedWarning(t *testing.T) {
	e := newEnv(t)
	d := Deps{Git: fakeTracker{tr: gitmeta.Tracking{InRepo: true, AwaTracked: true}}}
	res := e.run(t, d, Request{})
	got := findingsByCode(res, dom.CodeAwaTrackedByGit)
	if len(got) != 1 || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one awa-tracked-by-git warning, got %+v", res.Findings())
	}
	// A warning alone does not need attention and keeps exit 0.
	if res.NeedsAttention() {
		t.Fatalf("a git-tracking warning should not need attention")
	}
}

func TestStateGitignoreMissingRepairedAndIdempotent(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// newEnv ran init, which wrote the guard; remove it to simulate a project
	// created before the guard existed (or a user who deleted it).
	if err := os.Remove(layout.StateGitignore()); err != nil {
		t.Fatal(err)
	}

	// Without --repair: reported as a repairable warning, unrepaired.
	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeStateGitignoreMissing)
	if len(got) != 1 || got[0].Repaired() || !got[0].Repairable() || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one unrepaired repairable state-gitignore-missing warning, got %+v", res.Findings())
	}

	// With --repair: the guard is recreated with canonical content and marked repaired.
	res = e.run(t, noGit(), Request{Repair: true})
	got = findingsByCode(res, dom.CodeStateGitignoreMissing)
	if len(got) != 1 || !got[0].Repaired() {
		t.Fatalf("want one repaired state-gitignore-missing, got %+v", res.Findings())
	}
	guard, err := os.ReadFile(layout.StateGitignore())
	if err != nil {
		t.Fatalf("guard not restored: %v", err)
	}
	if string(guard) != projfs.StateGitignoreContent {
		t.Errorf("restored guard = %q, want canonical %q", guard, projfs.StateGitignoreContent)
	}

	// Idempotent: a second --repair run finds nothing to do.
	res = e.run(t, noGit(), Request{Repair: true})
	if len(findingsByCode(res, dom.CodeStateGitignoreMissing)) != 0 {
		t.Fatalf("second repair should find no missing guard, got %+v", res.Findings())
	}
}

func TestStateGitignoreIneffectiveRepaired(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// A guard that exists but does not ignore .awa contents.
	if err := os.Remove(layout.StateGitignore()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.StateGitignore(), []byte("# ignores nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeStateGitignoreIneffective); len(got) != 1 || !got[0].Repairable() {
		t.Fatalf("want one repairable state-gitignore-ineffective, got %+v", res.Findings())
	}

	res = e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeStateGitignoreIneffective); len(got) != 1 || !got[0].Repaired() {
		t.Fatalf("want one repaired state-gitignore-ineffective, got %+v", res.Findings())
	}
	guard, err := os.ReadFile(layout.StateGitignore())
	if err != nil {
		t.Fatal(err)
	}
	if string(guard) != projfs.StateGitignoreContent {
		t.Errorf("guard = %q, want canonical %q", guard, projfs.StateGitignoreContent)
	}
}

func TestStateGitignoreRepairLeavesRootGitignoreUntouched(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	const rootContent = "node_modules/\n*.log\n"
	rootGitignore := filepath.Join(e.root, ".gitignore")
	if err := os.WriteFile(rootGitignore, []byte(rootContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(layout.StateGitignore()); err != nil {
		t.Fatal(err)
	}

	e.run(t, noGit(), Request{Repair: true})

	got, err := os.ReadFile(rootGitignore)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != rootContent {
		t.Errorf("root .gitignore = %q, want it untouched as %q", got, rootContent)
	}
}

func TestStateGitignoreUnreadableIsNonRepairableWarning(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// Replace the guard with a directory: ReadFile then fails for a reason other
	// than "missing", which doctor must report as unreadable, not missing, and must
	// not try to overwrite (a directory in the way is not the repairable case). A
	// directory is a portable stand-in that even root cannot read as a file.
	if err := os.Remove(layout.StateGitignore()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(layout.StateGitignore(), 0o755); err != nil {
		t.Fatal(err)
	}

	res := e.run(t, noGit(), Request{Repair: true})
	if len(findingsByCode(res, dom.CodeStateGitignoreMissing)) != 0 {
		t.Fatalf("an unreadable guard must not be reported as missing, got %+v", res.Findings())
	}
	got := findingsByCode(res, dom.CodeStateGitignoreUnreadable)
	if len(got) != 1 || got[0].Repairable() || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one non-repairable state-gitignore-unreadable warning, got %+v", res.Findings())
	}
}

func TestHealthyProjectHasNoGuardFinding(t *testing.T) {
	// A freshly initialized project carries the guard, so the state-gitignore check
	// passes and never produces a finding.
	e := newEnv(t)
	res := e.run(t, noGit(), Request{})
	if got := findingsByCode(res, dom.CodeStateGitignoreMissing); len(got) != 0 {
		t.Fatalf("healthy project should have no state-gitignore-missing finding, got %+v", got)
	}
	if got := findingsByCode(res, dom.CodeStateGitignoreIneffective); len(got) != 0 {
		t.Fatalf("healthy project should have no state-gitignore-ineffective finding, got %+v", got)
	}
}
