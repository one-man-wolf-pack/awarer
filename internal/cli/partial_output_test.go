package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"awarer/internal/app/changes"
	"awarer/internal/domain/compare"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/output"
)

// oneThenErrCursor yields exactly one change and then fails, so a test can drive the
// streaming renderer into a mid-stream error after output has already been written.
type oneThenErrCursor struct {
	change compare.Change
	err    error
	step   int
	closed bool
}

func (c *oneThenErrCursor) Next() bool {
	c.step++
	return c.step == 1
}
func (c *oneThenErrCursor) Change() compare.Change { return c.change }
func (c *oneThenErrCursor) Err() error {
	if c.step >= 2 {
		return c.err
	}
	return nil
}
func (c *oneThenErrCursor) Close() error {
	c.closed = true
	return nil
}

// cleanCursor yields a fixed list of changes and ends cleanly.
type cleanCursor struct {
	changes []compare.Change
	i       int
	closed  bool
}

func (c *cleanCursor) Next() bool {
	if c.i >= len(c.changes) {
		return false
	}
	c.i++
	return true
}
func (c *cleanCursor) Change() compare.Change { return c.changes[c.i-1] }
func (c *cleanCursor) Err() error             { return nil }
func (c *cleanCursor) Close() error           { c.closed = true; return nil }

func mustRel(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	r, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", s, err)
	}
	return r
}

func addedChange(t *testing.T, path string) compare.Change {
	return compare.Change{Status: compare.Added, NewPath: mustRel(t, path), NewKind: worktree.KindRegular}
}

func deletedChange(t *testing.T, path string) compare.Change {
	return compare.Change{Status: compare.Deleted, OldPath: mustRel(t, path), OldKind: worktree.KindRegular}
}

// TestRenderChangesPartialOutputOnMidStreamError proves the default human view frames
// a cursor error that arrives after output as a partial-output failure: a non-zero
// coded error whose message is prefixed "partial output:", with the already-printed
// change kept but no "no changes" line and no summary that would read as complete.
func TestRenderChangesPartialOutputOnMidStreamError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)

	sentinel := errors.New("manifest line 2 is corrupt")
	cur := &oneThenErrCursor{change: addedChange(t, "a.txt"), err: sentinel}
	sr := changes.StreamResult{Changes: cur}

	err := renderChanges(w, sr, nil, false, false, domainconfig.DefaultTimeDisplay(), baselineGitContext{})
	// The caller owns the StreamResult and closes it (cursor + states) after rendering.
	_ = sr.Close()
	if err == nil {
		t.Fatal("expected a partial-output error, got nil")
	}
	ce, ok := err.(*codedError)
	if !ok {
		t.Fatalf("error type = %T, want *codedError", err)
	}
	if ce.Code() == ExitSuccess {
		t.Error("partial-output error must carry a non-zero exit code")
	}
	if !strings.Contains(err.Error(), "partial output:") {
		t.Errorf("error = %q, want it prefixed with 'partial output:'", err.Error())
	}
	if !strings.Contains(stdout.String(), "A a.txt") {
		t.Errorf("stdout = %q, want the change printed before the failure", stdout.String())
	}
	if strings.Contains(stdout.String(), "no changes") {
		t.Errorf("stdout = %q, must not print 'no changes' after a failure", stdout.String())
	}
	if !cur.closed {
		t.Error("cursor must be closed even on the error path")
	}
}

// TestRenderChangesNameOnlyPartialOutput proves the scripting form also fails loud on
// a mid-stream error once it has emitted a path.
func TestRenderChangesNameOnlyPartialOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)

	cur := &oneThenErrCursor{change: addedChange(t, "a.txt"), err: errors.New("boom")}
	sr := changes.StreamResult{Changes: cur}

	err := renderChanges(w, sr, nil, false, true, domainconfig.DefaultTimeDisplay(), baselineGitContext{})
	// The caller owns the StreamResult and closes it (cursor + states) after rendering.
	_ = sr.Close()
	if err == nil || !strings.Contains(err.Error(), "partial output:") {
		t.Fatalf("err = %v, want a partial-output error", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "a.txt" {
		t.Errorf("stdout = %q, want the single path already streamed", got)
	}
}

// TestRenderChangesCleanStreams proves the success path streams every change and ends
// without error, and that an empty stream reports "no changes".
func TestRenderChangesCleanStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)
	cur := &cleanCursor{changes: []compare.Change{addedChange(t, "a.txt"), addedChange(t, "b.txt")}}
	sr := changes.StreamResult{Changes: cur}
	if err := renderChanges(w, sr, nil, false, false, domainconfig.DefaultTimeDisplay(), baselineGitContext{}); err != nil {
		t.Fatalf("renderChanges: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "A a.txt") || !strings.Contains(out, "A b.txt") {
		t.Errorf("stdout = %q, want both changes streamed", out)
	}
	if strings.Contains(out, "no changes") {
		t.Errorf("stdout = %q, must not say 'no changes' when there are changes", out)
	}
	if _ = sr.Close(); !cur.closed {
		t.Error("StreamResult.Close must close the cursor")
	}

	var s2, e2 bytes.Buffer
	w2 := output.New(&s2, &e2)
	empty := &cleanCursor{}
	sr2 := changes.StreamResult{Changes: empty}
	if err := renderChanges(w2, sr2, nil, false, false, domainconfig.DefaultTimeDisplay(), baselineGitContext{}); err != nil {
		t.Fatalf("renderChanges empty: %v", err)
	}
	if !strings.Contains(s2.String(), "no changes") {
		t.Errorf("empty stream stdout = %q, want 'no changes'", s2.String())
	}
	if _ = sr2.Close(); !empty.closed {
		t.Error("StreamResult.Close must close the cursor on the empty path too")
	}
}

