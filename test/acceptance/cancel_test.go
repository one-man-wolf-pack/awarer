package acceptance

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startAwa starts the built binary in dir without waiting, so a test can signal it
// mid-run. It returns the command and the buffers its streams are captured into.
func startAwa(t *testing.T, dir string, args ...string) (*exec.Cmd, *strings.Builder, *strings.Builder) {
	t.Helper()
	cmd := exec.Command(awaBin, args...)
	cmd.Dir = dir
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start awa %v: %v", args, err)
	}
	return cmd, &out, &errBuf
}

// waitCode waits for cmd and returns its exit code, failing on a non-exit error.
func waitCode(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("waiting for awa: %v", err)
	return -1
}

// TestRunSIGINTCancelsChildLeavesNoReusableEntry proves the root-cancellation
// contract for `awa run`: a SIGINT delivered to the awa process while a child is
// running cancels the run through the root context — the child is stopped and the
// command exits non-zero. The interrupted run may be recorded as a non-reusable
// post-scan-failed history entry (honest evidence that an attempt ran), but it publishes
// no cache pointer, so a later run of the same command is a fresh miss, never a hit
// replayed from interrupted work, and doctor reports the store as clean.
func TestRunSIGINTCancelsChildLeavesNoReusableEntry(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")

	// A long-running child so the interrupt lands mid-run. Signal only the awa process
	// (not the child's group), exactly as a caller pressing Ctrl+C in awa's own shell
	// would reach awa; awa must then propagate cancellation to the child.
	cmd, out, errBuf := startAwa(t, root, "run", "--", h, "-out", "done", "-sleep", "3000")
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := waitCode(t, cmd); code == 0 {
		t.Fatalf("interrupted run exited 0; stdout=%q stderr=%q", out.String(), errBuf.String())
	}

	// Re-run the identical command to completion. If the interrupted run had leaked an
	// authoritative reusable entry, this would replay as a hit; it must be a miss.
	code, _, stderr := awa(t, root, "run", "--", h, "-out", "done", "-sleep", "3000")
	if code != 0 {
		t.Fatalf("re-run exit = %d, stderr = %q", code, stderr)
	}
	if isHit(stderr) {
		t.Errorf("re-run after interrupt was a cache hit; the incomplete run leaked authoritative state\nstderr=%q", stderr)
	}

	// The store must be clean/recoverable after the interruption — no dangling
	// authoritative state, only (at most) reclaimable leftovers doctor tolerates.
	if dcode, _, dstderr := awa(t, root, "doctor"); dcode != 0 {
		t.Errorf("doctor after interrupt exit = %d, stderr = %q", dcode, dstderr)
	}
}
