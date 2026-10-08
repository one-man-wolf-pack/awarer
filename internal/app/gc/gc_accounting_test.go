package gc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/infra/fsx"
)

// These tests pin GC byte accounting: logical regular-file lengths inside the selected
// record and temp deletion boundaries, each swept blob once, and honest completeness
// from planning through deletion. Their oracle is the filesystem itself — lengths the
// fixtures wrote, or an os-level Lstat walk of the record — never the sizing helper
// under test.

// regularBytes is the independent oracle: the sum of regular-file lengths beneath
// path, by a plain no-follow WalkDir.
func regularBytes(t *testing.T, path string) int64 {
	t.Helper()
	var n int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := os.Lstat(p)
			if err != nil {
				return err
			}
			n += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", path, err)
	}
	return n
}

func writeLen(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("z", n)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func onlyCandidate(t *testing.T, cs []gcdom.GCCandidate, what string) gcdom.GCCandidate {
	t.Helper()
	if len(cs) != 1 {
		t.Fatalf("want exactly one %s candidate, got %d: %+v", what, len(cs), cs)
	}
	return cs[0]
}

func assertExact(t *testing.T, c gcdom.GCCandidate, want int64) {
	t.Helper()
	if m := c.ByteMeasure(); !m.Exact() || m.Bytes() != want {
		t.Errorf("%s %s: measure = %d (%s), want exact %d", c.Kind(), c.ID(), m.Bytes(), m.Measurement(), want)
	}
}

// TestPlanAccountsSelectedRecordBoundaries covers every selected boundary at once: a
// run whose manifests dwarf its tiny logs, checkpoint and recovery metadata, a regular
// file planted inside a valid record, a blob referenced by two expired checkpoints,
// and a nested stale temp directory. Retained records stay unmeasured rather than
// reporting an exact zero.
func TestPlanAccountsSelectedRecordBoundaries(t *testing.T) {
	e := newEnv(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base.Add(60 * 24 * time.Hour)
	layout, _ := e.project.Paths()

	e.writeFiles(t, map[string]string{"shared.txt": "kept by the latest", "gone.txt": "only the old checkpoints hold this"})
	old1 := e.checkpointAt(t, base)
	old2 := e.checkpointAt(t, base.Add(time.Minute))
	e.removeFile(t, "gone.txt")
	latest := e.checkpointAt(t, now.Add(-time.Minute))

	oldRun := e.seedRun(t, "old", base)
	recentRun := e.seedRun(t, "recent", now.Add(-time.Minute))
	restoreID, _ := e.publishRecovery(t, base, 7, "pre-restore bytes")

	// A record-owned file this build does not know about still counts.
	old1Dir := filepath.Join(layout.CheckpointsDir(), old1.ID.String())
	writeLen(t, filepath.Join(old1Dir, "unexpected-extra"), 777)
	oldRunDir := e.mustRunPath(t, oldRun)
	writeLen(t, filepath.Join(oldRunDir, "unexpected-extra"), 333)

	staleDir := filepath.Join(layout.TmpDir(), "stale-op")
	writeLen(t, filepath.Join(staleDir, "a"), 10)
	writeLen(t, filepath.Join(staleDir, "nested", "b"), 20)
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(staleDir, old, old); err != nil {
		t.Fatal(err)
	}

	pl := e.plan(t, now, nil, reqKeepLast(t, 0))

	var wantPlanned int64
	for _, h := range []checkpoint.CheckpointHeader{old1, old2} {
		dir := filepath.Join(layout.CheckpointsDir(), h.ID.String())
		var c gcdom.GCCandidate
		for _, cand := range candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionDelete) {
			if cand.ID() == h.ID.String() {
				c = cand
			}
		}
		want := regularBytes(t, dir)
		assertExact(t, c, want)
		wantPlanned += want
	}
	if c := onlyCandidate(t, candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionRetain), "retained checkpoint"); c.ID() != latest.ID.String() || c.ByteMeasure().Measurement() != gcdom.MeasureUnmeasured {
		t.Errorf("retained latest = %s %s, want %s unmeasured", c.ID(), c.ByteMeasure().Measurement(), latest.ID.Short())
	}

	runDel := onlyCandidate(t, candidatesOf(pl, gcdom.KindRun, gcdom.ActionDelete), "deleted run")
	runWant := regularBytes(t, oldRunDir)
	assertExact(t, runDel, runWant)
	if declared := int64(len("out-old") + len("err-old")); runWant <= declared+333 {
		t.Errorf("run boundary %d bytes should exceed its declared logs (%d) plus the planted file: manifests and metadata count", runWant, declared)
	}
	wantPlanned += runWant
	if c := onlyCandidate(t, candidatesOf(pl, gcdom.KindRun, gcdom.ActionRetain), "retained run"); c.ID() != recentRun.String() || c.ByteMeasure().Measurement() != gcdom.MeasureUnmeasured || c.Bytes() != 0 {
		t.Errorf("retained run = %s %d (%s), want unmeasured and no declared bytes", c.ID(), c.Bytes(), c.ByteMeasure().Measurement())
	}

	restoreDel := onlyCandidate(t, candidatesOf(pl, gcdom.KindRestore, gcdom.ActionDelete), "deleted recovery observation")
	restoreWant := regularBytes(t, filepath.Join(layout.RestoresDir(), restoreID.String()))
	if restoreWant == 0 {
		t.Fatal("fixture recovery observation has no metadata on disk")
	}
	assertExact(t, restoreDel, restoreWant)
	wantPlanned += restoreWant

	// gone.txt is referenced by both expired checkpoints yet is one blob, counted once;
	// the expired recovery observation's captured content is the other unreachable blob.
	blobDels := candidatesOf(pl, gcdom.KindBlob, gcdom.ActionDelete)
	seen := map[string]int{}
	for _, c := range blobDels {
		seen[c.ID()]++
		info, err := os.Lstat(c.Path())
		if err != nil {
			t.Fatal(err)
		}
		assertExact(t, c, info.Size())
		wantPlanned += info.Size()
	}
	if len(blobDels) != 2 || len(seen) != 2 {
		t.Fatalf("want two distinct unreachable blobs, got %+v", seen)
	}

	tempDel := onlyCandidate(t, candidatesOf(pl, gcdom.KindTemp, gcdom.ActionDelete), "stale temp")
	assertExact(t, tempDel, 30)
	wantPlanned += 30

	s := pl.Summary()
	if s.BytesPlanned != wantPlanned || !s.BytesPlannedComplete() {
		t.Errorf("summary bytes = %d complete=%v, want exact %d", s.BytesPlanned, s.BytesPlannedComplete(), wantPlanned)
	}

	// Execution credits exactly what this plan measured and deleted.
	res := e.execute(t, now, nil, pl)
	es := res.ExecSummary()
	if es.Failed != 0 || es.BytesDeleted != wantPlanned || !es.BytesDeletedComplete() {
		t.Errorf("exec summary = %+v, want %d bytes exact with no failures", es, wantPlanned)
	}
}

