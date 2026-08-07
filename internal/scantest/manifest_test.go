package scantest_test

import (
	"context"
	"errors"
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

func mustPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", s, err)
	}
	return p
}

func entry(t *testing.T, path string) worktree.Entry {
	t.Helper()
	return worktree.Entry{
		Path: mustPath(t, path),
		Kind: worktree.KindDir,
	}
}

func skip(t *testing.T, path string) worktree.SkippedInput {
	t.Helper()
	return worktree.SkippedInput{
		Path:   mustPath(t, path),
		Kind:   worktree.KindRegular,
		Reason: worktree.ReasonLargeFileSkipPolicy,
		Size:   1,
	}
}

// drain collects the paths a cursor yields, returning them with the cursor's
// terminal error so a test can assert on both the sequence and how it ended.
func drain(t *testing.T, cur worktree.ManifestCursor) ([]string, error) {
	t.Helper()
	defer func() {
		if err := cur.Close(); err != nil {
			t.Fatalf("close cursor: %v", err)
		}
	}()
	var got []string
	for cur.Next() {
		got = append(got, cur.Record().Path().String())
	}
	return got, cur.Err()
}

// TestCanonicalCursorSortsEntriesAndSkipsTogether pins the ordering contract every
// consumer relies on: entries and skips are one path-keyed sequence, not two
// concatenated groups, and the caller's argument order does not reach the cursor.
func TestCanonicalCursorSortsEntriesAndSkipsTogether(t *testing.T) {
	cur := scantest.CanonicalCursor(
		[]worktree.Entry{entry(t, "z/last.txt"), entry(t, "a/first.txt"), entry(t, "m/mid.txt")},
		[]worktree.SkippedInput{skip(t, "n/skipped.bin"), skip(t, "b/skipped.bin")},
	)
	got, err := drain(t, cur)
	if err != nil {
		t.Fatalf("cursor error: %v", err)
	}
	want := []string{"a/first.txt", "b/skipped.bin", "m/mid.txt", "n/skipped.bin", "z/last.txt"}
	if len(got) != len(want) {
		t.Fatalf("got %d records %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestCanonicalCursorRejectsDuplicatePaths proves the Ordered composition is load
// bearing rather than decorative: a fixture that names one path twice — including
// once as an entry and once as a skip — fails on iteration instead of being folded
// into a hash as two records.
func TestCanonicalCursorRejectsDuplicatePaths(t *testing.T) {
	cases := map[string]worktree.ManifestCursor{
		"two entries": scantest.CanonicalCursor(
			[]worktree.Entry{entry(t, "dup.txt"), entry(t, "dup.txt")}, nil),
		"two skips": scantest.CanonicalCursor(
			nil, []worktree.SkippedInput{skip(t, "dup.bin"), skip(t, "dup.bin")}),
		"entry and skip collide": scantest.CanonicalCursor(
			[]worktree.Entry{entry(t, "dup.txt")}, []worktree.SkippedInput{skip(t, "dup.txt")}),
	}
	for name, cur := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := drain(t, cur)
			if !errors.Is(err, worktree.ErrDuplicatePath) {
				t.Fatalf("err = %v, want %v", err, worktree.ErrDuplicatePath)
			}
		})
	}
}

// TestCanonicalStreamOpensIndependentCursors pins the re-openable half of the port:
// a consumer that walks a stream twice (comparison opens one cursor per side,
// verification re-walks a manifest) must see the same records each time, and one
// exhausted cursor must not silently empty the next.
func TestCanonicalStreamOpensIndependentCursors(t *testing.T) {
	stream := scantest.CanonicalStream(
		[]worktree.Entry{entry(t, "b.txt"), entry(t, "a.txt")},
		[]worktree.SkippedInput{skip(t, "c.bin")},
	)
	want := []string{"a.txt", "b.txt", "c.bin"}

	first, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	gotFirst, err := drain(t, first)
	if err != nil {
		t.Fatalf("first cursor: %v", err)
	}

	second, err := stream.Open(context.Background())
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	gotSecond, err := drain(t, second)
	if err != nil {
		t.Fatalf("second cursor: %v", err)
	}

	for _, got := range [][]string{gotFirst, gotSecond} {
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
}
