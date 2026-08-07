// Package gitmeta captures best-effort git context for a checkpoint.
//
// It shells out to the git binary with an explicit working-tree root (git -C
// <root>), never depending on the caller's current directory, and it is
// deliberately forgiving: a project that is not a git repo, or a host without
// git installed, yields an absent result rather than an error, because git
// metadata must never make a non-git checkpoint fail.
package gitmeta

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/gitfresh"
)

// gitRunner runs one git command and returns its stdout, stderr, and error. It is
// the seam that lets tests drive git behavior (e.g. a failing status) without a
// real repository. It is used only for commands with small, bounded output
// (rev-parse, log); the potentially large `git status -z` stream goes through
// gitStreamer instead so its output is never buffered whole.
type gitRunner func(ctx context.Context, args ...string) (stdout []byte, stderr string, err error)

// gitProber runs one git command whose meaning is carried by its exit code rather
// than its stdout — `merge-base --is-ancestor`, which prints nothing and answers only
// through exit 0/1/other. It returns the exit code and stderr; runErr is non-nil only
// when the process could not run at all (missing binary) or the context was cancelled,
// so an ordinary non-zero exit is reported as a code, not an error. It is a separate
// seam because the stdout/stderr gitRunner cannot convey an exit code to a test.
type gitProber func(ctx context.Context, args ...string) (exitCode int, stderr string, runErr error)

// gitStreamer runs one git command and hands consume the command's stdout as a
// stream, returning the command's stderr and error. It is the seam for the one
// git command whose output scales with the worktree — `git status -z` in a dirty
// monorepo — so the parser folds records incrementally and the whole output is
// never held in memory or split into a large temporary slice. consume is called
// exactly once; a consume error is reported only when the command itself exited
// cleanly (a non-zero git exit is the surfaced failure).
type gitStreamer func(ctx context.Context, consume func(stdout io.Reader) error, args ...string) (stderr string, err error)

// Provider captures git metadata for one project root.
type Provider struct {
	root   string
	run    gitRunner
	stream gitStreamer
	probe  gitProber
}

// New builds a provider rooted at the project's absolute root.
func New(root string) *Provider {
	p := &Provider{root: root}
	p.run = p.execGit
	p.stream = p.streamGit
	p.probe = p.execGitCode
	return p
}

// newWithRunner builds a provider with an injected git runner and the real
// streaming git implementation, for tests that do not exercise the status stream.
func newWithRunner(root string, run gitRunner) *Provider {
	p := &Provider{root: root, run: run}
	p.stream = p.streamGit
	p.probe = p.execGitCode
	return p
}

// newWithRunnerStreamer builds a provider with both seams injected, for tests that
// drive the `git status -z` stream without a real repository.
func newWithRunnerStreamer(root string, run gitRunner, stream gitStreamer) *Provider {
	p := &Provider{root: root, run: run, stream: stream}
	p.probe = p.execGitCode
	return p
}

// newWithRunnerProber builds a provider with the stdout runner and the exit-code
// prober injected, for tests that drive the ancestor probe without a real repository.
func newWithRunnerProber(root string, run gitRunner, probe gitProber) *Provider {
	p := &Provider{root: root, run: run, probe: probe}
	p.stream = p.streamGit
	return p
}

