package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcceptanceReviewUX exercises the review surfaces end to end: the
// changes/diff context header, the compact run footer, run ls --near with a
// changed-path sample and advisory hint, run explain reasons consistent with the
// near miss, and the status review dashboard.
func TestAcceptanceReviewUX(t *testing.T) {
	root := initProject(t)
	write(t, root, "src/example.txt", "ok\n")
	writeExec(t, root, "validate", "#!/bin/sh\nexit 0\n")

	if code, _, stderr := awa(t, root, "checkpoint", "-m", "specific review checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	// changes --stat identifies its baseline/range and the checkpoint message.
	code, stdout, _ := awa(t, root, "changes", "--stat")
	if code != 0 {
		t.Fatalf("changes --stat exit = %d", code)
	}
	if !strings.Contains(stdout, "range:") || !strings.Contains(stdout, "-> now") {
		t.Errorf("changes --stat missing range header:\n%s", stdout)
	}
	if !strings.Contains(stdout, "specific review checkpoint") {
		t.Errorf("changes --stat missing checkpoint message:\n%s", stdout)
	}

	// A validation run prints the compact footer: state, run id, cwd, exit, and the
	// inspection command.
	code, _, stderr := awa(t, root, "run", "--display", "tail:20", "--", "./validate")
	if code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"awa run: stored", "exit:     0", "run:      ", "cwd:      ", "inspect:  awa run show"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("run footer missing %q:\n%s", want, stderr)
		}
	}

	// Change a real source file and add a generated-looking artifact.
	write(t, root, "src/example.txt", "ok\nchange\n")
	write(t, root, "obj/generated.cache", "artifact")

	// diff src/ shows the range and the path filter.
	code, stdout, _ = awa(t, root, "diff", "src/")
	if code != 0 {
		t.Fatalf("diff src/ exit = %d", code)
	}
	if !strings.Contains(stdout, "range:") || !strings.Contains(stdout, "paths: src") {
		t.Errorf("diff src/ missing range/path header:\n%s", stdout)
	}

	// Default run ls does not claim a stale run is reusable.
	code, stdout, _ = awa(t, root, "run", "ls")
	if code != 0 {
		t.Fatalf("run ls exit = %d", code)
	}
	if strings.Contains(stdout, "  reusable") {
		t.Errorf("run ls claimed a stale run reusable:\n%s", stdout)
	}

	// run ls --near shows the earlier validation as a near miss explaining that the
	// input tree differs, with a bounded changed-path sample and an advisory hint.
	code, stdout, nearErr := awa(t, root, "run", "ls", "--near")
	if code != 0 {
		t.Fatalf("run ls --near exit = %d", code)
	}
	all := stdout + nearErr
	for _, want := range []string{
		"reusable now:",
		"near misses:",
		"not-reusable(input-tree-differs)",
		"changed inputs",
		"src/example.txt",
		"hint:",
		"extra_excludes",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("run ls --near missing %q:\n%s", want, all)
		}
	}
	// A near miss must never read as a cache hit.
	if strings.Contains(stdout, "reusable(input-tree-differs)") && !strings.Contains(stdout, "not-reusable(input-tree-differs)") {
		t.Errorf("near miss rendered as a hit:\n%s", stdout)
	}

	// --near --json emits the machine-readable near-miss object: the same reason,
	// changed-path sample, and hint the human view shows. The corrupt warning (if any)
	// goes to stderr; stdout stays a single JSON document.
	code, njStdout, njErr := awa(t, root, "run", "ls", "--near", "--json")
	if code != 0 {
		t.Fatalf("run ls --near --json exit = %d, stderr=%q", code, njErr)
	}
	var nearEnv struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Near       bool `json:"near"`
			NearMisses []struct {
				ShortID      string `json:"short_id"`
				Reason       string `json:"reason"`
				ChangedPaths struct {
					Completeness string `json:"completeness"`
					Total        int    `json:"total"`
					Items        []struct {
						Path   string `json:"path"`
						Status string `json:"status"`
					} `json:"items"`
				} `json:"changed_paths"`
				Hints []struct {
					Message string `json:"message"`
				} `json:"hints"`
			} `json:"near_misses"`
			Corrupt int `json:"corrupt"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(njStdout), &nearEnv); err != nil {
		t.Fatalf("invalid run ls --near JSON: %v\n%s", err, njStdout)
	}
	if nearEnv.SchemaVersion != 1 || nearEnv.Command != "run.ls" || !nearEnv.Data.Near {
		t.Errorf("near envelope = schema %d command %q near %v, want 1/run.ls/true",
			nearEnv.SchemaVersion, nearEnv.Command, nearEnv.Data.Near)
	}
	if len(nearEnv.Data.NearMisses) == 0 {
		t.Fatalf("run ls --near --json reported no near misses:\n%s", njStdout)
	}
	nm := nearEnv.Data.NearMisses[0]
	if nm.Reason != "input-tree-differs" {
		t.Errorf("near miss reason = %q, want input-tree-differs", nm.Reason)
	}
	if nm.ShortID == "" {
		t.Errorf("near miss missing short_id:\n%s", njStdout)
	}
	if nm.ChangedPaths.Completeness != "complete" && nm.ChangedPaths.Completeness != "truncated" {
		t.Errorf("near miss changed_paths completeness = %q, want an available sample:\n%s", nm.ChangedPaths.Completeness, njStdout)
	}
	var sawExample bool
	for _, it := range nm.ChangedPaths.Items {
		if it.Path == "src/example.txt" {
			sawExample = true
		}
	}
	if !sawExample {
		t.Errorf("changed_paths missing src/example.txt:\n%s", njStdout)
	}
	if len(nm.Hints) == 0 {
		t.Errorf("near miss missing advisory hint:\n%s", njStdout)
	}

	// run explain reports a reason consistent with run ls --near.
	code, exStdout, exStderr := awa(t, root, "run", "explain", "--", "./validate")
	if code != 0 {
		t.Fatalf("run explain exit = %d", code)
	}
	if !strings.Contains(exStdout+exStderr, "input tree differs") {
		t.Errorf("run explain reason inconsistent with near miss:\n%s", exStdout)
	}

	// status is the review dashboard: latest checkpoint + message, dirty summary since
	// it, reusable/near run state, and suggested next commands.
	code, statusOut, statusErr := awa(t, root, "status")
	if code != 0 {
		t.Fatalf("status exit = %d", code)
	}
	dash := statusOut + statusErr
	for _, want := range []string{
		"checkpoint:",
		"specific review checkpoint",
		"dirty:",
		"near miss",
		"next:",
	} {
		if !strings.Contains(dash, want) {
			t.Errorf("status dashboard missing %q:\n%s", want, dash)
		}
	}

	// status --json carries the same review facts the human dashboard shows: the
	// checkpoint and its message, the dirty summary, the near-miss count and nearest,
	// and the suggested follow-up commands — no text parsing required.
	code, sjStdout, sjErr := awa(t, root, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json exit = %d, stderr=%q", code, sjErr)
	}
	var sEnv struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Initialized bool `json:"initialized"`
			Review      struct {
				Checkpoint struct {
					Present bool   `json:"present"`
					ShortID string `json:"short_id"`
					Message string `json:"message"`
				} `json:"checkpoint"`
				Dirty struct {
					Computed bool `json:"computed"`
					Dirty    bool `json:"dirty"`
				} `json:"dirty"`
				Runs struct {
					Reusable int `json:"reusable"`
					Near     int `json:"near"`
					Nearest  *struct {
						Reason string `json:"reason"`
					} `json:"nearest"`
				} `json:"runs"`
				Next []struct {
					Kind string   `json:"kind"`
					Argv []string `json:"argv"`
				} `json:"next"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(sjStdout), &sEnv); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, sjStdout)
	}
	if sEnv.SchemaVersion != 1 || sEnv.Command != "status" || !sEnv.Data.Initialized {
		t.Errorf("status envelope = schema %d command %q init %v, want 1/status/true",
			sEnv.SchemaVersion, sEnv.Command, sEnv.Data.Initialized)
	}
	rv := sEnv.Data.Review
	if !rv.Checkpoint.Present || rv.Checkpoint.Message != "specific review checkpoint" {
		t.Errorf("review checkpoint = %+v, want present with the checkpoint message", rv.Checkpoint)
	}
	if !rv.Dirty.Computed || !rv.Dirty.Dirty {
		t.Errorf("review dirty = %+v, want computed and dirty", rv.Dirty)
	}
	if rv.Runs.Near == 0 || rv.Runs.Nearest == nil || rv.Runs.Nearest.Reason != "input-tree-differs" {
		t.Errorf("review runs = %+v, want a near miss with input-tree-differs nearest", rv.Runs)
	}
	// next is machine-actionable: a near-miss state suggests explaining a stale check,
	// as a typed entry with a tokenized argv, not a human string to parse.
	var sawExplain bool
	for _, n := range rv.Next {
		if n.Kind == "run-explain" && len(n.Argv) > 0 && n.Argv[0] == "awa" {
			sawExplain = true
		}
	}
	if !sawExplain {
		t.Errorf("review next = %+v, want a run-explain suggestion with argv", rv.Next)
	}
}

// writeExec writes an executable script file under the project root.
func writeExec(t *testing.T, root, name, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
