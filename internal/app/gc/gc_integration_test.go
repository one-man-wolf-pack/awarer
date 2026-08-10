package gc

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/checkpointcmd"
	"awarer/internal/app/initcmd"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

var errGitBoom = errors.New("git boom")

// fakeGit lets the committed policy be driven without a real repository.
type fakeGit struct {
	info gitmeta.CommitInfo
	ok   bool
	err  error
}

func (f fakeGit) LatestCommit(context.Context) (gitmeta.CommitInfo, bool, error) {
	return f.info, f.ok, f.err
}

type fakeCapture struct{}

func (fakeCapture) Capture(context.Context) (*checkpoint.GitMetadata, bool, error) {
	return nil, false, nil
}

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

// writeFiles writes the given worktree files, creating parent dirs as needed.
func (e *env) writeFiles(t testing.TB, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(e.root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) removeFile(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(e.root, filepath.FromSlash(name))); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// checkpointAt records a checkpoint of the current worktree at the given timestamp,
// returning its header. The timestamp drives CreatedAt so latest/keep-last ordering
// is deterministic.
func (e *env) checkpointAt(t testing.TB, ts time.Time) checkpoint.CheckpointHeader {
	t.Helper()
	layout, _ := e.project.Paths()
	hasher := blake3hash.New()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	defer func() { _ = idx.Close() }()

	svc := checkpointcmd.New(checkpointcmd.Deps{
		Scanner:     scanner.New(worktreefs.New(), hasher, idx),
		Index:       idx,
		Hasher:      hasher,
		Blobs:       blobstore.New(layout, hasher),
		Checkpoints: checkpointjson.NewRepo(layout),
		Git:         fakeCapture{},
		Content:     worktreefs.NewContentReader(layout.Root()),
		Now:         func() time.Time { return ts },
		Rand:        rand.Reader,
		Version:     "test",
	})
	res, err := svc.Run(context.Background(), checkpointcmd.Request{Project: e.project, Config: e.cfg, CommandCwd: e.root})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return res.Header
}

// seedRun publishes a run-cache entry finishing at the given time, discriminated so
// distinct runs get distinct keys.
func (e *env) seedRun(t testing.TB, disc string, finishedAt time.Time) runcache.RunID {
	t.Helper()
	layout, _ := e.project.Paths()
	hasher := blake3hash.New()
	r := runstore.New(layout, hasher)
	pending, err := r.Begin(runcache.CaptureLimits{MaxStdout: 1 << 20, MaxStderr: 1 << 20})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_, _ = io.WriteString(pending.Stdout(), "out-"+disc)
	_, _ = io.WriteString(pending.Stderr(), "err-"+disc)
	so, se, err := pending.Finalize()
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	id, err := runcache.NewRunID(finishedAt.UnixNano(), strings.NewReader("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewRunID: %v", err)
	}
	cwd, _ := runcache.NewExecutionCWD(".")
	keyInput := runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         "test",
		InvocationMode:     runcache.InvocationArgv,
		Command:            runcache.Command{Argv: []string{"echo", disc}, RawExecutable: "echo"},
		CWD:                cwd,
		InputTreeHash:      gcEmptyTreeHash(hasher),
		Effect:             gcObservedEffect(hasher),
		IncludeScope:       []string{"."},
		TrustMode:          config.TrustNormal,
		RunConfigHash:      hashing.ConfigHashFromTree(hasher.HashBytes([]byte("cfg"))),
		Env:                runcache.NewEnvironment(nil),
		Platform:           runcache.Platform{GOOS: "linux", GOARCH: "amd64"},
		StdinMode:          runcache.StdinNull,
	}
	entry := runcache.RunEntry{
		ID:          id,
		Key:         keyInput.Compute(hasher),
		KeyInput:    keyInput,
		StartedAt:   finishedAt.Add(-time.Second),
		FinishedAt:  finishedAt,
		Exit:        runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Stdout:      so,
		Stderr:      se,
		Decision:    runcache.CacheDecision{Cacheable: true},
		Reuse:       runcache.ReusableCacheEntry(),
		Mutation:    gcUnchanged(),
		EffectGuard: gcUnchangedEffect(),
	}
	if err := pending.Commit(context.Background(), entry, gcUnchangedObs()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return id
}

func gcEmptyTreeHash(h hashing.Hasher) hashing.TreeHash {
	red, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(nil, nil))
	if err != nil {
		panic(err)
	}
	return red.Hash
}

func gcTestScanCfg() hashing.ConfigHash {
	ch, err := hashing.ParseConfigHash("blake3:" + strings.Repeat("a", 64))
	if err != nil {
		panic(err)
	}
	return ch
}

