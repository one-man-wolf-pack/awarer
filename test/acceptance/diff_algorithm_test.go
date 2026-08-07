package acceptance

import (
	"path/filepath"
	"strings"
	"testing"
)

// setDiffAlgorithm writes an override config selecting [diff].algorithm so
// config-default selection can be exercised end-to-end.
func setDiffAlgorithm(t *testing.T, root, algo string) {
	t.Helper()
	writeProjectConfig(t, root, "[diff]\nalgorithm = \""+algo+"\"\n")
}

// diffBody returns the rendered diff from a human report, dropping the leading
// range/baseline/git header. The header states the baseline's age through
// time.Now(), so it changes between two invocations whenever the rendered age
// crosses a threshold ("just now" holds for 5s, then it ticks per second) — and
// through the shipped binary each invocation is a separate process, so that gap is
// wider than in-process. Comparing whole reports would mix a clock difference into
// every engine comparison below: an equality assertion can flake red, and — worse —
// a difference assertion can pass for the wrong reason, hiding two selections that
// actually reached the same engine.
func diffBody(t *testing.T, report string) string {
	t.Helper()
	i := strings.Index(report, "\ndiff ")
	if i < 0 {
		t.Fatalf("report carries no diff body:\n%s", report)
	}
	return report[i+1:]
}

