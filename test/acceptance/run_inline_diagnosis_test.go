package acceptance

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// runDiagnosisJSON decodes the inline cache diagnosis block from a `run --json`
// envelope. The shapes mirror the CLI docs: cache.diagnosis carries a nearest
// near-miss candidate and a structured, non-null suggestions array.
type runDiagnosisJSON struct {
	Status    string `json:"status"`
	Diagnosis struct {
		Available bool `json:"available"`
		Nearest   *struct {
			ShortID      string `json:"short_id"`
			Reason       string `json:"reason"`
			ChangedPaths struct {
				Completeness string `json:"completeness"`
				Items        []struct {
					Path   string `json:"path"`
					Status string `json:"status"`
				} `json:"items"`
			} `json:"changed_paths"`
			Hints []struct {
				Suggestion string `json:"suggestion"`
			} `json:"hints"`
		} `json:"nearest"`
		Suggestions []struct {
			Kind string   `json:"kind"`
			Argv []string `json:"argv"`
		} `json:"suggestions"`
	} `json:"diagnosis"`
}

func decodeRunDiagnosis(t *testing.T, stdout string) runDiagnosisJSON {
	t.Helper()
	var env struct {
		Data struct {
			Cache runDiagnosisJSON `json:"cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid run --json: %v\n%s", err, stdout)
	}
	return env.Data.Cache
}

// TestRunInlineDiagnosisStaleMiss confirms that after an input edit the run footer
// itself explains the miss: it names the nearest previous run, its input-tree-differs
// reason, a bounded changed-input sample, and the deeper run explain / run ls --near
// commands — so the first response is useful without a follow-up command.
func TestRunInlineDiagnosisStaleMiss(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")

	if code, _, _ := awa(t, root, "run", "--", h, "-cat", "data.txt"); code != 0 {
		t.Fatalf("first run exit != 0")
	}

	write(t, root, "data.txt", "v2")
	code, _, stderr := awa(t, root, "run", "--", h, "-cat", "data.txt")
	if code != 0 {
		t.Fatalf("second run exit = %d, stderr = %q", code, stderr)
	}
	if isHit(stderr) {
		t.Fatalf("second run after edit should miss, stderr = %q", stderr)
	}
	for _, want := range []string{
		"cache diagnosis:",
		"nearest previous:",
		"not-reusable(input-tree-differs)",
		"changed inputs: data.txt",
		"run explain --",
		"run ls --near",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("footer missing %q:\n%s", want, stderr)
		}
	}
}

// TestRunInlineDiagnosisGeneratedHint confirms an advisory hint appears for a changed
// generated artifact without ever removing the real changed path from the sample.
func TestRunInlineDiagnosisGeneratedHint(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "main.c", "s1")
	write(t, root, "obj/foo.o", "o1")

	if code, _, _ := awa(t, root, "run", "--", h, "-cat", "main.c"); code != 0 {
		t.Fatalf("first run exit != 0")
	}
	write(t, root, "obj/foo.o", "o2")
	_, _, stderr := awa(t, root, "run", "--", h, "-cat", "main.c")

	if !strings.Contains(stderr, "hint:") || !strings.Contains(stderr, "obj/") {
		t.Errorf("footer missing generated-artifact hint:\n%s", stderr)
	}
	// The hint is advisory: it must not hide the real changed path.
	if !strings.Contains(stderr, "changed inputs: obj/foo.o") {
		t.Errorf("hint removed the real changed path:\n%s", stderr)
	}
}

// TestRunInlineDiagnosisMutatingCompact confirms a mutating run stays compact: it
// leads with the mutated-state reason and the exact pre/post compare command, points
// at run explain, and does not add a redundant nearest-candidate diagnosis block.
func TestRunInlineDiagnosisMutatingCompact(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "code.txt", "raw")

	_, _, stderr := awa(t, root, "run", "--", h, "-fix", "code.txt", "-content", "done")
	if !strings.Contains(stderr, "not reusable (mutated-state)") {
		t.Errorf("mutating footer missing mutated-state reason:\n%s", stderr)
	}
	if !strings.Contains(stderr, "compare:  awa changes run:") {
		t.Errorf("mutating footer missing pre/post compare command:\n%s", stderr)
	}
	if !strings.Contains(stderr, "run explain --") {
		t.Errorf("mutating footer should still point to run explain:\n%s", stderr)
	}
	if strings.Contains(stderr, "cache diagnosis:") {
		t.Errorf("mutating footer should not add a nearest-candidate block:\n%s", stderr)
	}
}

// TestRunInlineDiagnosisDisplayNone confirms --display none suppresses the wrapped
// command's output but still prints the awa cache diagnosis: display controls command
// output, not awa's own diagnostics.
func TestRunInlineDiagnosisDisplayNone(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	if code, _, _ := awa(t, root, "run", "--", h, "-cat", "data.txt"); code != 0 {
		t.Fatalf("first run exit != 0")
	}
	write(t, root, "data.txt", "v2")

	code, stdout, stderr := awa(t, root, "run", "--display", "none", "--", h, "-cat", "data.txt")
	if code != 0 {
		t.Fatalf("run --display none exit = %d", code)
	}
	if stdout != "" {
		t.Errorf("--display none should suppress command output, stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "cache diagnosis:") {
		t.Errorf("--display none suppressed the awa cache diagnosis:\n%s", stderr)
	}
}

// TestRunInlineDiagnosisJSON confirms the diagnosis is exposed as structured,
// machine-readable facts on a stale miss: available, a nearest candidate with the
// canonical reason and changed-path sample, and a non-null suggestions array of
// kind+argv commands.
func TestRunInlineDiagnosisJSON(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	if code, _, _ := awa(t, root, "run", "--", h, "-cat", "data.txt"); code != 0 {
		t.Fatalf("first run exit != 0")
	}
	write(t, root, "data.txt", "v2")

	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-cat", "data.txt")
	if code != 0 {
		t.Fatalf("run --json exit = %d", code)
	}
	d := decodeRunDiagnosis(t, stdout)
	if d.Status != "miss" {
		t.Fatalf("status = %q, want miss", d.Status)
	}
	if !d.Diagnosis.Available || d.Diagnosis.Nearest == nil {
		t.Fatalf("diagnosis = %+v, want available with a nearest candidate", d.Diagnosis)
	}
	if d.Diagnosis.Nearest.Reason != "input-tree-differs" {
		t.Errorf("nearest reason = %q, want input-tree-differs", d.Diagnosis.Nearest.Reason)
	}
	foundPath := false
	for _, it := range d.Diagnosis.Nearest.ChangedPaths.Items {
		if it.Path == "data.txt" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Errorf("changed_paths did not name data.txt: %+v", d.Diagnosis.Nearest.ChangedPaths)
	}
	kinds := map[string]bool{}
	for _, s := range d.Diagnosis.Suggestions {
		kinds[s.Kind] = true
		if len(s.Argv) == 0 {
			t.Errorf("suggestion %q has empty argv", s.Kind)
		}
	}
	if !kinds["run-explain"] || !kinds["run-ls-near"] {
		t.Errorf("suggestions missing expected kinds: %+v", d.Diagnosis.Suggestions)
	}
}

// TestRunInlineDiagnosisCompareUsesFullID confirms the machine-readable run-compare
// suggestion carries the full, unambiguous run id in its argv — not the display-only
// short prefix — so a consumer can run the command without risking prefix ambiguity.
func TestRunInlineDiagnosisCompareUsesFullID(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "code.txt", "raw")

	// A mutating run is non-reusable history with a before/after to compare.
	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-fix", "code.txt", "-content", "done")
	if code != 0 {
		t.Fatalf("run --json exit = %d\n%s", code, stdout)
	}
	var env struct {
		Data struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
			Cache runDiagnosisJSON `json:"cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("invalid run --json: %v\n%s", err, stdout)
	}
	fullID := env.Data.Run.ID
	if fullID == "" {
		t.Fatalf("run id missing from a recorded mutating run:\n%s", stdout)
	}
	var compare *struct {
		Kind string   `json:"kind"`
		Argv []string `json:"argv"`
	}
	for i := range env.Data.Cache.Diagnosis.Suggestions {
		if env.Data.Cache.Diagnosis.Suggestions[i].Kind == "run-compare" {
			compare = &env.Data.Cache.Diagnosis.Suggestions[i]
		}
	}
	if compare == nil {
		t.Fatalf("no run-compare suggestion for a mutating run:\n%s", stdout)
	}
	wantRef := "run:" + fullID + ":before..run:" + fullID + ":after"
	wantArgv := []string{"awa", "changes", wantRef}
	if !reflect.DeepEqual(compare.Argv, wantArgv) {
		t.Errorf("run-compare argv = %q, want %q", compare.Argv, wantArgv)
	}
}

// TestRunInlineDiagnosisAvailableIsIndependentOfSuggestions locks the JSON contract:
// available reports only whether a nearest candidate exists, so a non-reusable run
// with no prior run (no nearest) still reports available=false yet carries actionable
// suggestions (run-explain, run-compare). Only a cache hit yields both false and empty.
func TestRunInlineDiagnosisAvailableIsIndependentOfSuggestions(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "code.txt", "raw")

	// First run in a fresh store, and it mutates: non-reusable, and there is no earlier
	// run to be a nearest candidate.
	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-fix", "code.txt", "-content", "done")
	if code != 0 {
		t.Fatalf("run --json exit = %d\n%s", code, stdout)
	}
	d := decodeRunDiagnosis(t, stdout)
	if d.Diagnosis.Available || d.Diagnosis.Nearest != nil {
		t.Fatalf("first mutating run should have no nearest candidate: %+v", d.Diagnosis)
	}
	kinds := map[string]bool{}
	for _, s := range d.Diagnosis.Suggestions {
		kinds[s.Kind] = true
	}
	if !kinds["run-explain"] || !kinds["run-compare"] {
		t.Errorf("available=false must still carry actionable suggestions, got %+v", d.Diagnosis.Suggestions)
	}
}

