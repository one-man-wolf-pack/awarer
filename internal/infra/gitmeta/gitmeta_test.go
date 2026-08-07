package gitmeta

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureNonGitProjectIsAbsent(t *testing.T) {
	// A plain directory (no git) must yield absent, never an error — even when
	// git is installed, a non-repo is not a failure.
	dir := t.TempDir()
	md, ok, err := New(dir).Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if ok || md != nil {
		t.Fatalf("expected absent, got ok=%v md=%v", ok, md)
	}
}

func TestCaptureGitProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	writeFile(t, dir, "tracked.txt", "hello")
	git(t, dir, "add", "tracked.txt")
	git(t, dir, "commit", "-m", "initial")

	// Dirty the worktree: modify tracked, add untracked.
	writeFile(t, dir, "tracked.txt", "changed")
	writeFile(t, dir, "untracked.txt", "new")

	md, ok, err := New(dir).Capture(context.Background())
	if err != nil || !ok || md == nil {
		t.Fatalf("Capture: ok=%v md=%v err=%v", ok, md, err)
	}
	if !md.InWorktree {
		t.Fatal("InWorktree should be true")
	}
	if md.Commit == "" || md.ShortCommit == "" {
		t.Fatalf("missing commit info: %+v", md)
	}
	if md.Branch == "" {
		t.Fatalf("expected a branch, got %+v", md)
	}
	if md.Dirty.Clean {
		t.Fatal("worktree should be dirty")
	}
	if md.Dirty.Modified != 1 || md.Dirty.Untracked != 1 {
		t.Fatalf("dirty summary = %+v, want 1 modified + 1 untracked", md.Dirty)
	}
}

func TestCaptureCleanGitProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	writeFile(t, dir, "f.txt", "x")
	git(t, dir, "add", "f.txt")
	git(t, dir, "commit", "-m", "c")

	md, ok, err := New(dir).Capture(context.Background())
	if err != nil || !ok {
		t.Fatalf("Capture: ok=%v err=%v", ok, err)
	}
	if !md.Dirty.Clean {
		t.Fatalf("expected clean, got %+v", md.Dirty)
	}
}

func TestCaptureStatusFailureIsSurfaced(t *testing.T) {
	// The worktree probe and HEAD lookups succeed, but `git status` fails. Because
	// the dirty summary has no honest "unknown" representation, Capture must report
	// a genuine error rather than recording a misleading zero-value summary.
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		return []byte("true\n"), "", nil
	}
	stream := func(_ context.Context, _ func(io.Reader) error, _ ...string) (string, error) {
		return "fatal: unable to read index", errors.New("exit status 128")
	}
	_, ok, err := newWithRunnerStreamer("/x", run, stream).Capture(context.Background())
	if ok {
		t.Fatal("ok should be false when git state could not be captured")
	}
	if err == nil || !strings.Contains(err.Error(), "git status failed") {
		t.Fatalf("err = %v, want a surfaced git status failure", err)
	}
}

func TestCaptureStatusStreamParsesIncrementally(t *testing.T) {
	// A dirty worktree with several classes of change, driven through the streaming
	// seam. The parser must fold each NUL-terminated record without a whole-buffer
	// split, consume a rename's source path, and classify each file exactly once.
	// Paths with spaces and embedded newlines must not confuse record boundaries.
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		if args[0] == "rev-parse" && args[1] == "--is-inside-work-tree" {
			return []byte("true\n"), "", nil
		}
		return []byte("\n"), "", nil
	}
	// Records: modified (with a space), untracked, deleted, added, and a rename whose
	// source path (with an embedded newline) follows as its own NUL-terminated token.
	porcelain := "M  a file.txt\x00" +
		"?? new one.txt\x00" +
		" D gone.txt\x00" +
		"A  added.txt\x00" +
		"R  renamed to.txt\x00old\nname.txt\x00"
	stream := func(_ context.Context, consume func(io.Reader) error, _ ...string) (string, error) {
		return "", consume(strings.NewReader(porcelain))
	}
	md, ok, err := newWithRunnerStreamer("/x", run, stream).Capture(context.Background())
	if err != nil || !ok {
		t.Fatalf("Capture: ok=%v err=%v", ok, err)
	}
	got := md.Dirty
	if got.Clean {
		t.Fatal("worktree should be dirty")
	}
	if got.Modified != 1 || got.Untracked != 1 || got.Deleted != 1 || got.Added != 1 || got.Renamed != 1 {
		t.Fatalf("dirty = %+v, want 1 of each class (M/??/D/A/R)", got)
	}
}

