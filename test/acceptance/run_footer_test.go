package acceptance

import (
	"strings"
	"testing"
)

// assertAlignedFooter checks the footer header token and that each present aligned
// label renders with its canonical padding, so the footer keeps one stable shape
// matching the run show meta block and the status dashboard.
func assertAlignedFooter(t *testing.T, stderr, wantHeader string) {
	t.Helper()
	if !strings.Contains(stderr, wantHeader) {
		t.Errorf("footer missing header %q:\n%s", wantHeader, stderr)
	}
	for _, label := range []string{"  exit:     ", "  duration: "} {
		if !strings.Contains(stderr, label) {
			t.Errorf("footer missing aligned label %q:\n%s", label, stderr)
		}
	}
}

// TestRunFooterHitAndMissShape pins the aligned footer for a plain stored miss and
// the following cache hit: header names state+reuse, and the identity block aligns.
func TestRunFooterHitAndMissShape(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	_, _, missErr := awa(t, root, "run", "--", h, "-out", "hello")
	assertAlignedFooter(t, missErr, "awa run: stored, reusable")
	if !strings.Contains(missErr, "  inspect:  awa run show ") {
		t.Errorf("miss footer missing inspect command:\n%s", missErr)
	}

	_, _, hitErr := awa(t, root, "run", "--", h, "-out", "hello")
	assertAlignedFooter(t, hitErr, "awa run: hit, reusable")
}

// TestRunFooterRecordOnlyShape pins that a --record run is labeled record-only and
// not reusable, and still carries the aligned identity block and inspect command.
func TestRunFooterRecordOnlyShape(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	_, _, stderr := awa(t, root, "run", "--record", "--", h, "-out", "recorded")
	assertAlignedFooter(t, stderr, "awa run: record-only, not reusable")
	if !strings.Contains(stderr, "  inspect:  awa run show ") {
		t.Errorf("record-only footer missing inspect command:\n%s", stderr)
	}
}

// TestRunFooterDisplayNoneStillPrintsFooter pins that --display none suppresses the
// command output but still prints the aligned footer (it is awa's own diagnostics).
func TestRunFooterDisplayNoneStillPrintsFooter(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	_, stdout, stderr := awa(t, root, "run", "--display", "none", "--", h, "-out", "hidden")
	if strings.Contains(stdout, "hidden") {
		t.Errorf("--display none must suppress command stdout, got %q", stdout)
	}
	assertAlignedFooter(t, stderr, "awa run: stored, reusable")
}
