//go:build darwin || linux || freebsd

package lockfile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests assert the flock backend's contract: the collector lock's file contents
// are advisory, so a leftover or unrecognized gc.lock with no live holder is never an
// obstacle. The file-content fallback makes the opposite judgment (a leftover may be a
// live owner, an unrecognized lock is an actionable blocker), so its contract is
// covered separately in exclusive_other_test.go.

func TestAcquireExclusiveReacquiresLeftoverLock(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A leftover lock file from a previous run that no process holds. The flock backend
	// reacquires it with no stale detection or steal, then overwrites the diagnostic
	// record with this acquirer's identity.
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	leftover, err := Encode(Record{Operation: string(OpGC), PID: 999, Hostname: "host", CreatedAt: now.Add(-time.Minute)})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, "gc.lock"), leftover, 0o644); err != nil {
		t.Fatalf("write leftover: %v", err)
	}
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	l, err := AcquireExclusive(context.Background(), root, locks, "gc.lock", OpGC, owner)
	if err != nil {
		t.Fatalf("AcquireExclusive over leftover lock: %v", err)
	}
	defer func() { _ = l.Release() }()
	if l.Operation() != OpGC {
		t.Fatalf("operation = %v, want gc", l.Operation())
	}
	// The diagnostic record is now ours: its pid is the reacquirer's.
	raw, err := readLockFile(root, locks, "gc.lock")
	if err != nil {
		t.Fatalf("readLockFile: %v", err)
	}
	rec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rec.PID != 4321 {
		t.Fatalf("diagnostic record pid = %d, want 4321 (the reacquirer)", rec.PID)
	}
}

func TestAcquireExclusiveIgnoresUnrecognizedLeftover(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A leftover gc.lock whose contents this build cannot parse. The flock backend
	// never treats the file's contents as authoritative, so with no live holder it is
	// reacquired (and the record overwritten): an unparsable leftover is not actionable.
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, "gc.lock"), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("write unknown lock: %v", err)
	}
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := AcquireExclusive(ctx, root, locks, "gc.lock", OpGC, owner)
	if err != nil {
		t.Fatalf("AcquireExclusive over unrecognized leftover: %v", err)
	}
	_ = l.Release()
}

func TestAcquirePresenceProceedsWhenCollectorUnheld(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A leftover collector lock file that no process holds must not make a writer stand
	// down: the flock probe sees it is free.
	writeLockFixture(t, locks, Record{Operation: string(OpGC), PID: 999, Hostname: "host", CreatedAt: now.Add(-time.Minute)})
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := AcquirePresence(ctx, root, locks, OpCheckpoint, owner)
	if err != nil {
		t.Fatalf("AcquirePresence should proceed past an unheld collector lock: %v", err)
	}
	if l.State() != LeaseAcquired {
		t.Fatalf("state = %v, want acquired", l.State())
	}
}

func TestAcquirePresenceProceedsPastUnrecognizedLeftoverCollector(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A leftover collector lock file with unparsable contents but no live holder. The
	// flock backend judges the lock by who holds it, not by the file's contents, so a
	// writer proceeds rather than refusing.
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, CollectorLockName), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("write unknown collector lock: %v", err)
	}
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := AcquirePresence(ctx, root, locks, OpCheckpoint, owner)
	if err != nil {
		t.Fatalf("AcquirePresence should proceed past an unheld leftover collector lock: %v", err)
	}
	_ = l.Release()
}
