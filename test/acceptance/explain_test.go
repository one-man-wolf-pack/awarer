package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// explainJSON decodes the run.explain envelope's data into a comparable shape.
type explainJSON struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Current struct {
		Key       string `json:"key"`
		Cacheable bool   `json:"cacheable"`
	} `json:"current"`
	ExactMatch struct {
		Exists  bool   `json:"exists"`
		RunID   string `json:"run_id"`
		Healthy bool   `json:"healthy"`
	} `json:"exact_match"`
	Candidates []struct {
		RunID string `json:"run_id"`
		Score struct {
			Matched   []string `json:"matched"`
			Different []string `json:"different"`
		} `json:"score"`
		ChangedPaths struct {
			Completeness string `json:"completeness"`
			Shown        int    `json:"shown"`
			Total        int    `json:"total"`
			Items        []struct {
				Path   string `json:"path"`
				Status string `json:"status"`
			} `json:"items"`
		} `json:"changed_paths"`
		Hints []struct {
			Path       string `json:"path"`
			Suggestion string `json:"suggestion"`
			Message    string `json:"message"`
		} `json:"hints"`
	} `json:"candidates"`
	Warnings []string `json:"warnings"`
}

func decodeExplain(t *testing.T, stdout string) explainJSON {
	t.Helper()
	var env struct {
		SchemaVersion int         `json:"schema_version"`
		Command       string      `json:"command"`
		Data          explainJSON `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid explain JSON: %v\n%s", err, stdout)
	}
	if env.SchemaVersion != 1 || env.Command != "run.explain" {
		t.Errorf("explain envelope = schema %d command %q, want 1/run.explain", env.SchemaVersion, env.Command)
	}
	return env.Data
}

func TestExplainBeforeRunMisses(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	code, stdout, stderr := awa(t, root, "run", "explain", "--json", "--", h, "-out", "x")
	if code != 0 {
		t.Fatalf("explain exit = %d, stderr=%q", code, stderr)
	}
	d := decodeExplain(t, stdout)
	if d.Outcome != "miss" || d.Reason != "no-entry-for-key" {
		t.Errorf("explain = %s/%s, want miss/no-entry-for-key", d.Outcome, d.Reason)
	}
	if !d.Current.Cacheable || !strings.HasPrefix(d.Current.Key, "blake3:") {
		t.Errorf("current = %+v, want cacheable with a blake3 key", d.Current)
	}
}

func TestExplainAfterRunHits(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	awa(t, root, "run", "--", h, "-out", "x")

	code, stdout, _ := awa(t, root, "run", "explain", "--json", "--", h, "-out", "x")
	if code != 0 {
		t.Fatalf("explain exit = %d", code)
	}
	d := decodeExplain(t, stdout)
	if d.Outcome != "hit" || d.Reason != "exact-key" {
		t.Errorf("explain = %s/%s, want hit/exact-key", d.Outcome, d.Reason)
	}
	if !d.ExactMatch.Exists || !d.ExactMatch.Healthy || d.ExactMatch.RunID == "" {
		t.Errorf("exact match = %+v, want an existing healthy entry", d.ExactMatch)
	}
}

func TestExplainNeverExecutes(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// Explaining must not run the command or store an entry: a following normal run
	// is therefore still a miss.
	if code, _, _ := awa(t, root, "run", "explain", "--", h, "-out", "x"); code != 0 {
		t.Fatalf("explain exit = %d", code)
	}
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("explain must not store an entry; the next run should miss")
	}
}

func TestExplainInputEditMisses(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "one")
	// Verify the seeding run stored an entry: a silently-failed setup run would later
	// surface only as a confusing "no nearest candidate", masking the real cause.
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); code != 0 {
		t.Fatalf("seeding run exit = %d, stderr=%q", code, stderr)
	}

	write(t, root, "data.txt", "two")
	code, stdout, _ := awa(t, root, "run", "explain", "--json", "--", h, "-out", "x")
	if code != 0 {
		t.Fatalf("explain exit = %d", code)
	}
	d := decodeExplain(t, stdout)
	if d.Outcome != "miss" || d.Reason != "input-tree-differs" {
		t.Errorf("explain = %s/%s, want miss/input-tree-differs\njson=%s", d.Outcome, d.Reason, stdout)
	}
	if len(d.Candidates) == 0 {
		t.Fatalf("expected a nearest candidate\njson=%s", stdout)
	}
	if !containsStr(d.Candidates[0].Score.Different, "input_tree") {
		t.Errorf("candidate different = %v, want input_tree", d.Candidates[0].Score.Different)
	}
	// The candidate carries the same bounded changed-path sample the human view prints,
	// naming the edited input and declaring its completeness — so an agent gets the
	// path-level detail without parsing text.
	cp := d.Candidates[0].ChangedPaths
	if cp.Completeness != "complete" && cp.Completeness != "truncated" {
		t.Errorf("candidate changed_paths completeness = %q, want an available sample\njson=%s", cp.Completeness, stdout)
	}
	if cp.Shown != len(cp.Items) || cp.Total < cp.Shown {
		t.Errorf("candidate changed_paths counts inconsistent: %+v", cp)
	}
	var sawData bool
	for _, it := range cp.Items {
		if it.Path == "data.txt" && it.Status != "" {
			sawData = true
		}
	}
	if !sawData {
		t.Errorf("candidate changed_paths missing data.txt:\n%s", stdout)
	}
}

func TestExplainNoCacheDisabled(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	awa(t, root, "run", "--", h, "-out", "x")

	code, stdout, _ := awa(t, root, "run", "explain", "--no-cache", "--json", "--", h, "-out", "x")
	if code != 0 {
		t.Fatalf("explain exit = %d", code)
	}
	d := decodeExplain(t, stdout)
	if d.Outcome != "disabled" || d.Reason != "no-cache" {
		t.Errorf("explain = %s/%s, want disabled/no-cache", d.Outcome, d.Reason)
	}
}

func TestExplainLast(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	awa(t, root, "run", "--", h, "-out", "x")

	code, stdout, stderr := awa(t, root, "run", "explain", "--last", "--json")
	if code != 0 {
		t.Fatalf("explain --last exit = %d, stderr=%q", code, stderr)
	}
	d := decodeExplain(t, stdout)
	if d.Outcome != "hit" {
		t.Errorf("explain --last = %s, want hit", d.Outcome)
	}
}

func TestExplainInvalidCombinations(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	cases := [][]string{
		{"run", "explain", "--last", "--from-run", "abc", "--to-now"},
		{"run", "explain", "--last", "--", h},
		{"run", "explain", "--to-now"},
		{"run", "explain", "--from-run", "abc"}, // missing --to-now
		{"run", "explain"},                      // no command, no mode
		{"run", "explain", "--no-cache", "--refresh", "--", h},
	}
	for _, args := range cases {
		if code, _, stderr := awa(t, root, args...); code != 2 {
			t.Errorf("awa %v exit = %d, want 2 (usage); stderr=%q", args, code, stderr)
		}
	}
}

func TestExplainMalformedRunIDIsUsageError(t *testing.T) {
	root := initProject(t)
	if code, _, stderr := awa(t, root, "run", "explain", "--from-run", "zzz", "--to-now"); code != 2 {
		t.Errorf("malformed --from-run exit = %d, want 2 (usage); stderr=%q", code, stderr)
	}
}

func TestExplainRunReservedVsExecutable(t *testing.T) {
	root := initProject(t)
	// "awa run -- explain ..." runs an executable named explain, not the subcommand;
	// with no such binary it fails to start rather than explaining.
	code, _, _ := awa(t, root, "run", "--", "explain-does-not-exist-xyz")
	if code == 0 {
		t.Error("running a missing executable named explain should fail")
	}
}

func TestExplainDoesNotOpenIndex(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	// Explain reasons read-only and holds no presence lock, so it must never open the
	// mutable worktree index (opening it writes: schema init, journal mode, incomplete-
	// scan cleanup) — that would race a collector rebuilding the index outside the
	// interlock. A fresh project's index file must therefore still be absent afterward.
	if code, _, stderr := awa(t, root, "run", "explain", "--", h, "-out", "x"); code != 0 {
		t.Fatalf("explain exit = %d, stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, ".awa", "indexes", "worktree.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("run explain must not open or create the worktree index, stat err = %v", err)
	}
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