// TestRenderChangesRenameSkippedNote proves the bounded rename presentation policy is
// surfaced but not silent: when rename detection was requested yet skipped for size, the
// changes still print (as plain add/delete) on stdout, the command succeeds, and the
// compact note goes to stderr so a piped stdout stays clean.
func TestRenderChangesRenameSkippedNote(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)
	cur := &cleanCursor{changes: []compare.Change{addedChange(t, "new.go"), deletedChange(t, "old.go")}}
	sr := changes.StreamResult{
		Changes:         cur,
		RenameDetection: compare.RenameDetection{Attempted: true, Applied: false, Reason: compare.RenameReasonLimitExceeded, Limit: 100000},
	}
	if err := renderChanges(w, sr, nil, false, false, domainconfig.DefaultTimeDisplay(), baselineGitContext{}); err != nil {
		t.Fatalf("renderChanges: %v", err)
	}
	if !strings.Contains(stdout.String(), "A new.go") || !strings.Contains(stdout.String(), "D old.go") {
		t.Errorf("stdout = %q, want the changes shown as add/delete", stdout.String())
	}
	if strings.Contains(stdout.String(), "rename detection skipped") {
		t.Errorf("the note must not pollute stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "note: rename detection skipped: change set exceeded limit; showing adds/deletes") {
		t.Errorf("stderr = %q, want the compact rename-skipped note", stderr.String())
	}
}

// TestRenameSkippedNotePolicy proves the shared note helper (used by both changes and
// diff) speaks only for the limit-exceeded outcome: applied and not-attempted stay
// silent, so an ordinary comparison never prints a spurious note.
func TestRenameSkippedNotePolicy(t *testing.T) {
	cases := []struct {
		name     string
		rd       compare.RenameDetection
		wantNote bool
	}{
		{"limit-exceeded", compare.RenameDetection{Attempted: true, Applied: false, Reason: compare.RenameReasonLimitExceeded, Limit: 10}, true},
		{"applied", compare.RenameDetection{Attempted: true, Applied: true, Limit: 10}, false},
		{"not-attempted", compare.RenameDetection{}, false},
		// A future not-applied reason must not inherit the limit-exceeded wording: the
		// note matches the specific reason, not any not-applied outcome.
		{"other-not-applied-reason", compare.RenameDetection{Attempted: true, Applied: false, Reason: "some-future-reason", Limit: 10}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			w := output.New(&stdout, &stderr)
			renameSkippedNote(w, tc.rd)
			got := strings.Contains(stderr.String(), "rename detection skipped")
			if got != tc.wantNote {
				t.Errorf("note printed = %v, want %v (stderr = %q)", got, tc.wantNote, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("note must never touch stdout, got %q", stdout.String())
			}
		})
	}
}

// TestToRenameDetectionView proves the JSON view is a faithful structural mapping of the
// limit-exceeded fact — attempted/applied/reason/limit, not prose.
func TestToRenameDetectionView(t *testing.T) {
	v := toRenameDetectionView(compare.RenameDetection{Attempted: true, Applied: false, Reason: compare.RenameReasonLimitExceeded, Limit: 100000})
	if !v.Attempted || v.Applied || v.Reason != "limit-exceeded" || v.Limit != 100000 {
		t.Fatalf("view = %+v, want {attempted, !applied, limit-exceeded, 100000}", v)
	}
	// The applied case carries no reason (omitempty keeps the JSON clean).
	if got := toRenameDetectionView(compare.RenameDetection{Attempted: true, Applied: true, Limit: 100000}); got.Reason != "" {
		t.Errorf("applied view reason = %q, want empty", got.Reason)
	}
}
