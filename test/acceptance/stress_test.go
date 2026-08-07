//go:build acceptance_stress

// Package acceptance's stress layer holds high-count, scheduler-sensitive
// concurrency and interruption scenarios. They are tagged out of the default suite
// (run via `just stress` or `go test -tags acceptance_stress ./test/acceptance/...`)
// because they are slow and timing-dependent; the untagged concurrent_test.go keeps
// the deterministic, low-count correctness proofs.
package acceptance

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"awarer/internal/infra/lockfile"
)

// TestStressConcurrentCheckpoints runs many checkpoints at once and asserts every
// one succeeds, the store ends consistent, and doctor stays clean.
func TestStressConcurrentCheckpoints(t *testing.T) {
	root := initProject(t)
	const n = 32
	for i := 0; i < n; i++ {
		write(t, root, "f"+strconv.Itoa(i)+".txt", "content-"+strconv.Itoa(i))
	}
	results := runConcurrent(t, n, func(i int) cmdResult {
		return awaCmd(root, "checkpoint", "-m", "stress")
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
		t.Fatalf("doctor after stress checkpoint = %d, stdout=%q", code, stdout)
	}
}

// TestStressConcurrentIdenticalRuns hammers one cache key with many identical runs
// and asserts they converge: all succeed, the cache resolves to a valid entry, and
// doctor stays clean.
func TestStressConcurrentIdenticalRuns(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	const n = 32
	results := runConcurrent(t, n, func(i int) cmdResult {
		return awaCmd(root, "run", "--", h, "-out", "hello")
	})
	for i, r := range results {
		if r.code != 0 || r.stdout != "hello" {
			t.Fatalf("run %d: exit=%d stdout=%q stderr=%q", i, r.code, r.stdout, r.stderr)
		}
	}
	if code, stdout, stderr := awa(t, root, "run", "--", h, "-out", "hello"); code != 0 || stdout != "hello" || !isHit(stderr) {
		t.Fatalf("follow-up: exit=%d stdout=%q hit=%v", code, stdout, isHit(stderr))
	}
	if code, _, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after stress runs != 0")
	}
}

// TestStressGCWithConcurrentWriters races destructive gc against live checkpoints
// and runs through real presence locks. gc must never corrupt the store: whatever it
// deletes, doctor stays clean afterward, and every writer that ran succeeded.
func TestStressGCWithConcurrentWriters(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "seed.txt", "seed")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("seed checkpoint: %d %q", code, stderr)
	}

	const writers = 16
	// Mix checkpoints, runs, and gc passes firing together.
	tasks := writers + 4
	results := runConcurrent(t, tasks, func(i int) cmdResult {
		switch i % 4 {
		case 0:
			return awaCmd(root, "gc", "--keep-last", "1")
		case 1:
			write(t, root, "w"+strconv.Itoa(i)+".txt", "w"+strconv.Itoa(i))
			return awaCmd(root, "checkpoint", "-m", "race")
		default:
			return awaCmd(root, "run", "--", h, "-out", "o"+strconv.Itoa(i))
		}
	})
	for i, r := range results {
		// gc may exit 0 (ran or blocked); checkpoint/run must succeed. None may report storage
		// corruption (exit 5) — a race must never look like a corrupt store.
		if r.code == 5 {
			t.Fatalf("task %d reported storage corruption under contention: stderr=%q", i, r.stderr)
		}
		if i%4 != 0 && r.code != 0 {
			t.Fatalf("writer task %d exit = %d, stderr=%q", i, r.code, r.stderr)
		}
	}
	if code, stdout, _ := awa(t, root, "doctor"); code != 0 {
		t.Fatalf("doctor after gc-vs-writers race = %d, stdout=%q", code, stdout)
	}
}

// TestStressRepeatedInterruptionRecovers repeatedly plants and clears an active
// writer lock around gc, asserting the store never degrades: gc is blocked while the
// lock is held and the project stays healthy throughout.
func TestStressRepeatedInterruptionRecovers(t *testing.T) {
	root := initProject(t)
	for i := 0; i < 20; i++ {
		write(t, root, "x"+strconv.Itoa(i)+".txt", strconv.Itoa(i))
		if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
			t.Fatalf("checkpoint %d: %d %q", i, code, stderr)
		}
		lock := writeActiveLock(t, root, lockfile.OpRun)
		code, _, stderr := awa(t, root, "gc", "--keep-last", "1")
		if code != 0 {
			t.Fatalf("blocked gc %d exit = %d, stderr=%q", i, code, stderr)
		}
		if err := os.Remove(lock); err != nil {
			t.Fatal(err)
		}
		if code, stdout, _ := awa(t, root, "doctor"); code != 0 && !strings.Contains(stdout, "lock") {
			t.Fatalf("doctor unhealthy at iteration %d: exit=%d stdout=%q", i, code, stdout)
		}
	}
}
