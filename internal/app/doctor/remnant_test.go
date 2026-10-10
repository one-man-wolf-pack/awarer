package doctor

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	dom "awarer/internal/domain/doctor"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/lockfile"
)

func (e *env) headerlessCheckpoint(t *testing.T, content string) (dir string, restore func()) {
	t.Helper()
	layout, _ := e.project.Paths()
	h := e.checkpointWith(t, e.cfg, map[string]string{"remnant.txt": content})
	dir = filepath.Join(layout.CheckpointsDir(), h.ID.String())
	header := filepath.Join(dir, "header.json")
	aside := filepath.Join(t.TempDir(), "header.json")
	if err := os.Rename(header, aside); err != nil {
		t.Fatal(err)
	}
	return dir, func() {
		if err := os.Rename(aside, header); err != nil {
			t.Error(err)
		}
	}
}

func (e *env) emptyCheckpointDir(t *testing.T) string {
	t.Helper()
	layout, _ := e.project.Paths()
	id, err := checkpoint.NewCheckpointID(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fileBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

func assertFileBytes(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := fileBytes(t, dir)
	if len(got) != len(want) {
		t.Fatalf("%s holds %d files, want %d", dir, len(got), len(want))
	}
	for name, data := range want {
		if got[name] != data {
			t.Fatalf("%s/%s changed", dir, name)
		}
	}
}

type hookLocker struct{ before func() }

func (l hookLocker) Acquire() (func() error, error) {
	l.before()
	return func() error { return nil }, nil
}

type collectorLock struct {
	root, locksDir string
	timeout        time.Duration
	reached        chan struct{}
}

func (l collectorLock) Acquire() (func() error, error) {
	if l.reached != nil {
		close(l.reached)
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	lease, err := lockfile.AcquireCollectorExclusive(ctx, l.root, l.locksDir, lockfile.OpDoctorRepair, lockfile.Owner{})
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

func collectorLockAppears(locksDir string) bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if _, err := os.Stat(filepath.Join(locksDir, lockfile.CollectorLockName)); err == nil {
			return true
		}
	}
	return false
}

func (e *env) collectorLock(timeout time.Duration) collectorLock {
	layout, _ := e.project.Paths()
	return collectorLock{root: layout.Root(), locksDir: layout.LocksDir(), timeout: timeout}
}

func (e *env) activeWriter(t *testing.T) *lockfile.Lease {
	t.Helper()
	layout, _ := e.project.Paths()
	lease, err := lockfile.AcquirePresence(context.Background(), layout.Root(), layout.LocksDir(), lockfile.OpCheckpoint, lockfile.Owner{})
	if err != nil {
		t.Fatalf("AcquirePresence: %v", err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}

func TestHeaderlessCheckpointDiagnosedThenRepaired(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	kept := e.checkpointWith(t, e.cfg, map[string]string{"kept.txt": "kept"})
	keptDir := filepath.Join(layout.CheckpointsDir(), kept.ID.String())
	keptBytes := fileBytes(t, keptDir)
	payload, _ := e.headerlessCheckpoint(t, "payload")
	payloadBytes := fileBytes(t, payload)
	if err := os.WriteFile(filepath.Join(payload, ".tmp-4242-1"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	payloadBytes[".tmp-4242-1"] = "partial"
	empty := e.emptyCheckpointDir(t)

	res := e.run(t, noGit(), Request{})
	got := findingsByCode(res, dom.CodeIncompletePublication)
	if len(got) != 2 {
		t.Fatalf("want two incomplete-publication findings, got %+v", res.Findings())
	}
	paths := map[string]bool{}
	for _, f := range got {
		paths[f.Path()] = true
		if f.Severity() != dom.SeverityWarning || !f.Repairable() || f.Repaired() {
			t.Errorf("finding %+v: want an unrepaired repairable warning", f)
		}
	}
	if !paths[payload] || !paths[empty] {
		t.Fatalf("findings locate %v, want %s and %s", paths, payload, empty)
	}
	if res.Health() != dom.HealthFailed || res.Summary().Errors != 0 {
		t.Fatalf("health = %s with %d errors; want failed without an error", res.Health(), res.Summary().Errors)
	}
	assertFileBytes(t, payload, payloadBytes)
	assertFileBytes(t, empty, nil)

	res = e.run(t, noGit(), Request{Repair: true})
	got = findingsByCode(res, dom.CodeIncompletePublication)
	if len(got) != 2 || !got[0].Repaired() || !got[1].Repaired() {
		t.Fatalf("want two repaired findings, got %+v", res.Findings())
	}
	for _, dir := range []string{payload, empty} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s survived repair: %v", dir, err)
		}
	}
	if res.NeedsAttention() {
		t.Fatalf("repair left attention needed: %+v", res.Findings())
	}
	assertFileBytes(t, keptDir, keptBytes)
	if _, err := checkpointjson.NewRepo(layout).Header(kept.ID); err != nil {
		t.Fatalf("adjacent committed checkpoint unreadable after repair: %v", err)
	}

	res = e.run(t, noGit(), Request{Repair: true})
	if len(res.Findings()) != 0 {
		t.Fatalf("second repair still reports %+v", res.Findings())
	}
}

func TestHeaderlessCheckpointWithUnknownEntryIsNotRepairable(t *testing.T) {
	e := newEnv(t)
	dir, _ := e.headerlessCheckpoint(t, "payload")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, dir)

	res := e.run(t, noGit(), Request{Repair: true})
	got := findingsByCode(res, dom.CodeIncompletePublication)
	if len(got) != 1 || got[0].Repairable() || got[0].Repaired() || got[0].Severity() != dom.SeverityWarning {
		t.Fatalf("want one non-repairable warning, got %+v", res.Findings())
	}
	if res.NeedsAttention() {
		t.Fatal("a remnant doctor will never remove must not hold the run at attention-needed")
	}
	assertFileBytes(t, dir, before)
}

func TestRepairKeepsPublicationCommittedWhileItWaitsForTheWriter(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	dir, commit := e.headerlessCheckpoint(t, "payload")
	payloads := fileBytes(t, dir)
	writer := e.activeWriter(t)

	lock := e.collectorLock(30 * time.Second)
	lock.reached = make(chan struct{})
	waiting := make(chan bool, 1)
	go func() {
		<-lock.reached
		// The repair takes the collector lock before it waits on writers, so the lock
		// file marks the point where only this writer keeps it from mutating.
		seen := collectorLockAppears(layout.LocksDir())
		if seen {
			commit()
		}
		_ = writer.Release()
		waiting <- seen
	}()

	d := noGit()
	d.Locks = lock
	res, err := New(d).Run(context.Background(), Request{Project: e.project, Resolved: resolvedConfig(e.cfg), Repair: true})
	if !<-waiting {
		t.Fatal("the repair never reached its wait for the writer; nothing was published during it")
	}

	payloads["header.json"] = fileBytes(t, dir)["header.json"]
	if payloads["header.json"] == "" {
		t.Fatal("the committed header is gone")
	}
	assertFileBytes(t, dir, payloads)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}

	if got := findingsByCode(res, dom.CodeIncompletePublication); len(got) != 0 {
		t.Fatalf("a publication that committed is still reported: %+v", got)
	}
	if res.Summary().Repaired != 0 {
		t.Fatalf("repair claims %d repairs for a directory it must not touch", res.Summary().Repaired)
	}
	id, err := checkpoint.ParseCheckpointID(filepath.Base(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpointjson.NewRepo(layout).Header(id); err != nil {
		t.Fatalf("committed checkpoint unreadable: %v", err)
	}
}

func TestRepairRemovesRemnantOnlyAfterTheWriterFinishes(t *testing.T) {
	e := newEnv(t)
	dir, _ := e.headerlessCheckpoint(t, "payload")
	before := fileBytes(t, dir)
	writer := e.activeWriter(t)

	d := noGit()
	d.Locks = e.collectorLock(50 * time.Millisecond)
	if _, err := New(d).Run(context.Background(), Request{Project: e.project, Resolved: resolvedConfig(e.cfg), Repair: true}); !errors.Is(err, lockfile.ErrLockTimeout) {
		t.Fatalf("repair under an active writer: err = %v, want lock timeout", err)
	}
	assertFileBytes(t, dir, before)

	if err := writer.Release(); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, d, Request{Repair: true})
	if got := findingsByCode(res, dom.CodeIncompletePublication); len(got) != 1 || !got[0].Repaired() {
		t.Fatalf("want the remnant repaired once the writer is gone, got %+v", res.Findings())
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("remnant survived: %v", err)
	}
}

func TestRepairLeavesRemnantUnderAnUnknownWriter(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	dir, _ := e.headerlessCheckpoint(t, "payload")
	before := fileBytes(t, dir)
	if err := os.WriteFile(filepath.Join(layout.LocksDir(), "future.lock"), []byte("not a lock record"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := noGit()
	d.Locks = e.collectorLock(time.Second)
	if _, err := New(d).Run(context.Background(), Request{Project: e.project, Resolved: resolvedConfig(e.cfg), Repair: true}); !errors.Is(err, lockfile.ErrLockUnknown) {
		t.Fatalf("repair under an unknown writer: err = %v, want unknown lock", err)
	}
	assertFileBytes(t, dir, before)
}

func TestRepairRewritesFindingToWhatItSeesUnderTheLock(t *testing.T) {
	t.Run("newly ineligible", func(t *testing.T) {
		e := newEnv(t)
		dir, _ := e.headerlessCheckpoint(t, "payload")
		var before map[string]string
		d := noGit()
		d.Locks = hookLocker{before: func() {
			if err := os.WriteFile(filepath.Join(dir, "late.txt"), []byte("late"), 0o600); err != nil {
				t.Error(err)
			}
			before = fileBytes(t, dir)
		}}
		res := e.run(t, d, Request{Repair: true})
		got := findingsByCode(res, dom.CodeIncompletePublication)
		if len(got) != 1 || got[0].Repairable() || got[0].Repaired() {
			t.Fatalf("want one non-repairable finding, got %+v", res.Findings())
		}
		assertFileBytes(t, dir, before)
	})
	t.Run("disappeared", func(t *testing.T) {
		e := newEnv(t)
		dir := e.emptyCheckpointDir(t)
		d := noGit()
		d.Locks = hookLocker{before: func() {
			if err := os.Remove(dir); err != nil {
				t.Error(err)
			}
		}}
		res := e.run(t, d, Request{Repair: true})
		if len(res.Findings()) != 0 || res.Summary().Repaired != 0 {
			t.Fatalf("a directory repair did not remove is reported: %+v", res.Findings())
		}
	})
}

func TestRemnantRepairCancellationIsAnInterruption(t *testing.T) {
	e := newEnv(t)
	dir, _ := e.headerlessCheckpoint(t, "payload")
	before := fileBytes(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	d := noGit()
	d.Locks = hookLocker{before: cancel}
	_, err := New(d).Run(ctx, Request{Project: e.project, Resolved: resolvedConfig(e.cfg), Repair: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled repair: err = %v, want context.Canceled", err)
	}
	assertFileBytes(t, dir, before)
}
