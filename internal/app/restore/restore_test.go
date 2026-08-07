package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/checkpointcmd"
	"awarer/internal/app/initcmd"
	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	domrestore "awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/restorespool"
	"awarer/internal/infra/restorestore"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/infra/worktreemut"
)

type harness struct {
	t       *testing.T
	root    string
	layout  paths.Layout
	project projfs.Project
	cfg     domainconfig.Config
	hasher  *blake3hash.Hasher
	blobs   *blobstore.FS
	recov   *restorestore.Repo
	svc     *Service
	clock   time.Time
	// checkpointClock advances per recorded checkpoint so position-based references
	// order deterministically.
	checkpointClock time.Time
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
	layout, err := project.Paths()
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	hasher := blake3hash.New()
	h := &harness{
		t: t, root: root, layout: layout, project: project,
		cfg: domainconfig.Defaults(), hasher: hasher,
		blobs:           blobstore.New(layout, hasher),
		recov:           restorestore.New(layout, hasher),
		clock:           time.Unix(1_700_000_000, 0).UTC(),
		checkpointClock: time.Unix(1_700_000_000, 0).UTC(),
	}
	h.svc = New(Deps{
		Resolver: h.resolver(),
		Scanner:  scanner.NewReadOnly(worktreefs.New(), hasher, nil),
		Blobs:    h.blobs,
		Recovery: h.recov,
		Current:  worktreefs.NewContentReader(root),
		Spools: func(id domrestore.OperationID) (Spool, error) {
			return restorespool.Open(layout, id)
		},
		Appliers: h.applierFactory(),
		Now:      func() time.Time { h.clock = h.clock.Add(time.Second); return h.clock },
		Rand:     rand.Reader,
		Version:  "test",
	})
	return h
}

// applierFactory is the production applier factory this harness wires. It is a
// method so a test that swapped in a failing wrapper can put the real one back.
func (h *harness) applierFactory() func(domrestore.OperationID) (Applier, error) {
	layout := h.layout
	hasher := h.hasher
	return func(id domrestore.OperationID) (Applier, error) {
		return worktreemut.New(layout, id, hasher)
	}
}

func (h *harness) resolver() *state.Resolver {
	h.t.Helper()
	return h.resolverWith(checkpointjson.NewRepo(h.layout))
}

// resolverWith builds the resolver over a caller-supplied checkpoint repository, so
// a test can observe the repository's stream contract while every layer above it —
// prefix and name resolution, header reads, and the manifest tree-hash/stats/count
// verification the resolver puts on top of the stream — stays the production one.
func (h *harness) resolverWith(repo checkpoint.Repository) *state.Resolver {
	h.t.Helper()
	return state.NewResolver(state.Deps{
		Checkpoints: repo,
		Scanner:     scanner.NewReadOnly(worktreefs.New(), h.hasher, nil),
		Blobs:       h.blobs,
		Hasher:      h.hasher,
		Restores:    h.recov,
	})
}

func (h *harness) abs(rel string) string { return filepath.Join(h.root, filepath.FromSlash(rel)) }

func (h *harness) write(rel, content string) {
	h.t.Helper()
	p := h.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", rel, err)
	}
}

func (h *harness) remove(rel string) {
	h.t.Helper()
	if err := os.RemoveAll(h.abs(rel)); err != nil {
		h.t.Fatalf("remove %s: %v", rel, err)
	}
}

