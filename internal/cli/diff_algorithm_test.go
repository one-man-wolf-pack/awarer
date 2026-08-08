package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigFile writes a (possibly partial) config.toml to a temp path and
// returns it, for use with the global --config flag. Decode seeds defaults, so a
// file carrying only the [diff] section exercises the algorithm setting in
// isolation.
func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// diffBody returns the rendered diff from a human report, dropping the leading
// range/baseline/git header. The header states the baseline's age through
// time.Now(), so it changes between two invocations whenever the rendered age
// crosses a threshold ("just now" holds for 5s, then it ticks per second).
// Comparing whole reports therefore mixes a clock difference into every engine
// comparison below: an equality assertion can flake red, and — worse — a
// difference assertion can pass for the wrong reason, hiding two selections that
// actually reached the same engine. These tests are about the rendered diff, so
// they compare exactly that.
func diffBody(t *testing.T, report string) string {
	t.Helper()
	i := strings.Index(report, "\ndiff ")
	if i < 0 {
		t.Fatalf("report carries no diff body:\n%s", report)
	}
	return report[i+1:]
}

// modifiedCheckpointProject records a baseline then edits calc.go so a diff has content.
func modifiedCheckpointProject(t *testing.T) string {
	t.Helper()
	root := checkpointProject(t)
	writeFile(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n")
	return root
}

func TestDiffAlgorithmFormsProduceDiff(t *testing.T) {
	root := modifiedCheckpointProject(t)
	variants := [][]string{
		{"diff", "--root", root, "--algorithm", "myers"},
		{"diff", "--root", root, "--algorithm", "histogram"},
		{"diff", "--root", root, "--algorithm=myers"},
		{"diff", "--root", root, "--algorithm=histogram"},
	}
	for _, args := range variants {
		code, stdout, stderr := run(args...)
		if code != int(ExitSuccess) {
			t.Errorf("%v exit = %d, stderr = %q", args, code, stderr)
			continue
		}
		if !strings.Contains(stdout, "@@") || !strings.Contains(stdout, "+\treturn a + b + 1") {
			t.Errorf("%v stdout missing diff: %q", args, stdout)
		}
	}
}

func TestDiffDefaultEqualsExplicitHistogram(t *testing.T) {
	// Histogram is the built-in default, so the no-flag diff must match an
	// explicit --algorithm histogram; a regression to a Myers default breaks this.
	root := modifiedCheckpointProject(t)
	_, def, _ := run("diff", "--root", root)
	_, hist, _ := run("diff", "--root", root, "--algorithm", "histogram")
	if diffBody(t, def) != diffBody(t, hist) {
		t.Errorf("default diff != explicit histogram\ndefault:\n%s\nhistogram:\n%s", def, hist)
	}
}

// TestDiffAlgorithmSelectsDistinctEngines proves --algorithm reaches two different
// engines rather than two names for one. A moved block is the case where histogram's
// rare-line anchoring and Myers' shortest-edit-script choose different (equally
// correct) hunks, so the rendered output must diverge — the same seam
// difftext.TestHistogramDiffersFromMyersOnMovedBlock pins one layer down. Without
// this, routing both values to one engine would leave every other test green.
func TestDiffAlgorithmSelectsDistinctEngines(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "moved.txt", "funcA{\nbodyA\n}\nfuncB{\nbodyB\n}\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	writeFile(t, root, "moved.txt", "funcB{\nbodyB\n}\nfuncA{\nbodyA\n}\n")
	backdate(t, filepath.Join(root, "moved.txt"))

	code, myers, stderr := run("diff", "--root", root, "--algorithm", "myers")
	if code != int(ExitSuccess) {
		t.Fatalf("myers diff exit = %d, stderr = %q", code, stderr)
	}
	code, hist, stderr := run("diff", "--root", root, "--algorithm", "histogram")
	if code != int(ExitSuccess) {
		t.Fatalf("histogram diff exit = %d, stderr = %q", code, stderr)
	}
	for _, e := range []struct{ name, out string }{{"myers", myers}, {"histogram", hist}} {
		if !strings.Contains(e.out, "@@") {
			t.Fatalf("%s produced no hunk:\n%s", e.name, e.out)
		}
	}
	if diffBody(t, myers) == diffBody(t, hist) {
		t.Errorf("--algorithm myers and --algorithm histogram rendered identically, so both reach one engine:\n%s", hist)
	}
}

func TestDiffConfigDefaultHistogram(t *testing.T) {
	root := modifiedCheckpointProject(t)
	cfg := writeConfigFile(t, "[diff]\nalgorithm = \"histogram\"\n")
	// Config selects histogram; the diff must still be valid and show the change.
	code, stdout, stderr := run("diff", "--root", root, "--config", cfg)
	if code != int(ExitSuccess) {
		t.Fatalf("diff with histogram config exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "+\treturn a + b + 1") {
		t.Errorf("histogram-config diff missing change: %q", stdout)
	}
	// A CLI selection overrides the histogram config and matches the plain Myers diff.
	_, override, _ := run("diff", "--root", root, "--config", cfg, "--algorithm", "myers")
	_, plain, _ := run("diff", "--root", root, "--algorithm", "myers")
	if diffBody(t, override) != diffBody(t, plain) {
		t.Errorf("--algorithm myers should override histogram config\ngot:\n%s\nwant:\n%s", override, plain)
	}
}

func TestDiffRepeatedAlgorithmSelectionAccepted(t *testing.T) {
	root := modifiedCheckpointProject(t)
	ok := [][]string{
		{"diff", "--root", root, "--algorithm", "myers", "--algorithm", "myers"},
		{"diff", "--root", root, "--algorithm", "histogram", "--algorithm=histogram"},
	}
	for _, args := range ok {
		if code, _, stderr := run(args...); code != int(ExitSuccess) {
			t.Errorf("redundant %v exit = %d, want success; stderr=%q", args, code, stderr)
		}
	}
}

func TestDiffAlgorithmUsageErrors(t *testing.T) {
	root := modifiedCheckpointProject(t)
	bad := [][]string{
		{"diff", "--root", root, "--algorithm", "myers", "--algorithm", "histogram"},
		{"diff", "--root", root, "--algorithm=histogram", "--algorithm", "myers"},
		{"diff", "--root", root, "--algorithm", "bogus"},
		{"diff", "--root", root, "--algorithm"},
		{"diff", "--root", root, "--algorithm="},
	}
	for _, args := range bad {
		if code, _, _ := run(args...); code != int(ExitUsageError) {
			t.Errorf("%v exit = %d, want %d (usage error)", args, code, ExitUsageError)
		}
	}
}

func TestDiffInvalidConfigAlgorithmIsConfigError(t *testing.T) {
	root := modifiedCheckpointProject(t)
	cfg := writeConfigFile(t, "[diff]\nalgorithm = \"bogus\"\n")
	if code, _, _ := run("diff", "--root", root, "--config", cfg); code != int(ExitConfigError) {
		t.Errorf("invalid [diff].algorithm exit = %d, want %d (config error)", code, ExitConfigError)
	}
}

func TestDiffHistogramJSONStable(t *testing.T) {
	root := modifiedCheckpointProject(t)
	code, stdout, _ := run("diff", "--root", root, "--algorithm", "histogram", "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("diff --algorithm histogram --json exit = %d", code)
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

func TestDiffHistogramComposesWithContextAndStat(t *testing.T) {
	root := modifiedCheckpointProject(t)
	// --context composes with an explicit algorithm selection.
	code, stdout, stderr := run("diff", "--root", root, "--algorithm", "histogram", "--context", "1")
	if code != int(ExitSuccess) {
		t.Fatalf("histogram --context exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "@@") || !strings.Contains(stdout, "+\treturn a + b + 1") {
		t.Errorf("histogram --context diff wrong: %q", stdout)
	}
	// --stat composes with it too and reports a summary.
	if code, out, _ := run("diff", "--root", root, "--algorithm", "histogram", "--stat"); code != int(ExitSuccess) || !strings.Contains(out, "changed:") {
		t.Errorf("histogram --stat exit=%d out=%q", code, out)
	}
}