func gcUnchangedObs() runcache.RunObservations {
	cfg := gcTestScanCfg()
	return runcache.RunObservations{
		Before:               scantest.CanonicalStream(nil, nil),
		After:                scantest.CanonicalStream(nil, nil),
		BeforeScanConfigHash: cfg,
		AfterScanConfigHash:  cfg,
	}
}

func gcUnchangedEffect() runcache.EffectGuardStatus {
	g, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnchanged)
	if err != nil {
		panic(err)
	}
	return g
}

// gcObservedEffect is the effect identity every execution keys on: production always
// observes the non-empty built-in watch set.
func gcObservedEffect(h hashing.Hasher) runcache.EffectObservation {
	o, err := runcache.ObservedEffect(runcache.EffectHashFromTree(h.HashBytes([]byte("effect"))), 1)
	if err != nil {
		panic(err)
	}
	return o
}

func gcUnchanged() runcache.MutationStatus {
	st, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		panic(err)
	}
	return st
}

// plan builds a plan with an injected clock and (optional) git provider.
func (e *env) plan(t testing.TB, now time.Time, git GitProvider, req gcdom.GCRequest) gcdom.GCPlan {
	t.Helper()
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return false },
		Git:          git,
	})
	pl, err := svc.Plan(context.Background(), req, Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return pl
}

func (e *env) execute(t *testing.T, now time.Time, git GitProvider, pl gcdom.GCPlan) gcdom.GCResult {
	t.Helper()
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return false },
		Git:          git,
	})
	res, err := runPlanUnderLease(svc, Request{Project: e.project, Config: e.cfg}, pl)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return res
}

// runPlanUnderLease drives deletion of a caller-supplied plan under the collector
// lease — the composition Collect performs internally (acquire the lease, then
// delete under it). It exists only in tests: production code never executes an
// externally built plan, so this is where the stale-plan hazard, the lease timeout,
// and the writer recheck are exercised. Wrapping pl as authoritativePlan is a
// deliberate test-only act — mislabeling a possibly-stale plan authoritative is
// exactly the hazard the production invariant forbids. A dry-run or blocked plan
// deletes nothing and takes no lease, matching Collect.
func runPlanUnderLease(svc *Service, req Request, pl gcdom.GCPlan) (gcdom.GCResult, error) {
	if pl.DryRun() || pl.Blocked() {
		return gcdom.NewResult(pl, nil, pl.Warnings()), nil
	}
	r, layout, err := svc.open(req)
	if err != nil {
		return gcdom.GCResult{}, err
	}
	release, err := svc.acquireCollector(context.Background(), req, layout)
	if err != nil {
		return gcdom.GCResult{}, err
	}
	defer release()
	return svc.deleteAuthoritative(context.Background(), authoritativePlan{plan: pl}, r, layout)
}