func (h *harness) read(rel string) string {
	h.t.Helper()
	b, err := os.ReadFile(h.abs(rel)) //nolint:gosec // test fixture path
	if err != nil {
		h.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (h *harness) exists(rel string) bool {
	h.t.Helper()
	_, err := os.Lstat(h.abs(rel))
	return err == nil
}

// checkpointNow records a checkpoint over the current worktree and returns its id.
// Each checkpoint gets a distinct recorded time, because position-based references
// (latest, @-N) order by recorded creation time: two checkpoints stamped
// identically would make "latest" ambiguous and this fixture nondeterministic.
func (h *harness) checkpointNow(cfg domainconfig.Config) checkpoint.CheckpointID {
	h.t.Helper()
	h.checkpointClock = h.checkpointClock.Add(time.Minute)
	stamp := h.checkpointClock
	svc := checkpointcmd.New(checkpointcmd.Deps{
		Scanner:     scanner.New(worktreefs.New(), h.hasher, nil),
		Hasher:      h.hasher,
		Blobs:       h.blobs,
		Checkpoints: checkpointjson.NewRepo(h.layout),
		Git:         noGit{},
		Now:         func() time.Time { return stamp },
		Rand:        rand.Reader,
		Version:     "test",
	})
	res, err := svc.Run(context.Background(), checkpointcmd.Request{
		Project: h.project, Config: cfg, CommandCwd: h.root,
	})
	if err != nil {
		h.t.Fatalf("checkpoint: %v", err)
	}
	return res.Header.ID
}

type noGit struct{}

func (noGit) Capture(context.Context) (*checkpoint.GitMetadata, bool, error) { return nil, false, nil }

func (h *harness) ref(token string) state.Ref {
	h.t.Helper()
	r, err := state.ParseRef(token)
	if err != nil {
		h.t.Fatalf("parse ref %q: %v", token, err)
	}
	return r
}

func (h *harness) selection(p ...string) domrestore.Selection {
	h.t.Helper()
	if len(p) == 0 {
		return domrestore.NewAllSelection()
	}
	rps := make([]worktree.RelPath, 0, len(p))
	for _, s := range p {
		rp, err := worktree.ParseRelPath(s)
		if err != nil {
			h.t.Fatalf("rel path %q: %v", s, err)
		}
		rps = append(rps, rp)
	}
	sel, err := domrestore.NewPathSelection(rps)
	if err != nil {
		h.t.Fatalf("selection: %v", err)
	}
	return sel
}

func (h *harness) run(t *testing.T, refTok string, sel domrestore.Selection, apply bool) Result {
	t.Helper()
	res, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(refTok), Selection: sel, Apply: apply,
	})
	if err != nil {
		t.Fatalf("Run(apply=%v): %v", apply, err)
	}
	return res
}

// --- the dogfood scenario -------------------------------------------------

// generatorRewrite sets up the situation restore exists for: a reviewed checkpoint,
// then a generator that rewrote a generated subtree while the developer also has
// unrelated dirty edits and a local ignored file.
func (h *harness) generatorRewrite() checkpoint.CheckpointID {
	h.t.Helper()
	h.write("generated/client/openapi.json", `{"paths":{"/v1":{}}}`)
	h.write("generated/client/model.go", "package client\n\ntype Model struct{}\n")
	h.write("src/service.go", "package src\n\nfunc A() int { return 1 }\n")
	id := h.checkpointNow(h.cfg)

	// The generator rewrites the generated subtree...
	h.write("generated/client/openapi.json", `{"paths":{"/v2":{}}}`)
	h.write("generated/client/model.go", "package client\n\ntype Model struct{ Broken bool }\n")
	h.write("generated/client/extra.go", "package client\n\n// accidental\n")
	// ...while the developer has an unrelated dirty edit in hand.
	h.write("src/service.go", "package src\n\nfunc A() int { return 2 }\n")
	return id
}

func TestPreviewIsSideEffectFreeAndPredictsTheApply(t *testing.T) {
	h := setup(t)
	id := h.generatorRewrite()

	before := snapshotTree(t, h.root)
	preview := h.run(t, id.String(), h.selection("generated/client"), false)

	if got := preview.Result.Outcome(); got != domrestore.OutcomePreview {
		t.Fatalf("outcome = %s, want preview", got)
	}
	counts := preview.Result.Counts()
	if counts.Replace != 2 || counts.DeleteFile != 1 {
		t.Fatalf("counts = %+v, want 2 replaces and 1 delete", counts)
	}
	if counts.Unavailable() != 0 {
		t.Errorf("preview reported %d unavailable operation(s): %+v", counts.Unavailable(), preview.Result)
	}
	if !preview.Result.Recovery().IsZero() {
		t.Error("a preview published a recovery observation")
	}
	// The oracle is the filesystem, not a flag on the result.
	if after := snapshotTree(t, h.root); after != before {
		t.Errorf("preview mutated the worktree:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	applied := h.run(t, id.String(), h.selection("generated/client"), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s, want applied (%+v)", got, applied.Result.Reasons())
	}
	if applied.Result.Counts() != counts {
		t.Errorf("apply counts %+v differ from the preview's %+v", applied.Result.Counts(), counts)
	}
	if applied.Result.Completed() != 3 || applied.Result.Remaining() != 0 {
		t.Errorf("completed=%d remaining=%d, want 3 and 0", applied.Result.Completed(), applied.Result.Remaining())
	}
}

