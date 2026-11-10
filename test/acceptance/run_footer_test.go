package acceptance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// footerSummary matches the compact run summary row: state, optional short id, exit,
// duration and cwd, with an optional technical non-reuse reason.
var footerSummary = regexp.MustCompile(`(?m)^awa run: (stored|hit \(replay\)|record-only|uncached)( [0-9a-f]{12})? exit=(\d+|signal:[A-Z0-9]+) duration=\d+\.\ds cwd=\S+( not-reusable\([a-z-]+\))?$`)

// assertCompactFooter checks that stderr carries exactly one compact summary row with
// the wanted prefix, and returns that row.
func assertCompactFooter(t *testing.T, stderr, wantPrefix string) string {
	t.Helper()
	rows := footerSummary.FindAllString(stderr, -1)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], wantPrefix) {
		t.Fatalf("footer summary rows = %q, want one starting %q:\n%s", rows, wantPrefix, stderr)
	}
	return rows[0]
}

// TestRunFooterHitAndMissShape pins the compact footer for a plain stored miss and the
// following genuine cache hit, which is labeled a replay of the same stored run.
func TestRunFooterHitAndMissShape(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	code, stdout, missErr := awa(t, root, "run", "--", h, "-out", "hello")
	if code != 0 || stdout != "hello" {
		t.Fatalf("miss: exit %d stdout %q", code, stdout)
	}
	assertCompactFooter(t, missErr, "awa run: stored ")
	id := latestRunID(t, root)
	if !strings.HasSuffix(missErr, "inspect: awa run show "+id+" --tail 200\n") {
		t.Errorf("miss footer missing exact inspect command:\n%s", missErr)
	}

	code, stdout, hitErr := awa(t, root, "run", "--", h, "-out", "hello")
	if code != 0 || stdout != "hello" {
		t.Fatalf("hit: exit %d stdout %q", code, stdout)
	}
	assertCompactFooter(t, hitErr, "awa run: hit (replay) "+id[:12]+" exit=0 ")
	if !strings.Contains(hitErr, "inspect: awa run show "+id+" --tail 200\n") {
		t.Errorf("hit footer missing inspect command of the replayed run:\n%s", hitErr)
	}
}

// TestRunFooterCleanRecordingIsCompact pins that a clean --record run prints only its
// summary and inspection command: no repeated non-reuse reason, no compare, no explain.
func TestRunFooterCleanRecordingIsCompact(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	code, stdout, stderr := awa(t, root, "run", "--record", "--", h, "-out", "recorded")
	if code != 0 || stdout != "recorded" {
		t.Fatalf("record: exit %d stdout %q", code, stdout)
	}
	id := latestRunID(t, root)
	want := "awa run: record-only " + id[:12] + " exit=0 "
	row := assertCompactFooter(t, stderr, want)
	if strings.Contains(row, "not-reusable") {
		t.Errorf("deliberate recording repeated as a non-reuse reason: %q", row)
	}
	if stderr != row+"\ninspect: awa run show "+id+" --tail 200\n" {
		t.Errorf("clean recording footer is not base density:\n%s", stderr)
	}
}

// TestRunFooterChildOutcomeAndLongArgv runs an uncached failing child with a long multiline
// argument from a non-root cwd: the child's exit and streams pass through unchanged,
// and the footer stays one summary row that never repeats the argv.
func TestRunFooterChildOutcomeAndLongArgv(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := "first line\nsecond line\x1b[31m " + strings.Repeat("long ", 80)

	code, stdout, stderr := awa(t, root, "run", "--no-cache-failures", "--cwd", "sub", "--", h, "-out", "child-out", "-err", "child-err\n", "-exit", "3", prompt)
	if code != 3 || stdout != "child-out" {
		t.Fatalf("child outcome changed: exit %d stdout %q", code, stdout)
	}
	if !strings.HasPrefix(stderr, "child-err\n") {
		t.Errorf("child stderr not passed through first:\n%s", stderr)
	}
	row := assertCompactFooter(t, stderr, "awa run: record-only ")
	if !strings.Contains(row, " exit=3 ") || !strings.Contains(row, " cwd=sub ") || !strings.HasSuffix(row, "not-reusable(failed-not-cached)") {
		t.Errorf("failed-run summary = %q", row)
	}
	footer := strings.TrimPrefix(stderr, "child-err\n")
	if strings.Contains(footer, "long long") || strings.Contains(footer, "second line") || strings.ContainsRune(footer, 0x1b) || strings.Contains(footer, "explain") {
		t.Errorf("footer repeats argv or leaks controls:\n%q", footer)
	}
}

// TestRunFooterDisplayNoneStillPrintsFooter pins that --display none suppresses the
// command output but still prints the footer (it is awa's own diagnostics).
func TestRunFooterDisplayNoneStillPrintsFooter(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	_, stdout, stderr := awa(t, root, "run", "--display", "none", "--", h, "-out", "hidden")
	if strings.Contains(stdout, "hidden") {
		t.Errorf("--display none must suppress command stdout, got %q", stdout)
	}
	assertCompactFooter(t, stderr, "awa run: stored ")
}
