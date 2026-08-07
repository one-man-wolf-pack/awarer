package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// checkpointProject initializes a project, writes calc.go, and records one checkpoint.
func checkpointProject(t *testing.T) string {
	t.Helper()
	root := initProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	return root
}

func TestChangesDefaultRangeReportsModified(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n")
	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "M calc/calc.go") {
		t.Errorf("stdout = %q, want M calc/calc.go", stdout)
	}
}

func TestChangesNoCheckpointsIsNotFound(t *testing.T) {
	root := initProject(t)
	if code, _, _ := run("changes", "--root", root); code != int(ExitNotFound) {
		t.Errorf("changes with no checkpoints exit = %d, want %d", code, ExitNotFound)
	}
}

func TestChangesJSONSchema(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n")
	code, stdout, stderr := run("changes", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("changes --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Left    map[string]any `json:"left"`
			Right   map[string]any `json:"right"`
			Summary map[string]int `json:"summary"`
			Changes []any          `json:"changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "changes" {
		t.Errorf("envelope = %+v", env)
	}
	if env.Data.Right["kind"] != "now" || env.Data.Left["kind"] != "checkpoint" {
		t.Errorf("state kinds wrong: left=%v right=%v", env.Data.Left["kind"], env.Data.Right["kind"])
	}
}

// TestChangesJSONReportsRenameDetection pins the rename-detection contract in
// JSON: the structural rename_detection fact reports whether pairing was requested and
// applied, so an agent reads the presentation policy without parsing prose. The two
// reachable states are covered: default (attempted + applied) and --no-renames
// (not attempted). The limit-exceeded state is unit-tested where the fact is produced
// (compare) and mapped (toRenameDetectionView), since triggering it end-to-end would
// need a pathologically huge change set.
func TestChangesJSONReportsRenameDetection(t *testing.T) {
	unmarshalRD := func(t *testing.T, stdout string) map[string]any {
		t.Helper()
		var env struct {
			Data struct {
				RenameDetection map[string]any `json:"rename_detection"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, stdout)
		}
		return env.Data.RenameDetection
	}

	// A rename between baseline and now: old path gone, identical content at a new path.
	root := checkpointProject(t)
	if err := os.Remove(filepath.Join(root, "calc", "calc.go")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	writeFile(t, root, "calc/renamed.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")

	code, stdout, stderr := run("changes", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("changes --json exit = %d, stderr = %q", code, stderr)
	}
	rd := unmarshalRD(t, stdout)
	if rd == nil {
		t.Fatal("rename_detection missing from changes --json")
	}
	if rd["attempted"] != true || rd["applied"] != true {
		t.Errorf("default rename_detection = %v, want attempted+applied", rd)
	}
	if _, hasReason := rd["reason"]; hasReason {
		t.Errorf("applied rename_detection must omit reason, got %v", rd)
	}

	code, stdout, stderr = run("changes", "--root", root, "--json", "--no-renames")
	if code != int(ExitSuccess) {
		t.Fatalf("changes --json --no-renames exit = %d, stderr = %q", code, stderr)
	}
	rd = unmarshalRD(t, stdout)
	if rd["attempted"] != false || rd["applied"] != false {
		t.Errorf("--no-renames rename_detection = %v, want not attempted", rd)
	}
}

