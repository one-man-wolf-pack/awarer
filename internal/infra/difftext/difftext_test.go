package difftext_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"awarer/internal/infra/difftext"
)

// mustUnified runs Unified and fails on error, returning the diff text.
func mustUnified(t *testing.T, oldName, newName string, old, new []byte, context int) string {
	t.Helper()
	out, err := difftext.Unified(oldName, newName, old, new, context)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	return out
}

func TestIsBinary(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"empty", nil, false},
		{"plain text", []byte("hello\nworld\n"), false},
		{"utf8 text", []byte("café ☃ ok\n"), false},
		{"nul byte", []byte("ab\x00cd"), true},
		{"binary noise", []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := difftext.IsBinary(c.data); got != c.want {
				t.Errorf("IsBinary(%q) = %v, want %v", c.data, got, c.want)
			}
		})
	}
}

func TestUnifiedIdenticalIsEmpty(t *testing.T) {
	out := mustUnified(t, "a", "b", []byte("x\ny\n"), []byte("x\ny\n"), 3)
	if out != "" {
		t.Errorf("identical content should produce no diff, got:\n%s", out)
	}
}

func TestUnifiedModify(t *testing.T) {
	old := []byte("line1\nline2\nline3\n")
	new := []byte("line1\nCHANGED\nline3\n")
	out := mustUnified(t, "old.txt", "new.txt", old, new, 3)
	if !strings.Contains(out, "--- old.txt") || !strings.Contains(out, "+++ new.txt") {
		t.Errorf("missing headers:\n%s", out)
	}
	if !strings.Contains(out, "-line2") || !strings.Contains(out, "+CHANGED") {
		t.Errorf("missing change lines:\n%s", out)
	}
	if !strings.Contains(out, " line1") || !strings.Contains(out, " line3") {
		t.Errorf("missing context lines:\n%s", out)
	}
}

func TestUnifiedAddedFromEmpty(t *testing.T) {
	out := mustUnified(t, "/dev/null", "new.txt", nil, []byte("a\nb\n"), 3)
	if !strings.Contains(out, "+a") || !strings.Contains(out, "+b") {
		t.Errorf("added file should show insertions:\n%s", out)
	}
	if !strings.Contains(out, "@@ -0,0 +1,2 @@") {
		t.Errorf("added file header wrong:\n%s", out)
	}
}

func TestUnifiedDeletedToEmpty(t *testing.T) {
	out := mustUnified(t, "old.txt", "/dev/null", []byte("a\nb\n"), nil, 3)
	if !strings.Contains(out, "-a") || !strings.Contains(out, "-b") {
		t.Errorf("deleted file should show deletions:\n%s", out)
	}
	if !strings.Contains(out, "@@ -1,2 +0,0 @@") {
		t.Errorf("deleted file header wrong:\n%s", out)
	}
}

func TestUnifiedCRLFvsLFIsHonest(t *testing.T) {
	// A CRLF line and an LF line of the same text must show as a change, not be
	// normalized away.
	out := mustUnified(t, "a", "b", []byte("hello\r\nworld\n"), []byte("hello\nworld\n"), 1)
	if out == "" {
		t.Fatal("CRLF vs LF should produce a diff")
	}
	if !strings.Contains(out, "-hello\r") {
		t.Errorf("CRLF line should be reported with its CR:\n%q", out)
	}
}

func TestUnifiedEOFNewlineChange(t *testing.T) {
	// "a" -> "a\n" differs only by the final newline. It must produce a non-empty
	// diff with the "no newline at end of file" marker, not vanish into equal text.
	out := mustUnified(t, "a", "b", []byte("a"), []byte("a\n"), 3)
	if out == "" {
		t.Fatal("adding a trailing newline must produce a diff")
	}
	if !strings.Contains(out, "\\ No newline at end of file") {
		t.Errorf("missing no-newline marker:\n%s", out)
	}
	if !strings.Contains(out, "-a") || !strings.Contains(out, "+a") {
		t.Errorf("EOF-newline change should rewrite the last line:\n%s", out)
	}

	// The reverse direction also diffs, with the marker on the new side.
	rev := mustUnified(t, "a", "b", []byte("a\n"), []byte("a"), 3)
	if rev == "" || !strings.Contains(rev, "\\ No newline at end of file") {
		t.Errorf("removing a trailing newline should diff with the marker:\n%s", rev)
	}
}

func TestUnifiedFinalNewlinePreservedNoFalseDiff(t *testing.T) {
	// Two files that both end in a newline and share content must not diff.
	if out := mustUnified(t, "a", "b", []byte("x\ny\n"), []byte("x\ny\n"), 3); out != "" {
		t.Errorf("identical newline-terminated files must not diff:\n%s", out)
	}
}

func TestUnifiedTooDifferentIsRefused(t *testing.T) {
	// Two large, entirely different inputs exceed the edit-distance cap, so the
	// diff is refused rather than computed at unbounded cost.
	var a, b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&a, "alpha line %d\n", i)
		fmt.Fprintf(&b, "beta line %d\n", i)
	}
	_, err := difftext.Unified("a", "b", []byte(a.String()), []byte(b.String()), 3)
	if !errors.Is(err, difftext.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestUnifiedLargeButSimilarSucceeds(t *testing.T) {
	// A large file with a single changed line stays well under the cap and diffs.
	var a, b strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&a, "line %d\n", i)
		if i == 10000 {
			fmt.Fprintf(&b, "CHANGED %d\n", i)
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	out, err := difftext.Unified("a", "b", []byte(a.String()), []byte(b.String()), 3)
	if err != nil {
		t.Fatalf("Unified: %v", err)
	}
	if !strings.Contains(out, "+CHANGED 10000") {
		t.Errorf("missing the single change in large file")
	}
}

func TestUnifiedIdenticalOversizedIsEmpty(t *testing.T) {
	// Identical inputs are "no diff" even when they exceed the size budget: the
	// equality check wins over the budget, so the contract never leaks ErrTooLarge
	// for unchanged content.
	big := bytes.Repeat([]byte("a"), 5<<20)
	out, err := difftext.Unified("a", "b", big, big, 3)
	if err != nil {
		t.Fatalf("identical oversized inputs should not error, got %v", err)
	}
	if out != "" {
		t.Errorf("identical inputs should produce no diff, got %d bytes", len(out))
	}
}

func TestUnifiedOversizedInputRefused(t *testing.T) {
	// 5 MiB exceeds the per-side byte budget, so the diff is refused outright.
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := difftext.Unified("a", "b", big, []byte("x\n"), 3); !errors.Is(err, difftext.ErrTooLarge) {
		t.Fatalf("oversized input should be refused, got %v", err)
	}
}

func TestUnifiedContextWindow(t *testing.T) {
	// Two distant changes with small context produce two separate hunks.
	var oldLines, newLines []string
	for i := 0; i < 20; i++ {
		oldLines = append(oldLines, "line")
		newLines = append(newLines, "line")
	}
	oldLines[1] = "old-a"
	newLines[1] = "new-a"
	oldLines[18] = "old-b"
	newLines[18] = "new-b"
	out := mustUnified(t, "a", "b", []byte(strings.Join(oldLines, "\n")+"\n"), []byte(strings.Join(newLines, "\n")+"\n"), 1)
	if got := strings.Count(out, "@@ -"); got != 2 {
		t.Errorf("want 2 hunks, got %d:\n%s", got, out)
	}
}