func TestFoldDirtyStreamParity(t *testing.T) {
	// The incremental fold must produce the same counts a whole-buffer parse would on
	// a representative dirty output, including a rename's consumed source record.
	porcelain := "M  m.txt\x00?? u.txt\x00 D d.txt\x00A  a.txt\x00C  copy.txt\x00src.txt\x00 M m2.txt\x00"
	d, err := foldDirtyStream(strings.NewReader(porcelain))
	if err != nil {
		t.Fatalf("foldDirtyStream: %v", err)
	}
	if d.Modified != 2 || d.Untracked != 1 || d.Deleted != 1 || d.Added != 1 || d.Renamed != 1 {
		t.Fatalf("dirty = %+v, want M=2 U=1 D=1 A=1 R=1", d)
	}
}

func TestFoldDirtyStreamEmptyIsClean(t *testing.T) {
	d, err := foldDirtyStream(strings.NewReader(""))
	if err != nil {
		t.Fatalf("foldDirtyStream: %v", err)
	}
	if !d.Clean {
		t.Fatalf("empty output should be clean, got %+v", d)
	}
}

func TestFoldDirtyStreamTruncatedRecordFails(t *testing.T) {
	// A final record with no terminating NUL means git's output was truncated. That
	// must fail loud rather than silently drop or miscount the change.
	if _, err := foldDirtyStream(strings.NewReader("M  ok.txt\x00 M truncated.txt")); err == nil {
		t.Fatal("expected a truncated-record error, got nil")
	}
}

func TestFoldDirtyStreamTruncatedRenameSourceFails(t *testing.T) {
	// A rename record whose source-path token is missing at end of stream is truncation.
	if _, err := foldDirtyStream(strings.NewReader("R  new.txt\x00")); err == nil {
		t.Fatal("expected a truncated rename-source error, got nil")
	}
}

func TestLatestCommitNoGitBinaryIsAbsent(t *testing.T) {
	// Git is not installed: --committed must delete nothing, so LatestCommit reports
	// absent without an error.
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		return nil, "", exec.ErrNotFound
	}
	info, ok, err := newWithRunner("/x", run).LatestCommit(context.Background())
	if err != nil || ok {
		t.Fatalf("no-git LatestCommit: ok=%v err=%v info=%+v", ok, err, info)
	}
}

func TestLatestCommitNotAWorktreeIsAbsent(t *testing.T) {
	// The probe succeeds but reports this is not a worktree (e.g. the .git dir but not
	// a working tree): absent, not an error.
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		if args[0] == "rev-parse" && args[1] == "--is-inside-work-tree" {
			return []byte("false\n"), "", nil
		}
		return []byte("\n"), "", nil
	}
	_, ok, err := newWithRunner("/x", run).LatestCommit(context.Background())
	if err != nil || ok {
		t.Fatalf("not-a-worktree LatestCommit: ok=%v err=%v", ok, err)
	}
}

func TestLatestCommitUnbornBranchIsAbsent(t *testing.T) {
	// A worktree with no commits yet: absent, not an error (nothing committed).
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return nil, "fatal: your current branch 'main' does not have any commits yet", errors.New("exit status 128")
		default:
			return []byte("\n"), "", nil
		}
	}
	_, ok, err := newWithRunner("/x", run).LatestCommit(context.Background())
	if err != nil || ok {
		t.Fatalf("unborn-branch LatestCommit: ok=%v err=%v", ok, err)
	}
}

func TestLatestCommitReturnsTimestampBranchHash(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return []byte("2026-06-24T10:00:00+00:00\n"), "", nil
		case args[0] == "rev-parse" && args[1] == "--abbrev-ref":
			return []byte("main\n"), "", nil
		case args[0] == "rev-parse":
			return []byte("abc123def456\n"), "", nil
		default:
			return []byte("\n"), "", nil
		}
	}
	info, ok, err := newWithRunner("/x", run).LatestCommit(context.Background())
	if err != nil || !ok {
		t.Fatalf("LatestCommit: ok=%v err=%v", ok, err)
	}
	if info.Branch != "main" || info.Commit != "abc123def456" {
		t.Fatalf("info = %+v, want branch main, commit abc123def456", info)
	}
	if info.Committed.UTC().Format("2006-01-02T15:04:05") != "2026-06-24T10:00:00" {
		t.Fatalf("committed timestamp = %v", info.Committed)
	}
}

func TestLatestCommitGenuineFailureIsSurfaced(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return nil, "fatal: bad object HEAD", errors.New("exit status 128")
		default:
			return []byte("\n"), "", nil
		}
	}
	_, ok, err := newWithRunner("/x", run).LatestCommit(context.Background())
	if ok || err == nil || !strings.Contains(err.Error(), "git log failed") {
		t.Fatalf("genuine git failure: ok=%v err=%v", ok, err)
	}
}

