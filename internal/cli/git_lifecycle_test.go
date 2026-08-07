package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitCmd runs a git command in dir, failing the test on error.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initGitRepo initializes a git repo in dir with a first commit, returning nothing.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "a.txt")
	gitCmd(t, dir, "commit", "-m", "first commit")
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// initGitProject makes a temp dir that is both a git repo (with one commit) and an
// initialized awa project.
func initGitProject(t *testing.T) string {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	initGitRepo(t, root)
	if code, _, stderr := run("init", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("awa init exit = %d, stderr = %q", code, stderr)
	}
	return root
}

// TestBaselinePredatesHead: a checkpoint taken at commit A, then a second commit,
// makes the baseline predate the current HEAD. changes must note it without failing.
func TestBaselinePredatesHead(t *testing.T) {
	root := initGitProject(t)
	// Checkpoint at the first commit.
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "before more commits"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	// Move HEAD forward with a second commit.
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "add", "b.txt")
	gitCmd(t, root, "commit", "-m", "second commit")

	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "git: branch ") {
		t.Errorf("changes header should name the current git branch/HEAD:\n%s", stdout)
	}
	if !strings.Contains(stderr, "baseline predates git HEAD") {
		t.Errorf("changes should note the baseline predates HEAD:\nstderr=%s", stderr)
	}
}

// TestBaselineDivergesFromHead: after an amend rewrites HEAD, the baseline commit is
// no longer an ancestor, so freshness is "differs", never a confident wrong answer.
func TestBaselineDivergesFromHead(t *testing.T) {
	root := initGitProject(t)
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "before amend"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	// Amend the tip so the baseline's commit is no longer HEAD nor an ancestor of it.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("amended"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "add", "a.txt")
	gitCmd(t, root, "commit", "--amend", "-m", "amended commit")

	code, _, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "diverges from git HEAD") {
		t.Errorf("an amended HEAD should make the baseline diverge:\nstderr=%s", stderr)
	}
}

// TestEmptyDeltaCoverageNote: an empty awa delta with a dirty/moved git worktree
// must print the review-coverage note and never imply git is clean.
func TestEmptyDeltaCoverageNote(t *testing.T) {
	root := initGitProject(t)
	// Checkpoint the current tree so there is no awa delta.
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "clean baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "no changes") {
		t.Errorf("expected 'no changes' on stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "review the full git diff before acceptance") {
		t.Errorf("empty delta must carry the review-coverage note:\nstderr=%s", stderr)
	}
	// The most dangerous case: an empty delta with no skipped inputs must STILL carry the
	// ignored-paths scope caveat, not hide it in JSON alone.
	if !strings.Contains(stderr, "ignored paths are outside awa evidence") {
		t.Errorf("empty delta must carry the ignored-paths scope caveat:\nstderr=%s", stderr)
	}
	// The scope caveat is stderr-only advisory; stdout stays a clean "no changes".
	if strings.Contains(stdout, "ignored paths") {
		t.Errorf("the scope caveat must not pollute stdout:\n%s", stdout)
	}

	// --name-only is the pipe-clean scripting form and must omit the caveat on both streams.
	code, nameOut, nameErr := run("changes", "--name-only", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes --name-only exit = %d, stderr = %q", code, nameErr)
	}
	if strings.Contains(nameOut, "ignored paths") || strings.Contains(nameErr, "ignored paths") {
		t.Errorf("--name-only must not emit the scope caveat:\nstdout=%s\nstderr=%s", nameOut, nameErr)
	}
}

