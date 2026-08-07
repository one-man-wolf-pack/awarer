package acceptance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awarer/internal/infra/lockfile"
)

// cmdResult is one concurrent invocation's outcome.
type cmdResult struct {
	code   int
	stdout string
	stderr string
}

// awaCmd runs the built binary without touching *testing.T, so it is safe to call
// from concurrent goroutines (t.Fatalf may only be called from the test goroutine).
// A non-exit failure (the binary could not run at all) is reported as code -1 with
// the error appended to stderr.
func awaCmd(dir string, args ...string) cmdResult {
	cmd := exec.Command(awaBin, args...)
	cmd.Dir = dir
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return cmdResult{code: -1, stdout: out.String(), stderr: errBuf.String() + err.Error()}
		}
	}
	return cmdResult{code: code, stdout: out.String(), stderr: errBuf.String()}
}

// runConcurrent launches n goroutines that each call fn(i), releasing them as
// close together as possible behind a barrier (no sleeps), and returns the results
// indexed by goroutine. Results are written to a preallocated slice — no shared map
// — so the orchestration is race-clean under `go test -race`.
func runConcurrent(t *testing.T, n int, fn func(i int) cmdResult) []cmdResult {
	t.Helper()
	results := make([]cmdResult, n)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start // all goroutines block here until released together
			results[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results
}

// writeActiveLock plants a recognized presence lock owned by this live test process
// (this host, this pid), so the lock classifier treats it as active. It crosses the
// same Encode boundary writers use, so doctor and gc parse it exactly as a real one.
func writeActiveLock(t *testing.T, root string, op lockfile.Operation) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	rec := lockfile.Record{
		Operation: string(op),
		PID:       os.Getpid(),
		Hostname:  host,
		CreatedAt: time.Now().UTC(),
	}
	data, err := lockfile.Encode(rec)
	if err != nil {
		t.Fatalf("encode lock: %v", err)
	}
	locks := filepath.Join(root, ".awa", "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	name := string(op) + "-fixture.lock"
	path := filepath.Join(locks, name)
	if err := os.WriteFile(path, data, 0o444); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	return path
}

// assertEnvelope checks the JSON envelope's schema_version and command, returning
// the raw data payload for command-specific decoding.
func assertEnvelope(t *testing.T, stdout, wantCommand string) json.RawMessage {
	t.Helper()
	var env struct {
		SchemaVersion int             `json:"schema_version"`
		Command       string          `json:"command"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("envelope decode: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", env.SchemaVersion)
	}
	if env.Command != wantCommand {
		t.Fatalf("command = %q, want %q", env.Command, wantCommand)
	}
	return env.Data
}