// TestChangesDiffJSONExposesBaselineFacts pins the JSON contract: the
// resolved left (baseline) side of changes --json and diff --json carries the
// checkpoint_id, message, and created_at, so an agent learns which
// checkpoint a comparison ran against without parsing the human header. changes and
// diff are separate surfaces, so both are asserted.
func TestChangesDiffJSONExposesBaselineFacts(t *testing.T) {
	for _, cmd := range []string{"changes", "diff"} {
		root := initProject(t)
		writeFile(t, root, "calc/calc.go", "package calc\n")
		id := checkpointWithMessage(t, root, "baseline for JSON contract")
		writeFile(t, root, "calc/calc.go", "package calc\n// edited\n")

		code, stdout, stderr := run(cmd, "--root", root, "--json")
		if code != int(ExitSuccess) {
			t.Fatalf("%s --json exit = %d, stderr = %q", cmd, code, stderr)
		}
		var env struct {
			Data struct {
				Left struct {
					Kind         string `json:"kind"`
					CheckpointID string `json:"checkpoint_id"`
					Message      string `json:"message"`
					CreatedAt    string `json:"created_at"`
				} `json:"left"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &env); err != nil {
			t.Fatalf("%s --json invalid JSON: %v\n%s", cmd, err, stdout)
		}
		left := env.Data.Left
		if left.Kind != "checkpoint" {
			t.Errorf("%s --json left.kind = %q, want checkpoint", cmd, left.Kind)
		}
		if left.CheckpointID != id {
			t.Errorf("%s --json left.checkpoint_id = %q, want %q", cmd, left.CheckpointID, id)
		}
		if left.Message != "baseline for JSON contract" {
			t.Errorf("%s --json left.message = %q, want the checkpoint message", cmd, left.Message)
		}
		// created_at must be a machine-readable timestamp, not a human age like
		// "just now": an agent relies on parsing it as RFC3339Nano.
		if _, err := time.Parse(time.RFC3339Nano, left.CreatedAt); err != nil {
			t.Errorf("%s --json left.created_at = %q is not RFC3339Nano: %v", cmd, left.CreatedAt, err)
		}
	}
}

// TestJSONRejectsHumanShapingFlags pins the contract, applied uniformly across the
// CLI, that a human text-shaping flag (--stat, --name-only, --oneline) is not silently
// ignored under --json: combining them is a usage error with no JSON on stdout,
// because the JSON document already carries the structured data those flags reshape.
func TestJSONRejectsHumanShapingFlags(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n// edited\n")
	cases := []struct {
		name string
		args []string
	}{
		{"changes stat json", []string{"changes", "--stat", "--json", "--root", root}},
		{"changes name-only json", []string{"changes", "--name-only", "--json", "--root", root}},
		{"diff stat json", []string{"diff", "--stat", "--json", "--root", root}},
		{"log oneline json", []string{"log", "--oneline", "--json", "--root", root}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(tc.args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d (usage error); stderr=%q", code, ExitUsageError, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty (no JSON emitted)", stdout)
			}
			if !strings.Contains(stderr, "human output mode") {
				t.Errorf("stderr = %q, want it to explain the flag is a human output mode", stderr)
			}
		})
	}
}

func TestDiffJSONSchema(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n// changed\n")
	code, stdout, _ := run("diff", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("diff --json exit = %d", code)
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Files []struct {
				Availability string `json:"availability"`
				Diff         string `json:"diff"`
			} `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "diff" {
		t.Errorf("envelope wrong: %+v", env)
	}
	if len(env.Data.Files) != 1 || env.Data.Files[0].Availability != "text" || env.Data.Files[0].Diff == "" {
		t.Errorf("files = %+v", env.Data.Files)
	}
}

func TestChangesModeOnlyChangeIsStructural(t *testing.T) {
	root := checkpointProject(t)
	// chmod without touching content: a metadata-only change. It must be carried
	// structurally on its change element — a normal modify with the mode fields and
	// content_diff_available=false — never dressed up as degraded/unavailable content.
	if err := os.Chmod(filepath.Join(root, "calc", "calc.go"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	code, stdout, _ := run("changes", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("changes --json exit = %d", code)
	}
	var env struct {
		Data struct {
			Changes []struct {
				Status               string `json:"status"`
				ContentDiffAvailable bool   `json:"content_diff_available"`
				OldMode              string `json:"old_mode"`
				NewMode              string `json:"new_mode"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if len(env.Data.Changes) != 1 {
		t.Fatalf("want one change, got %+v", env.Data.Changes)
	}
	ch := env.Data.Changes[0]
	// The mode change is carried structurally and must not claim content-diff
	// availability.
	if ch.Status != "modified" || ch.ContentDiffAvailable {
		t.Errorf("mode-only change = %+v", ch)
	}
	if ch.OldMode != "0644" || ch.NewMode != "0755" {
		t.Errorf("mode fields = %q -> %q, want 0644 -> 0755", ch.OldMode, ch.NewMode)
	}
}

func TestChangesFlagErrors(t *testing.T) {
	root := checkpointProject(t)
	cases := [][]string{
		{"changes", "--root", root, "--stat", "--name-only"},
		{"changes", "--root", root, "--bogus"},
		{"changes", "--root", root, "-1", "@-2..@-1"},
		{"changes", "--root", root, "a..b..c"},
		{"changes", "--root", root, "--stat=1"},
		{"diff", "--root", root, "--context", "x"},
		{"changes", "--root", root, "/etc"},
	}
	for _, args := range cases {
		if code, _, _ := run(args...); code != int(ExitUsageError) {
			t.Errorf("%v exit = %d, want %d", args, code, ExitUsageError)
		}
	}
}

func TestChangesStatAndNameOnly(t *testing.T) {
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n// edited\n")
	writeFile(t, root, "new.go", "package main\n")
	if code, stdout, _ := run("changes", "--root", root, "--stat"); code != int(ExitSuccess) || !strings.Contains(stdout, "changed:") {
		t.Errorf("--stat stdout = %q", stdout)
	}
	code, stdout, _ := run("changes", "--root", root, "--name-only")
	if code != int(ExitSuccess) {
		t.Fatalf("--name-only exit = %d", code)
	}
	if strings.Contains(stdout, "M ") || strings.Contains(stdout, "A ") {
		t.Errorf("--name-only should print only paths, got %q", stdout)
	}
	if !strings.Contains(stdout, "calc/calc.go") || !strings.Contains(stdout, "new.go") {
		t.Errorf("--name-only missing paths: %q", stdout)
	}
}

// corruptCheckpointLastManifestLine rewrites the last record of the only checkpoint's
// manifest.jsonl to malformed JSON, so a reader fails on that record mid-stream while
// the earlier records still decode. It keeps the line count unchanged, so the failure
// is a decode error at that line, not a count mismatch.
func corruptCheckpointLastManifestLine(t *testing.T, root string) {
	t.Helper()
	checkpointsDir := filepath.Join(root, ".awa", "checkpoints")
	entries, err := os.ReadDir(checkpointsDir)
	if err != nil {
		t.Fatalf("read checkpoints dir: %v", err)
	}
	var manifest string
	for _, e := range entries {
		if e.IsDir() {
			manifest = filepath.Join(checkpointsDir, e.Name(), "manifest.jsonl")
		}
	}
	if manifest == "" {
		t.Fatal("no checkpoint directory found to corrupt")
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("manifest has %d records, need at least 2 to corrupt the last", len(lines))
	}
	lines[len(lines)-1] = "{ this is not valid json"
	// Checkpoint files are published read-only (0o444); make it writable to inject the
	// corruption, then restore the immutable mode so the store looks untampered.
	if err := os.Chmod(manifest, 0o644); err != nil {
		t.Fatalf("chmod manifest: %v", err)
	}
	if err := os.WriteFile(manifest, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	if err := os.Chmod(manifest, 0o444); err != nil {
		t.Fatalf("restore manifest mode: %v", err)
	}
}

// TestChangesHumanPartialOutputOnCorruptManifest drives the real streaming path into a
// mid-stream failure: the baseline's last manifest record is corrupt, and an edit to
// the first file makes a change stream out before the reader reaches the bad record.
// The human command must exit non-zero, print the change it already emitted, and
// surface a "partial output:" diagnostic on stderr rather than a clean report.
//
// It uses --no-renames: rename detection must buffer and re-sort the whole change set
// (a delete can pair with an add anywhere), so with it on the corrupt record is hit
// while the cursor is built, before any output — a clean (non-partial) failure. The
// genuinely streaming, incrementally-printed path is --no-renames, which is what this
// partial-output contract is about.
func TestChangesHumanPartialOutputOnCorruptManifest(t *testing.T) {
	root := initProject(t)
	// Three ordered records: a.txt (edited, emits a change), b.txt (a spacer, since the
	// merge look-ahead prefetches the next record before yielding the current one), and
	// c.txt (the corrupt last record). This guarantees "M a.txt" is fully emitted before
	// the reader reaches the bad record.
	writeFile(t, root, "a.txt", "one\n")
	writeFile(t, root, "b.txt", "mid\n")
	writeFile(t, root, "c.txt", "two\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	corruptCheckpointLastManifestLine(t, root)
	writeFile(t, root, "a.txt", "one changed\n")

	code, stdout, stderr := run("changes", "--root", root, "--no-renames")
	if code == int(ExitSuccess) {
		t.Fatalf("changes over a corrupt manifest exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stdout, "M a.txt") {
		t.Errorf("stdout = %q, want the change streamed before the failure", stdout)
	}
	if strings.Contains(stdout, "no changes") {
		t.Errorf("stdout = %q, must not read as a clean report", stdout)
	}
	if !strings.Contains(stderr, "partial output:") {
		t.Errorf("stderr = %q, want a 'partial output:' diagnostic", stderr)
	}
}

// TestChangesJSONNoPartialDocumentOnCorruptManifest proves JSON stays all-or-nothing:
// a comparison failure yields no JSON on stdout (never a truncated document) and a
// non-zero exit.
func TestChangesJSONNoPartialDocumentOnCorruptManifest(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "a.txt", "one\n")
	writeFile(t, root, "z.txt", "two\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	corruptCheckpointLastManifestLine(t, root)
	writeFile(t, root, "a.txt", "one changed\n")

	code, stdout, _ := run("changes", "--root", root, "--json")
	if code == int(ExitSuccess) {
		t.Fatalf("changes --json over a corrupt manifest exited 0; stdout=%q", stdout)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want no JSON at all (never a partial document)", stdout)
	}
}

// TestDiffHumanPartialOutputOnCorruptManifest is the diff counterpart: the streaming
// per-file renderer must also fail loud with a partial-output diagnostic when the
// baseline manifest is corrupt mid-stream.
func TestDiffHumanPartialOutputOnCorruptManifest(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "a.txt", "one\n")
	writeFile(t, root, "z.txt", "two\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	corruptCheckpointLastManifestLine(t, root)
	writeFile(t, root, "a.txt", "one changed\n")

	code, stdout, stderr := run("diff", "--root", root, "--no-renames")
	if code == int(ExitSuccess) {
		t.Fatalf("diff over a corrupt manifest exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "partial output:") {
		t.Errorf("stderr = %q, want a 'partial output:' diagnostic", stderr)
	}
}

// parseCheckpointID extracts the full checkpoint id from the "id:" line of awa checkpoint's output.
func parseCheckpointID(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		}
	}
	t.Fatalf("checkpoint output missing an 'id:' line:\n%s", stdout)
	return ""
}

// checkpointWithMessage records a checkpoint with the given message and returns its full
// checkpoint id. It is the test-side equivalent of a reviewer keeping the id awa checkpoint
// prints for a long fix loop.
func checkpointWithMessage(t *testing.T, root, message string) string {
	t.Helper()
	code, stdout, stderr := run("checkpoint", "--root", root, "-m", message)
	if code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	return parseCheckpointID(t, stdout)
}

// awa checkpoint prints a full, copyable checkpoint id and the exact follow-up
// ranges a reviewer needs for a long fix loop, and those ranges name the same id.
func TestCheckpointPrintsCopyableIDAndRanges(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n")
	code, stdout, stderr := run("checkpoint", "--root", root, "-m", "before OC-1 fixes")
	if code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	id := parseCheckpointID(t, stdout)
	if len(id) != 32 {
		t.Fatalf("checkpoint did not print a full copyable id (got %q):\n%s", id, stdout)
	}
	for _, want := range []string{
		"compare:    awa changes " + id + "..now",
		"diff:       awa diff " + id + "..now",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("checkpoint output missing %q:\n%s", want, stdout)
		}
	}
}

// TestChangesEmptyOutputNamesBaseline pins that an empty comparison still names the
// range and baseline (id + message), so "no changes since the shared latest" can
// never be mistaken for "no changes since my private checkpoint".
func TestChangesEmptyOutputNamesBaseline(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n")
	_ = checkpointWithMessage(t, root, "review checkpoint alpha")

	for _, cmd := range []string{"changes", "diff"} {
		code, stdout, stderr := run(cmd, "--root", root)
		if code != int(ExitSuccess) {
			t.Fatalf("%s exit = %d, stderr = %q", cmd, code, stderr)
		}
		if !strings.Contains(stdout, "no changes") {
			t.Errorf("%s should report no changes:\n%s", cmd, stdout)
		}
		if !strings.Contains(stdout, "range: latest -> now") {
			t.Errorf("%s empty output missing default range header:\n%s", cmd, stdout)
		}
		if !strings.Contains(stdout, "baseline: checkpoint ") || !strings.Contains(stdout, "review checkpoint alpha") {
			t.Errorf("%s empty output missing baseline id/message:\n%s", cmd, stdout)
		}
	}
}

// TestChangesEmptyPathFilteredNamesBaselineAndFilter pins that a path-filtered
// empty comparison still names both the baseline and the path filter.
func TestChangesEmptyPathFilteredNamesBaselineAndFilter(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n")
	_ = checkpointWithMessage(t, root, "review checkpoint beta")

	// The path filter is resolved relative to the caller's cwd (the test process),
	// so name it absolutely; parsePathFilters normalizes it to the root-relative
	// "calc" for display.
	code, stdout, stderr := run("changes", "--root", root, filepath.Join(root, "calc"))
	if code != int(ExitSuccess) {
		t.Fatalf("changes calc exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no changes") {
		t.Errorf("path-filtered changes should be empty:\n%s", stdout)
	}
	if !strings.Contains(stdout, "baseline: checkpoint ") || !strings.Contains(stdout, "review checkpoint beta") {
		t.Errorf("path-filtered empty output missing baseline:\n%s", stdout)
	}
	if !strings.Contains(stdout, "paths: calc") {
		t.Errorf("path-filtered empty output missing paths filter:\n%s", stdout)
	}
}

// The acceptance scenario an explicit range exists to prevent: a second agent's
// checkpoint moves the shared latest, so the default awa changes compares against
// B..now, while an explicit <A-id>..now still compares against A. The explicit range
// header names the explicit left ref, not whatever latest currently is.
func TestReviewLoopExplicitRangeNamesExplicitLeft(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n")
	idA := checkpointWithMessage(t, root, "checkpoint A")

	// A change lands after checkpoint A.
	writeFile(t, root, "calc/calc.go", "package calc\n// change 1\n")

	// A second "agent" checkpoints, moving the shared latest to B.
	idB := checkpointWithMessage(t, root, "checkpoint B")
	if idA == idB {
		t.Fatalf("checkpoints A and B must be distinct")
	}

	// A further change lands after checkpoint B.
	writeFile(t, root, "calc/calc.go", "package calc\n// change 1\n// change 2\n")

	// Default awa changes compares B..now: only the second change is reported, and
	// the baseline names checkpoint B, not A.
	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("default changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "range: latest -> now") {
		t.Errorf("default changes should name the latest range:\n%s", stdout)
	}
	if !strings.Contains(stdout, "checkpoint B") || strings.Contains(stdout, "checkpoint A") {
		t.Errorf("default changes baseline should be B, not A:\n%s", stdout)
	}

	// Explicit <A-id>..now compares against checkpoint A: the range names the
	// explicit left id and the baseline names checkpoint A.
	code, stdout, stderr = run("changes", "--root", root, idA+"..now")
	if code != int(ExitSuccess) {
		t.Fatalf("explicit range changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "range: "+idA+" -> now") {
		t.Errorf("explicit range should name the explicit left id %q:\n%s", idA, stdout)
	}
	if !strings.Contains(stdout, "checkpoint A") {
		t.Errorf("explicit <A-id>..now baseline should be checkpoint A:\n%s", stdout)
	}
}

// TestDuplicateCheckpointMessagesAreUnambiguous pins that reusing an identical
// checkpoint message across two checkpoints changes nothing: resolution is by id, not
// by fuzzy message lookup, so the default range still resolves to the newest.
func TestDuplicateCheckpointMessagesAreUnambiguous(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n")
	idFirst := checkpointWithMessage(t, root, "same message")
	writeFile(t, root, "calc/calc.go", "package calc\n// edit\n")
	idSecond := checkpointWithMessage(t, root, "same message")
	if idFirst == idSecond {
		t.Fatalf("two checkpoints must have distinct ids even with the same message")
	}
	// The default range resolves latest to the newest checkpoint by id, not by the
	// (duplicated) message.
	code, stdout, stderr := run("changes", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("default changes --json exit = %d, stderr = %q", code, stderr)
	}
	if got := jsonLeftCheckpointID(t, stdout); got != idSecond {
		t.Errorf("default latest resolved to %q, want the newest checkpoint %q", got, idSecond)
	}

	// An explicit id for the older checkpoint still resolves that exact checkpoint — no
	// ambiguity from the shared message, no error.
	code, stdout, stderr = run("changes", "--root", root, idFirst+"..now")
	if code != int(ExitSuccess) {
		t.Fatalf("explicit older-id range exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "range: "+idFirst+" -> now") {
		t.Errorf("explicit older id should resolve unambiguously:\n%s", stdout)
	}
}

// jsonLeftCheckpointID decodes the resolved left checkpoint_id from a changes/diff
// --json document.
func jsonLeftCheckpointID(t *testing.T, stdout string) string {
	t.Helper()
	var env struct {
		Data struct {
			Left struct {
				CheckpointID string `json:"checkpoint_id"`
			} `json:"left"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	return env.Data.Left.CheckpointID
}
