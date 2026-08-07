package difftext_test

import (
	"fmt"
	"strings"
	"testing"

	"awarer/internal/infra/difftext"
)

// reconstruct rebuilds the old and new line lists from a unified diff produced
// with full context (one hunk covering every line). With every line present in
// the body, the old side is the ' ' and '-' lines and the new side is the ' '
// and '+' lines, so a self-consistent diff reconstructs exactly its inputs. It
// is the correctness oracle for both engines: a diff that does not reconstruct
// its inputs is wrong regardless of which algorithm produced it.
func reconstruct(diff string) (old, new []string) {
	for _, ln := range strings.Split(diff, "\n") {
		if ln == "" {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "--- "), strings.HasPrefix(ln, "+++ "),
			strings.HasPrefix(ln, "@@ "), strings.HasPrefix(ln, "\\ "):
			continue
		}
		switch ln[0] {
		case ' ':
			old = append(old, ln[1:])
			new = append(new, ln[1:])
		case '-':
			old = append(old, ln[1:])
		case '+':
			new = append(new, ln[1:])
		}
	}
	return old, new
}

func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func eqLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fullContext is large enough that group() merges the whole file into a single
// hunk, so reconstruct sees every line.
const fullContext = 1 << 20

// unifiedFn is the shared signature of difftext.Unified and
// difftext.UnifiedHistogram, so a test can drive either engine uniformly.
type unifiedFn = func(oldName, newName string, old, new []byte, context int) (string, error)

// checkEngineReconstructs runs one engine over newline-terminated inputs and
// asserts the diff reconstructs both sides — the core correctness property.
func checkEngineReconstructs(t *testing.T, name string, fn unifiedFn, oldLines, newLines []string) {
	t.Helper()
	diff, err := fn("a", "b", joinLines(oldLines), joinLines(newLines), fullContext)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", name, err)
	}
	if eqLines(oldLines, newLines) {
		if diff != "" {
			t.Errorf("%s: identical inputs must produce no diff, got:\n%s", name, diff)
		}
		return
	}
	gotOld, gotNew := reconstruct(diff)
	if !eqLines(gotOld, oldLines) {
		t.Errorf("%s: reconstructed old side mismatch\n got: %q\nwant: %q\ndiff:\n%s", name, gotOld, oldLines, diff)
	}
	if !eqLines(gotNew, newLines) {
		t.Errorf("%s: reconstructed new side mismatch\n got: %q\nwant: %q\ndiff:\n%s", name, gotNew, newLines, diff)
	}
}

