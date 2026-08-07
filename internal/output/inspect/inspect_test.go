package inspect

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParseTailLimit(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1", 1, false},
		{"20", 20, false},
		{"100000", 100000, false},
		{" 7 ", 7, false},
		{"0", 0, true},
		{"-3", 0, true},
		{"abc", 0, true},
		{"", 0, true},
		{"100001", 0, true},
		{"3.5", 0, true},
	} {
		got, err := ParseTailLimit(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTailLimit(%q): want error, got %d", tc.in, got.Lines())
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTailLimit(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.Lines() != tc.want {
			t.Errorf("ParseTailLimit(%q) = %d, want %d", tc.in, got.Lines(), tc.want)
		}
	}
}

func TestParseGrepPattern(t *testing.T) {
	g, err := ParseGrepPattern("Passed!|Failed!")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !g.Valid() {
		t.Fatal("pattern should be valid")
	}
	if !g.Match("All Passed! now") || g.Match("nothing here") {
		t.Errorf("match behavior wrong")
	}
	if g.String() != "Passed!|Failed!" {
		t.Errorf("String() = %q", g.String())
	}
	if _, err := ParseGrepPattern("("); err == nil {
		t.Error("invalid regex should error")
	}
}

func TestParseDisplayMode(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantKind DisplayKind
		wantTail int
		wantErr  bool
	}{
		{"full", DisplayFull, 0, false},
		{"summary", DisplaySummary, 0, false},
		{"none", DisplayNone, 0, false},
		{"tail:20", DisplayTail, 20, false},
		{"tail:1", DisplayTail, 1, false},
		{"tail:0", 0, 0, true},
		{"tail:-5", 0, 0, true},
		{"tail:abc", 0, 0, true},
		{"tail:", 0, 0, true},
		{"tail", 0, 0, true},
		{"verbose", 0, 0, true},
		{"", 0, 0, true},
	} {
		got, err := ParseDisplayMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDisplayMode(%q): want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDisplayMode(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got.Kind() != tc.wantKind {
			t.Errorf("ParseDisplayMode(%q) kind = %v, want %v", tc.in, got.Kind(), tc.wantKind)
		}
		if got.Kind() == DisplayTail && got.Tail().Lines() != tc.wantTail {
			t.Errorf("ParseDisplayMode(%q) tail = %d, want %d", tc.in, got.Tail().Lines(), tc.wantTail)
		}
	}
}

func TestDefaultDisplayMode(t *testing.T) {
	if DefaultDisplayMode().Kind() != DisplayFull {
		t.Error("default should be full")
	}
	if DefaultDisplayMode().String() != "full" {
		t.Error("default String should be full")
	}
}

func tail(t *testing.T, in string, n int) ([]string, bool) {
	t.Helper()
	limit, err := ParseTailLimit(strconv.Itoa(n))
	if err != nil {
		t.Fatalf("ParseTailLimit: %v", err)
	}
	lines, overflow, err := TailLines(strings.NewReader(in), limit)
	if err != nil {
		t.Fatalf("TailLines: %v", err)
	}
	return lines, overflow
}

func TestTailLines(t *testing.T) {
	lines, overflow := tail(t, "a\nb\nc\nd\ne\n", 3)
	if want := []string{"c", "d", "e"}; !slices.Equal(lines, want) {
		t.Errorf("tail = %v, want %v", lines, want)
	}
	if !overflow {
		t.Error("expected overflow")
	}

	lines, overflow = tail(t, "a\nb\n", 5)
	if want := []string{"a", "b"}; !slices.Equal(lines, want) {
		t.Errorf("tail = %v, want %v", lines, want)
	}
	if overflow {
		t.Error("did not expect overflow")
	}

	// No trailing newline: last partial line is retained.
	lines, _ = tail(t, "x\ny\nz", 2)
	if want := []string{"y", "z"}; !slices.Equal(lines, want) {
		t.Errorf("tail = %v, want %v", lines, want)
	}

	// CRLF is normalized.
	lines, _ = tail(t, "a\r\nb\r\n", 2)
	if want := []string{"a", "b"}; !slices.Equal(lines, want) {
		t.Errorf("tail = %v, want %v", lines, want)
	}
}

func TestLineRing(t *testing.T) {
	r := NewLineRing(3)
	// Write in arbitrary chunks that split lines mid-stream.
	_, _ = r.Write([]byte("one\ntw"))
	_, _ = r.Write([]byte("o\nthree\nfour\nfi"))
	_, _ = r.Write([]byte("ve\n"))
	if want := []string{"three", "four", "five"}; !slices.Equal(r.Lines(), want) {
		t.Errorf("ring = %v, want %v", r.Lines(), want)
	}
	if !r.Overflowed() {
		t.Error("expected overflow")
	}

	r2 := NewLineRing(5)
	_, _ = r2.Write([]byte("a\nb\nc"))
	if want := []string{"a", "b", "c"}; !slices.Equal(r2.Lines(), want) {
		t.Errorf("ring = %v, want %v", r2.Lines(), want)
	}
	if r2.Overflowed() {
		t.Error("did not expect overflow")
	}
}

func TestLineRingByteCap(t *testing.T) {
	r := newLineRing(1000, 16) // a tiny byte cap forces eviction well before the line cap
	for i := 0; i < 100; i++ {
		_, _ = r.Write([]byte("0123456789\n"))
	}
	total := 0
	for _, l := range r.Lines() {
		total += len(l)
	}
	if total > 16 {
		t.Errorf("retained %d bytes, want <= 16", total)
	}
	if !r.Overflowed() {
		t.Error("expected overflow from byte cap")
	}
}

func TestCollectSample(t *testing.T) {
	// Line cap keeps the most recent lines.
	lines, dropped, err := CollectSample(strings.NewReader("a\nb\nc\nd\n"), nil, 2, noByteCap)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"c", "d"}) || !dropped {
		t.Errorf("line cap: lines=%v dropped=%v", lines, dropped)
	}

	// Predicate filters before the window applies.
	g, _ := ParseGrepPattern("keep")
	lines, _, err = CollectSample(strings.NewReader("keep1\ndrop\nkeep2\n"), g.Match, 10, noByteCap)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(lines, []string{"keep1", "keep2"}) {
		t.Errorf("predicate: lines=%v", lines)
	}

	// Byte cap evicts oldest lines until the total fits.
	lines, dropped, err = CollectSample(strings.NewReader("aaaa\nbbbb\ncccc\n"), nil, 100, 8)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total > 8 || !dropped {
		t.Errorf("byte cap: lines=%v total=%d dropped=%v", lines, total, dropped)
	}
}