// TestRunInlineDiagnosisHitJSON confirms a cache hit reports no diagnosis: available
// is false, no nearest candidate, and suggestions is a stable empty array (not null).
func TestRunInlineDiagnosisHitJSON(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	write(t, root, "data.txt", "v1")
	if code, _, _ := awa(t, root, "run", "--", h, "-cat", "data.txt"); code != 0 {
		t.Fatalf("first run exit != 0")
	}

	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-cat", "data.txt")
	if code != 0 {
		t.Fatalf("hit run --json exit = %d", code)
	}
	d := decodeRunDiagnosis(t, stdout)
	if d.Status != "hit" {
		t.Fatalf("status = %q, want hit", d.Status)
	}
	if d.Diagnosis.Available || d.Diagnosis.Nearest != nil {
		t.Errorf("hit diagnosis should be unavailable: %+v", d.Diagnosis)
	}
	// A non-null empty array: the field decodes to a non-nil, zero-length slice and is
	// never serialized as null (which would break a machine consumer that ranges it).
	if d.Diagnosis.Suggestions == nil || len(d.Diagnosis.Suggestions) != 0 {
		t.Errorf("hit suggestions = %+v, want a non-nil empty array", d.Diagnosis.Suggestions)
	}
	if strings.Contains(stdout, `"suggestions": null`) {
		t.Errorf("hit suggestions serialized as null:\n%s", stdout)
	}
}