func TestLatestCommitMalformedTimestampIsError(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return []byte("not-a-timestamp\n"), "", nil
		default:
			return []byte("\n"), "", nil
		}
	}
	_, ok, err := newWithRunner("/x", run).LatestCommit(context.Background())
	if ok || err == nil || !strings.Contains(err.Error(), "parsing commit timestamp") {
		t.Fatalf("malformed timestamp: ok=%v err=%v", ok, err)
	}
}

func TestCurrentHeadParsesFields(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return []byte("abc123full\x1fabc123\x1f2026-06-24T10:00:00+00:00\x1ffix the parser cache\n"), "", nil
		case args[0] == "rev-parse" && args[1] == "--abbrev-ref":
			return []byte("main\n"), "", nil
		default:
			return []byte("\n"), "", nil
		}
	}
	h, ok, err := newWithRunner("/x", run).CurrentHead(context.Background())
	if err != nil || !ok {
		t.Fatalf("CurrentHead: ok=%v err=%v", ok, err)
	}
	if h.Commit != "abc123full" || h.ShortCommit != "abc123" {
		t.Fatalf("hashes = %q/%q", h.Commit, h.ShortCommit)
	}
	if h.Subject != "fix the parser cache" {
		t.Fatalf("subject = %q", h.Subject)
	}
	if h.Branch != "main" {
		t.Fatalf("branch = %q", h.Branch)
	}
	if h.Committed.UTC().Format("2006-01-02T15:04:05") != "2026-06-24T10:00:00" {
		t.Fatalf("committed = %v", h.Committed)
	}
}

func TestCurrentHeadDetachedHasNoBranch(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return []byte("full\x1fshort\x1f2026-06-24T10:00:00Z\x1fsubject\n"), "", nil
		case args[0] == "rev-parse" && args[1] == "--abbrev-ref":
			return []byte("HEAD\n"), "", nil // detached
		default:
			return []byte("\n"), "", nil
		}
	}
	h, ok, err := newWithRunner("/x", run).CurrentHead(context.Background())
	if err != nil || !ok {
		t.Fatalf("CurrentHead: ok=%v err=%v", ok, err)
	}
	if h.Branch != "" {
		t.Fatalf("detached HEAD should have no branch, got %q", h.Branch)
	}
}

func TestCurrentHeadNonGitIsAbsent(t *testing.T) {
	run := func(_ context.Context, _ ...string) ([]byte, string, error) {
		return nil, "", exec.ErrNotFound
	}
	_, ok, err := newWithRunner("/x", run).CurrentHead(context.Background())
	if ok || err != nil {
		t.Fatalf("non-git CurrentHead: ok=%v err=%v", ok, err)
	}
}

func TestCurrentHeadUnbornBranchIsAbsent(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return nil, "fatal: your current branch 'main' does not have any commits yet", errors.New("exit status 128")
		default:
			return []byte("\n"), "", nil
		}
	}
	_, ok, err := newWithRunner("/x", run).CurrentHead(context.Background())
	if ok || err != nil {
		t.Fatalf("unborn CurrentHead: ok=%v err=%v", ok, err)
	}
}

func TestCurrentHeadMalformedLineIsError(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return []byte("only-two\x1ffields\n"), "", nil
		default:
			return []byte("\n"), "", nil
		}
	}
	_, ok, err := newWithRunner("/x", run).CurrentHead(context.Background())
	if ok || err == nil || !strings.Contains(err.Error(), "unexpected git commit line") {
		t.Fatalf("malformed line: ok=%v err=%v", ok, err)
	}
}

func TestIsAncestorTriState(t *testing.T) {
	probeCode := func(code int, runErr error) gitProber {
		return func(_ context.Context, _ ...string) (int, string, error) { return code, "", runErr }
	}
	cases := []struct {
		name       string
		probe      gitProber
		wantIs     bool
		wantKnown  bool
		wantErrNil bool
	}{
		{"is ancestor", probeCode(0, nil), true, true, true},
		{"not ancestor", probeCode(1, nil), false, true, true},
		{"bad revision (128) undetermined", probeCode(128, nil), false, false, true},
		{"git missing undetermined", probeCode(-1, exec.ErrNotFound), false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is, known, err := newWithRunnerProber("/x", nil, tc.probe).IsAncestor(context.Background(), "a", "b")
			if is != tc.wantIs || known != tc.wantKnown || (err == nil) != tc.wantErrNil {
				t.Fatalf("IsAncestor = (%v,%v,%v)", is, known, err)
			}
		})
	}
}

