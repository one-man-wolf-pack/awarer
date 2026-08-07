package gc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"strings"

	domconfig "awarer/internal/domain/config"
	"awarer/internal/infra/lockfile"
)

// plantLock writes a recognized lock record into the project's locks directory,
// crossing the same Encode boundary writers use.
func plantLock(t *testing.T, root, name string, rec lockfile.Record) {
	t.Helper()
	locks := filepath.Join(root, ".awa", "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	data, err := lockfile.Encode(rec)
	if err != nil {
		t.Fatalf("encode lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, name), data, 0o444); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

// TestExecuteTimesOutOnActiveCollectorLock proves a second gc, finding the collector
// lock actually held by another collector, waits up to the configured timeout and then
// returns ErrLockTimeout — distinct from the writer-presence block, which inspectLocks
// ignores for gc.lock.
func TestExecuteTimesOutOnActiveCollectorLock(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	e.checkpointAt(t, now)

	// A collector actually holding the exclusive lock for the duration of this test.
	locksDir := filepath.Join(e.root, ".awa", "locks")
	held, err := lockfile.AcquireExclusive(context.Background(), e.root, locksDir, lockfile.CollectorLockName, lockfile.OpGC, lockfile.Owner{})
	if err != nil {
		t.Fatalf("seed collector lock: %v", err)
	}
	defer func() { _ = held.Release() }()
	e.cfg.Locks.Timeout = domconfig.Duration(40 * time.Millisecond)

	// Plan with the collector lock held: it must NOT block the plan (gc.lock is
	// reserved), so Execute proceeds to the exclusive acquire and times out.
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return true },
	})
	pl, err := svc.Plan(context.Background(), defaultReq(t), Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if pl.LockBlocked() {
		t.Fatalf("plan should not be lock-blocked by the reserved gc.lock")
	}
	_, err = runPlanUnderLease(svc, Request{Project: e.project, Config: e.cfg}, pl)
	if !errors.Is(err, lockfile.ErrLockTimeout) {
		t.Fatalf("execute err = %v, want ErrLockTimeout", err)
	}
}

// TestExecuteReacquiresLeftoverCollectorLock proves a leftover gc.lock file that no
// process holds does not block a new gc: the exclusive acquire reacquires it (the
// flock backend needs no stale detection) and execution proceeds.
func TestExecuteReacquiresLeftoverCollectorLock(t *testing.T) {
	e := newEnv(t)
	e.writeFiles(t, map[string]string{"a.go": "package a"})
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	e.checkpointAt(t, now)

	// A leftover collector lock file from a previous, now-gone collector.
	plantLock(t, e.root, lockfile.CollectorLockName, lockfile.Record{Operation: "gc", PID: 999, Hostname: "testhost", CreatedAt: now.Add(-time.Minute)})
	e.cfg.Locks.Timeout = domconfig.Duration(time.Second)

	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return false },
	})
	pl, err := svc.Plan(context.Background(), defaultReq(t), Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := runPlanUnderLease(svc, Request{Project: e.project, Config: e.cfg}, pl); err != nil {
		t.Fatalf("execute should reacquire the leftover collector lock: %v", err)
	}
}

// TestPlanIsNotWriterBlockedByAnyExclusiveLock proves the writer scan classifies a
// lock by what it is, not by one hard-coded filename. Every exclusive lock is a
// serialization token owned by the acquire path: gc.lock, because gc-vs-gc must
// time out rather than exit "blocked"; restore.lock, because under the flock
// backend it deliberately outlives its holder, so reading it as a writer would let
// a finished restore suppress collection until the stale threshold.
//
// The writer-presence case is in the same test on purpose: the exemption must be
// the exact exclusive name, never a prefix. A restore that is actually running
// holds "restore-<host>-<pid>-<rand>.lock", and that must still block.
func TestPlanIsNotWriterBlockedByAnyExclusiveLock(t *testing.T) {
	cases := []struct {
		name      string
		lockName  string
		operation string
		wantBlock bool
	}{
		{name: "collector", lockName: lockfile.CollectorLockName, operation: string(lockfile.OpGC)},
		{name: "restore", lockName: lockfile.RestoreLockName, operation: string(lockfile.OpRestore)},
		{
			name: "an in-flight restore's writer presence lock still blocks",
			// Deliberately built from the same operation token: only the exclusive
			// filename is exempt, not everything a restore touches.
			lockName: string(lockfile.OpRestore) + "-testhost-7777-abcd.lock", operation: string(lockfile.OpRestore),
			wantBlock: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.writeFiles(t, map[string]string{"a.go": "package a"})
			now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
			e.checkpointAt(t, now)

			plantLock(t, e.root, tc.lockName, lockfile.Record{
				Operation: tc.operation, PID: 7777, Hostname: "testhost", CreatedAt: now,
			})

			svc := New(Deps{
				Now:          func() time.Time { return now },
				Hostname:     "testhost",
				ProcessAlive: func(int) bool { return true },
			})
			pl, err := svc.Plan(context.Background(), defaultReq(t), Request{Project: e.project, Config: e.cfg})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if got := pl.LockBlocked(); got != tc.wantBlock {
				t.Fatalf("LockBlocked() = %v with %q present, want %v", got, tc.lockName, tc.wantBlock)
			}
		})
	}
}

// TestExecuteRetainsWhenWriterLockAppears proves the defense-in-depth writer
// recheck: a writer presence lock that appears after a plan is built (so the plan
// is not blocked) is caught under the held collector lease, which retains all state
// and warns rather than deleting into a concurrent writer's in-flight data. It drives
// the deletion composition directly via runPlanUnderLease;
// TestCollectRetainsWhenWriterLockActive proves the analogous guard through the
// production Collect path.
func TestExecuteRetainsWhenWriterLockAppears(t *testing.T) {
	e := newEnv(t)
	// Two checkpoints so the older one is a deletion candidate.
	e.writeFiles(t, map[string]string{"a.go": "package a // uniqueA"})
	t0 := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	e.checkpointAt(t, t0)
	e.writeFiles(t, map[string]string{"b.go": "package b // uniqueB"})
	e.checkpointAt(t, t0.Add(time.Hour))

	now := t0.Add(2 * time.Hour)
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "testhost",
		ProcessAlive: func(int) bool { return true },
	})
	pl, err := svc.Plan(context.Background(), reqKeepLast(t, 1), Request{Project: e.project, Config: e.cfg})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if pl.Blocked() {
		t.Fatalf("plan should not be blocked before any lock appears")
	}
	if pl.Summary().PlannedDelete == 0 {
		t.Fatalf("expected at least one deletion candidate to make the retain meaningful")
	}

	// A checkpoint starts after planning.
	plantLock(t, e.root, "checkpoint-testhost-7777-abcd.lock",
		lockfile.Record{Operation: "checkpoint", PID: 7777, Hostname: "testhost", CreatedAt: now})

	res, err := runPlanUnderLease(svc, Request{Project: e.project, Config: e.cfg}, pl)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExecSummary().Deleted > 0 {
		t.Fatalf("execute deleted %d candidates despite a writer lock appearing", res.ExecSummary().Deleted)
	}
	if !res.LockSuppressed() {
		t.Fatal("a writer-appeared retain must be marked LockSuppressed so it is machine-legible")
	}
	if !hasWarningContaining(res.Warnings(), "retaining state") {
		t.Fatalf("expected a retain warning, got %v", res.Warnings())
	}
}

func hasWarningContaining(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