func defaultReq(t *testing.T) gcdom.GCRequest {
	t.Helper()
	r, err := gcdom.NewGCRequest(false, gcdom.FilterAll, gcdom.KeepLastDefault, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func reqKeepLast(t *testing.T, n int) gcdom.GCRequest {
	t.Helper()
	r, err := gcdom.NewGCRequest(false, gcdom.FilterAll, n, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// candidatesOf returns the candidates of a given kind+action.
func candidatesOf(pl gcdom.GCPlan, kind gcdom.CandidateKind, action gcdom.CandidateAction) []gcdom.GCCandidate {
	var out []gcdom.GCCandidate
	for _, c := range pl.Candidates() {
		if c.Kind() == kind && c.Action() == action {
			out = append(out, c)
		}
	}
	return out
}

func TestCleanProjectIsNoOp(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())

	now := time.Unix(1_700_000_100, 0).UTC()
	pl := e.plan(t, now, nil, defaultReq(t))
	if s := pl.Summary(); s.PlannedDelete != 0 {
		t.Fatalf("clean project planned %d deletes, want 0; candidates=%+v", s.PlannedDelete, pl.Candidates())
	}
	if pl.Blocked() {
		t.Fatal("clean project should not be blocked")
	}
}

func TestExpiredCheckpointsDeletedLatestKept(t *testing.T) {
	e := newEnv(t)
	// Checkpoint 1: a.go + common.go (oldest).
	e.writeFiles(t, map[string]string{"a.go": "package a // unique", "common.go": "package common"})
	h1 := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	// Checkpoint 2: drop a.go, keep common.go, add b.go.
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // unique"})
	h2 := e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())
	// Checkpoint 3: newest.
	e.writeFiles(t, map[string]string{"c.go": "package c // unique"})
	h3 := e.checkpointAt(t, time.Unix(1_700_001_000, 0).UTC())

	now := time.Unix(1_700_002_000, 0).UTC()
	// keep-last 1 protects only the newest, h3, which is also latest. h1 and h2 are
	// both eligible; the shared blob under common.go must still survive because h2
	// references it right up until it is deleted.
	pl := e.plan(t, now, nil, reqKeepLast(t, 1))

	dels := candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionDelete)
	delIDs := map[string]bool{}
	for _, c := range dels {
		delIDs[c.ID()] = true
	}
	if !delIDs[h1.ID.String()] || !delIDs[h2.ID.String()] {
		t.Fatalf("want h1 and h2 deleted, got %+v", delIDs)
	}
	if delIDs[h3.ID.String()] {
		t.Fatal("latest checkpoint h3 must not be deleted")
	}

	res := e.execute(t, now, nil, pl)
	if es := res.ExecSummary(); es.Failed != 0 {
		t.Fatalf("execution had %d failures", es.Failed)
	}
	// h3 still resolvable; h1/h2 gone.
	layout, _ := e.project.Paths()
	checkpoints := checkpointjson.NewRepo(layout)
	if _, err := checkpoints.Header(h3.ID); err != nil {
		t.Fatalf("latest checkpoint should survive: %v", err)
	}
	for _, id := range []checkpoint.CheckpointID{h1.ID, h2.ID} {
		if _, err := checkpoints.Header(id); err == nil {
			t.Fatalf("checkpoint %s should be deleted", id.Short())
		}
	}
}

func TestUnreachableBlobSweptReferencedKept(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // unique-AAA", "common.go": "package common // shared"})
	h1 := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // unique-BBB"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	layout, _ := e.project.Paths()
	blobs := blobstore.New(layout, blake3hash.New())
	before, _ := blobs.List()
	if len(before) < 3 {
		t.Fatalf("expected at least 3 blobs (a, b, common), got %d", len(before))
	}

	now := time.Unix(1_700_002_000, 0).UTC()
	pl := e.plan(t, now, nil, reqKeepLast(t, 1)) // keep only latest → h1 deletable

	// Sweep should target a.go's blob (only in h1), not common.go's (still in h2).
	if pl.StateActionBlocked() || pl.LockBlocked() {
		t.Fatalf("plan should not be blocked")
	}
	delBlobs := candidatesOf(pl, gcdom.KindBlob, gcdom.ActionDelete)
	if len(delBlobs) != 1 {
		t.Fatalf("want exactly 1 unreachable blob, got %d: %+v", len(delBlobs), blobIDs(delBlobs))
	}

	res := e.execute(t, now, nil, pl)
	if es := res.ExecSummary(); es.Failed != 0 || es.Deleted == 0 {
		t.Fatalf("exec summary unexpected: %+v", es)
	}
	after, _ := blobs.List()
	if len(after) != len(before)-1 {
		t.Fatalf("blob count after = %d, want %d", len(after), len(before)-1)
	}
	// h1's checkpoint was beyond keep-last → also deleted, but the shared
	// blob must remain because h2 still references it.
	_ = h1
}

// TestIncompatibleCheckpointIsRetainedUnderItsOwnReason is the checkpoint half of
// "unknown means retain". A header declaring a schema this build cannot read blocks
// the sweep exactly as damage does — reachability is unprovable either way — but it
// carries its own reason, so a report never tells a user their store is broken when
// it is merely unreadable by this binary.
func TestIncompatibleCheckpointIsRetainedUnderItsOwnReason(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	h1 := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.writeFiles(t, map[string]string{"b.go": "package b"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	// Restamp h1 one version past the current schema, reading the number back out of
	// the record so the fixture carries no version literal.
	layout, _ := e.project.Paths()
	headerPath := filepath.Join(layout.CheckpointsDir(), h1.ID.String(), "header.json")
	data, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	current, err := strconv.Atoi(string(m["schema_version"]))
	if err != nil {
		t.Fatalf("header has no numeric schema_version: %v", err)
	}
	m["schema_version"] = json.RawMessage(strconv.Itoa(current + 1))
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(headerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_700_002_000, 0).UTC()
	pl := e.plan(t, now, nil, reqKeepLast(t, 1))

	blocked := candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionBlocked)
	if len(blocked) != 1 || blocked[0].ID() != h1.ID.String() || blocked[0].Reason() != gcdom.ReasonCheckpointIncompatible {
		t.Fatalf("want one checkpoint-incompatible block for %s, got %+v", h1.ID.Short(), reasonsOf(blocked))
	}
	// One record, one candidate: the blob-reachability pass must not re-report the
	// same refusal it already accounted for.
	if n := len(candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionBlocked)); n != 1 {
		t.Fatalf("one unreadable record produced %d blocked candidates", n)
	}
	if !pl.StateActionBlocked() {
		t.Fatal("an incompatible checkpoint must mark the plan as needing user action")
	}
	if s := candidatesOf(pl, gcdom.KindBlob, gcdom.ActionDelete); len(s) != 0 {
		t.Fatalf("an incompatible checkpoint must suppress the blob sweep, got %d swept blobs", len(s))
	}

	// The record survives: classifying is never reclaiming.
	if es := e.execute(t, now, nil, pl).ExecSummary(); es.Deleted != 0 {
		t.Fatalf("a blocked plan deleted %d records", es.Deleted)
	}
	if _, err := os.Stat(headerPath); err != nil {
		t.Fatalf("gc removed an incompatible checkpoint: %v", err)
	}
}

func TestCorruptCheckpointBlocksDeletion(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	h1 := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.writeFiles(t, map[string]string{"b.go": "package b"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	// Corrupt h1's header.
	layout, _ := e.project.Paths()
	headerPath := filepath.Join(layout.CheckpointsDir(), h1.ID.String(), "header.json")
	if err := os.Remove(headerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, []byte("{ broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_700_002_000, 0).UTC()
	pl := e.plan(t, now, nil, reqKeepLast(t, 1))
	if !pl.StateActionBlocked() {
		t.Fatalf("corrupt checkpoint should make the plan corruption-blocked; candidates=%+v", pl.Candidates())
	}
	if !pl.Blocked() {
		t.Fatal("plan should be blocked")
	}

	// Execute must delete nothing while blocked.
	blobs := blobstore.New(layout, blake3hash.New())
	before, _ := blobs.List()
	res := e.execute(t, now, nil, pl)
	if len(res.Deletions()) != 0 {
		t.Fatalf("blocked plan must perform no deletions, got %d", len(res.Deletions()))
	}
	after, _ := blobs.List()
	if len(after) != len(before) {
		t.Fatalf("blocked plan must not change blob count: before %d after %d", len(before), len(after))
	}
}

func TestPlanningWarningsReachResult(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())

	// --committed with git reporting a genuine failure: the planner records a warning
	// and retains everything; that warning must survive into the result.
	now := time.Unix(1_700_002_000, 0).UTC()
	req, err := gcdom.NewGCRequest(false, gcdom.FilterAll, gcdom.KeepLastDefault, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	git := fakeGit{err: errGitBoom}
	pl := e.plan(t, now, git, req)
	if len(pl.Warnings()) == 0 {
		t.Fatalf("expected a planning warning for the git failure")
	}
	res := e.execute(t, now, git, pl)
	if len(res.Warnings()) == 0 {
		t.Fatalf("planning warnings must survive into the result")
	}
}

func TestCorruptManifestCandidatesAreDeterministic(t *testing.T) {
	e := newEnv(t)
	// Three checkpoints; corrupt the manifests of two of them so markCheckpointBlobs fails
	// for both, emitting two corrupt candidates whose order must be stable.
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	h1 := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.writeFiles(t, map[string]string{"b.go": "package b"})
	h2 := e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())
	e.writeFiles(t, map[string]string{"c.go": "package c"})
	e.checkpointAt(t, time.Unix(1_700_001_000, 0).UTC()) // latest, readable

	layout, _ := e.project.Paths()
	for _, id := range []checkpoint.CheckpointID{h1.ID, h2.ID} {
		mf := filepath.Join(layout.CheckpointsDir(), id.String(), "manifest.jsonl")
		if err := os.Remove(mf); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mf, []byte("{ not a record\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Unix(1_700_002_000, 0).UTC()
	// Two plans of the same corrupt project must emit corrupt candidates in identical
	// order (the map iteration was the last nondeterministic source).
	first := corruptCheckpointIDs(e.plan(t, now, nil, defaultReq(t)))
	second := corruptCheckpointIDs(e.plan(t, now, nil, defaultReq(t)))
	if len(first) < 2 {
		t.Fatalf("expected at least 2 corrupt-manifest candidates, got %v", first)
	}
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Fatalf("corrupt candidate order is nondeterministic: %v vs %v", first, second)
	}
	if !sort.StringsAreSorted(first) {
		t.Fatalf("corrupt candidate ids are not sorted: %v", first)
	}
}

func corruptCheckpointIDs(pl gcdom.GCPlan) []string {
	var out []string
	for _, c := range pl.Candidates() {
		if c.Kind() == gcdom.KindCheckpoint && c.Reason() == gcdom.ReasonCheckpointCorrupt {
			out = append(out, c.ID())
		}
	}
	return out
}

func TestFailedDeletionIsRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	e := newEnv(t)
	layout, _ := e.project.Paths()
	stalePath := filepath.Join(layout.TmpDir(), "blob-stale")
	if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_100_000, 0).UTC()
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	pl := e.plan(t, now, nil, defaultReq(t))
	if d := candidatesOf(pl, gcdom.KindTemp, gcdom.ActionDelete); len(d) != 1 {
		t.Fatalf("expected one stale temp delete, got %d", len(d))
	}
	// Make the unlink fail: a read-only temp dir means RemoveTreeAt cannot remove the
	// child. The plan was already built (it only read the dir), so execution attempts
	// the delete and records the failure.
	if err := os.Chmod(layout.TmpDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(layout.TmpDir(), 0o755) })

	res := e.execute(t, now, nil, pl)
	if es := res.ExecSummary(); es.Failed != 1 || es.Deleted != 0 {
		t.Fatalf("expected one failed deletion, got %+v", es)
	}
	// The failed deletion is visible per-record, not swallowed.
	var sawErr bool
	for _, d := range res.Deletions() {
		if d.Err() != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("a failed deletion must carry a non-nil error")
	}
}

func TestStaleTempDeletedFreshKept(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	stalePath := filepath.Join(layout.TmpDir(), "blob-stale")
	freshPath := filepath.Join(layout.TmpDir(), "blob-fresh")
	if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshPath, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_100_000, 0).UTC()
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(freshPath, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	pl := e.plan(t, now, nil, defaultReq(t))
	stale := candidatesOf(pl, gcdom.KindTemp, gcdom.ActionDelete)
	if len(stale) != 1 || filepath.Base(stale[0].Path()) != "blob-stale" {
		t.Fatalf("want one stale temp delete (blob-stale), got %+v", tempPaths(stale))
	}
	res := e.execute(t, now, nil, pl)
	if es := res.ExecSummary(); es.Failed != 0 {
		t.Fatalf("temp delete failed: %+v", es)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale temp should be removed: %v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh temp must be kept: %v", err)
	}
}

func TestActiveLockBlocksDestructiveGC(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// Backdate a stale temp so there is real work to suppress.
	stalePath := filepath.Join(layout.TmpDir(), "blob-stale")
	if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_100_000, 0).UTC()
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	// An active lock: same host, alive PID.
	writeActiveLock(t, layout.LocksDir(), now)

	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return true }, // owner alive → active
	})
	pl, err := svc.Plan(context.Background(), defaultReq(t), Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !pl.LockBlocked() || pl.StateActionBlocked() {
		t.Fatalf("active lock should make the plan lock-blocked, not corruption-blocked")
	}
	res, err := runPlanUnderLease(svc, Request{Project: e.project, Config: e.cfg}, pl)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Deletions()) != 0 {
		t.Fatalf("lock-blocked plan must delete nothing")
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Fatalf("stale temp must remain under active lock: %v", err)
	}
}