// diffCases are old/new line lists exercising both engines: simple edits, moved
// blocks, repeated/no-anchor inputs that force Histogram to fall back, and a
// deep anchor chain that trips the recursion cap.
func diffCases() map[string][2][]string {
	rep := func(s string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	deepOld := make([]string, 0, 120)
	deepNew := make([]string, 0, 120)
	for i := 0; i < 60; i++ {
		deepOld = append(deepOld, fmt.Sprintf("anchor-%d", i), fmt.Sprintf("old-gap-%d", i))
		deepNew = append(deepNew, fmt.Sprintf("anchor-%d", i), fmt.Sprintf("new-gap-%d", i))
	}
	return map[string][2][]string{
		"identical":   {{"a", "b", "c"}, {"a", "b", "c"}},
		"modify":      {{"a", "b", "c"}, {"a", "X", "c"}},
		"insert":      {{"a", "d"}, {"a", "b", "c", "d"}},
		"delete":      {{"a", "b", "c", "d"}, {"a", "d"}},
		"prefixonly":  {{"head", "1", "2"}, {"head", "9", "8"}},
		"moved-block": {{"funcA{", "bodyA", "}", "funcB{", "bodyB", "}"}, {"funcB{", "bodyB", "}", "funcA{", "bodyA", "}"}},
		"no-common":   {{"x1", "x2", "x3"}, {"y1", "y2", "y3"}},
		"repeated-braces": {
			append(append(rep("}", 80), "real-old"), rep("}", 5)...),
			append(append(rep("}", 80), "real-new"), rep("}", 5)...),
		},
		"json-like": {
			{`{"a":1},`, `{"a":1},`, `{"a":1},`, `{"k":"old"},`, `{"a":1},`, `{"a":1},`},
			{`{"a":1},`, `{"a":1},`, `{"a":1},`, `{"k":"new"},`, `{"a":1},`, `{"a":1},`},
		},
		"deep-anchor-chain": {deepOld, deepNew},
	}
}

func TestHistogramReconstructsAllCases(t *testing.T) {
	for name, c := range diffCases() {
		t.Run(name, func(t *testing.T) {
			checkEngineReconstructs(t, "histogram", difftext.UnifiedHistogram, c[0], c[1])
			// Myers through the same boundary must also reconstruct: parity of
			// correctness, even where the two choose different hunks.
			checkEngineReconstructs(t, "myers", difftext.Unified, c[0], c[1])
		})
	}
}

// TestHistogramDiffersFromMyersOnMovedBlock proves the engine seam is actually
// exercised end to end: on a moved block the two engines anchor differently, so
// their rendered output must diverge. Both must still reconstruct their inputs —
// divergence is in hunk shape, not correctness. This guards against a regression
// where UnifiedHistogram silently produced Myers output for every input.
func TestHistogramDiffersFromMyersOnMovedBlock(t *testing.T) {
	old := joinLines([]string{"funcA{", "bodyA", "}", "funcB{", "bodyB", "}"})
	new := joinLines([]string{"funcB{", "bodyB", "}", "funcA{", "bodyA", "}"})
	myers, err := difftext.Unified("a", "b", old, new, 3)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	hist, err := difftext.UnifiedHistogram("a", "b", old, new, 3)
	if err != nil {
		t.Fatalf("UnifiedHistogram: %v", err)
	}
	if myers == hist {
		t.Errorf("histogram output should differ from Myers on a moved block:\n%s", hist)
	}
}

func TestHistogramDeterministic(t *testing.T) {
	for name, c := range diffCases() {
		first, err := difftext.UnifiedHistogram("a", "b", joinLines(c[0]), joinLines(c[1]), 3)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for i := 0; i < 3; i++ {
			again, err := difftext.UnifiedHistogram("a", "b", joinLines(c[0]), joinLines(c[1]), 3)
			if err != nil || again != first {
				t.Errorf("%s: histogram output not deterministic", name)
				break
			}
		}
	}
}

func TestHistogramIdenticalIsEmpty(t *testing.T) {
	out, err := difftext.UnifiedHistogram("a", "b", []byte("x\ny\n"), []byte("x\ny\n"), 3)
	if err != nil || out != "" {
		t.Errorf("identical inputs: out=%q err=%v", out, err)
	}
}

func TestHistogramFrequencyCapStillCorrect(t *testing.T) {
	// A line repeated far past the frequency cap is excluded from anchoring, so
	// the region falls back to Myers — but the result must still be a correct
	// diff that reconstructs both sides.
	old := make([]string, 0, 210)
	new := make([]string, 0, 210)
	for i := 0; i < 200; i++ {
		old = append(old, "x")
		new = append(new, "x")
	}
	old = append(old, "old-tail")
	new = append(new, "new-tail")
	checkEngineReconstructs(t, "histogram-freqcap", difftext.UnifiedHistogram, old, new)
}

func TestHistogramCRLFvsLFIsHonest(t *testing.T) {
	out, err := difftext.UnifiedHistogram("a", "b", []byte("hello\r\nworld\n"), []byte("hello\nworld\n"), 1)
	if err != nil {
		t.Fatalf("UnifiedHistogram: %v", err)
	}
	if !strings.Contains(out, "-hello\r") {
		t.Errorf("CRLF line should be reported with its CR:\n%q", out)
	}
}

func TestHistogramEOFNewlineChange(t *testing.T) {
	out, err := difftext.UnifiedHistogram("a", "b", []byte("a"), []byte("a\n"), 3)
	if err != nil {
		t.Fatalf("UnifiedHistogram: %v", err)
	}
	if !strings.Contains(out, "\\ No newline at end of file") {
		t.Errorf("missing no-newline marker:\n%s", out)
	}
}

func TestHistogramOversizedRefusedLikeMyers(t *testing.T) {
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := difftext.UnifiedHistogram("a", "b", big, []byte("x\n"), 3); err != difftext.ErrTooLarge {
		t.Fatalf("oversized input should be refused with ErrTooLarge, got %v", err)
	}
}
