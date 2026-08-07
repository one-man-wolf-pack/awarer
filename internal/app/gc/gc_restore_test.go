package gc

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/config"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	restoredom "awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/restorestore"
	"awarer/internal/scantest"
)

// pathScope is a test-local CoveredScopeStream over a small, ordered path slice. It
// is the fixture side of the port: production scope streams are read from a durable
// record or derived from an operation set, and neither shape belongs in a test that
// only needs to name two paths. It re-opens a fresh cursor per Open, like every real
// stream, and refuses a misordered fixture so a broken test input fails at
// construction instead of producing a scope the reader would later reject.
type pathScope struct{ paths []worktree.RelPath }

func newPathScope(t *testing.T, paths ...worktree.RelPath) pathScope {
	t.Helper()
	for i, p := range paths {
		if p.IsZero() {
			t.Fatalf("covered scope entry %d has no path", i)
		}
		if i > 0 && !paths[i-1].Less(p) {
			t.Fatalf("covered scope fixture is not in strictly ascending canonical order at %q", p)
		}
	}
	return pathScope{paths: paths}
}

func (s pathScope) Open(context.Context) (restoredom.CoveredScopeCursor, error) {
	return &pathScopeCursor{paths: s.paths}, nil
}

type pathScopeCursor struct {
	paths []worktree.RelPath
	i     int
	cur   worktree.RelPath
}

func (c *pathScopeCursor) Next() bool {
	if c.i >= len(c.paths) {
		return false
	}
	c.cur = c.paths[c.i]
	c.i++
	return true
}

func (c *pathScopeCursor) Path() worktree.RelPath { return c.cur }
func (c *pathScopeCursor) Err() error             { return nil }
func (c *pathScopeCursor) Close() error           { return nil }

// Restore recovery observations are the one kind of local evidence whose loss is
// irreversible in a way the others' is not: a reclaimed checkpoint can be recorded
// again from the worktree, but a reclaimed recovery observation permanently removes
// the ability to undo the restore that produced it. These tests pin the policy that
// protects it — its own window, its exclusion from subsystem filters, its immunity
// to git classification, and its role as a blob-reachability root.