func TestApplyRestoresOnlyTheSelectedSubtree(t *testing.T) {
	h := setup(t)
	id := h.generatorRewrite()
	dirtyBefore := h.read("src/service.go")

	res := h.run(t, id.String(), h.selection("generated/client"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s, want applied", got)
	}

	if got := h.read("generated/client/openapi.json"); got != `{"paths":{"/v1":{}}}` {
		t.Errorf("openapi.json = %q, want the checkpointed bytes", got)
	}
	if got := h.read("generated/client/model.go"); !strings.Contains(got, "type Model struct{}") {
		t.Errorf("model.go = %q, want the checkpointed bytes", got)
	}
	if h.exists("generated/client/extra.go") {
		t.Error("the accidentally generated file survived the restore")
	}
	// The unrelated dirty edit is byte-identical: restore touched only the selection.
	if got := h.read("src/service.go"); got != dirtyBefore {
		t.Errorf("an unrelated dirty edit changed: %q, want %q", got, dirtyBefore)
	}
}

func TestApplyPublishesRecoveryEvidenceThatUndoesIt(t *testing.T) {
	h := setup(t)
	id := h.generatorRewrite()
	overwritten := h.read("generated/client/openapi.json")
	deleted := h.read("generated/client/extra.go")

	res := h.run(t, id.String(), h.selection("generated/client"), true)
	if res.Result.Recovery().IsZero() {
		t.Fatal("an applied restore published no recovery observation")
	}
	recoveryRef := res.RecoveryRef
	if recoveryRef == "" || !strings.HasPrefix(recoveryRef, "restore:") {
		t.Fatalf("recovery reference = %q", recoveryRef)
	}

	// The restore worked...
	if h.exists("generated/client/extra.go") {
		t.Fatal("extra.go survived the restore")
	}
	// ...and the pre-restore state is fully recoverable from the observation: the
	// overwritten bytes come back AND the file the restore deleted comes back,
	// which is what the covered scope exists to make possible.
	undo := h.run(t, recoveryRef, h.selection("generated/client"), true)
	if got := undo.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("undo outcome = %s, want applied (%v)", got, undo.Result.Reasons())
	}
	if got := h.read("generated/client/openapi.json"); got != overwritten {
		t.Errorf("undo did not restore the overwritten bytes: %q, want %q", got, overwritten)
	}
	if !h.exists("generated/client/extra.go") {
		t.Fatal("undo did not restore the file the restore deleted")
	}
	if got := h.read("generated/client/extra.go"); got != deleted {
		t.Errorf("undo restored %q, want %q", got, deleted)
	}
}

func TestRecoveryObservationCapturesContentUnderHashOnlyPreferences(t *testing.T) {
	h := setup(t)
	// A project that deliberately stores no checkpoint content still gets fully
	// reversible restores: undo evidence is not subject to that preference, because
	// the bytes are precisely what would otherwise be unrecoverable.
	h.write("data.txt", "original")
	storing := h.cfg
	id := h.checkpointNow(storing)
	h.write("data.txt", "locally edited")

	hashOnly := h.cfg
	hashOnly.Checkpoint.StoreFileContents = false
	h.cfg = hashOnly

	res := h.run(t, id.String(), h.selection("data.txt"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s, want applied (%v)", got, res.Result.Reasons())
	}
	if got := h.read("data.txt"); got != "original" {
		t.Fatalf("data.txt = %q, want original", got)
	}
	undo := h.run(t, res.RecoveryRef, h.selection("data.txt"), true)
	if got := undo.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("undo outcome = %s, want applied (%v)", got, undo.Result.Reasons())
	}
	if got := h.read("data.txt"); got != "locally edited" {
		t.Errorf("undo restored %q, want the locally edited bytes", got)
	}
}