// TestAcceptanceDiffAlgorithmSelection covers spec scenario 2a: algorithm
// selection, config default, CLI override, and bounded fallback on repeated
// input.
func TestAcceptanceDiffAlgorithmSelection(t *testing.T) {
	root := initProject(t)
	// A small source file with a function-like block to move/edit.
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\nfunc Sub(a, b int) int {\n\treturn a - b\n}\n")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, stderr)
	}
	// Edit the first function-like block.
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n\nfunc Sub(a, b int) int {\n\treturn a - b\n}\n")

	// --algorithm histogram shows the change.
	code, hist, stderr := awa(t, root, "diff", "--algorithm", "histogram")
	if code != 0 {
		t.Fatalf("diff --algorithm histogram exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(hist, "@@") || !strings.Contains(hist, "+\treturn a + b + 1") {
		t.Errorf("histogram diff missing change:\n%s", hist)
	}

	// The default (no flag) equals the explicit histogram selection: Histogram is
	// the built-in default, so a regression back to a Myers default breaks this.
	_, def, _ := awa(t, root, "diff")
	if diffBody(t, def) != diffBody(t, hist) {
		t.Errorf("default diff != explicit histogram\ndefault:\n%s\nhistogram:\n%s", def, hist)
	}

	// Myers stays selectable and valid, producing the same change via Myers.
	code, myers, stderr := awa(t, root, "diff", "--algorithm", "myers")
	if code != 0 {
		t.Fatalf("diff --algorithm myers exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(myers, "+\treturn a + b + 1") {
		t.Errorf("myers diff missing change:\n%s", myers)
	}
}

// TestAcceptanceDiffAlgorithmReachesBothEngines proves through the shipped binary
// that the single selector reaches two distinct engines. A moved block is where
// histogram's rare-line anchoring and Myers' shortest-edit-script pick different
// (equally correct) hunks, so the rendered output must diverge.
func TestAcceptanceDiffAlgorithmReachesBothEngines(t *testing.T) {
	root := initProject(t)
	write(t, root, "moved.txt", "funcA{\nbodyA\n}\nfuncB{\nbodyB\n}\n")
	if code, _, stderr := awa(t, root, "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit=%d stderr=%q", code, stderr)
	}
	write(t, root, "moved.txt", "funcB{\nbodyB\n}\nfuncA{\nbodyA\n}\n")

	code, myers, stderr := awa(t, root, "diff", "--algorithm", "myers")
	if code != 0 {
		t.Fatalf("myers diff exit=%d stderr=%q", code, stderr)
	}
	code, hist, stderr := awa(t, root, "diff", "--algorithm", "histogram")
	if code != 0 {
		t.Fatalf("histogram diff exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(myers, "@@") || !strings.Contains(hist, "@@") {
		t.Fatalf("both engines must produce a hunk\nmyers:\n%s\nhistogram:\n%s", myers, hist)
	}
	if diffBody(t, myers) == diffBody(t, hist) {
		t.Errorf("both --algorithm values rendered identically, so they reach one engine:\n%s", hist)
	}
}

func TestAcceptanceDiffConfigDefaultAndOverride(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	write(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n")

	// Capture the plain Myers diff before switching config.
	_, myers, _ := awa(t, root, "diff", "--algorithm", "myers")

	// Config selects histogram: the default diff now uses it but stays valid.
	setDiffAlgorithm(t, root, "histogram")
	code, cfgDiff, stderr := awa(t, root, "diff")
	if code != 0 {
		t.Fatalf("diff with histogram config exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(cfgDiff, "+\treturn a + b + 1") {
		t.Errorf("histogram-config diff missing change:\n%s", cfgDiff)
	}

	// A CLI selection overrides the histogram config and matches the plain Myers diff.
	code, override, _ := awa(t, root, "diff", "--algorithm", "myers")
	if code != 0 {
		t.Fatalf("diff --algorithm myers over histogram config exit=%d", code)
	}
	if diffBody(t, override) != diffBody(t, myers) {
		t.Errorf("--algorithm myers should override histogram config\ngot:\n%s\nwant:\n%s", override, myers)
	}
}

func TestAcceptanceHistogramRepeatedInputFallsBack(t *testing.T) {
	root := initProject(t)
	// Many repeated JSON/CSV-like lines: poor anchors that force local fallback.
	var b strings.Builder
	for i := 0; i < 300; i++ {
		b.WriteString("{\"v\":1},\n")
	}
	base := b.String()
	write(t, root, "data.json", base)
	baseline := checkpointID(t, root)
	// Edit one repeated line in the middle.
	edited := strings.Replace(base, "{\"v\":1},\n", "{\"v\":2},\n", 1)
	write(t, root, "data.json", edited)

	// Histogram must complete and produce a valid diff that shows the edit.
	code, stdout, stderr := awa(t, root, "diff", "--algorithm", "histogram", baseline+"..now")
	if code != 0 {
		t.Fatalf("histogram repeated diff exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "@@") || !strings.Contains(stdout, "+{\"v\":2},") {
		t.Errorf("histogram repeated diff missing edit:\n%s", stdout)
	}
	// The Myers form over the same range also works.
	if code, _, _ := awa(t, root, "diff", "--algorithm", "myers", baseline+"..now"); code != 0 {
		t.Errorf("myers repeated diff exit=%d", code)
	}
}

func TestAcceptanceHistogramPathLimitedFromNested(t *testing.T) {
	root := initProject(t)
	write(t, root, "packages/api/api.go", "package api\n\nfunc H() {}\n")
	write(t, root, "calc/calc.go", "package calc\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	write(t, root, "packages/api/api.go", "package api\n\nfunc H() int { return 1 }\n")

	nested := filepath.Join(root, "packages", "api")
	code, stdout, stderr := awa(t, nested, "diff", "--algorithm", "histogram", "api.go")
	if code != 0 {
		t.Fatalf("nested histogram diff exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "packages/api/api.go") || strings.Contains(stdout, "calc/calc.go") {
		t.Errorf("path-limited histogram diff wrong:\n%s", stdout)
	}
}

func TestAcceptanceDiffConflictingAlgorithmsUsageError(t *testing.T) {
	root := initProject(t)
	write(t, root, "calc/calc.go", "package calc\n")
	if code, _, _ := awa(t, root, "checkpoint"); code != 0 {
		t.Fatal("checkpoint")
	}
	if code, _, _ := awa(t, root, "diff", "--algorithm", "myers", "--algorithm", "histogram"); code != 2 {
		t.Errorf("conflicting algorithm flags exit = %d, want 2 (usage error)", code)
	}
	// Invalid config value is a config error (exit 4).
	setDiffAlgorithm(t, root, "bogus")
	if code, _, _ := awa(t, root, "diff"); code != 4 {
		t.Errorf("invalid [diff].algorithm exit = %d, want 4 (config error)", code)
	}
}