// publishRecovery writes one recovery observation whose manifest references a real
// stored blob, so the reachability assertions below have something to protect. It
// returns the operation id and the blob's content hash.
func (e *env) publishRecovery(t *testing.T, createdAt time.Time, seed byte, content string) (restoredom.OperationID, hashing.ContentHash) {
	t.Helper()
	layout := paths.New(e.root)
	hasher := blake3hash.New()
	blobs := blobstore.New(layout, hasher)
	repo := restorestore.New(layout, hasher)

	hash, err := hasher.HashReader(strings.NewReader(content))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, _, err := blobs.Materialize(hash, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	}); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	rel, err := worktree.ParseRelPath("captured/file.txt")
	if err != nil {
		t.Fatalf("rel path: %v", err)
	}
	entry, err := worktree.NewRegularEntry(rel, hash, worktree.StorageBlob,
		worktree.StatSignature{Size: int64(len(content)), Mode: 0o644}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	scope := newPathScope(t, rel)

	id, err := restoredom.NewOperationID(createdAt.UnixNano(), bytes.NewReader([]byte{seed, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		t.Fatalf("operation id: %v", err)
	}
	src, err := restoredom.NewSource(restoredom.SourceCheckpoint, "gmqd3dbpvs42abcd", "gmqd3dbpvs42abcd", "latest")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	sel, err := restoredom.NewPathSelection([]worktree.RelPath{rel})
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	tree, err := hashing.ParseTreeHash("blake3:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("tree hash: %v", err)
	}
	if _, err := repo.Publish(context.Background(), restoredom.RecoveryBuild{
		ID:             id,
		CreatedAt:      createdAt,
		AwaVersion:     "test",
		ScanConfigHash: hashing.ConfigHashFromTree(tree),
		Source:         src,
		Selection:      sel,
	}, scantest.CanonicalStream([]worktree.Entry{entry}, nil), scope); err != nil {
		t.Fatalf("publish recovery observation: %v", err)
	}
	return id, hash
}

// candidatesOfKind returns every candidate of a kind, for assertions that care about
// the decision rather than the whole plan.
func candidatesOfKind(pl gcdom.GCPlan, kind gcdom.CandidateKind) []gcdom.GCCandidate {
	var out []gcdom.GCCandidate
	for _, c := range pl.Candidates() {
		if c.Kind() == kind {
			out = append(out, c)
		}
	}
	return out
}

func reasonForBlob(pl gcdom.GCPlan, h hashing.ContentHash) (gcdom.RetentionReason, bool) {
	for _, c := range pl.Candidates() {
		if c.Kind() == gcdom.KindBlob && c.ID() == h.String() {
			return c.Reason(), true
		}
	}
	return "", false
}

func TestRecoveryObservationUsesItsOwnRetentionWindow(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name           string
		age            time.Duration
		keepRestores   time.Duration
		keepRuns       time.Duration
		wantReason     gcdom.RetentionReason
		explainsPolicy string
	}{
		{
			name: "inside the default window", age: 24 * time.Hour,
			keepRestores: 14 * 24 * time.Hour, keepRuns: 14 * 24 * time.Hour,
			wantReason: gcdom.ReasonRestoreTooRecent,
		},
		{
			name: "past the default window", age: 20 * 24 * time.Hour,
			keepRestores: 14 * 24 * time.Hour, keepRuns: 14 * 24 * time.Hour,
			wantReason: gcdom.ReasonRestoreExpired,
		},
		{
			// The whole point of the independent key: a project that keeps almost no run
			// history still keeps its undo evidence.
			name: "run history expired but restore evidence is not", age: 10 * 24 * time.Hour,
			keepRestores: 30 * 24 * time.Hour, keepRuns: time.Hour,
			wantReason:     gcdom.ReasonRestoreTooRecent,
			explainsPolicy: "keep_runs_for must not govern recovery evidence",
		},
		{
			// And the reverse: a long run history does not extend the undo window.
			name: "restore evidence expired but run history is not", age: 10 * 24 * time.Hour,
			keepRestores: time.Hour, keepRuns: 365 * 24 * time.Hour,
			wantReason:     gcdom.ReasonRestoreExpired,
			explainsPolicy: "keep_runs_for must not extend recovery evidence",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.GC.KeepRestoresFor = config.Duration(tc.keepRestores)
			e.cfg.GC.KeepRunsFor = config.Duration(tc.keepRuns)
			id, _ := e.publishRecovery(t, now.Add(-tc.age), 1, "captured bytes")

			pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false))
			cands := candidatesOfKind(pl, gcdom.KindRestore)
			if len(cands) != 1 {
				t.Fatalf("plan has %d restore candidate(s), want 1: %+v", len(cands), cands)
			}
			if got := cands[0].Reason(); got != tc.wantReason {
				t.Errorf("reason = %q, want %q (%s)", got, tc.wantReason, tc.explainsPolicy)
			}
			if cands[0].ID() != id.String() {
				t.Errorf("candidate id = %q, want %q", cands[0].ID(), id)
			}
		})
	}
}

func TestExplicitOlderThanOverridesTheRestoreWindow(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	e.cfg.GC.KeepRestoresFor = config.Duration(365 * 24 * time.Hour)
	e.publishRecovery(t, now.Add(-48*time.Hour), 1, "captured bytes")

	// Configured window says keep; the explicit override says otherwise for this
	// invocation only.
	pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, time.Hour, false))
	cands := candidatesOfKind(pl, gcdom.KindRestore)
	if len(cands) != 1 || cands[0].Reason() != gcdom.ReasonRestoreExpired {
		t.Fatalf("candidates = %+v, want one restore-expired", cands)
	}
}

func TestSubsystemFiltersExcludeRecoveryObservations(t *testing.T) {
	now := time.Now()
	for _, filter := range []gcdom.SubsystemFilter{gcdom.FilterRunsOnly, gcdom.FilterCheckpointsOnly, gcdom.FilterBlobsOnly} {
		t.Run(filter.String(), func(t *testing.T) {
			e := newEnv(t)
			e.cfg.GC.KeepRestoresFor = config.Duration(time.Hour)
			_, hash := e.publishRecovery(t, now.Add(-48*time.Hour), 1, "captured bytes")

			pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, filter, gcdom.KeepLastDefault, 0, false))
			cands := candidatesOfKind(pl, gcdom.KindRestore)
			if len(cands) != 1 {
				t.Fatalf("plan has %d restore candidate(s), want 1", len(cands))
			}
			if got := cands[0].Reason(); got != gcdom.ReasonSubsystemFiltered {
				t.Errorf("reason under %s = %q, want subsystem-filtered", filter, got)
			}
			if cands[0].Action() == gcdom.ActionDelete {
				t.Errorf("a %s filter planned to delete a recovery observation", filter)
			}
			// A skipped record is still a reachability root: the same invocation must
			// not sweep the bytes it would need.
			if r, ok := reasonForBlob(pl, hash); !ok || r != gcdom.ReasonBlobReferenced {
				t.Errorf("blob reason = %q (found=%v), want blob-referenced", r, ok)
			}
		})
	}
}

