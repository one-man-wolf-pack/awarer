//go:build !darwin && !linux && !freebsd

package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests assert the file-content fallback's contract, used where advisory
// whole-file locks are unavailable: the lock record's contents ARE authoritative, so a
// leftover gc.lock is classified by its owner's liveness (stale → reclaimed, live →
// waited on) and an unrecognized gc.lock is an actionable blocker that is refused, not
// reacquired. The flock backend makes the opposite judgment; its contract is covered
// in exclusive_unix_test.go.

func TestFallbackAcquireExclusiveStealsStaleLock(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// Same host, dead owner: provably stale, so it is reclaimed and the record becomes
	// ours.
	writeLockFixture(t, locks, Record{Operation: string(OpGC), PID: 999, Hostname: "host", CreatedAt: now.Add(-time.Minute)})
	owner := fixedOwner("host", 4321, now, func(int) bool { return false })

	l, err := AcquireExclusive(context.Background(), root, locks, "gc.lock", OpGC, owner)
	if err != nil {
		t.Fatalf("AcquireExclusive over stale lock: %v", err)
	}
	defer func() { _ = l.Release() }()
	raw, err := readLockFile(root, locks, "gc.lock")
	if err != nil {
		t.Fatalf("readLockFile: %v", err)
	}
	rec, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if rec.PID != 4321 {
		t.Fatalf("reclaimed lock pid = %d, want 4321", rec.PID)
	}
}

func TestFallbackAcquireExclusiveTimesOutOnActiveContent(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// Same host, live owner: the record classifies as active, so acquisition waits and
	// then times out rather than stealing.
	writeLockFixture(t, locks, Record{Operation: string(OpGC), PID: 999, Hostname: "host", CreatedAt: now.Add(-time.Second)})
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := AcquireExclusive(ctx, root, locks, "gc.lock", OpGC, owner); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
}

func TestFallbackAcquireExclusiveRefusesUnknownLock(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, "gc.lock"), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("write unknown lock: %v", err)
	}
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := AcquireExclusive(ctx, root, locks, "gc.lock", OpGC, owner); !errors.Is(err, ErrLockUnknown) {
		t.Fatalf("err = %v, want ErrLockUnknown", err)
	}
	// An unrecognized lock is never removed.
	if _, err := os.Stat(filepath.Join(locks, "gc.lock")); err != nil {
		t.Fatalf("unknown lock should be left intact: %v", err)
	}
}

func TestFallbackAcquirePresenceProceedsWhenCollectorStale(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A stale collector record (dead owner) must not make a writer stand down.
	writeLockFixture(t, locks, Record{Operation: string(OpGC), PID: 999, Hostname: "host", CreatedAt: now.Add(-time.Minute)})
	owner := fixedOwner("host", 4321, now, func(pid int) bool { return pid == 4321 }) // 999 dead

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := AcquirePresence(ctx, root, locks, OpCheckpoint, owner)
	if err != nil {
		t.Fatalf("AcquirePresence should proceed past a stale collector: %v", err)
	}
	if l.State() != LeaseAcquired {
		t.Fatalf("state = %v, want acquired", l.State())
	}
}

func TestFallbackAcquirePresenceRefusesUnrecognizedCollector(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// An unrecognized collector record is an actionable blocker a retry cannot clear, so
	// a writer refuses it immediately rather than waiting out the timeout.
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, CollectorLockName), []byte(`{"schema_version":99}`), 0o644); err != nil {
		t.Fatalf("write unknown collector lock: %v", err)
	}
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := AcquirePresence(ctx, root, locks, OpCheckpoint, owner); !errors.Is(err, ErrLockUnknown) {
		t.Fatalf("err = %v, want ErrLockUnknown", err)
	}
}
