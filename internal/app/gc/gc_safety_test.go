package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	doctorapp "awarer/internal/app/doctor"
	"awarer/internal/domain/checkpoint"
	domconfig "awarer/internal/domain/config"
	doctordom "awarer/internal/domain/doctor"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/lockfile"
)

// errInterrupt is the sentinel the executor interruption seam returns to stop a
// sweep at a phase boundary in these tests.
var errInterrupt = errors.New("gc test: simulated interruption")

// corruptCheckpointHeader replaces a checkpoint's header with invalid JSON so the
// retained-state read fails, matching the corruption other gc tests inject.
func (e *env) corruptCheckpointHeader(t *testing.T, id checkpoint.CheckpointID) {
	t.Helper()
	layout, _ := e.project.Paths()
	headerPath := filepath.Join(layout.CheckpointsDir(), id.String(), "header.json")
	// The store writes headers read-only, so replace rather than overwrite in place.
	if err := os.Remove(headerPath); err != nil {
		t.Fatalf("remove header: %v", err)
	}
	if err := os.WriteFile(headerPath, []byte("{ broken"), 0o644); err != nil {
		t.Fatalf("corrupt header: %v", err)
	}
}

// writeRawLock writes raw bytes into the project's locks directory, bypassing Encode
// so a test can plant a malformed record.
func (e *env) writeRawLock(t *testing.T, name string, data []byte) {
	t.Helper()
	locks := filepath.Join(e.root, ".awa", "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, name), data, 0o444); err != nil {
		t.Fatalf("write raw lock: %v", err)
	}
}

func findingCodes(res doctordom.DoctorResult) []doctordom.FindingCode {
	var out []doctordom.FindingCode
	for _, f := range res.Findings() {
		out = append(out, f.Code())
	}
	return out
}

// collect runs the authoritative destructive path (Service.Collect) with an
// injected clock and optional git provider, mirroring the plan/execute helpers.
func (e *env) collect(t *testing.T, now time.Time, git GitProvider, req gcdom.GCRequest) gcdom.GCResult {
	t.Helper()
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return false },
		Git:          git,
	})
	res, err := svc.Collect(context.Background(), req, Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return res
}

// newExecutor builds an executor over the project's real repositories with an
// optional interruption seam, so a test can drive a partial sweep directly. It is
// the same executor Collect uses; only the seam is test-injected.
func (e *env) newExecutor(t *testing.T, now time.Time, fail func(gcdom.CandidateKind) error) *executor {
	t.Helper()
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return false },
	})
	r, layout, err := svc.open(Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return &executor{ctx: context.Background(), repos: r, root: layout.Root(), fail: fail}
}

// doctorResult runs doctor as the integrity witness at the given clock.
func (e *env) doctorResult(t *testing.T, now time.Time) doctordom.DoctorResult {
	t.Helper()
	svc := doctorapp.New(doctorapp.Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return false },
	})
	resolved, err := domconfig.NewResolvedConfig(e.cfg, domconfig.DefaultOrigins(), nil)
	if err != nil {
		t.Fatalf("resolved config: %v", err)
	}
	res, err := svc.Run(context.Background(), doctorapp.Request{Project: e.project, Resolved: resolved})
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	return res
}

func hasFinding(res doctordom.DoctorResult, code doctordom.FindingCode) bool {
	for _, f := range res.Findings() {
		if f.Code() == code {
			return true
		}
	}
	return false
}

func hasSubsystemFinding(res doctordom.DoctorResult, sub doctordom.Subsystem) bool {
	for _, f := range res.Findings() {
		if f.Subsystem() == sub {
			return true
		}
	}
	return false
}

// referencedBlobHashes returns the content hashes a checkpoint's manifest stores as
// blobs — the set that must stay present for the checkpoint to remain valid.
func (e *env) referencedBlobHashes(t *testing.T, id checkpoint.CheckpointID) map[string]bool {
	t.Helper()
	layout, _ := e.project.Paths()
	repo := checkpointjson.NewRepo(layout)
	stream, err := repo.OpenManifest(id)
	if err != nil {
		t.Fatalf("OpenManifest %s: %v", id.Short(), err)
	}
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("manifest open: %v", err)
	}
	defer func() { _ = cur.Close() }()
	out := map[string]bool{}
	for cur.Next() {
		ent, ok := cur.Record().Entry()
		if !ok {
			continue
		}
		if ent.Kind != worktree.KindRegular || ent.Storage != worktree.StorageBlob {
			continue
		}
		out[ent.Content.String()] = true
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("manifest stream: %v", err)
	}
	return out
}