func TestUnreadableLocksDirBlocksDestructiveGC(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	// A stale temp gives GC real work to suppress.
	stalePath := filepath.Join(layout.TmpDir(), "blob-stale")
	if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_100_000, 0).UTC()
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	// Replace .awa/locks with a symlink to elsewhere: ScanDir's no-follow read fails,
	// so GC cannot prove no writer holds a lock and must fail closed.
	if err := os.RemoveAll(layout.LocksDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), layout.LocksDir()); err != nil {
		t.Fatal(err)
	}

	pl := e.plan(t, now, nil, defaultReq(t))
	if !pl.LockBlocked() {
		t.Fatalf("an unreadable locks dir must lock-block the plan; candidates=%+v", pl.Candidates())
	}
	res := e.execute(t, now, nil, pl)
	if len(res.Deletions()) != 0 {
		t.Fatalf("lock-blocked plan must delete nothing, got %d", len(res.Deletions()))
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Fatalf("stale temp must remain when locks are unreadable: %v", err)
	}
}

func TestDryRunMatchesExecutePlanAndMutatesNothing(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.writeFiles(t, map[string]string{"b.go": "package b"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	now := time.Unix(1_700_002_000, 0).UTC()
	dry, err := gcdom.NewGCRequest(true, gcdom.FilterAll, 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	wet := reqKeepLast(t, 1)

	dryPlan := e.plan(t, now, nil, dry)
	wetPlan := e.plan(t, now, nil, wet)
	if dryPlan.Summary() != wetPlan.Summary() {
		t.Fatalf("dry-run and execute plans differ: %+v vs %+v", dryPlan.Summary(), wetPlan.Summary())
	}

	// Executing a dry-run plan mutates nothing.
	layout, _ := e.project.Paths()
	checkpoints := checkpointjson.NewRepo(layout)
	idsBefore, _ := checkpoints.ListIDs(context.Background())
	res := e.execute(t, now, nil, dryPlan)
	if len(res.Deletions()) != 0 {
		t.Fatalf("dry-run execute must not delete")
	}
	idsAfter, _ := checkpoints.ListIDs(context.Background())
	if len(idsAfter) != len(idsBefore) {
		t.Fatalf("dry-run must not change checkpoints: %d -> %d", len(idsBefore), len(idsAfter))
	}
}

func TestCommittedWithoutGitIsNoDeletePlan(t *testing.T) {
	e := newEnv(t)
	// Two checkpoints whose older blob would otherwise be unreachable, plus a stale temp:
	// under --committed with no git, NONE of these may be deleted.
	e.writeFiles(t, map[string]string{"a.go": "package a // unique", "common.go": "package common"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // unique"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	layout, _ := e.project.Paths()
	stalePath := filepath.Join(layout.TmpDir(), "blob-stale")
	if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_002_000, 0).UTC()
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	req, err := gcdom.NewGCRequest(false, gcdom.FilterAll, 1, 0, true) // committed + keep-last 1
	if err != nil {
		t.Fatal(err)
	}
	// git unavailable: provider returns ok=false.
	pl := e.plan(t, now, fakeGit{ok: false}, req)
	if pl.Summary().PlannedDelete != 0 {
		t.Fatalf("--committed without git must be a no-delete plan, got %d planned: %+v",
			pl.Summary().PlannedDelete, pl.Candidates())
	}
	if !hasWarning(pl.Warnings(), "--committed") {
		t.Fatalf("expected a --committed git-unavailable warning, got %+v", pl.Warnings())
	}
	res := e.execute(t, now, fakeGit{ok: false}, pl)
	if len(res.Deletions()) != 0 {
		t.Fatal("no-delete plan must perform no deletions")
	}
}

func TestCommittedDeletesOnlyPreCommitCheckpoints(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	old := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC()) // before commit
	e.writeFiles(t, map[string]string{"b.go": "package b"})
	e.checkpointAt(t, time.Unix(1_700_005_000, 0).UTC()) // after commit (and latest)

	commit := time.Unix(1_700_002_000, 0).UTC()
	now := time.Unix(1_700_009_000, 0).UTC()
	req, err := gcdom.NewGCRequest(false, gcdom.FilterAll, 1, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	git := fakeGit{ok: true, info: gitmeta.CommitInfo{Committed: commit, Branch: "main"}}
	pl := e.plan(t, now, git, req)

	dels := candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionDelete)
	if len(dels) != 1 || dels[0].ID() != old.ID.String() {
		t.Fatalf("only the pre-commit checkpoint should be eligible, got %+v", blobIDs(dels))
	}
}

func TestRunDeletionRemovesEntryAndPointer(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(1_700_100_000, 0).UTC()
	// Old run (past the 14d default window) and a fresh run.
	oldID := e.seedRun(t, "old", now.Add(-30*24*time.Hour))
	freshID := e.seedRun(t, "fresh", now.Add(-time.Hour))

	pl := e.plan(t, now, nil, defaultReq(t))
	dels := candidatesOf(pl, gcdom.KindRun, gcdom.ActionDelete)
	if len(dels) != 1 || dels[0].ID() != oldID.String() {
		t.Fatalf("only the old run should be eligible, got %+v", blobIDs(dels))
	}

	res := e.execute(t, now, nil, pl)
	if es := res.ExecSummary(); es.Failed != 0 || es.Deleted == 0 {
		t.Fatalf("run delete failed: %+v", es)
	}
	layout, _ := e.project.Paths()
	runs := runstore.New(layout, blake3hash.New())
	if _, err := runs.Get(oldID); err == nil {
		t.Fatal("old run should be deleted")
	}
	if _, err := runs.Get(freshID); err != nil {
		t.Fatalf("fresh run must survive: %v", err)
	}
	// The old run's key pointer must be gone too: no key-pointer corruption remains.
	if probs, err := runs.InspectKeyPointers(); err != nil || len(probs) != 0 {
		t.Fatalf("key pointers should be consistent after run delete: err=%v probs=%+v", err, probs)
	}
}

func TestOnlyFiltersDoNotDeleteTemp(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	stalePath := filepath.Join(layout.TmpDir(), "blob-stale")
	if err := os.WriteFile(stalePath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_100_000, 0).UTC()
	old := now.Add(-48 * time.Hour)
	_ = os.Chtimes(stalePath, old, old)

	for _, f := range []gcdom.SubsystemFilter{gcdom.FilterRunsOnly, gcdom.FilterCheckpointsOnly, gcdom.FilterBlobsOnly} {
		req, err := gcdom.NewGCRequest(false, f, gcdom.KeepLastDefault, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		pl := e.plan(t, now, nil, req)
		if d := candidatesOf(pl, gcdom.KindTemp, gcdom.ActionDelete); len(d) != 0 {
			t.Fatalf("%s must not delete temp, got %d", f, len(d))
		}
	}
	// A full GC, by contrast, does collect the stale temp.
	full := e.plan(t, now, nil, defaultReq(t))
	if d := candidatesOf(full, gcdom.KindTemp, gcdom.ActionDelete); len(d) != 1 {
		t.Fatalf("full gc should delete the stale temp, got %d", len(d))
	}
}

func TestCheckpointsOnlyDoesNotDeleteBlobs(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // unique-AAA", "common.go": "package common"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // unique-BBB"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	now := time.Unix(1_700_002_000, 0).UTC()
	req, err := gcdom.NewGCRequest(false, gcdom.FilterCheckpointsOnly, 1, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	pl := e.plan(t, now, nil, req)
	if d := candidatesOf(pl, gcdom.KindBlob, gcdom.ActionDelete); len(d) != 0 {
		t.Fatalf("checkpoints-only must not delete blobs, got %d", len(d))
	}
	if d := candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionDelete); len(d) == 0 {
		t.Fatal("checkpoints-only should still delete the eligible checkpoint")
	}
}

// helpers

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func blobIDs(cs []gcdom.GCCandidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID())
	}
	return out
}

func tempPaths(cs []gcdom.GCCandidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Path())
	}
	return out
}

