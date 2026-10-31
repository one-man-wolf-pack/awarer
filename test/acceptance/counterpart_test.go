package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// counterpartFacts maps each changed path of `awa changes --json <rng>` to
// "<status> <side>/<qualification>", or "<status>" when the element carries no
// counterpart fact.
func counterpartFacts(t *testing.T, root, rng string) (map[string]string, string) {
	t.Helper()
	code, out, stderr := awa(t, root, "--json", "changes", rng)
	if code != 0 {
		t.Fatalf("changes %s --json exit = %d, stderr = %q", rng, code, stderr)
	}
	var env struct {
		Data struct {
			Changes []struct {
				Status      string `json:"status"`
				OldPath     string `json:"old_path"`
				NewPath     string `json:"new_path"`
				Counterpart *struct {
					Side          string `json:"side"`
					Qualification string `json:"qualification"`
				} `json:"counterpart"`
			} `json:"changes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode changes --json: %v\n%s", err, out)
	}
	facts := map[string]string{}
	for _, c := range env.Data.Changes {
		p := c.NewPath
		if c.Status == "deleted" {
			p = c.OldPath
		}
		v := c.Status
		if c.Counterpart != nil {
			v += " " + c.Counterpart.Side + "/" + c.Counterpart.Qualification
		}
		facts[p] = v
	}
	return facts, stderr
}

func expectFacts(t *testing.T, rng string, got, want map[string]string) {
	t.Helper()
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: %s = %q, want %q (all %v)", rng, p, got[p], w, got)
		}
	}
}

// Required predecessors stay readable: their missing coverage qualifies a missing
// side as unknown, never as proved absence, while a recovery record still confines
// what it says to its covered path set.
func TestCounterpartPredecessorRecordsQualifyUnknown(t *testing.T) {
	root := predecessorProject(t)

	rng := predecessorCheckpoint + "..now"
	facts, _ := counterpartFacts(t, root, rng)
	expectFacts(t, rng, facts, map[string]string{
		"run-made.txt": "added left/unknown",
		"z-hidden.txt": "deleted right/outside-observation",
		".awaignore":   "modified",
	})
	_, human, _ := awa(t, root, "changes", rng)
	if !strings.Contains(human, "A run-made.txt (left: coverage unknown)\n") {
		t.Errorf("human listing lacks the unknown-coverage annotation:\n%s", human)
	}

	rng = predecessorRecovery + "..now"
	facts, _ = counterpartFacts(t, root, rng)
	expectFacts(t, rng, facts, map[string]string{
		"a-visible.txt": "added left/unknown",
		"keep.txt":      "added left/outside-observation",
	})
}

// A recovery observation proves absence only at its exact covered paths; every
// other path is outside its observation, not a whole-worktree absence.
func TestCounterpartRecoveryCoverageStaysInsideItsPathSet(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, "keep.txt", "keep\n")
	writeAt(t, root, "gone.txt", "gone\n")
	id := checkpointID(t, root)
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	code, applied := restoreJSON(t, root, "--apply", id, "--", "gone.txt")
	if code != 0 || applied.Data.Recovery == nil {
		t.Fatalf("restore exit = %d recovery = %+v failures = %+v", code, applied.Data.Recovery, applied.Data.Failures)
	}

	rng := applied.Data.Recovery.Ref + "..now"
	facts, _ := counterpartFacts(t, root, rng)
	expectFacts(t, rng, facts, map[string]string{
		"gone.txt": "added left/absent",
		"keep.txt": "added left/outside-observation",
	})
	_, human, _ := awa(t, root, "changes", rng)
	if !strings.Contains(human, "A gone.txt\n") || !strings.Contains(human, "A keep.txt (left: outside observation)\n") {
		t.Errorf("human listing:\n%s", human)
	}
}

// Under one unchanged configuration a run observation and the history family scan
// different default scopes: build/ is history-excluded. The qualification says the
// history side did not observe the path, and the mismatch warning names that
// boundary instead of guessing a configuration edit.
func TestCounterpartRunScopeAgainstHistoryUnderUnchangedConfig(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, "src.txt", "src\n")
	writeAt(t, root, "build/out.txt", "artifact\n")
	id := checkpointID(t, root)
	if code, _, stderr := awa(t, root, "run", "--record", "--stdin", "null", "--", "sh", "-c", ":"); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}
	_, logOut, _ := awa(t, root, "run", "log")
	short := strings.Fields(strings.SplitN(logOut, "\n", 2)[0])[0]
	after := "run:" + short + ":after"

	rng := after + "..now"
	facts, stderr := counterpartFacts(t, root, rng)
	expectFacts(t, rng, facts, map[string]string{"build/out.txt": "deleted right/outside-observation"})
	if !strings.Contains(stderr, "the differing settings are not retained") || !strings.Contains(stderr, "run scope") {
		t.Errorf("%s stderr = %q, want the unavailable-cause and run-scope boundary", rng, stderr)
	}

	rng = id + ".." + after
	facts, _ = counterpartFacts(t, root, rng)
	expectFacts(t, rng, facts, map[string]string{"build/out.txt": "added left/outside-observation"})
	if _, ok := facts["src.txt"]; ok {
		t.Errorf("%s reports src.txt, which both sides recorded identically: %v", rng, facts)
	}
}

// A damaged frontier is corruption, not unknown coverage. With rename pairing the
// comparison drains before writing, so it fails with empty stdout; without it the
// failure arrives after the selected change, from the frontier's tail outside the
// path filter, and the report is framed as partial output.
func TestCounterpartDamagedFrontierFailsInsteadOfQualifying(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, "keep.txt", "keep\n")
	writeAt(t, root, ".awaignore", "/zz-excluded/\n")
	writeAt(t, root, "zz-excluded/x.txt", "x\n")
	id := checkpointID(t, root)
	writeAt(t, root, "new.txt", "new\n")

	frontier := filepath.Join(root, ".awa", "checkpoints", id, "frontier.jsonl")
	if err := os.Chmod(frontier, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(frontier) //nolint:gosec // test fixture path
	if err != nil || !strings.Contains(string(data), "zz-excluded") {
		t.Fatalf("frontier = %q (%v), want the excluded boundary", data, err)
	}
	forged := strings.Replace(string(data), "zz-excluded", "zz-forged", 1)
	if err := os.WriteFile(frontier, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, asJSON := range []bool{false, true} {
		args := []string{"changes", id + "..now"}
		if asJSON {
			args = append([]string{"--json"}, args...)
		}
		code, stdout, stderr := awa(t, root, args...)
		if code != 5 || stdout != "" {
			t.Errorf("%v exit = %d stdout = %q stderr = %q, want exit 5 with empty stdout", args, code, stdout, stderr)
		}
	}
	for _, args := range [][]string{
		{"changes", "--no-renames", id + "..now", "--", "new.txt"},
		{"--json", "changes", "--no-renames", id + "..now", "--", "new.txt"},
		{"diff", "--no-renames", id + "..now", "--", "new.txt"},
	} {
		code, stdout, stderr := awa(t, root, args...)
		if code != 5 || !strings.Contains(stdout, "new.txt") || !strings.Contains(stderr, "partial output:") {
			t.Errorf("%v exit = %d stdout = %q stderr = %q, want exit 5 framed as partial output", args, code, stdout, stderr)
		}
		if strings.Contains(stdout, "no changes") || (args[0] == "--json" && json.Valid([]byte(stdout))) {
			t.Errorf("%v produced a complete-looking report:\n%s", args, stdout)
		}
	}
}