// blobDeleteHashes returns the blob hashes a plan marked for deletion.
func blobDeleteHashes(pl gcdom.GCPlan) map[string]bool {
	out := map[string]bool{}
	for _, c := range pl.Candidates() {
		if c.Kind() == gcdom.KindBlob && c.Action() == gcdom.ActionDelete {
			out[c.ID()] = true
		}
	}
	return out
}

func (e *env) blobsPresent(t *testing.T, hashes map[string]bool) bool {
	t.Helper()
	layout, _ := e.project.Paths()
	store := blobstore.New(layout, blake3hash.New())
	infos, err := store.List()
	if err != nil {
		t.Fatalf("blob list: %v", err)
	}
	have := map[string]bool{}
	for _, in := range infos {
		have[in.Ref.Hash().String()] = true
	}
	for h := range hashes {
		if !have[h] {
			return false
		}
	}
	return true
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// An advisory plan marks checkpoint A and its blob X unreachable. A new checkpoint C
// reusing content X is published before the destructive run. Because Collect plans
// under the held collector lease, it observes C and never sweeps X, so C stays valid and
// doctor reports no missing blob. Against an implementation that simply executes the
// stale advisory plan, X would be deleted out from under C (see
// TestStalePlanExecuteWouldStrandReusedBlob).
func TestCollectRePlansAndKeepsReusedBlob(t *testing.T) {
	e := newEnv(t)
	const sharedX = "package shared // X-REUSED-CONTENT"

	// A: shared.go(X) + a.go — oldest, will be deletable.
	e.writeFiles(t, map[string]string{"shared.go": sharedX, "a.go": "package a // A"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	e.checkpointAt(t, t0)

	// B: replace with b.go(Y). shared.go(X) is now referenced only by A.
	e.removeFile(t, "shared.go")
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // Y"})
	e.checkpointAt(t, t0.Add(500*time.Second))

	now := t0.Add(3000 * time.Second)

	// Advisory plan built BEFORE C exists: keep-last 1 keeps B, so A and its blob X
	// are deletable.
	adv := e.plan(t, now, nil, reqKeepLast(t, 1))
	if adv.Blocked() {
		t.Fatalf("advisory plan should not be blocked; candidates=%+v", adv.Candidates())
	}
	stalePlannedBlobs := blobDeleteHashes(adv)
	if len(stalePlannedBlobs) == 0 {
		t.Fatal("advisory plan should mark at least one unreachable blob (X) deletable")
	}

	// C published AFTER the advisory plan, reusing content X (the store deduplicates
	// on content hash, so C's manifest references the same blob X).
	e.writeFiles(t, map[string]string{"shared.go": sharedX, "c.go": "package c // C"})
	hC := e.checkpointAt(t, t0.Add(1000*time.Second))
	cBlobs := e.referencedBlobHashes(t, hC.ID)

	// Sanity: the stale plan targets a blob that C now needs — the exact hazard.
	if !intersects(stalePlannedBlobs, cBlobs) {
		t.Fatal("test setup: advisory plan must target a blob checkpoint C references")
	}

	// Real destructive GC through the production path.
	res := e.collect(t, now, nil, reqKeepLast(t, 1))
	if es := res.ExecSummary(); es.Failed != 0 {
		t.Fatalf("collect had failures: %+v", es)
	}

	// C survives with every referenced blob present.
	layout, _ := e.project.Paths()
	if _, err := checkpointjson.NewRepo(layout).Header(hC.ID); err != nil {
		t.Fatalf("reusing checkpoint C should survive Collect: %v", err)
	}
	if !e.blobsPresent(t, cBlobs) {
		t.Fatal("Collect must not sweep a blob a newly-published checkpoint references")
	}
	if hasFinding(e.doctorResult(t, now), doctordom.CodeCheckpointMissingBlob) {
		t.Fatal("doctor must report no missing blob after Collect re-planned under the lease")
	}
}

// TestStalePlanExecuteWouldStrandReusedBlob proves the hazard the authoritativePlan
// invariant closes. It can only be reproduced by a deliberate test-only act:
// wrapping a stale advisory plan as authoritative (runPlanUnderLease) and deleting by
// it. That strands blob X even though checkpoint C now references it, and doctor
// reports a missing blob. Production cannot express this — deletion consumes only an
// authoritativePlan, which Collect mints solely from a plan built under the lease.
func TestStalePlanExecuteWouldStrandReusedBlob(t *testing.T) {
	e := newEnv(t)
	const sharedX = "package shared // X-REUSED-CONTENT"

	e.writeFiles(t, map[string]string{"shared.go": sharedX, "a.go": "package a // A"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	e.checkpointAt(t, t0)
	e.removeFile(t, "shared.go")
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // Y"})
	e.checkpointAt(t, t0.Add(500*time.Second))

	now := t0.Add(3000 * time.Second)
	stale := e.plan(t, now, nil, reqKeepLast(t, 1))

	// C reuses X after the stale plan was built.
	e.writeFiles(t, map[string]string{"shared.go": sharedX, "c.go": "package c // C"})
	hC := e.checkpointAt(t, t0.Add(1000*time.Second))
	cBlobs := e.referencedBlobHashes(t, hC.ID)

	// Deliberately mislabel the stale plan authoritative and delete by it — the only
	// way (and test-only) to reach the hazard that production structurally prevents.
	if res := e.execute(t, now, nil, stale); res.ExecSummary().Failed != 0 {
		t.Fatalf("execute failures: %+v", res.ExecSummary())
	}

	// The reused blob is now gone and doctor sees C's missing blob — the corruption
	// the production path cannot cause.
	if e.blobsPresent(t, cBlobs) {
		t.Fatal("expected the stale plan to strand C's reused blob (hazard proof)")
	}
	if !hasFinding(e.doctorResult(t, now), doctordom.CodeCheckpointMissingBlob) {
		t.Fatal("expected doctor to report a missing blob after executing the stale plan")
	}
}

// TestCollectObservesNewProtection proves the destructive decision honors protection
// that appears after the advisory plan rather than executing the plan it was shown.
//
// The lever is the keep-last window itself, which is computed over the checkpoints
// that are actually live: A is outside a keep-last-2 window while three checkpoints
// exist, but once B is gone — reclaimed by another collector between the advisory
// plan and this one — the same window reaches A. A stale plan would delete it; a
// re-planning Collect keeps it, and says which rule saved it.
func TestCollectObservesNewProtection(t *testing.T) {
	e := newEnv(t)
	t0 := time.Unix(1_700_000_000, 0).UTC()
	e.writeFiles(t, map[string]string{"a.go": "package a // A"})
	hA := e.checkpointAt(t, t0)
	e.writeFiles(t, map[string]string{"b.go": "package b // B"})
	hB := e.checkpointAt(t, t0.Add(500*time.Second))
	e.writeFiles(t, map[string]string{"c.go": "package c // C"})
	e.checkpointAt(t, t0.Add(1000*time.Second))

	now := t0.Add(2000 * time.Second)

	// Advisory: keep-last 2 keeps the two newest, so A is deletable.
	adv := e.plan(t, now, nil, reqKeepLast(t, 2))
	advDeletes := map[string]bool{}
	for _, c := range candidatesOf(adv, gcdom.KindCheckpoint, gcdom.ActionDelete) {
		advDeletes[c.ID()] = true
	}
	if !advDeletes[hA.ID.String()] {
		t.Fatalf("advisory plan should mark A deletable; deletes=%v", advDeletes)
	}

	// B disappears after the advisory plan, so the keep-last window now reaches A.
	layout, _ := e.project.Paths()
	repo := checkpointjson.NewRepo(layout)
	if err := repo.Delete(hB.ID); err != nil {
		t.Fatalf("removing B behind the advisory plan: %v", err)
	}

	// Collect re-plans under the lease and must retain the now-protected A.
	res := e.collect(t, now, nil, reqKeepLast(t, 2))
	if es := res.ExecSummary(); es.Failed != 0 {
		t.Fatalf("collect failures: %+v", es)
	}
	if _, err := repo.Header(hA.ID); err != nil {
		t.Fatalf("newly-protected checkpoint A must survive Collect: %v", err)
	}
	// Output honesty: A appears retained under the rule that saved it, not deleted,
	// in the authoritative plan.
	var retained bool
	for _, c := range res.Plan().Candidates() {
		if c.ID() != hA.ID.String() {
			continue
		}
		if c.Action() == gcdom.ActionDelete {
			t.Fatal("authoritative plan must not mark the newly-protected checkpoint deletable")
		}
		if c.Action() == gcdom.ActionRetain && c.Reason() == gcdom.ReasonCheckpointWithinKeepN {
			retained = true
		}
	}
	if !retained {
		t.Fatalf("authoritative plan should report A retained as %q, got %+v",
			gcdom.ReasonCheckpointWithinKeepN, res.Plan().Candidates())
	}
}

// TestCollectRetainsWhenWriterLockActive proves a writer presence lock present at the
// real run blocks all deletion through the production path and the outcome is
// machine-legible (lock-blocked candidates), not a silent no-op.
func TestCollectRetainsWhenWriterLockActive(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // A"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	e.checkpointAt(t, t0)
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // B"})
	e.checkpointAt(t, t0.Add(500*time.Second))

	now := t0.Add(2000 * time.Second)

	layout, _ := e.project.Paths()
	blobsBefore, _ := blobstore.New(layout, blake3hash.New()).List()

	// An active writer checkpoint lock present during the run.
	plantLock(t, e.root, "checkpoint-testhost-4242-abcd.lock",
		lockfile.Record{Operation: "checkpoint", PID: 4242, Hostname: "testhost", CreatedAt: now})

	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return true }, // owner alive → active writer
	})
	res, err := svc.Collect(context.Background(), reqKeepLast(t, 1), Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.ExecSummary().Deleted != 0 {
		t.Fatalf("Collect deleted %d despite an active writer lock", res.ExecSummary().Deleted)
	}
	if !res.Plan().LockBlocked() {
		t.Fatal("an active writer lock at run time must make the authoritative plan lock-blocked")
	}
	blobsAfter, _ := blobstore.New(layout, blake3hash.New()).List()
	if len(blobsAfter) != len(blobsBefore) {
		t.Fatalf("no blob may be swept under an active writer lock: before %d after %d", len(blobsBefore), len(blobsAfter))
	}
}

// TestCollectBlocksAndDoctorReportsCorruption proves that under a corrupt retained
// manifest the blob sweep is skipped through the production path, nothing is deleted,
// and doctor reports the corruption rather than the output implying a clean sweep.
func TestCollectBlocksAndDoctorReportsCorruption(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // A"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	hA := e.checkpointAt(t, t0)
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // B"})
	e.checkpointAt(t, t0.Add(500*time.Second))

	// Corrupt A's header.
	layout, _ := e.project.Paths()
	e.corruptCheckpointHeader(t, hA.ID)

	now := t0.Add(2000 * time.Second)
	blobsBefore, _ := blobstore.New(layout, blake3hash.New()).List()

	res := e.collect(t, now, nil, reqKeepLast(t, 1))
	if len(res.Deletions()) != 0 {
		t.Fatalf("a corruption-blocked run must delete nothing, got %d", len(res.Deletions()))
	}
	if !res.Plan().StateActionBlocked() {
		t.Fatal("authoritative plan should be corruption-blocked")
	}
	blobsAfter, _ := blobstore.New(layout, blake3hash.New()).List()
	if len(blobsAfter) != len(blobsBefore) {
		t.Fatalf("blob sweep must be skipped under corruption: before %d after %d", len(blobsBefore), len(blobsAfter))
	}
	if !hasSubsystemFinding(e.doctorResult(t, now), doctordom.SubsystemCheckpoints) {
		t.Fatal("doctor must report the underlying checkpoint corruption")
	}
}

// TestCollectRetainsUnderTrailingGarbageLock proves a lock file that is a valid
// record followed by trailing bytes is treated as unknown: it blocks destructive GC
// (nothing deleted) and doctor reports it as an unknown lock without removing it.
func TestCollectRetainsUnderTrailingGarbageLock(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // A"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	e.checkpointAt(t, t0)
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // B"})
	e.checkpointAt(t, t0.Add(500*time.Second))

	now := t0.Add(2000 * time.Second)
	layout, _ := e.project.Paths()
	blobsBefore, _ := blobstore.New(layout, blake3hash.New()).List()

	// A valid writer lock record with appended trailing garbage → unknown.
	valid, err := lockfile.Encode(lockfile.Record{Operation: "checkpoint", PID: 5, Hostname: "otherhost", CreatedAt: now})
	if err != nil {
		t.Fatalf("encode lock: %v", err)
	}
	e.writeRawLock(t, "checkpoint-weird.lock", append(append([]byte{}, valid...), []byte(" trailing junk")...))

	res := e.collect(t, now, nil, reqKeepLast(t, 1))
	if res.ExecSummary().Deleted != 0 {
		t.Fatalf("an unknown lock must block deletion, deleted %d", res.ExecSummary().Deleted)
	}
	if !res.Plan().LockBlocked() {
		t.Fatal("a trailing-garbage lock must make the authoritative plan lock-blocked")
	}
	blobsAfter, _ := blobstore.New(layout, blake3hash.New()).List()
	if len(blobsAfter) != len(blobsBefore) {
		t.Fatal("no state may be swept while an unknown lock is present")
	}

	// Doctor reports it unknown and, without --repair, never removes it.
	dr := e.doctorResult(t, now)
	if !hasFinding(dr, doctordom.CodeLockUnknown) {
		t.Fatalf("doctor should report the trailing-garbage lock as unknown; findings=%v", findingCodes(dr))
	}
}

// TestInterruptedGCLeavesNoMissingBlob proves the interruption contract via the
// executor seam: a sweep interrupted after checkpoint deletion but before blob
// deletion leaves only harmless extra blobs — no retained checkpoint loses a blob —
// and a later clean Collect finishes with doctor reporting no corruption.
func TestInterruptedGCLeavesNoMissingBlob(t *testing.T) {
	e := newEnv(t)
	// A (unique blob) then B (latest). keep-last 1 → A and A's blob deletable.
	e.writeFiles(t, map[string]string{"a.go": "package a // A-UNIQUE"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	hA := e.checkpointAt(t, t0)
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // B-UNIQUE"})
	hB := e.checkpointAt(t, t0.Add(500*time.Second))

	now := t0.Add(2000 * time.Second)
	pl := e.plan(t, now, nil, reqKeepLast(t, 1))
	if pl.Blocked() || pl.Summary().PlannedDelete == 0 {
		t.Fatalf("plan should have deletions and not be blocked: %+v", pl.Summary())
	}
	bBlobs := e.referencedBlobHashes(t, hB.ID)

	// Interrupt right before the blob phase: checkpoints (and runs) are swept, blobs
	// are not.
	stopAtBlobs := func(kind gcdom.CandidateKind) error {
		if kind == gcdom.KindBlob {
			return errInterrupt
		}
		return nil
	}
	results := e.newExecutor(t, now, stopAtBlobs).run(pl)
	for _, r := range results {
		if r.Err() != nil {
			t.Fatalf("pre-blob deletions must succeed: %v", r.Err())
		}
		if r.Candidate().Kind() == gcdom.KindBlob {
			t.Fatal("no blob should have been deleted before the interruption")
		}
	}

	// A is gone; B and every blob B references remain — the interruption left only
	// harmless extra (now-unreachable) blobs, never a missing one.
	layout, _ := e.project.Paths()
	repo := checkpointjson.NewRepo(layout)
	if _, err := repo.Header(hA.ID); err == nil {
		t.Fatal("checkpoint A should have been deleted before the interruption")
	}
	if _, err := repo.Header(hB.ID); err != nil {
		t.Fatalf("retained checkpoint B must survive: %v", err)
	}
	if !e.blobsPresent(t, bBlobs) {
		t.Fatal("interruption must not remove a blob a retained checkpoint references")
	}
	if hasFinding(e.doctorResult(t, now), doctordom.CodeCheckpointMissingBlob) {
		t.Fatal("doctor must see no missing blob after an interrupted sweep")
	}

	// A second clean Collect finishes the job: the orphan blob is swept, doctor clean.
	if es := e.collect(t, now, nil, reqKeepLast(t, 1)).ExecSummary(); es.Failed != 0 {
		t.Fatalf("second collect failures: %+v", es)
	}
	if hasFinding(e.doctorResult(t, now), doctordom.CodeCheckpointMissingBlob) {
		t.Fatal("doctor must be clean of missing blobs after the follow-up Collect")
	}
}

// TestInterruptedBeforeAnyDeletionLeavesStoreUnchanged proves interruption at the
// first phase boundary deletes nothing.
func TestInterruptedBeforeAnyDeletionLeavesStoreUnchanged(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a // A"})
	t0 := time.Unix(1_700_000_000, 0).UTC()
	hA := e.checkpointAt(t, t0)
	e.removeFile(t, "a.go")
	e.writeFiles(t, map[string]string{"b.go": "package b // B"})
	e.checkpointAt(t, t0.Add(500*time.Second))

	now := t0.Add(2000 * time.Second)
	pl := e.plan(t, now, nil, reqKeepLast(t, 1))

	layout, _ := e.project.Paths()
	blobsBefore, _ := blobstore.New(layout, blake3hash.New()).List()

	stopAtStart := func(kind gcdom.CandidateKind) error {
		if kind == gcdom.KindCheckpoint {
			return errInterrupt
		}
		return nil
	}
	if results := e.newExecutor(t, now, stopAtStart).run(pl); len(results) != 0 {
		t.Fatalf("interruption at the first phase must delete nothing, got %d", len(results))
	}
	if _, err := checkpointjson.NewRepo(layout).Header(hA.ID); err != nil {
		t.Fatalf("checkpoint A must be untouched: %v", err)
	}
	blobsAfter, _ := blobstore.New(layout, blake3hash.New()).List()
	if len(blobsAfter) != len(blobsBefore) {
		t.Fatalf("blob count must be unchanged: before %d after %d", len(blobsBefore), len(blobsAfter))
	}
}