func TestRetainedRecoveryObservationProtectsItsBlobs(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	e.cfg.GC.KeepRestoresFor = config.Duration(14 * 24 * time.Hour)
	_, hash := e.publishRecovery(t, now.Add(-time.Hour), 1, "the bytes an undo needs")

	pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false))
	reason, ok := reasonForBlob(pl, hash)
	if !ok {
		t.Fatalf("the recovery observation's blob is not in the plan at all")
	}
	if reason != gcdom.ReasonBlobReferenced {
		t.Fatalf("blob reason = %q, want blob-referenced: sweeping it would make an applied restore irreversible", reason)
	}
}

func TestExpiredRecoveryObservationReleasesItsBlobs(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	e.cfg.GC.KeepRestoresFor = config.Duration(time.Hour)
	id, hash := e.publishRecovery(t, now.Add(-48*time.Hour), 1, "no longer needed")

	pl := e.plan(t, now, fakeGit{}, mustRequest(t, false, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false))
	if reason, ok := reasonForBlob(pl, hash); !ok || reason != gcdom.ReasonBlobUnreachable {
		t.Fatalf("blob reason = %q (found=%v), want blob-unreachable once the record expires", reason, ok)
	}

	res := e.execute(t, now, fakeGit{}, pl)
	for _, d := range res.Deletions() {
		if d.Err() != nil {
			t.Fatalf("deleting %s %s failed: %v", d.Candidate().Kind(), d.Candidate().ID(), d.Err())
		}
	}
	// The oracle is the filesystem: the record directory is gone.
	if _, err := os.Stat(filepath.Join(paths.New(e.root).RestoresDir(), id.String())); !os.IsNotExist(err) {
		t.Errorf("the expired recovery observation survived collection: %v", err)
	}
}

func TestCommittedDoesNotClassifyRecoveryObservations(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	// Well inside its window. A commit advancing says nothing about whether the
	// pre-restore state of an uncommitted worktree is disposable, so --committed must
	// not turn this into a deletion.
	e.cfg.GC.KeepRestoresFor = config.Duration(30 * 24 * time.Hour)
	e.publishRecovery(t, now.Add(-24*time.Hour), 1, "captured bytes")

	git := fakeGit{info: gitmeta.CommitInfo{Committed: now, Branch: "main"}, ok: true}
	pl := e.plan(t, now, git, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, true))
	cands := candidatesOfKind(pl, gcdom.KindRestore)
	if len(cands) != 1 {
		t.Fatalf("plan has %d restore candidate(s), want 1", len(cands))
	}
	if cands[0].Action() == gcdom.ActionDelete {
		t.Fatalf("--committed reclaimed a recovery observation inside its window (reason %q)", cands[0].Reason())
	}
	if cands[0].Reason() != gcdom.ReasonRestoreTooRecent {
		t.Errorf("reason = %q, want restore-too-recent: the age rule is the only rule that applies", cands[0].Reason())
	}
}