func TestRestoreNeverMovesLatestOrCreatesACheckpoint(t *testing.T) {
	h := setup(t)
	id := h.generatorRewrite()
	repo := checkpointjson.NewRepo(h.layout)
	before, err := repo.ListHeaders(context.Background())
	if err != nil {
		t.Fatalf("list headers: %v", err)
	}

	res := h.run(t, id.String(), h.selection("generated/client"), true)
	if res.Result.Outcome() != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s", res.Result.Outcome())
	}

	after, err := repo.ListHeaders(context.Background())
	if err != nil {
		t.Fatalf("list headers: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("restore changed the checkpoint count from %d to %d", len(before), len(after))
	}
	latest, ok, err := repo.LatestHeader(context.Background())
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if !ok || latest.ID != before[0].ID {
		t.Errorf("restore moved latest to %v, want %s", latest.ID, before[0].ID)
	}
}

// --- refusals and honesty -------------------------------------------------

func TestApplyRefusesWhenTheSourceOnlyProvesIdentity(t *testing.T) {
	h := setup(t)
	h.write("big.bin", strings.Repeat("x", 64))
	storing := h.cfg
	storing.Checkpoint.StoreFileContents = false
	h.write("keep.txt", "untouched")
	id := h.checkpointNow(storing)
	h.write("big.bin", "rewritten")

	preview := h.run(t, id.String(), h.selection("big.bin"), false)
	if preview.Result.Counts().Blocked == 0 {
		t.Fatalf("a hash-only source was not reported as blocked: %+v", preview.Result.Counts())
	}

	applied := h.run(t, id.String(), h.selection("big.bin"), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got)
	}
	if !hasReason(applied.Result, domrestore.ReasonHashOnlyContent) {
		t.Errorf("refusal reasons = %v, want hash-only-content", applied.Result.Reasons())
	}
	if got := h.read("big.bin"); got != "rewritten" {
		t.Errorf("a refused apply mutated the worktree: %q", got)
	}
}

// TestApplyRefusesWhenARequiredBlobIsMissing keeps blob-missing meaning what it
// says. The checkpoint here truthfully promised a blob — it published one — and the
// bytes later went away, so "the store no longer holds what the manifest names" is
// the accurate report. It is deliberately a different answer from a source that
// never held bytes at all (a hash-only checkpoint, or any run observation), which
// reports hash-only-content: one sends a reader looking at retention, the other at
// the source's own storage policy.
func TestApplyRefusesWhenARequiredBlobIsMissing(t *testing.T) {
	h := setup(t)
	h.write("data.txt", "original")
	id := h.checkpointNow(h.cfg)
	h.write("data.txt", "changed")

	// Empty the blob store behind awa's back: the manifest still names content the
	// store can no longer produce.
	if err := os.RemoveAll(h.layout.BlobsDir()); err != nil {
		t.Fatalf("remove blobs: %v", err)
	}

	preview := h.run(t, id.String(), h.selection("data.txt"), false)
	if preview.Result.Counts().Blocked != 1 {
		t.Fatalf("preview counts = %+v, want one blocked operation", preview.Result.Counts())
	}
	applied := h.run(t, id.String(), h.selection("data.txt"), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got)
	}
	if !hasReason(applied.Result, domrestore.ReasonBlobMissing) {
		t.Errorf("reasons = %v, want blob-missing", applied.Result.Reasons())
	}
	requireFailure(t, applied.Result, "data.txt", domrestore.ReasonBlobMissing)
	if got := h.read("data.txt"); got != "changed" {
		t.Errorf("a refused apply mutated the worktree: %q", got)
	}
}