// Capture returns git metadata for the project. ok is false (with a nil
// GitMetadata and nil error) when the project is not in a git worktree or git is
// unavailable — both are normal, not failures. A non-nil error is reserved for a
// genuine execution failure that is not "no repo" or "no git" (for example the
// context being cancelled), so the caller can decide whether to surface it.
func (p *Provider) Capture(ctx context.Context) (*checkpoint.GitMetadata, bool, error) {
	out, stderr, err := p.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		// A cancelled or timed-out context is a real failure, not an absent repo.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		absent, genuine := classifyProbe(err, stderr)
		if absent {
			return nil, false, nil
		}
		return nil, false, genuine
	}
	if strings.TrimSpace(string(out)) != "true" {
		return nil, false, nil
	}

	md := checkpoint.GitMetadata{InWorktree: true}

	// Branch: "HEAD" means a detached head, which we record as no branch rather
	// than the literal token.
	if b, _, err := p.run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if br := strings.TrimSpace(string(b)); br != "HEAD" {
			md.Branch = br
		}
	}
	// Commit: absent in a freshly initialized repo with no commits; that is still
	// a worktree, so we keep InWorktree true and leave the commit empty.
	if c, _, err := p.run(ctx, "rev-parse", "HEAD"); err == nil {
		md.Commit = strings.TrimSpace(string(c))
	}
	if c, _, err := p.run(ctx, "rev-parse", "--short", "HEAD"); err == nil {
		md.ShortCommit = strings.TrimSpace(string(c))
	}
	// The dirty summary, unlike branch/commit, has no honest "unknown" form: a
	// zero-value summary reads as "dirty with no changes". So a status failure
	// after the worktree probe succeeded is surfaced as a genuine error (the
	// caller turns it into a warning and records no git context) rather than
	// silently recording a misleading state. The status output is streamed and
	// folded incrementally (foldDirtyStream), so a dirty monorepo with many changed
	// paths never buffers the whole `git status -z` output nor splits it into a
	// large slice; a truncated or malformed record is surfaced as a failure too.
	var dirty checkpoint.DirtySummary
	statStderr, err := p.stream(ctx, func(stdout io.Reader) error {
		var perr error
		dirty, perr = foldDirtyStream(stdout)
		return perr
	}, "status", "--porcelain=v1", "-z")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		msg := strings.TrimSpace(statStderr)
		if msg == "" {
			msg = err.Error()
		}
		return nil, false, fmt.Errorf("git status failed: %s", msg)
	}
	md.Dirty = dirty

	return &md, true, nil
}

// CommitInfo is the conservative git context "awa gc --committed" needs: the
// current branch and the latest commit's timestamp on it. It is intentionally
// minimal — GC uses only the timestamp as an eligibility cutoff and never compares
// trees.
type CommitInfo struct {
	Branch    string
	Commit    string
	Committed time.Time
}

// LatestCommit reports the latest commit on the current branch for the conservative
// --committed policy. ok is false (with a zero CommitInfo and nil error) when the
// project is not a git worktree, git is unavailable, or the branch has no commits
// yet — all normal states under which --committed simply deletes nothing. A non-nil
// error is reserved for a genuine git failure (for example a cancelled context or a
// dubious-ownership repo), which the caller turns into a no-delete plan with a
// reported reason rather than a silent cleanup.
func (p *Provider) LatestCommit(ctx context.Context) (CommitInfo, bool, error) {
	out, stderr, err := p.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CommitInfo{}, false, ctxErr
		}
		absent, genuine := classifyProbe(err, stderr)
		if absent {
			return CommitInfo{}, false, nil
		}
		return CommitInfo{}, false, genuine
	}
	if strings.TrimSpace(string(out)) != "true" {
		return CommitInfo{}, false, nil
	}

	// %cI is the committer date in strict ISO-8601 (RFC3339), the format time.Parse
	// reads directly. A repo with no commits yet fails here; that is "nothing
	// committed", reported as absent rather than an error.
	c, cStderr, err := p.run(ctx, "log", "-1", "--format=%cI", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CommitInfo{}, false, ctxErr
		}
		if isUnbornBranch(cStderr) {
			return CommitInfo{}, false, nil
		}
		msg := strings.TrimSpace(cStderr)
		if msg == "" {
			msg = err.Error()
		}
		return CommitInfo{}, false, fmt.Errorf("git log failed: %s", msg)
	}
	ts := strings.TrimSpace(string(c))
	if ts == "" {
		return CommitInfo{}, false, nil
	}
	committed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return CommitInfo{}, false, fmt.Errorf("parsing commit timestamp %q: %w", ts, err)
	}

	info := CommitInfo{Committed: committed}
	if b, _, err := p.run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if br := strings.TrimSpace(string(b)); br != "HEAD" {
			info.Branch = br
		}
	}
	if h, _, err := p.run(ctx, "rev-parse", "HEAD"); err == nil {
		info.Commit = strings.TrimSpace(string(h))
	}
	return info, true, nil
}

// headFormat is the pretty-format for a single commit line: full hash, short hash,
// committer date (strict RFC3339 via %cI, the format time.Parse reads directly), and
// the single-line subject, separated by ASCII unit separators (%x1f) so a subject
// with spaces or tabs parses unambiguously. It is shared by Head and CommitsBetween so
// the two parse identically.
const headFormat = "--format=%H%x1f%h%x1f%cI%x1f%s"

// unitSep is the ASCII unit separator (0x1f) headFormat delimits fields with. A commit
// subject is single-line and cannot contain it, so a field split is unambiguous.
const unitSep = "\x1f"