// TestCommittedWithoutGitRetainsAnExpiredRecoveryObservation covers the other half
// of the --committed rule: the observation is past its own window, so the age rule
// alone would reclaim it, but the invocation as a whole plans no deletions because
// git could not anchor a cutoff.
//
// The reason token is the point. This is the same invocation-wide fact checkpoints,
// runs, and temp report as git-unavailable, and reporting it as restore-too-recent
// would publish a retention claim that is simply false — the record IS past its
// window. Nothing else pins that, so a regression to the friendlier-looking token
// would otherwise be invisible.
func TestCommittedWithoutGitRetainsAnExpiredRecoveryObservation(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	e.cfg.GC.KeepRestoresFor = config.Duration(time.Hour)
	_, hash := e.publishRecovery(t, now.Add(-48*time.Hour), 1, "captured bytes")

	// --committed requested, but git cannot anchor a cutoff (fakeGit's zero value).
	pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, true))
	cands := candidatesOfKind(pl, gcdom.KindRestore)
	if len(cands) != 1 {
		t.Fatalf("plan has %d restore candidate(s), want 1", len(cands))
	}
	if cands[0].Action() != gcdom.ActionRetain {
		t.Fatalf("an expired observation is %s on a pass that deletes nothing, want retained", cands[0].Action())
	}
	if cands[0].Reason() != gcdom.ReasonGitUnavailable {
		t.Errorf("reason = %q, want git-unavailable: the record is past its window, so too-recent would be untrue", cands[0].Reason())
	}
	// Retained means retained all the way down: its captured bytes stay reachable.
	if reason, ok := reasonForBlob(pl, hash); !ok || reason != gcdom.ReasonBlobReferenced {
		t.Errorf("captured blob reason = %q (found=%v), want blob-referenced", reason, ok)
	}
}

func TestUnreadableRecoveryObservationBlocksTheBlobSweep(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	e.cfg.GC.KeepRestoresFor = config.Duration(14 * 24 * time.Hour)
	id, _ := e.publishRecovery(t, now.Add(-time.Hour), 1, "captured bytes")

	// Damage the record's metadata: GC can see that a record exists but cannot prove
	// what it references, so it must not conclude anything is unreachable.
	metaPath := filepath.Join(paths.New(e.root).RestoresDir(), id.String(), "meta.json")
	if err := os.Chmod(metaPath, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(metaPath, []byte("{ this is not a record }"), 0o600); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}

	pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false))
	cands := candidatesOfKind(pl, gcdom.KindRestore)
	if len(cands) != 1 || cands[0].Reason() != gcdom.ReasonRestoreCorrupt {
		t.Fatalf("candidates = %+v, want one restore-corrupt", cands)
	}
	if cands[0].Action() != gcdom.ActionBlocked {
		t.Errorf("an unreadable recovery observation is %s, want blocked", cands[0].Action())
	}
	// Fail closed: no blob may be planned for deletion while reachability is unknown.
	for _, c := range pl.Candidates() {
		if c.Kind() == gcdom.KindBlob && c.Action() == gcdom.ActionDelete {
			t.Errorf("blob %s was planned for deletion despite an unreadable recovery observation", c.ID())
		}
	}
	if !pl.Blocked() {
		t.Error("the plan does not report itself blocked")
	}
}

func TestRestoreSpoolTempIsClassifiedByTheExistingTempRules(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	// An abandoned restore spool under the restore store's temp area must be
	// reclaimable through the ordinary age-based temp classification, with no new
	// sweeping logic and no risk to a real record.
	tmpDir := filepath.Join(paths.New(e.root).RestoresDir(), "tmp")
	if err := os.MkdirAll(tmpDir, paths.DirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := filepath.Join(tmpDir, ".record-abandoned")
	if err := os.MkdirAll(stale, paths.DirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := now.Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false))
	found := false
	for _, c := range pl.Candidates() {
		if c.Kind() == gcdom.KindTemp && strings.Contains(c.Path(), ".record-abandoned") {
			found = true
			if c.Reason() != gcdom.ReasonTempStale {
				t.Errorf("abandoned restore spool reason = %q, want temp-stale", c.Reason())
			}
		}
	}
	if !found {
		t.Error("an abandoned restore spool was not classified as a temp artifact")
	}
}

func TestPlanRestoresSurvivesAProjectThatNeverRestored(t *testing.T) {
	now := time.Now()
	e := newEnv(t)
	pl := e.plan(t, now, fakeGit{}, mustRequest(t, true, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false))
	if cands := candidatesOfKind(pl, gcdom.KindRestore); len(cands) != 0 {
		t.Errorf("a project with no restore store produced %d restore candidate(s): %+v", len(cands), cands)
	}
	if pl.Blocked() {
		t.Error("an absent restore store blocked the plan; it is a clean empty store")
	}
}

// mustRequest builds a GC request or fails the test.
func mustRequest(t *testing.T, dryRun bool, filter gcdom.SubsystemFilter, keepLast int, olderThan time.Duration, committed bool) gcdom.GCRequest {
	t.Helper()
	req, err := gcdom.NewGCRequest(dryRun, filter, keepLast, olderThan, committed)
	if err != nil {
		t.Fatalf("NewGCRequest: %v", err)
	}
	return req
}
