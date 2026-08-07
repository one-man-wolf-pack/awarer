package lockfile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedOwner builds an Owner with deterministic identity/clock/liveness for tests.
func fixedOwner(host string, pid int, now time.Time, alive func(int) bool) Owner {
	return Owner{
		Hostname:   host,
		PID:        pid,
		Now:        func() time.Time { return now },
		Rand:       fixedRand(),
		Alive:      alive,
		StaleAfter: DefaultStaleAfter,
	}
}

// fixedRand returns a reader that yields a predictable, non-repeating byte stream so
// successive AcquirePresence calls produce distinct suffixes without crypto/rand.
func fixedRand() *seqReader { return &seqReader{} }

type seqReader struct{ n byte }

func (r *seqReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

func locksDirFor(root string) string { return filepath.Join(root, ".awa", "locks") }

func TestAcquirePresenceCreatesDistinctTokens(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	live := func(int) bool { return true }
	owner := fixedOwner("host", 4321, now, live)

	a, err := AcquirePresence(context.Background(), root, locks, OpCheckpoint, owner)
	if err != nil {
		t.Fatalf("first AcquirePresence: %v", err)
	}
	b, err := AcquirePresence(context.Background(), root, locks, OpRun, owner)
	if err != nil {
		t.Fatalf("second AcquirePresence: %v", err)
	}
	if a.State() != LeaseAcquired || b.State() != LeaseAcquired {
		t.Fatalf("leases should be acquired: %v %v", a.State(), b.State())
	}
	if a.name == b.name {
		t.Fatalf("presence tokens collided: %q", a.name)
	}

	entries := scan(t, root, locks, now, "host", live)
	if len(entries) != 2 {
		t.Fatalf("want 2 lock files, got %d", len(entries))
	}
	for _, e := range entries {
		if e.State != EntryActive {
			t.Fatalf("presence token %q classified %v, want active", e.Name, e.State)
		}
	}
}

func TestAcquirePresenceRejectsUnknownOperation(t *testing.T) {
	root := t.TempDir()
	if _, err := AcquirePresence(context.Background(), root, locksDirFor(root), Operation("bogus"), Owner{}); err == nil {
		t.Fatalf("expected error for unknown operation")
	}
}

func TestReleaseRemovesOnlyOwnToken(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	live := func(int) bool { return true }
	owner := fixedOwner("host", 4321, now, live)

	a, err := AcquirePresence(context.Background(), root, locks, OpCheckpoint, owner)
	if err != nil {
		t.Fatalf("AcquirePresence a: %v", err)
	}
	b, err := AcquirePresence(context.Background(), root, locks, OpRun, owner)
	if err != nil {
		t.Fatalf("AcquirePresence b: %v", err)
	}

	if err := a.Release(); err != nil {
		t.Fatalf("Release a: %v", err)
	}
	if a.State() != LeaseReleased {
		t.Fatalf("a state = %v, want released", a.State())
	}
	// b must remain.
	if _, err := os.Stat(filepath.Join(locks, b.name)); err != nil {
		t.Fatalf("b token should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(locks, a.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a token should be gone, stat err = %v", err)
	}
	// Second release is a no-op.
	if err := a.Release(); err != nil {
		t.Fatalf("second Release a: %v", err)
	}
}

func TestAcquireExclusiveSucceedsOnFreeLock(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	owner := fixedOwner("host", 4321, now, func(int) bool { return true })

	l, err := AcquireExclusive(context.Background(), root, locks, "gc.lock", OpGC, owner)
	if err != nil {
		t.Fatalf("AcquireExclusive: %v", err)
	}
	if l.name != "gc.lock" {
		t.Fatalf("exclusive lock name = %q, want gc.lock", l.name)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireExclusiveTimesOutOnActiveLock(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A collector actually holding the exclusive lock.
	held, err := AcquireExclusive(context.Background(), root, locks, "gc.lock", OpGC, fixedOwner("host", 999, now, func(int) bool { return true }))
	if err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	defer func() { _ = held.Release() }()

	owner := fixedOwner("host", 4321, now, func(int) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = AcquireExclusive(ctx, root, locks, "gc.lock", OpGC, owner)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
}

func TestAcquirePresenceStandsDownUnderActiveCollector(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A collector actually holding the exclusive lock makes a writer stand down.
	held, err := AcquireExclusive(context.Background(), root, locks, "gc.lock", OpGC, fixedOwner("host", 999, now, func(int) bool { return true }))
	if err != nil {
		t.Fatalf("seed collector: %v", err)
	}
	defer func() { _ = held.Release() }()

	owner := fixedOwner("host", 4321, now, func(int) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = AcquirePresence(ctx, root, locks, OpCheckpoint, owner)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout under an active collector", err)
	}
	// The writer must leave no presence token behind after standing down.
	entries := scan(t, root, locks, now, "host", func(int) bool { return true })
	for _, e := range entries {
		if e.Name != CollectorLockName {
			t.Fatalf("writer left a presence token after standing down: %q", e.Name)
		}
	}
}

// TestIsExclusiveLockNameCoversEveryExclusiveLock keeps the semantic
// classification the single source of truth for gc's writer scan and doctor's
// repair pass. Both consumers ask this question instead of comparing filenames, so
// an exclusive lock added here without being classified would silently start
// reading as a writer presence token — blocking collection and inviting doctor to
// delete a lock a live holder is standing on.
func TestIsExclusiveLockNameCoversEveryExclusiveLock(t *testing.T) {
	for _, name := range []string{CollectorLockName, RestoreLockName} {
		if !IsExclusiveLockName(name) {
			t.Errorf("IsExclusiveLockName(%q) = false, want true", name)
		}
	}
	// A writer presence token is NOT exclusive, even when it belongs to the same
	// operation as an exclusive lock: an in-flight restore must still block gc.
	for _, name := range []string{
		"", "restore", string(OpRestore) + "-host-4321-abcd.lock",
		string(OpCheckpoint) + "-host-4321-abcd.lock", "run.lock",
	} {
		if IsExclusiveLockName(name) {
			t.Errorf("IsExclusiveLockName(%q) = true, want false", name)
		}
	}
}

// TestWriterPresentIgnoresEveryExclusiveLock is the third consumer of the
// exclusive/presence distinction, and the one whose mistake is quietest: gc's
// post-lock recheck and doctor --repair both ask this question, and a "yes" makes
// gc retain everything and doctor --repair fail closed.
//
// An exclusive lock file survives its holder by design, so its recorded pid is a
// dead process — until the OS reuses that pid, at which point a leftover file
// starts answering "a writer is active" forever. Classifying by name rather than by
// liveness is what makes that impossible.
func TestWriterPresentIgnoresEveryExclusiveLock(t *testing.T) {
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	live := func(int) bool { return true }

	for _, name := range []string{CollectorLockName, RestoreLockName} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			locks := locksDirFor(root)
			if err := os.MkdirAll(locks, 0o755); err != nil {
				t.Fatal(err)
			}
			// A leftover exclusive lock whose recorded owner reads as alive on this host.
			data, err := Encode(Record{Operation: "gc", PID: 4321, Hostname: "host", CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(locks, name), data, 0o444); err != nil {
				t.Fatal(err)
			}

			state, detail, err := WriterPresent(root, locks, now, DefaultStaleAfter, "host", live)
			if err != nil {
				t.Fatalf("WriterPresent: %v", err)
			}
			if state != 0 {
				t.Fatalf("WriterPresent with a leftover %s = %v (%s), want no writer", name, state, detail)
			}
		})
	}

	// And a real writer presence token is still seen, so the exemption is the exact
	// exclusive name rather than "ignore locks that mention an exclusive operation".
	// The name is spelled the way AcquirePresence mints it — writeLockFixture's
	// "<operation>.lock" would collide with RestoreLockName itself.
	root := t.TempDir()
	locks := locksDirFor(root)
	presence, err := AcquirePresence(context.Background(), root, locks, OpRestore, fixedOwner("host", 4321, now, live))
	if err != nil {
		t.Fatalf("AcquirePresence: %v", err)
	}
	defer func() { _ = presence.Release() }()
	state, _, err := WriterPresent(root, locks, now, DefaultStaleAfter, "host", live)
	if err != nil {
		t.Fatalf("WriterPresent: %v", err)
	}
	if state != EntryActive {
		t.Fatalf("WriterPresent with an in-flight restore presence lock = %v, want EntryActive", state)
	}
}

func TestAcquireCollectorExclusiveWaitsOutWriterThenSucceeds(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// An in-flight writer presence lock. Once it is gone, the collector acquisition
	// must succeed (no writers left to wait for).
	writer, err := AcquirePresence(context.Background(), root, locks, OpCheckpoint, fixedOwner("host", 4321, now, func(int) bool { return true }))
	if err != nil {
		t.Fatalf("AcquirePresence: %v", err)
	}
	if err := writer.Release(); err != nil {
		t.Fatalf("Release writer: %v", err)
	}

	owner := fixedOwner("host", 5000, now, func(int) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease, err := AcquireCollectorExclusive(ctx, root, locks, OpDoctorRepair, owner)
	if err != nil {
		t.Fatalf("AcquireCollectorExclusive: %v", err)
	}
	if lease.Operation() != OpDoctorRepair {
		t.Fatalf("operation = %v, want doctor-repair", lease.Operation())
	}
	_ = lease.Release()
}

func TestAcquireCollectorExclusiveTimesOutWhileWriterActive(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// A writer that stays in flight (never released) must make the collector
	// acquisition wait it out and then time out rather than mutate alongside it.
	if _, err := AcquirePresence(context.Background(), root, locks, OpCheckpoint, fixedOwner("host", 4321, now, func(int) bool { return true })); err != nil {
		t.Fatalf("AcquirePresence: %v", err)
	}
	owner := fixedOwner("host", 5000, now, func(int) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := AcquireCollectorExclusive(ctx, root, locks, OpDoctorRepair, owner)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	assertCollectorLockReleased(t, root, locks, owner)
}

func TestAcquireCollectorExclusiveFailsClosedOnUnknownWriter(t *testing.T) {
	root := t.TempDir()
	locks := locksDirFor(root)
	now := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	// An unrecognized lock — a newer-awa writer this build cannot parse, or garbage.
	// A repair must not rebuild the index or remove temp state under a writer it cannot
	// reason about, and cannot know when the lock will clear, so it fails closed at once
	// rather than draining until timeout.
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locks, "future.lock"), []byte("not a lock record"), 0o644); err != nil {
		t.Fatalf("write unknown lock: %v", err)
	}

	owner := fixedOwner("host", 5000, now, func(int) bool { return true })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := AcquireCollectorExclusive(ctx, root, locks, OpDoctorRepair, owner)
	if !errors.Is(err, ErrLockUnknown) {
		t.Fatalf("err = %v, want ErrLockUnknown", err)
	}
	// The collector lock taken before the writer check must be released on the failure.
	assertCollectorLockReleased(t, root, locks, owner)
}

// assertCollectorLockReleased proves no collector lock is still held: a fresh
// exclusive acquire (which, unlike AcquireCollectorExclusive, does not wait out
// writers) must succeed immediately. Under the flock backend the lock file persists on
// disk as a diagnostic record even when no one holds it, so presence of the file is
// not the signal — reacquirability is.
func assertCollectorLockReleased(t *testing.T, root, locks string, owner Owner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	l, err := AcquireExclusive(ctx, root, locks, CollectorLockName, OpGC, owner)
	if err != nil {
		t.Fatalf("collector lock should be released, but reacquire failed: %v", err)
	}
	_ = l.Release()
}

func TestOperationValid(t *testing.T) {
	for _, op := range []Operation{OpCheckpoint, OpRun, OpRunRm, OpGC, OpDoctorRepair} {
		if !op.Valid() {
			t.Fatalf("%q should be valid", op)
		}
	}
	if Operation("nope").Valid() {
		t.Fatalf("unknown operation should be invalid")
	}
}

// scan is a test helper around ScanDir with the standard threshold.
func scan(t *testing.T, root, locks string, now time.Time, host string, alive func(int) bool) []ScanEntry {
	t.Helper()
	entries, err := ScanDir(root, locks, now, DefaultStaleAfter, host, alive)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	return entries
}

// writeLockFixture writes a recognized lock record straight to disk, crossing the
// same Encode boundary writers use.
func writeLockFixture(t *testing.T, locks string, rec Record) {
	t.Helper()
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	data, err := Encode(rec)
	if err != nil {
		t.Fatalf("Encode fixture: %v", err)
	}
	name := strings.ReplaceAll(rec.Operation, "/", "_") + ".lock"
	if err := os.WriteFile(filepath.Join(locks, name), data, 0o444); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