func TestCommitsBetweenParsesAndOrders(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		switch {
		case args[0] == "rev-parse" && args[1] == "--is-inside-work-tree":
			return []byte("true\n"), "", nil
		case args[0] == "log":
			return []byte(
				"h2full\x1fh2\x1f2026-06-24T12:00:00Z\x1fsecond\n" +
					"h1full\x1fh1\x1f2026-06-24T11:00:00Z\x1ffirst\n"), "", nil
		default:
			return []byte("\n"), "", nil
		}
	}
	since := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 24, 13, 0, 0, 0, time.UTC)
	commits, ok, err := newWithRunner("/x", run).CommitsBetween(context.Background(), since, until, 100)
	if err != nil || !ok {
		t.Fatalf("CommitsBetween: ok=%v err=%v", ok, err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
	if commits[0].ShortCommit != "h2" || commits[0].Subject != "second" {
		t.Fatalf("first entry = %+v", commits[0])
	}
	if commits[1].ShortCommit != "h1" {
		t.Fatalf("second entry = %+v", commits[1])
	}
}

func TestCommitsBetweenNonGitIsAbsent(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, string, error) {
		if args[0] == "rev-parse" && args[1] == "--is-inside-work-tree" {
			return []byte("false\n"), "", nil
		}
		return []byte("\n"), "", nil
	}
	commits, ok, err := newWithRunner("/x", run).CommitsBetween(context.Background(), time.Now().UTC(), time.Now().UTC(), 10)
	if ok || err != nil || commits != nil {
		t.Fatalf("non-git CommitsBetween: ok=%v err=%v commits=%v", ok, err, commits)
	}
}

func TestCurrentHeadIsAncestorCommitsBetweenRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	writeFile(t, dir, "a.txt", "1")
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-m", "first commit")
	firstHash := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))

	writeFile(t, dir, "b.txt", "2")
	git(t, dir, "add", "b.txt")
	git(t, dir, "commit", "-m", "second commit")
	headHash := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))

	p := New(dir)
	h, ok, err := p.CurrentHead(context.Background())
	if err != nil || !ok {
		t.Fatalf("CurrentHead: ok=%v err=%v", ok, err)
	}
	if h.Commit != headHash || h.Subject != "second commit" {
		t.Fatalf("head = %+v, want commit %s subject 'second commit'", h, headHash)
	}

	// first is an ancestor of HEAD; HEAD is not an ancestor of first.
	is, known, err := p.IsAncestor(context.Background(), firstHash, headHash)
	if err != nil || !known || !is {
		t.Fatalf("IsAncestor(first, head) = (%v,%v,%v), want ancestor", is, known, err)
	}
	is, known, err = p.IsAncestor(context.Background(), headHash, firstHash)
	if err != nil || !known || is {
		t.Fatalf("IsAncestor(head, first) = (%v,%v,%v), want not-ancestor", is, known, err)
	}
	// A commit that does not exist -> undetermined (known=false), never a false answer.
	_, known, err = p.IsAncestor(context.Background(), "0000000000000000000000000000000000000000", headHash)
	if err != nil || known {
		t.Fatalf("IsAncestor(missing, head) known=%v err=%v, want undetermined", known, err)
	}

	commits, ok, err := p.CommitsBetween(context.Background(),
		time.Now().Add(-time.Hour).UTC(), time.Now().Add(time.Hour).UTC(), 100)
	if err != nil || !ok {
		t.Fatalf("CommitsBetween: ok=%v err=%v", ok, err)
	}
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func TestClassifyProbe(t *testing.T) {
	// git binary missing -> absent.
	if absent, genuine := classifyProbe(exec.ErrNotFound, ""); !absent || genuine != nil {
		t.Errorf("ErrNotFound: absent=%v genuine=%v, want absent/no error", absent, genuine)
	}
	// git ran and said "not a git repository" -> absent.
	if absent, genuine := classifyProbe(&exec.ExitError{}, "fatal: not a git repository (or any of the parent directories): .git"); !absent || genuine != nil {
		t.Errorf("not-a-repo: absent=%v genuine=%v, want absent/no error", absent, genuine)
	}
	// git ran but failed for a real reason -> genuine error, not absent.
	absent, genuine := classifyProbe(&exec.ExitError{}, "fatal: detected dubious ownership in repository at '/x'")
	if absent || genuine == nil {
		t.Errorf("dubious ownership: absent=%v genuine=%v, want a genuine error", absent, genuine)
	}
	if genuine != nil && !strings.Contains(genuine.Error(), "dubious ownership") {
		t.Errorf("genuine error should carry git's message, got %v", genuine)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	git(t, dir, "init")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "Test")
	git(t, dir, "config", "commit.gpgsign", "false")
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