func TestApplyRefusesWhenARequiredBlobIsCorrupt(t *testing.T) {
	h := setup(t)
	h.write("data.txt", "original")
	id := h.checkpointNow(h.cfg)
	h.write("data.txt", "changed")

	corruptEveryBlob(t, h.layout)

	// Preview reads no bytes, so it cannot know: it reports a ready plan. That is
	// the honest division — preview checks metadata, apply checks content — and it
	// is why apply verifies before mutating rather than trusting the preview.
	preview := h.run(t, id.String(), h.selection("data.txt"), false)
	if preview.Result.Counts().Replace != 1 {
		t.Fatalf("preview counts = %+v, want one replace", preview.Result.Counts())
	}

	applied := h.run(t, id.String(), h.selection("data.txt"), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused", got)
	}
	if !hasReason(applied.Result, domrestore.ReasonBlobCorrupt) {
		t.Errorf("reasons = %v, want blob-corrupt", applied.Result.Reasons())
	}
	// The token says what kind of problem this is; only the path says which file to
	// look at. A refusal discovered while reading has no blocked sample behind it, so
	// the failing operation has to carry the path itself or the report is unactionable.
	requireFailure(t, applied.Result, "data.txt", domrestore.ReasonBlobCorrupt)
	if got := h.read("data.txt"); got != "changed" {
		t.Errorf("a corrupt-blob refusal mutated the worktree: %q", got)
	}
	// And nothing durable was published: a refusal happens before the recovery
	// observation, so there is no record claiming an undo that never applies.
	findings, err := h.recov.List(context.Background())
	if err != nil {
		t.Fatalf("list recovery observations: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("a refused apply published %d recovery observation(s)", len(findings))
	}
}

func TestSelectingAPathOutsideEvidenceIsExplainedNotSilentlyEmpty(t *testing.T) {
	h := setup(t)
	h.write("src/service.go", "package src\n")
	id := h.checkpointNow(h.cfg)

	// node_modules is a baseline-excluded directory: it exists on disk but is
	// outside awa's evidence. Selecting it must say so, never report "nothing to
	// do", which reads exactly like "this path is already correct".
	h.write("node_modules/pkg/index.js", "module.exports = {}\n")

	res := h.run(t, id.String(), h.selection("node_modules"), false)
	if res.Result.Counts().Blocked != 1 {
		t.Fatalf("counts = %+v, want one blocked operation explaining the boundary", res.Result.Counts())
	}
	// A count alone is not an explanation. The path and the closed reason have to
	// reach the result, because that is all a human report or a JSON consumer has to
	// work from — "1 blocked" without "node_modules: ignored-boundary" leaves the
	// reader exactly where the silent-empty answer would have.
	failures := res.Result.Failures()
	if len(failures) != 1 {
		t.Fatalf("failures = %+v, want the blocked path named", failures)
	}
	if failures[0].Path.String() != "node_modules" {
		t.Errorf("failure path = %q, want node_modules", failures[0].Path)
	}
	if len(failures[0].Reasons) != 1 || failures[0].Reasons[0] != domrestore.ReasonIgnoredBoundary {
		t.Errorf("failure reasons = %v, want ignored-boundary", failures[0].Reasons)
	}
}

// TestSelectingAPathOutsideAProvenScopeIsExplained covers the recovery
// observation's own boundary. Such a record proves exactly the paths one restore
// was going to change; for anything else it holds no evidence at all. Reporting
// that as an empty plan would read as "your path already matches this source",
// which is the opposite of the truth and would let a user believe an undo covered
// work it never touched.
//
// The two negative cases matter as much as the positive one: a directory operand
// whose children are only partly in scope must still restore what IS proven, and
// --all must keep meaning "everything this source proves" rather than refusing over
// every path outside it.
func TestSelectingAPathOutsideAProvenScopeIsExplained(t *testing.T) {
	h := setup(t)
	h.write("gen/a.txt", "a1")
	h.write("gen/b.txt", "b1")
	h.write("other.txt", "o1")
	id := h.checkpointNow(h.cfg)
	h.write("gen/a.txt", "a2")
	h.write("gen/b.txt", "b2")
	h.write("other.txt", "o2")

	// The recovery observation this publishes covers gen/a.txt and nothing else.
	applied := h.run(t, id.String(), h.selection("gen/a.txt"), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("setup restore outcome = %s (%v)", got, applied.Result.Reasons())
	}
	undo := applied.Result.Recovery().BeforeRef()

	t.Run("a path the record never covered", func(t *testing.T) {
		res := h.run(t, undo, h.selection("other.txt"), false)
		if res.Result.Counts().Blocked != 1 || res.Result.Counts().Mutating() != 0 {
			t.Fatalf("counts = %+v, want one blocked operation and no work", res.Result.Counts())
		}
		failures := res.Result.Failures()
		if len(failures) != 1 || failures[0].Path.String() != "other.txt" {
			t.Fatalf("failures = %+v, want other.txt named", failures)
		}
		if len(failures[0].Reasons) != 1 || failures[0].Reasons[0] != domrestore.ReasonOutOfProvenScope {
			t.Errorf("reasons = %v, want out-of-proven-scope", failures[0].Reasons)
		}
	})

	t.Run("a directory only partly inside the scope", func(t *testing.T) {
		res := h.run(t, undo, h.selection("gen"), false)
		if res.Result.Counts().Blocked != 0 {
			t.Fatalf("counts = %+v, want no block: the operand did match proven evidence", res.Result.Counts())
		}
		if res.Result.Counts().Replace != 1 {
			t.Errorf("counts = %+v, want the one proven path still restorable", res.Result.Counts())
		}
	})

	t.Run("--all stays everything the source proves", func(t *testing.T) {
		res := h.run(t, undo, h.selection(), false)
		if res.Result.Counts().Blocked != 0 || res.Result.Counts().Replace != 1 {
			t.Errorf("counts = %+v, want exactly the proven scope with nothing blocked", res.Result.Counts())
		}
	})
}

func TestRestoreRefusesTheCurrentWorktreeAsASource(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "x")
	h.checkpointNow(h.cfg)
	_, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref("now"), Selection: h.selection("a.txt"),
	})
	if err == nil {
		t.Fatal("\"now\" was accepted as a restore source")
	}
}