// TestCommitBoundaryInTimeline: a git commit between two checkpoints appears as a
// context marker in "awa log --all" and is not addressable as a state reference.
func TestCommitBoundaryInTimeline(t *testing.T) {
	root := initGitProject(t)
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "before commit"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint A exit = %d, stderr = %q", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("3"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "add", "c.txt")
	gitCmd(t, root, "commit", "-m", "between checkpoints")
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "after commit"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint B exit = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr := run("log", "--all", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("log --all exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "-- git commit ") || !strings.Contains(stdout, "between checkpoints") {
		t.Errorf("log --all should interleave a git commit boundary:\n%s", stdout)
	}

	// The git-commit boundary is a context marker in the human timeline. In JSON it is a
	// typed event with no "ref" field.
	code, jsonOut, stderr := run("log", "--all", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("log --all --json exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(jsonOut, `"kind": "git_commit"`) {
		t.Errorf("JSON timeline should carry a git_commit event:\n%s", jsonOut)
	}
}

// TestNonGitSafety: a non-git awa project shows no git notes on changes, and
// "gc --committed --dry-run" deletes nothing and explains git metadata is unavailable.
func TestNonGitSafety(t *testing.T) {
	root := initProject(t) // non-git temp dir
	checkpointFile(t, root, "f.txt", "x")

	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	if strings.Contains(stdout, "git: ") || strings.Contains(stderr, "predates git HEAD") {
		t.Errorf("a non-git changes report must not show a git line or freshness note:\nstdout=%s\nstderr=%s", stdout, stderr)
	}

	code, gcOut, stderr := run("gc", "--committed", "--dry-run", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("gc --committed --dry-run exit = %d, stderr = %q", code, stderr)
	}
	// Nothing removed, and the reason (git unavailable) is surfaced.
	if !strings.Contains(gcOut, "nothing to remove") {
		t.Errorf("non-git gc --committed should remove nothing:\n%s", gcOut)
	}
	if !strings.Contains(stderr, "git metadata unavailable") && !strings.Contains(stderr, "git unavailable") {
		t.Errorf("non-git gc --committed should explain git is unavailable:\nstderr=%s", stderr)
	}
}

// TestStatusGitStateToken: the status dashboard JSON carries a closed git.state
// token ("available" in a git repo, "non-git" otherwise), not an available/reason combo.
func TestStatusGitStateToken(t *testing.T) {
	gitRoot := initGitProject(t)
	if code, _, stderr := run("checkpoint", "--root", gitRoot, "-m", "base"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	code, jsonOut, stderr := run("status", "--json", "--root", gitRoot)
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(jsonOut, `"state": "available"`) {
		t.Errorf("git repo status should report git.state=available:\n%s", jsonOut)
	}

	nonGit := initProject(t)
	checkpointFile(t, nonGit, "f.txt", "x")
	code, jsonOut, stderr = run("status", "--json", "--root", nonGit)
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(jsonOut, `"state": "non-git"`) {
		t.Errorf("non-git project status should report git.state=non-git:\n%s", jsonOut)
	}
}

// TestCommittedGcRetainedByReason: gc --committed --dry-run reports the retained
// records grouped by reason.
func TestCommittedGcRetainedByReason(t *testing.T) {
	root := initGitProject(t)
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	code, stdout, stderr := run("gc", "--committed", "--dry-run", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("gc --committed --dry-run exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "retained:") {
		t.Errorf("gc should report a retained breakdown:\n%s", stdout)
	}
	// The latest checkpoint is always protected under the committed policy.
	if !strings.Contains(stdout, "checkpoint-latest:") {
		t.Errorf("gc --committed should retain the latest checkpoint by reason:\n%s", stdout)
	}
}

// TestChangesJSONCarriesFreshnessAndCoverage: the changes JSON exposes the
// structured freshness token and the always-present review-coverage note.
func TestChangesJSONCarriesFreshnessAndCoverage(t *testing.T) {
	root := initGitProject(t)
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "add", "b.txt")
	gitCmd(t, root, "commit", "-m", "second")

	code, jsonOut, stderr := run("changes", "--json", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes --json exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(jsonOut, `"review_coverage"`) || !strings.Contains(jsonOut, `"checkpoint-delta-only"`) {
		t.Errorf("changes JSON must carry review_coverage:\n%s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"freshness"`) || !strings.Contains(jsonOut, `"predates-head"`) {
		t.Errorf("changes JSON must carry the freshness token:\n%s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"scope"`) || !strings.Contains(jsonOut, `"ignored_paths_outside_evidence": true`) {
		t.Errorf("changes JSON must carry the scope honesty fact:\n%s", jsonOut)
	}
}