func writeActiveLock(t *testing.T, locksDir string, now time.Time) {
	t.Helper()
	data := `{"schema_version":1,"operation":"run","pid":4242,"hostname":"testhost","created_at":"` +
		now.Add(-time.Minute).UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(filepath.Join(locksDir, "run.lock"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rewriteRunMeta overwrites a seeded run's meta.json, used to plant incompatible or
// corrupt store shapes at the exact path Get reads.
func (e *env) rewriteRunMeta(t *testing.T, id runcache.RunID, data []byte) {
	t.Helper()
	layout, _ := e.project.Paths()
	meta := filepath.Join(layout.RunsDir(), "entries", id.String()[:2], id.String(), "meta.json")
	if err := os.Chmod(meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, data, 0o444); err != nil {
		t.Fatal(err)
	}
}

// reasonsOf collects the retention reasons of the given candidates for assertions.
func reasonsOf(cs []gcdom.GCCandidate) []gcdom.RetentionReason {
	out := make([]gcdom.RetentionReason, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Reason())
	}
	return out
}

// TestIncompatibleRunIsRetainedAndBlocksSweep pins the "unknown means retain" rule.
// A run entry declaring a schema this build cannot read is NOT disposable: nothing can
// demonstrate what an undecodable record references, so GC can prove neither that the
// entry is safe to delete nor that the blob reachability set is complete without it.
// It is retained under its own blocked reason and the sweep stands down — the same
// conservative answer an unparseable record gets, reached for the same reason.
func TestIncompatibleRunIsRetainedAndBlocksSweep(t *testing.T) {
	e := newEnv(t)
	// Two checkpoints so a.go's blob would become unreachable under keep-last 1 — the
	// sweep this test proves is suppressed.
	e.writeFiles(t, map[string]string{"a.go": "package a // unique-AAA", "common.go": "package common // shared"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // unique-BBB"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	// Age the run well past the retention window, so retention alone would delete it:
	// what holds it back is the unreadable schema, not recency.
	now := time.Unix(1_700_002_000, 0).UTC()
	id := e.seedRun(t, "incompatible", now.Add(-30*24*time.Hour))
	e.rewriteRunMeta(t, id, []byte(`{"schema_version": 99, "run_id": "`+id.String()+`"}`+"\n"))

	pl := e.plan(t, now, nil, reqKeepLast(t, 1))

	blocked := candidatesOf(pl, gcdom.KindRun, gcdom.ActionBlocked)
	if len(blocked) != 1 || blocked[0].ID() != id.String() || blocked[0].Reason() != gcdom.ReasonRunIncompatible {
		t.Fatalf("incompatible run should be one run-incompatible block, got %+v", reasonsOf(blocked))
	}
	if dels := candidatesOf(pl, gcdom.KindRun, gcdom.ActionDelete); len(dels) != 0 {
		t.Fatalf("an incompatible run must never be a delete candidate, got %+v", reasonsOf(dels))
	}
	if s := candidatesOf(pl, gcdom.KindBlob, gcdom.ActionDelete); len(s) != 0 {
		t.Fatalf("an incompatible run must suppress the blob sweep, got %d swept blobs", len(s))
	}
	// It is user action, not damage-repair, that resolves this — but the exit signal is
	// the same state-action-required one, because either way a person must intervene.
	if !pl.StateActionBlocked() {
		t.Fatal("an incompatible run must mark the plan as needing user action")
	}

	// And execution removes nothing: the record survives for the user to reset.
	res := e.execute(t, now, nil, pl)
	if es := res.ExecSummary(); es.Deleted != 0 || es.Failed != 0 {
		t.Fatalf("a blocked plan must delete nothing, got %+v", es)
	}
	layout, _ := e.project.Paths()
	if _, err := runstore.New(layout, blake3hash.New()).Get(id); !errors.Is(err, runcache.ErrIncompatibleEntry) {
		t.Fatalf("incompatible run after gc: err = %v, want it still present and incompatible", err)
	}
}

// TestOneUnreadableCheckpointIsOneCandidate pins that a record GC cannot read is
// reported once, whichever way it is unreadable. The header read and the blob
// reachability pass both walk the retained ids and both hit the same failure, so
// without deduplication a single damaged checkpoint would appear twice in the plan
// and inflate the blocked count a caller reports or scripts against.
func TestOneUnreadableCheckpointIsOneCandidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		reason gcdom.RetentionReason
	}{
		{"corrupt", "{ not valid json", gcdom.ReasonCheckpointCorrupt},
		{"incompatible", `{"schema_version": 99}`, gcdom.ReasonCheckpointIncompatible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeFiles(t, map[string]string{"a.go": "package a"})
			h1 := e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
			e.writeFiles(t, map[string]string{"b.go": "package b"})
			e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

			layout, _ := e.project.Paths()
			headerPath := filepath.Join(layout.CheckpointsDir(), h1.ID.String(), "header.json")
			if err := os.Remove(headerPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(headerPath, []byte(tc.header), 0o644); err != nil {
				t.Fatal(err)
			}

			pl := e.plan(t, time.Unix(1_700_002_000, 0).UTC(), nil, reqKeepLast(t, 1))
			blocked := candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionBlocked)
			if len(blocked) != 1 {
				t.Fatalf("one unreadable checkpoint produced %d blocked candidates (%+v)", len(blocked), reasonsOf(blocked))
			}
			if blocked[0].ID() != h1.ID.String() || blocked[0].Reason() != tc.reason {
				t.Fatalf("blocked candidate = %s/%s, want %s/%s", blocked[0].ID(), blocked[0].Reason(), h1.ID.String(), tc.reason)
			}
			if got := pl.Summary().Blocked; got != 1 {
				t.Fatalf("summary blocked = %d, want 1", got)
			}
		})
	}
}

// TestCorruptRunStillBlocksSweep is the neighbour of the incompatible case: an
// unparseable run entry is corruption rather than an unreadable schema, and it is
// blocked under its own reason (run-corrupt) — so the two stay distinguishable in a
// report even though both suppress the blob sweep.
func TestCorruptRunStillBlocksSweep(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // unique-AAA", "common.go": "package common // shared"})
	e.checkpointAt(t, time.Unix(1_700_000_000, 0).UTC())
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // unique-BBB"})
	e.checkpointAt(t, time.Unix(1_700_000_500, 0).UTC())

	now := time.Unix(1_700_002_000, 0).UTC()
	id := e.seedRun(t, "corrupt", now.Add(-30*24*time.Hour))
	e.rewriteRunMeta(t, id, []byte("{ not valid json"))

	pl := e.plan(t, now, nil, reqKeepLast(t, 1))
	blocked := candidatesOf(pl, gcdom.KindRun, gcdom.ActionBlocked)
	if len(blocked) != 1 || blocked[0].Reason() != gcdom.ReasonRunCorrupt {
		t.Fatalf("corrupt run should be one run-corrupt block, got %+v", reasonsOf(blocked))
	}
	if s := candidatesOf(pl, gcdom.KindBlob, gcdom.ActionDelete); len(s) != 0 {
		t.Fatalf("a corrupt run must suppress the blob sweep, got %d swept blobs", len(s))
	}
}
