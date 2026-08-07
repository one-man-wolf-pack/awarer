package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/infra/lockfile"
)

// TestConcurrentCheckpointDistinct proves concurrent checkpoints publish distinct
// valid checkpoints without corrupting the store: every invocation succeeds, each
// publishes one checkpoint, and doctor stays clean.
func TestConcurrentCheckpointDistinct(t *testing.T) {
	root := initProject(t)
	const n = 4
	for i := 0; i < n; i++ {
		write(t, root, "f"+string(rune('0'+i))+".txt", "content-"+string(rune('0'+i)))
	}
	results := runConcurrent(t, n, func(i int) cmdResult {
		return awaCmd(root, "checkpoint", "-m", "checkpoint")
	})
	for i, r := range results {
		if r.code != 0 {
			t.Fatalf("checkpoint %d exit = %d, stderr=%q", i, r.code, r.stderr)
		}
	}
	if ids := checkpointIDs(t, root); len(ids) != n {
		t.Fatalf("checkpoint count = %d, want %d", len(ids), n)
	}
	if code, stdout, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after concurrent checkpoint = %d, stdout=%q", code, stdout)
	}
}

// TestConcurrentIdenticalRun proves concurrent identical runs converge: all succeed
// with the same output, the cache resolves to one valid entry (the next run hits),
// and doctor stays clean.
func TestConcurrentIdenticalRun(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	const n = 4
	results := runConcurrent(t, n, func(i int) cmdResult {
		return awaCmd(root, "run", "--", h, "-out", "hello")
	})
	for i, r := range results {
		if r.code != 0 || r.stdout != "hello" {
			t.Fatalf("identical run %d: exit=%d stdout=%q stderr=%q", i, r.code, r.stdout, r.stderr)
		}
	}
	// A subsequent run must be a hit against a complete, valid entry.
	if code, stdout, stderr := awa(t, root, "run", "--", h, "-out", "hello"); code != 0 || stdout != "hello" || !isHit(stderr) {
		t.Fatalf("follow-up run: exit=%d stdout=%q hit=%v", code, stdout, isHit(stderr))
	}
	if code, _, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after identical runs != 0")
	}
}

// TestConcurrentDistinctRun proves non-identical runs do not corrupt each other:
// each stores its own entry and each is individually a hit afterward.
func TestConcurrentDistinctRun(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	const n = 4
	outs := []string{"a", "b", "c", "d"}
	results := runConcurrent(t, n, func(i int) cmdResult {
		return awaCmd(root, "run", "--", h, "-out", outs[i])
	})
	for i, r := range results {
		if r.code != 0 || r.stdout != outs[i] {
			t.Fatalf("distinct run %d: exit=%d stdout=%q", i, r.code, r.stdout)
		}
	}
	for i := 0; i < n; i++ {
		code, stdout, stderr := awa(t, root, "run", "--", h, "-out", outs[i])
		if code != 0 || stdout != outs[i] || !isHit(stderr) {
			t.Fatalf("follow-up distinct run %d: exit=%d stdout=%q hit=%v stderr=%q", i, code, stdout, isHit(stderr), stderr)
		}
	}
	if code, _, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after distinct runs != 0")
	}
}

// TestGCBlockedByActiveWriterLock proves an active writer presence lock blocks
// destructive gc: nothing is deleted while the lock is held, and gc proceeds once it
// is gone — the safety gap that motivated writer-side locking.
func TestGCBlockedByActiveWriterLock(t *testing.T) {
	root := initProject(t)
	// Two checkpoints so the older is a deletion candidate under --keep-last 1.
	write(t, root, "a.txt", "aaa")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint 1: %d %q", code, stderr)
	}
	write(t, root, "b.txt", "bbb")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint 2: %d %q", code, stderr)
	}
	if ids := checkpointIDs(t, root); len(ids) != 2 {
		t.Fatalf("setup: checkpoint count = %d, want 2", len(ids))
	}

	// An active checkpoint lock owned by this live process blocks gc.
	lockPath := writeActiveLock(t, root, lockfile.OpCheckpoint)
	code, stdout, stderr := awa(t, root, "gc", "--keep-last", "1")
	if code != 0 {
		t.Fatalf("blocked gc exit = %d, want 0 (try-again-later); stderr=%q", code, stderr)
	}
	if ids := checkpointIDs(t, root); len(ids) != 2 {
		t.Fatalf("gc deleted while a writer lock was active: checkpoint count = %d, want 2; out=%q", len(ids), stdout)
	}

	// Remove the lock; gc now proceeds and reclaims the older checkpoint.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := awa(t, root, "gc", "--keep-last", "1"); code != 0 {
		t.Fatalf("gc after lock removed exit = %d, stderr=%q", code, stderr)
	}
	if ids := checkpointIDs(t, root); len(ids) != 1 {
		t.Fatalf("gc after unlock: checkpoint count = %d, want 1", len(ids))
	}
	if code, _, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after gc != 0")
	}
}