// CurrentHead reports the current HEAD for baseline-freshness classification, as the
// domain value object the classifier consumes. ok is false (with a zero head and nil
// error) when the project is not a git worktree, git is unavailable, or the branch has
// no commits yet — all normal states under which a baseline simply cannot be related to
// HEAD. A non-nil error is reserved for a genuine git failure or a malformed commit
// line, which the caller downgrades to "unknown freshness" rather than failing the
// command.
func (p *Provider) CurrentHead(ctx context.Context) (gitfresh.GitHead, bool, error) {
	out, stderr, err := p.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return gitfresh.GitHead{}, false, ctxErr
		}
		absent, genuine := classifyProbe(err, stderr)
		if absent {
			return gitfresh.GitHead{}, false, nil
		}
		return gitfresh.GitHead{}, false, genuine
	}
	if strings.TrimSpace(string(out)) != "true" {
		return gitfresh.GitHead{}, false, nil
	}

	c, cStderr, err := p.run(ctx, "log", "-1", headFormat, "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return gitfresh.GitHead{}, false, ctxErr
		}
		if isUnbornBranch(cStderr) {
			return gitfresh.GitHead{}, false, nil
		}
		msg := strings.TrimSpace(cStderr)
		if msg == "" {
			msg = err.Error()
		}
		return gitfresh.GitHead{}, false, fmt.Errorf("git log failed: %s", msg)
	}
	commit, ok, err := parseCommitLine(strings.TrimRight(string(c), "\n"))
	if err != nil {
		return gitfresh.GitHead{}, false, err
	}
	if !ok {
		return gitfresh.GitHead{}, false, nil
	}
	h := gitfresh.GitHead{
		Commit:      commit.Commit,
		ShortCommit: commit.ShortCommit,
		Subject:     commit.Subject,
		Committed:   commit.Committed,
	}
	if b, _, err := p.run(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if br := strings.TrimSpace(string(b)); br != "HEAD" {
			h.Branch = br
		}
	}
	return h, true, nil
}

// IsAncestor reports whether ancestor is an ancestor of descendant. The result is
// tri-state: known is false when git could not decide — the commit no longer exists
// after a history rewrite (a "bad revision"/exit 128), git is unavailable, or a
// shallow clone's graft hides the answer — so the caller distinguishes "not an
// ancestor" from "cannot determine" and never reports a garbage-collected baseline as
// a confident "differs from HEAD". A non-nil error is reserved for a cancelled
// context; every other failure degrades to known=false.
func (p *Provider) IsAncestor(ctx context.Context, ancestor, descendant string) (isAncestor, known bool, err error) {
	code, _, runErr := p.probe(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, false, ctxErr
		}
		// The process could not run at all (e.g. git missing): undetermined, not fatal.
		return false, false, nil
	}
	switch code {
	case 0:
		return true, true, nil
	case 1:
		return false, true, nil
	default:
		// Exit 128 (bad/unknown revision, not a worktree) or any other non-zero code: the
		// ancestry cannot be decided. Report undetermined rather than a false "not ancestor".
		return false, false, nil
	}
}

// Commit is one commit boundary for the timeline: the identity and committer time of a
// commit that occurred within a bounded window between two awa records.
type Commit struct {
	Commit      string
	ShortCommit string
	Subject     string
	Committed   time.Time
}

// commitWindowCap bounds how many commit boundaries CommitsBetween returns, so a dense
// window between two old records cannot flood the timeline. The window is already
// bounded to the span of the awa records shown, so this is a safety ceiling, not a
// normal limit.
const commitWindowCap = 1000

