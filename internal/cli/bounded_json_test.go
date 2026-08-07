package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChangesJSONNoRenamesPartialOutputOnCorruptManifest pins the streaming JSON
// contract for the genuinely streaming path: with --no-renames the change set is not
// paired eagerly, so a corrupt manifest surfaces mid-stream after the envelope has begun.
// The command must exit non-zero with a "partial output:" diagnostic — never a clean,
// complete-looking document — mirroring the human streaming path. (The default,
// rename-detection-on path pairs eagerly and still fails before any byte: see
// TestChangesJSONNoPartialDocumentOnCorruptManifest.)
func TestChangesJSONNoRenamesPartialOutputOnCorruptManifest(t *testing.T) {
	root := initProject(t)
	// a.txt emits a change first; b.txt spaces the look-ahead; c.txt's manifest record is
	// corrupted, so the failure lands after "a.txt" has streamed into the document.
	writeFile(t, root, "a.txt", "one\n")
	writeFile(t, root, "b.txt", "mid\n")
	writeFile(t, root, "c.txt", "two\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	corruptCheckpointLastManifestLine(t, root)
	writeFile(t, root, "a.txt", "one changed\n")

	code, stdout, stderr := run("changes", "--root", root, "--json", "--no-renames")
	if code == int(ExitSuccess) {
		t.Fatalf("changes --json --no-renames over a corrupt manifest exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "partial output:") {
		t.Errorf("stderr = %q, want a 'partial output:' diagnostic on the streamed JSON path", stderr)
	}
	// The streamed document began (envelope prefix reached stdout) but is incomplete. This
	// is exactly the tempting-to-salvage case: a non-empty fragment that a naive consumer
	// might try to patch closed. The contract for pipe and JSON cleanliness is
	// that the non-zero exit plus the "partial output:" diagnostic mean
	// discard stdout — so the fragment must be present yet must not parse as a whole
	// document.
	if strings.TrimSpace(stdout) == "" {
		t.Errorf("expected a partial streamed fragment on stdout to exercise the discard rule, got empty stdout")
	}
	if json.Valid([]byte(stdout)) {
		t.Errorf("partial streamed JSON unexpectedly parsed as a complete document; a consumer must be able to detect it as incomplete:\n%s", stdout)
	}
}

// TestLogAllJSONCarriesBoundedFact pins the bounded-report contract for log --all:
// the machine document is not an unbounded history dump — it carries a completeness fact
// (complete, shown) and the full total, reusing the shared bounded-output vocabulary.
func TestLogAllJSONCarriesBoundedFact(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "a.txt", "one\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "base"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr := run("log", "--all", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("log --all --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			Total   int `json:"total"`
			Bounded struct {
				Complete bool `json:"complete"`
				Shown    int  `json:"shown"`
			} `json:"bounded"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("log --all --json invalid: %v\n%s", err, stdout)
	}
	if !env.Data.Bounded.Complete {
		t.Errorf("a small timeline must report bounded.complete=true, got %+v", env.Data.Bounded)
	}
	if env.Data.Bounded.Shown != env.Data.Total {
		t.Errorf("bounded.shown (%d) must equal total (%d) for a complete timeline", env.Data.Bounded.Shown, env.Data.Total)
	}
}

// TestGCJSONCarriesCandidatesBoundedFact pins the bounded-report contract for gc: the
// emitted candidate list carries a completeness fact, while the summary counts (which
// drive the destructive decision) remain complete over the whole store.
func TestGCJSONCarriesCandidatesBoundedFact(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "a.txt", "one\n")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "base"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr := run("gc", "--dry-run", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("gc --dry-run --json exit = %d, stderr = %q", code, stderr)
	}
	var env struct {
		Data struct {
			Candidates        []map[string]any `json:"candidates"`
			CandidatesBounded struct {
				Complete bool `json:"complete"`
				Shown    int  `json:"shown"`
			} `json:"candidates_bounded"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("gc --json invalid: %v\n%s", err, stdout)
	}
	if !env.Data.CandidatesBounded.Complete {
		t.Errorf("a small store must report candidates_bounded.complete=true, got %+v", env.Data.CandidatesBounded)
	}
	if env.Data.CandidatesBounded.Shown != len(env.Data.Candidates) {
		t.Errorf("candidates_bounded.shown (%d) must equal len(candidates) (%d)", env.Data.CandidatesBounded.Shown, len(env.Data.Candidates))
	}
}