func TestRunRequiresANormalizedSelection(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "x")
	id := h.checkpointNow(h.cfg)
	_, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
	})
	if err == nil {
		t.Fatal("an unbuilt selection was accepted")
	}
}

// --- source resolution ----------------------------------------------------

func TestSourceIsResolvedOnceEvenWhenLatestMoves(t *testing.T) {
	h := setup(t)
	h.write("data.txt", "first")
	first := h.checkpointNow(h.cfg)
	h.write("data.txt", "second")
	second := h.checkpointNow(h.cfg)
	if first == second {
		t.Fatal("fixture did not record two checkpoints")
	}
	h.write("data.txt", "dirty")

	res := h.run(t, "latest", h.selection("data.txt"), false)
	// The preview must publish the full immutable id it resolved, never the moving
	// token, so a later apply acts on the state the preview described.
	if got := res.Result.Source().ID(); got != second.String() {
		t.Errorf("resolved source id = %q, want %q", got, second)
	}
	if got := res.Result.Source().Requested(); got != "latest" {
		t.Errorf("requested expression = %q, want latest", got)
	}
}

func TestAllSelectionCoversTheWholeProvenScope(t *testing.T) {
	h := setup(t)
	h.write("a/one.txt", "1")
	h.write("b/two.txt", "2")
	id := h.checkpointNow(h.cfg)
	h.write("a/one.txt", "changed")
	h.remove("b/two.txt")
	h.write("c/three.txt", "new")

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v)", got, res.Result.Reasons())
	}
	if got := h.read("a/one.txt"); got != "1" {
		t.Errorf("a/one.txt = %q, want 1", got)
	}
	if got := h.read("b/two.txt"); got != "2" {
		t.Errorf("b/two.txt = %q, want 2", got)
	}
	if h.exists("c/three.txt") {
		t.Error("--all did not remove the added file the source proves absent")
	}
}