// CommitsBetween lists commits whose committer time falls within [since, until],
// newest first, bounded by limit (and hard-capped at commitWindowCap). ok is false
// (with nil commits and nil error) when the project is not a git worktree, git is
// unavailable, or the branch has no commits yet — the timeline then simply shows no
// git boundaries. A non-nil error is reserved for a genuine git failure or a malformed
// line.
func (p *Provider) CommitsBetween(ctx context.Context, since, until time.Time, limit int) ([]Commit, bool, error) {
	if limit <= 0 || limit > commitWindowCap {
		limit = commitWindowCap
	}
	out, stderr, err := p.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		absent, genuine := classifyProbe(err, stderr)
		if absent {
			return nil, false, nil
		}
		return nil, false, genuine
	}
	if strings.TrimSpace(string(out)) != "true" {
		return nil, false, nil
	}

	c, cStderr, err := p.run(ctx, "log", headFormat,
		"--since="+since.UTC().Format(time.RFC3339),
		"--until="+until.UTC().Format(time.RFC3339),
		"--max-count="+strconv.Itoa(limit), "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		if isUnbornBranch(cStderr) {
			return nil, false, nil
		}
		msg := strings.TrimSpace(cStderr)
		if msg == "" {
			msg = err.Error()
		}
		return nil, false, fmt.Errorf("git log failed: %s", msg)
	}
	var commits []Commit
	for _, line := range strings.Split(strings.TrimRight(string(c), "\n"), "\n") {
		if line == "" {
			continue
		}
		commit, ok, perr := parseCommitLine(line)
		if perr != nil {
			return nil, false, perr
		}
		if ok {
			commits = append(commits, commit)
		}
	}
	return commits, true, nil
}

// parseCommitLine parses one headFormat-delimited commit line into a Commit. ok is
// false for an empty line (no commit); a malformed non-empty line is an error so a
// format drift is caught rather than silently dropping a commit.
func parseCommitLine(line string) (Commit, bool, error) {
	if strings.TrimSpace(line) == "" {
		return Commit{}, false, nil
	}
	parts := strings.SplitN(line, unitSep, 4)
	if len(parts) < 4 {
		return Commit{}, false, fmt.Errorf("unexpected git commit line %q", line)
	}
	committed, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[2]))
	if err != nil {
		return Commit{}, false, fmt.Errorf("parsing commit timestamp %q: %w", parts[2], err)
	}
	return Commit{
		Commit:      strings.TrimSpace(parts[0]),
		ShortCommit: strings.TrimSpace(parts[1]),
		Committed:   committed,
		Subject:     parts[3],
	}, true, nil
}

// isUnbornBranch reports whether git's stderr is the ordinary "no commits yet"
// message a fresh repo gives for `log HEAD`. That is "nothing committed", a normal
// absent state, not a failure.
func isUnbornBranch(stderr string) bool {
	return strings.Contains(stderr, "does not have any commits yet") ||
		strings.Contains(stderr, "unknown revision") ||
		strings.Contains(stderr, "ambiguous argument 'HEAD'")
}

// gitCommand builds a git invocation rooted at the project, with git's message
// language pinned.
//
// The pin is a correctness requirement, not a preference: isUnbornBranch and
// isNotARepo classify git's stderr by matching its English prose, so under a translated
// locale "no commits yet" and "not a git repository" — both ordinary, benign absences —
// would be misread as genuine git failures. git localizes through gettext, which ignores
// LANGUAGE once the locale is C, so LC_ALL=C is sufficient; LANGUAGE is cleared anyway
// so the intent survives a future change to that precedence. Everything else is
// inherited, because this is awa's own subprocess rather than a supervised child: it is
// not part of any cache identity, and git needs the caller's real environment to find
// its config, credentials, and worktree.
//
// Pinning is safe for every invocation this provider makes because all of them are
// machine-shaped: rev-parse emits refs and hashes, log uses an explicit --format whose
// fields (%H, %h, %cI, %s) are repository bytes git never translates, status uses -z
// porcelain, and merge-base --is-ancestor answers through its exit code. A future
// invocation whose stdout awa forwarded to the user as git's own prose would not belong
// here: pinning it would change what the user reads.
func (p *Provider) gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"-C", p.root}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=")
	return cmd
}

// execGit executes git with the project root as its working tree and returns
// stdout and stderr. stderr is captured (not discarded) so a failed probe can be
// classified as "no repo" versus a genuine git problem from its message.
func (p *Provider) execGit(ctx context.Context, args ...string) ([]byte, string, error) {
	cmd := p.gitCommand(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.String(), err
}

// execGitCode executes git with the project root as its working tree and returns its
// exit code and stderr. A normal non-zero exit is reported as a code with a nil error,
// so a caller that keys off the exit code (merge-base --is-ancestor) is not forced to
// unwrap an *exec.ExitError. runErr is non-nil only when the process could not run at
// all (missing binary) or the context was cancelled.
func (p *Provider) execGitCode(ctx context.Context, args ...string) (int, string, error) {
	cmd := p.gitCommand(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, stderr.String(), ctxErr
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), stderr.String(), nil
	}
	// The process could not be started (e.g. git is not installed).
	return -1, stderr.String(), err
}