// TestCollectSampleLongLine verifies a single very long, newline-free line is
// clipped to the byte cap and reported as truncated, never held in full.
func TestCollectSampleLongLine(t *testing.T) {
	huge := strings.Repeat("x", 1_000_000) // no newline
	lines, dropped, err := CollectSample(strings.NewReader(huge), nil, 10, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || len(lines[0]) != 8 {
		t.Fatalf("clip: got %d lines, first len %d, want one 8-byte line", len(lines), lineLen(lines))
	}
	if !dropped {
		t.Error("expected truncated=true for a clipped line")
	}
}

// TestTailLinesLongLineBounded verifies TailLines (no explicit byte cap) still
// clips a pathological single line to the per-line cap rather than reading it whole.
func TestTailLinesLongLineBounded(t *testing.T) {
	huge := strings.Repeat("y", maxLineBytes+5000) // exceeds the per-line cap, no newline
	limit, _ := ParseTailLimit("5")
	lines, overflow, err := TailLines(strings.NewReader(huge), limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || len(lines[0]) != maxLineBytes {
		t.Fatalf("got %d lines, first len %d, want one %d-byte line", len(lines), lineLen(lines), maxLineBytes)
	}
	if !overflow {
		t.Error("expected overflow=true for a clipped line")
	}

	// GrepTail clips its matched lines the same way.
	g, _ := ParseGrepPattern("y")
	glines, _, err := GrepTail(strings.NewReader(huge), g, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(glines) != 1 || len(glines[0]) != maxLineBytes {
		t.Errorf("grep tail: got %d lines, first len %d, want one %d-byte line", len(glines), lineLen(glines), maxLineBytes)
	}
}

// TestLineRingLargeSingleWrite verifies a single huge newline-free Write is bounded
// to the byte cap and never retains the whole chunk.
func TestLineRingLargeSingleWrite(t *testing.T) {
	r := newLineRing(10, 1024)
	if _, err := r.Write([]byte(strings.Repeat("x", 10<<20))); err != nil { // 10 MiB, no newline
		t.Fatal(err)
	}
	lines := r.Lines()
	if len(lines) != 1 || len(lines[0]) != 1024 {
		t.Fatalf("got %d lines, first len %d, want one 1024-byte line", len(lines), lineLen(lines))
	}
	if !r.Overflowed() {
		t.Error("expected overflow from the byte cap")
	}

	// A single write that exactly fills the cap must not falsely report overflow.
	r2 := newLineRing(10, 1024)
	_, _ = r2.Write([]byte(strings.Repeat("y", 1024)))
	if got := r2.Lines(); len(got) != 1 || len(got[0]) != 1024 {
		t.Fatalf("exact-fill: got %d lines, first len %d", len(got), lineLen(got))
	}
	if r2.Overflowed() {
		t.Error("exact-fill must not overflow")
	}
}

func lineLen(lines []string) int {
	if len(lines) == 0 {
		return -1
	}
	return len(lines[0])
}

func TestGrepStream(t *testing.T) {
	g, _ := ParseGrepPattern("err|fail")
	var buf bytes.Buffer
	n, err := GrepStream(strings.NewReader("ok\nerr here\nfine\nfail now\n"), g, &buf)
	if err != nil {
		t.Fatalf("GrepStream: %v", err)
	}
	if n != 2 {
		t.Errorf("matched %d, want 2", n)
	}
	if buf.String() != "err here\nfail now\n" {
		t.Errorf("got %q", buf.String())
	}

	// No match: empty output, no error.
	buf.Reset()
	n, err = GrepStream(strings.NewReader("a\nb\n"), g, &buf)
	if err != nil || n != 0 || buf.Len() != 0 {
		t.Errorf("no-match: n=%d err=%v out=%q", n, err, buf.String())
	}
}

func TestGrepTail(t *testing.T) {
	g, _ := ParseGrepPattern("hit")
	limit, _ := ParseTailLimit("2")
	lines, overflow, err := GrepTail(strings.NewReader("hit1\nmiss\nhit2\nhit3\n"), g, limit)
	if err != nil {
		t.Fatalf("GrepTail: %v", err)
	}
	if want := []string{"hit2", "hit3"}; !slices.Equal(lines, want) {
		t.Errorf("GrepTail = %v, want %v", lines, want)
	}
	if !overflow {
		t.Error("expected overflow (3 matches, tail 2)")
	}
}
