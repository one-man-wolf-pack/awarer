package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAcceptancePerformanceQuietAndClean proves the no-diagnostic path through the real
// binary: an ordinary small project crosses no interactive latency threshold, so no
// `note: run state observation` advisory appears on stderr, and the --json surfaces stay
// exactly one clean document with no performance diagnostics block.
func TestAcceptancePerformanceQuietAndClean(t *testing.T) {
	root := t.TempDir()
	if code, _, stderr := awa(t, root, "init"); code != 0 {
		t.Fatalf("init exit = %d, stderr = %q", code, stderr)
	}

	// A recorded run so status/run ls have something to observe.
	if code, _, stderr := awa(t, root, "run", "--", "true"); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}

	// run --json: one document, no diagnostics, no perf note on stderr.
	code, stdout, stderr := awa(t, root, "run", "--json", "--", "true")
	if code != 0 {
		t.Fatalf("run --json exit = %d, stderr = %q", code, stderr)
	}
	assertOneJSONDoc(t, stdout)
	assertNoPerfNote(t, stderr)
	if strings.Contains(stdout, "large-effect-root") {
		t.Errorf("small project must not report a latency diagnostic: %s", stdout)
	}

	// status --json and run ls --near --json: same cleanliness.
	for _, args := range [][]string{
		{"status", "--json"},
		{"run", "ls", "--near", "--json"},
	} {
		code, stdout, stderr := awa(t, root, args...)
		if code != 0 {
			t.Fatalf("%v exit = %d, stderr = %q", args, code, stderr)
		}
		assertOneJSONDoc(t, stdout)
		assertNoPerfNote(t, stderr)
		if strings.Contains(stdout, "large-effect-root") {
			t.Errorf("%v must not report a latency diagnostic: %s", args, stdout)
		}
	}
}

func assertOneJSONDoc(t *testing.T, stdout string) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carried more than one JSON document:\n%s", stdout)
	}
}

func assertNoPerfNote(t *testing.T, stderr string) {
	t.Helper()
	if strings.Contains(stderr, "run state observation took") {
		t.Errorf("unexpected latency note on stderr: %q", stderr)
	}
}