// streamGit runs git with the project root as its working tree and streams stdout
// to consume through a pipe, so a large status output is folded as it arrives
// rather than buffered. stderr is still captured whole (it is small and needed to
// classify failures). The child's exit status takes precedence over a consume
// error: a non-zero git exit is the surfaced failure, and a consume (parse) error
// is returned only when git itself exited cleanly. On an early consume error the
// remaining stdout is drained so the child is never left blocked on a full pipe.
func (p *Provider) streamGit(ctx context.Context, consume func(stdout io.Reader) error, args ...string) (string, error) {
	cmd := p.gitCommand(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return stderr.String(), err
	}
	consumeErr := consume(stdout)
	if consumeErr != nil {
		// consume stopped early (a parse failure); drain the rest so Wait does not block.
		_, _ = io.Copy(io.Discard, stdout)
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		return stderr.String(), waitErr
	}
	return stderr.String(), consumeErr
}

// foldDirtyStream folds porcelain v1 (-z) status output into compact counts as it
// streams, without splitting the whole output into a slice. Each record is a NUL
// terminated "XY <path>"; an index rename or copy is followed by its NUL terminated
// source path, which is consumed so it is not counted as its own record. Each
// changed file is folded into exactly one class. A record left unterminated at end
// of stream (git exited but the output was truncated mid-record) is a failure, not a
// silently dropped change.
func foldDirtyStream(r io.Reader) (checkpoint.DirtySummary, error) {
	br := bufio.NewReader(r)
	d := checkpoint.DirtySummary{}
	for {
		rec, err := readZToken(br)
		if err == io.EOF {
			if rec != "" {
				return checkpoint.DirtySummary{}, fmt.Errorf("truncated git status record %q", rec)
			}
			break
		}
		if err != nil {
			return checkpoint.DirtySummary{}, err
		}
		// A well-formed porcelain entry is at least "XY " plus a path; anything shorter
		// is not a status record, so skip it (mirroring the tolerant single-buffer parse).
		if len(rec) < 3 {
			continue
		}
		x, y := rec[0], rec[1]
		switch {
		case x == '?' && y == '?':
			d.Untracked++
		case x == 'R' || y == 'R' || x == 'C' || y == 'C':
			d.Renamed++
			// An index rename/copy is followed by its source-path record; consume it so it
			// is not misread as its own change. Its absence at end of stream is truncation.
			if x == 'R' || x == 'C' {
				src, serr := readZToken(br)
				if serr == io.EOF {
					return checkpoint.DirtySummary{}, fmt.Errorf("truncated git status rename record %q (missing source path)", rec)
				}
				if serr != nil {
					return checkpoint.DirtySummary{}, serr
				}
				_ = src
			}
		case x == 'D' || y == 'D':
			d.Deleted++
		case x == 'A' || y == 'A':
			d.Added++
		default:
			d.Modified++
		}
	}
	d.Clean = d.Modified == 0 && d.Added == 0 && d.Deleted == 0 && d.Renamed == 0 && d.Untracked == 0
	return d, nil
}

// readZToken reads one NUL-terminated token and returns it without the terminator.
// At a clean end of stream it returns ("", io.EOF); a non-empty token followed by
// EOF (no terminating NUL) is returned with io.EOF so the caller can detect a
// truncated record rather than count a partial one.
func readZToken(br *bufio.Reader) (string, error) {
	s, err := br.ReadString(0)
	if err != nil {
		return s, err
	}
	return s[:len(s)-1], nil
}

// classifyProbe interprets a failed `rev-parse --is-inside-work-tree`. Two cases
// mean git is simply not present and the project has no git context: the binary
// is missing, or git ran and reported that this is not a repository. Anything
// else — dubious ownership, a corrupt or unreadable .git, a permission error — is
// a genuine problem the caller should surface rather than record as "no git".
func classifyProbe(err error, stderr string) (absent bool, genuine error) {
	if errors.Is(err, exec.ErrNotFound) {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if isNotARepo(stderr) {
			return true, nil
		}
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Errorf("git: %s", msg)
	}
	// A non-exit error (e.g. the process could not be started for another reason)
	// is genuine.
	return false, err
}

// isNotARepo reports whether git's stderr is its ordinary "this is not a git
// repository" message, the one benign reason an is-inside-work-tree probe fails.
func isNotARepo(stderr string) bool {
	return strings.Contains(stderr, "not a git repository")
}