// TestKeyPointersAreOutsideTheRunBoundary: the run's key pointer is removed with the
// run but is a derived index file, not part of the accounted entry directory.
func TestKeyPointersAreOutsideTheRunBoundary(t *testing.T) {
	e := newEnv(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base.Add(60 * 24 * time.Hour)
	layout, _ := e.project.Paths()
	id := e.seedRun(t, "keyed", base)
	keys := regularBytes(t, filepath.Join(layout.RunsDir(), "keys"))
	if keys == 0 {
		t.Fatal("fixture run published no key pointer")
	}
	pl := e.plan(t, now, nil, defaultReq(t))
	c := onlyCandidate(t, candidatesOf(pl, gcdom.KindRun, gcdom.ActionDelete), "deleted run")
	assertExact(t, c, regularBytes(t, e.mustRunPath(t, id)))
}

// TestCollectMeasuresUnderTheLeaseNotTheAdvisoryPlan: bytes that appear in a selected
// record after an advisory preview are what Collect measures and credits; the preview
// is never an input.
func TestCollectMeasuresUnderTheLeaseNotTheAdvisoryPlan(t *testing.T) {
	e := newEnv(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base.Add(60 * 24 * time.Hour)
	layout, _ := e.project.Paths()
	e.writeFiles(t, map[string]string{"a.txt": "a"})
	old := e.checkpointAt(t, base)
	e.checkpointAt(t, now.Add(-time.Minute))

	dry, err := gcdom.NewGCRequest(true, gcdom.FilterCheckpointsOnly, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	preview := e.plan(t, now, nil, dry)
	before := onlyCandidate(t, candidatesOf(preview, gcdom.KindCheckpoint, gcdom.ActionDelete), "previewed checkpoint").Bytes()

	writeLen(t, filepath.Join(layout.CheckpointsDir(), old.ID.String(), "grown-after-preview"), 5000)

	collectReq, err := gcdom.NewGCRequest(false, gcdom.FilterCheckpointsOnly, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	res := e.collect(t, now, nil, collectReq)
	es := res.ExecSummary()
	if es.BytesDeleted != before+5000 || !es.BytesDeletedComplete() {
		t.Fatalf("Collect credited %d (complete=%v), want its own measurement %d, not the preview's %d", es.BytesDeleted, es.BytesDeletedComplete(), before+5000, before)
	}
}

// TestBlockedRecordsContributeNoGuessedBytes: a corrupt checkpoint is retained and
// blocked with an unmeasured size, and the selected total (nothing) stays exact.
func TestBlockedRecordsContributeNoGuessedBytes(t *testing.T) {
	e := newEnv(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	e.writeFiles(t, map[string]string{"a.txt": "a"})
	h := e.checkpointAt(t, base)
	e.checkpointAt(t, base.Add(time.Hour))
	e.corruptCheckpointHeader(t, h.ID)

	pl := e.plan(t, base.Add(60*24*time.Hour), nil, reqKeepLast(t, 0))
	blocked := onlyCandidate(t, candidatesOf(pl, gcdom.KindCheckpoint, gcdom.ActionBlocked), "blocked checkpoint")
	if m := blocked.ByteMeasure(); m.Measurement() != gcdom.MeasureUnmeasured || m.Bytes() != 0 {
		t.Errorf("blocked checkpoint measure = %d (%s), want unmeasured", m.Bytes(), m.Measurement())
	}
	if !pl.Blocked() {
		t.Fatal("a corrupt checkpoint must still block the plan")
	}
}

// executorPlan builds a real, unblocked plan of hand-made delete candidates, for
// driving the executor's crediting directly.
func executorPlan(cands ...gcdom.GCCandidate) gcdom.GCPlan {
	return gcdom.NewPlan(false, cands, nil)
}

// TestIdempotentDeletionSuppliesNoMissingSize: deleting a record that is already gone
// succeeds, but credits only what its measurement established — an unmeasured record
// stays unmeasured and a partial one stays a lower bound.
func TestIdempotentDeletionSuppliesNoMissingSize(t *testing.T) {
	e := newEnv(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	layout, _ := e.project.Paths()
	absent := func(seed string, size gcdom.ByteMeasure) gcdom.GCCandidate {
		id, err := checkpoint.NewCheckpointID(strings.NewReader(strings.Repeat(seed, 64)))
		if err != nil {
			t.Fatal(err)
		}
		c, err := gcdom.NewCandidate(gcdom.KindCheckpoint, gcdom.ReasonCheckpointExpired, id.String(),
			filepath.Join(layout.CheckpointsDir(), id.String()), "expired", size)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	unmeasured := absent("a", gcdom.UnmeasuredBytes())
	partial := absent("b", gcdom.PartialBytes(64))

	res := gcdom.NewResult(executorPlan(unmeasured, partial), e.newExecutor(t, now, nil).run(executorPlan(unmeasured, partial)), nil)
	es := res.ExecSummary()
	if es.Deleted != 2 || es.Failed != 0 {
		t.Fatalf("idempotent deletes = %+v, want 2 successes", es)
	}
	if es.BytesDeleted != 64 || es.DeletedNotFullyMeasured != 2 || es.BytesDeletedComplete() {
		t.Fatalf("exec summary = %+v, want 64 bytes as a lower bound over 2 not fully measured successes", es)
	}
}

// TestInterruptedExecutionAccountsOnlyAttemptedDeletions: phases never reached are not
// failures and do not make the executed total incomplete; the interruption itself is
// visible as the unattempted plan remainder.
func TestInterruptedExecutionAccountsOnlyAttemptedDeletions(t *testing.T) {
	e := newEnv(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base.Add(60 * 24 * time.Hour)
	layout, _ := e.project.Paths()
	e.writeFiles(t, map[string]string{"a.txt": "a"})
	old := e.checkpointAt(t, base)
	e.checkpointAt(t, now.Add(-time.Minute))
	writeLen(t, filepath.Join(layout.TmpDir(), "stale-file"), 50)
	stale := now.Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(layout.TmpDir(), "stale-file"), stale, stale); err != nil {
		t.Fatal(err)
	}
	pl := e.plan(t, now, nil, reqKeepLast(t, 0))
	cpBytes := regularBytes(t, filepath.Join(layout.CheckpointsDir(), old.ID.String()))

	ex := e.newExecutor(t, now, func(k gcdom.CandidateKind) error {
		if k == gcdom.KindBlob {
			return errInterrupt
		}
		return nil
	})
	res := gcdom.NewResult(pl, ex.run(pl), nil)
	es := res.ExecSummary()
	if es.Deleted != 1 || es.Failed != 0 || es.BytesDeleted != cpBytes || !es.BytesDeletedComplete() {
		t.Fatalf("interrupted exec = %+v, want only the checkpoint's exact %d bytes", es, cpBytes)
	}
	if len(res.Deletions()) >= len(pl.Deletions()) {
		t.Fatal("the interruption must leave planned deletions unattempted")
	}
}

// TestMeasurementCancellationIsNotAPartialSize: cancellation stops planning; any other
// failure to inspect a boundary is an unmeasured value, not an error.
func TestMeasurementCancellationIsNotAPartialSize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &planner{ctx: ctx}
	if _, err := p.measured(fsx.TreeSize{}, ctx.Err()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled measurement = %v, want context.Canceled", err)
	}
	live := &planner{ctx: context.Background()}
	m, err := live.measured(fsx.TreeSize{}, errors.New("permission denied"))
	if err != nil || m.Measurement() != gcdom.MeasureUnmeasured {
		t.Fatalf("uninspectable boundary = %+v, %v; want unmeasured", m, err)
	}
	if m, _ := live.measured(fsx.TreeSize{Bytes: 9}, nil); m.Measurement() != gcdom.MeasurePartial || m.Bytes() != 9 {
		t.Fatalf("incomplete walk = %+v, want partial 9", m)
	}
	if m, _ := live.measured(fsx.TreeSize{Complete: true}, nil); !m.Exact() || m.Bytes() != 0 {
		t.Fatalf("absent boundary = %+v, want exact zero", m)
	}
}

func (e *env) mustRunPath(t *testing.T, id interface{ String() string }) string {
	t.Helper()
	layout, _ := e.project.Paths()
	var found string
	_ = filepath.WalkDir(filepath.Join(layout.RunsDir(), "entries"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == id.String() {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		t.Fatalf("run %s has no entry directory", id.String())
	}
	return found
}