func TestApplyIsANoOpWhenTheWorktreeAlreadyMatches(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "same")
	id := h.checkpointNow(h.cfg)

	res := h.run(t, id.String(), h.selection("a.txt"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeNoOp {
		t.Fatalf("outcome = %s, want no-op", got)
	}
	if !res.Result.Recovery().IsZero() {
		t.Error("a no-op published a recovery observation for a mutation that never happened")
	}
	findings, err := h.recov.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("a no-op published %d recovery observation(s)", len(findings))
	}
}

func TestRerunAfterAPartialApplyConverges(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one-changed")
	h.write("b.txt", "two-changed")

	// Restore only one path, then restore the rest: the second invocation re-plans
	// from current reality and does only the remaining work.
	first := h.run(t, id.String(), h.selection("a.txt"), true)
	if first.Result.Completed() != 1 {
		t.Fatalf("first apply completed %d, want 1", first.Result.Completed())
	}
	second := h.run(t, id.String(), h.selection(), true)
	if second.Result.Completed() != 1 {
		t.Fatalf("second apply completed %d, want 1 (only the remaining path)", second.Result.Completed())
	}
	if h.read("a.txt") != "one" || h.read("b.txt") != "two" {
		t.Errorf("convergence failed: a=%q b=%q", h.read("a.txt"), h.read("b.txt"))
	}
	// A third run has nothing left to do.
	third := h.run(t, id.String(), h.selection(), true)
	if got := third.Result.Outcome(); got != domrestore.OutcomeNoOp {
		t.Errorf("third apply outcome = %s, want no-op", got)
	}
}

// --- helpers --------------------------------------------------------------

// recoveryRecords counts the recovery observations the store holds. It reads the
// store rather than a field of the result, so it can catch a durable record the
// result never mentioned — which is the whole failure mode it exists for.
func (h *harness) recoveryRecords(t *testing.T) int {
	t.Helper()
	found, err := h.recov.List(context.Background())
	if err != nil {
		t.Fatalf("listing recovery observations: %v", err)
	}
	return len(found)
}

// requireFailure asserts the result names one bounded per-path fact: the path, and
// the reason it stopped there.
func requireFailure(t *testing.T, res domrestore.ApplyResult, path string, reason domrestore.Reason) {
	t.Helper()
	for _, f := range res.Failures() {
		if f.Path.String() != path {
			continue
		}
		for _, r := range f.Reasons {
			if r == reason {
				return
			}
		}
	}
	t.Errorf("failures = %+v, want %s: %s", res.Failures(), path, reason)
}

func hasReason(res domrestore.ApplyResult, want domrestore.Reason) bool {
	for _, r := range res.Reasons() {
		if r == want {
			return true
		}
	}
	return false
}

// snapshotTree renders every worktree path with its content and mode, excluding
// awa's own state directory. It is an oracle independent of any production field:
// a preview that wrote anything shows up as a different string.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if rel == paths.Dir || strings.HasPrefix(rel, paths.Dir+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		b.WriteString(filepath.ToSlash(rel))
		b.WriteString(" ")
		b.WriteString(info.Mode().String())
		if info.Mode().IsRegular() {
			data, rerr := os.ReadFile(p) //nolint:gosec // test fixture path
			if rerr != nil {
				return rerr
			}
			b.WriteString(" ")
			b.Write(data)
		}
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return b.String()
}

// corruptEveryBlob rewrites every stored blob's bytes while leaving it at its
// content address, so only a read can detect the lie.
func corruptEveryBlob(t *testing.T, layout paths.Layout) {
	t.Helper()
	count := 0
	err := filepath.Walk(layout.BlobsDir(), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if cerr := os.Chmod(p, 0o600); cerr != nil {
			return cerr
		}
		if werr := os.WriteFile(p, bytes.Repeat([]byte("Z"), 16), 0o600); werr != nil {
			return werr
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("corrupt blobs: %v", err)
	}
	if count == 0 {
		t.Fatal("fixture stored no blobs to corrupt")
	}
}