// TestInterruptedCheckpointInvisibleAndRecoverable proves a crashed checkpoint that
// left only a manifest (no header) never becomes authoritative: it is absent from
// the log, and a fresh checkpoint proceeds normally.
func TestInterruptedCheckpointInvisibleAndRecoverable(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "aaa")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint: %d %q", code, stderr)
	}
	ids := checkpointIDs(t, root)
	if len(ids) != 1 {
		t.Fatalf("want 1 real checkpoint, got %d", len(ids))
	}
	realID := ids[0]

	// Simulate a crashed checkpoint: a checkpoint directory with a manifest but no
	// header (the commit point). Its name mirrors a real id's shape.
	orphanID := mutateID(realID)
	orphanDir := filepath.Join(root, ".awa", "checkpoints", orphanID)
	if err := os.MkdirAll(orphanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "manifest.jsonl"), []byte("{}\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	// The orphan is invisible to the log.
	code, stdout, _ := awa(t, root, "log")
	if code != 0 {
		t.Fatalf("log exit = %d", code)
	}
	if strings.Contains(stdout, orphanID) {
		t.Fatalf("header-less orphan leaked into the log:\n%s", stdout)
	}

	// A fresh checkpoint still works and is visible.
	write(t, root, "b.txt", "bbb")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint after interruption: %d %q", code, stderr)
	}
	visible := checkpointIDs(t, root)
	if len(visible) != 2 {
		t.Fatalf("visible checkpoints = %d, want 2 (orphan excluded)", len(visible))
	}
}

// TestGCLockTimeoutExit6 proves a second gc, finding a live collector lock it cannot
// acquire within the configured timeout, exits with the lock-timeout code (6) — not
// storage corruption (5) — and deletes nothing.
func TestGCLockTimeoutExit6(t *testing.T) {
	root := initProject(t)
	// A short lock timeout (the minimum 1s granularity) so the test does not wait the
	// 5s default.
	if err := os.WriteFile(filepath.Join(root, ".awa", "config.toml"),
		[]byte("[locks]\ntimeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "aaa")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint: %d %q", code, stderr)
	}

	// The collector lock actually held for the duration of the gc call.
	locksDir := filepath.Join(root, ".awa", "locks")
	held, err := lockfile.AcquireExclusive(context.Background(), root, locksDir, lockfile.CollectorLockName, lockfile.OpGC, lockfile.Owner{})
	if err != nil {
		t.Fatalf("hold collector lock: %v", err)
	}
	defer func() { _ = held.Release() }()

	code, _, stderr := awa(t, root, "gc")
	if code != 6 {
		t.Fatalf("gc exit = %d, want 6 (lock timeout); stderr=%q", code, stderr)
	}
}

// TestRunScanSerializesWithCollector proves a run's input scan — which writes the
// worktree index — is covered by the presence lock: while a collector holds the
// exclusive lock, even a would-be cache hit stands down and times out (exit 6) rather
// than writing the index next to a concurrent index rebuild.
func TestRunScanSerializesWithCollector(t *testing.T) {
	root := initProject(t)
	if err := os.WriteFile(filepath.Join(root, ".awa", "config.toml"),
		[]byte("[locks]\ntimeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, root, "a.txt", "aaa")
	h := helper(t)
	// Seed a cache entry so the next invocation is a hit — which still scans and so
	// still writes the index.
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); code != 0 {
		t.Fatalf("seeding run: %d %q", code, stderr)
	}

	locksDir := filepath.Join(root, ".awa", "locks")
	held, err := lockfile.AcquireExclusive(context.Background(), root, locksDir, lockfile.CollectorLockName, lockfile.OpGC, lockfile.Owner{})
	if err != nil {
		t.Fatalf("hold collector lock: %v", err)
	}
	defer func() { _ = held.Release() }()

	if code, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); code != 6 {
		t.Fatalf("run exit = %d, want 6 (stood down for the held collector); stderr=%q", code, stderr)
	}
}

// TestDoctorRepairExcludesActiveWriter proves doctor --repair, like gc, does not
// mutate the store while a writer is active: it cannot reach exclusive access, exits
// with the lock-timeout code, and removes nothing; once the writer is gone it
// repairs normally.
func TestDoctorRepairExcludesActiveWriter(t *testing.T) {
	root := initProject(t)
	if err := os.WriteFile(filepath.Join(root, ".awa", "config.toml"),
		[]byte("[locks]\ntimeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A repairable leftover: a stale orphan temp, backdated past the stale threshold.
	orphan := filepath.Join(root, ".awa", "store", "tmp", ".orphan-xyz")
	if err := os.WriteFile(orphan, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	// An active writer blocks the repair from reaching exclusive access.
	lockPath := writeActiveLock(t, root, lockfile.OpCheckpoint)
	code, _, stderr := awa(t, root, "doctor", "--repair")
	if code != 6 {
		t.Fatalf("doctor --repair with active writer exit = %d, want 6; stderr=%q", code, stderr)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan temp must survive a deferred repair: %v", err)
	}

	// With the writer gone, the repair proceeds and removes the leftover.
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := awa(t, root, "doctor", "--repair"); code != 0 {
		t.Fatalf("doctor --repair after writer gone exit = %d, stderr=%q", code, stderr)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan temp should be removed after repair: %v", err)
	}
}

// TestConfigEffectiveExposesLocksTimeout proves the [locks].timeout setting is
// surfaced in the agent-facing config envelope.
func TestConfigEffectiveExposesLocksTimeout(t *testing.T) {
	root := initProject(t)
	code, stdout, _ := awa(t, root, "config", "effective", "--json")
	if code != 0 {
		t.Fatalf("config effective --json exit = %d", code)
	}
	data := assertEnvelope(t, stdout, "config.effective")
	var view struct {
		Locks struct {
			Timeout string `json:"timeout"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(data, &view); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if view.Locks.Timeout == "" {
		t.Fatalf("locks.timeout missing from config effective JSON:\n%s", stdout)
	}
}

// mutateID returns an id-shaped string distinct from id by flipping its last hex
// character, so the orphan directory looks like a real checkpoint id.
func mutateID(id string) string {
	if id == "" {
		return "deadbeef"
	}
	last := id[len(id)-1]
	repl := byte('0')
	if last == '0' {
		repl = '1'
	}
	return id[:len(id)-1] + string(repl)
}
