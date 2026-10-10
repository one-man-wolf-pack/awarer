package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/infra/lockfile"
)

func headerlessCheckpointDir(t *testing.T, root string) string {
	t.Helper()
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "to be left headerless"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	dirs, err := filepath.Glob(filepath.Join(root, ".awa", "checkpoints", "*", "header.json"))
	if err != nil || len(dirs) != 1 {
		t.Fatalf("want one committed checkpoint, got %v (%v)", dirs, err)
	}
	if err := os.Remove(dirs[0]); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dirs[0])
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDoctorRepairLockWaitCancellationIsInterruptionAndTimeoutIsNot(t *testing.T) {
	root := initProject(t)
	remnant := headerlessCheckpointDir(t, root)
	before := entryNames(t, remnant)
	locksDir := filepath.Join(root, ".awa", "locks")
	writer, err := lockfile.AcquirePresence(context.Background(), root, locksDir, lockfile.OpCheckpoint, lockfile.Owner{})
	if err != nil {
		t.Fatalf("AcquirePresence: %v", err)
	}
	defer func() { _ = writer.Release() }()

	assertUntouched := func() {
		t.Helper()
		after := entryNames(t, remnant)
		if len(after) != len(before) {
			t.Fatalf("remnant changed without exclusive access: %v, was %v", after, before)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan bool, 1)
	go func() {
		// The repair takes the collector lock before it waits on writers, so the lock
		// file marks the point where a cancellation lands in that wait.
		seen := false
		for deadline := time.Now().Add(10 * time.Second); !seen && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			_, err := os.Stat(filepath.Join(locksDir, lockfile.CollectorLockName))
			seen = err == nil
		}
		cancel()
		waiting <- seen
	}()
	code, _, stderr := runCtx(ctx, "doctor", "--repair", "--root", root)
	if !<-waiting {
		t.Fatal("the repair never reached its wait for the writer; the cancellation did not land in it")
	}
	if code != int(ExitInterrupted) {
		t.Fatalf("cancelled lock wait exit = %d, want ExitInterrupted (%d); stderr = %q", code, ExitInterrupted, stderr)
	}
	assertUntouched()

	if err := os.WriteFile(filepath.Join(root, ".awa", "config.toml"), []byte("[locks]\ntimeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("doctor", "--repair", "--root", root); code != int(ExitLockTimeout) {
		t.Fatalf("expired lock wait exit = %d, want ExitLockTimeout (%d); stderr = %q", code, ExitLockTimeout, stderr)
	}
	assertUntouched()
}
