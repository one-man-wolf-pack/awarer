package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGcUsageErrors(t *testing.T) {
	root := initProject(t)
	cases := []struct {
		name string
		args []string
	}{
		{"two only-filters", []string{"gc", "--root", root, "--runs-only", "--blobs-only"}},
		{"bad keep-last", []string{"gc", "--root", root, "--keep-last", "-3"}},
		{"non-numeric keep-last", []string{"gc", "--root", root, "--keep-last", "lots"}},
		{"bad older-than", []string{"gc", "--root", root, "--older-than", "soon"}},
		{"unknown flag", []string{"gc", "--root", root, "--nope"}},
		{"unexpected operand", []string{"gc", "--root", root, "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, _ := run(tc.args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d (usage)", code, ExitUsageError)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on usage error", stdout)
			}
		})
	}
}

// checkpointFile writes a worktree file and records a checkpoint through the CLI.
func checkpointFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
}

func TestGcCleanProjectNoOp(t *testing.T) {
	root := initProject(t)
	checkpointFile(t, root, "a.go", "package a")

	code, stdout, stderr := run("gc", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("gc exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "nothing to remove") {
		t.Fatalf("expected no-op message, got %q", stdout)
	}
}

func TestGcDryRunJSON(t *testing.T) {
	root := initProject(t)
	checkpointFile(t, root, "a.go", "package a")
	checkpointFile(t, root, "b.go", "package b") // second checkpoint; first becomes deletable at keep-last 0

	code, stdout, stderr := run("gc", "--root", root, "--keep-last", "0", "--dry-run", "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("gc exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		SchemaVersion int             `json:"schema_version"`
		Command       string          `json:"command"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "gc" {
		t.Fatalf("envelope = %+v", env)
	}
	var data gcView
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("invalid data: %v", err)
	}
	if !data.DryRun {
		t.Errorf("dry_run = false, want true")
	}
	if data.Summary.PlannedDelete == 0 {
		t.Errorf("expected at least one planned delete with keep-last 0 and two checkpoints")
	}
	if data.Candidates == nil {
		t.Errorf("candidates must be an array, not null")
	}

	// Dry-run must not have mutated anything: a second identical dry-run sees the same
	// planned count.
	_, stdout2, _ := run("gc", "--root", root, "--keep-last", "0", "--dry-run", "--json")
	var env2 struct {
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal([]byte(stdout2), &env2)
	var data2 gcView
	_ = json.Unmarshal(env2.Data, &data2)
	if data2.Summary.PlannedDelete != data.Summary.PlannedDelete {
		t.Errorf("dry-run mutated state: planned %d then %d", data.Summary.PlannedDelete, data2.Summary.PlannedDelete)
	}
}

func TestGcPartialFailureExitsNonZero(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := initProject(t)
	// A stale temp artifact GC will try to remove, in a temp dir we then make
	// read-only so the unlink fails.
	tmpDir := filepath.Join(root, ".awa", "store", "tmp")
	stale := filepath.Join(tmpDir, "blob-stale")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmpDir, 0o755) })

	// JSON form: a partial failure must surface in the envelope and exit non-zero.
	code, stdout, _ := run("gc", "--root", root, "--json")
	if code != int(ExitGenericError) {
		t.Fatalf("exit = %d, want %d on partial failure", code, ExitGenericError)
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	var data gcView
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("invalid data: %v", err)
	}
	if data.Summary.Failed != 1 || len(data.Failures) != 1 {
		t.Fatalf("expected one failure in JSON, got failed=%d failures=%d", data.Summary.Failed, len(data.Failures))
	}
	if data.Failures[0].Kind != "temp" || data.Failures[0].Error == "" {
		t.Fatalf("failure record incomplete: %+v", data.Failures[0])
	}
}

func TestGcRemovesOldCheckpointAndDoctorClean(t *testing.T) {
	root := initProject(t)
	checkpointFile(t, root, "a.go", "package a")
	checkpointFile(t, root, "b.go", "package b")

	code, stdout, stderr := run("gc", "--root", root, "--keep-last", "0")
	if code != int(ExitSuccess) {
		t.Fatalf("gc exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "removed") || !strings.Contains(stdout, "freed:") {
		t.Fatalf("expected a removal summary, got %q", stdout)
	}

	// doctor must report a healthy project after gc.
	dcode, dout, derr := run("doctor", "--root", root)
	if dcode != int(ExitSuccess) {
		t.Fatalf("doctor after gc exit = %d, stdout=%q stderr=%q", dcode, dout, derr)
	}
	if !strings.Contains(dout, "health: ok") {
		t.Fatalf("doctor not healthy after gc: %q", dout)
	}
}
