// Package inspect holds the value objects and bounded text helpers that back
// awa's run-output display and inspection: the --display modes of "awa run" and
// the --tail/--grep filters of "awa run show".
//
// Everything here is CLI-independent and works over plain io.Reader/io.Writer
// and byte slices, so it can be unit-tested without driving the whole CLI. The
// helpers are deliberately bounded in memory (ring buffers, streaming line
// scans) because they run against captured validation output that can be large.
package inspect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// maxTailLines caps the line count accepted by --display tail:<n> and
// run show --tail <n>. It is a safety bound: a tail is meant to be a small
// terminal-friendly window, not a way to replay an unbounded log.
const maxTailLines = 100_000

// TailLimit is a validated, positive, bounded number of lines to retain for a
// tail view. It is constructed only through ParseTailLimit or NewTailLimit, so a
// zero value is never a usable limit.
type TailLimit struct {
	n int
}

// Lines reports the number of lines the limit retains.
func (t TailLimit) Lines() int { return t.n }

// NewTailLimit builds a tail limit from an integer, applying the same bounds as
// ParseTailLimit. It is for callers that already hold an int — typically a trusted
// constant — and so need no string round-trip.
func NewTailLimit(n int) (TailLimit, error) {
	if n < 1 {
		return TailLimit{}, fmt.Errorf("invalid tail line count %d: want a positive integer", n)
	}
	if n > maxTailLines {
		return TailLimit{}, fmt.Errorf("tail line count %d exceeds the maximum of %d", n, maxTailLines)
	}
	return TailLimit{n: n}, nil
}

// ParseTailLimit parses a tail line count. It rejects non-numeric input, zero,
// negative values, and values above maxTailLines, returning an error suitable
// for the caller to map to a usage error.
func ParseTailLimit(s string) (TailLimit, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return TailLimit{}, fmt.Errorf("invalid tail line count %q: want a positive integer", s)
	}
	return NewTailLimit(n)
}

// GrepPattern wraps a compiled regular expression (Go regexp syntax) used to
// filter output lines. The zero value is invalid; construct one with
// ParseGrepPattern.
type GrepPattern struct {
	re *regexp.Regexp
}

// ParseGrepPattern compiles a Go-regexp pattern. An invalid pattern returns an
// error the caller maps to a usage error.
func ParseGrepPattern(s string) (GrepPattern, error) {
	re, err := regexp.Compile(s)
	if err != nil {
		return GrepPattern{}, fmt.Errorf("invalid grep pattern %q: %v", s, err)
	}
	return GrepPattern{re: re}, nil
}

// Match reports whether the pattern matches line.
func (g GrepPattern) Match(line string) bool { return g.re != nil && g.re.MatchString(line) }

// String returns the original pattern text.
func (g GrepPattern) String() string {
	if g.re == nil {
		return ""
	}
	return g.re.String()
}

// Valid reports whether the pattern was compiled.
func (g GrepPattern) Valid() bool { return g.re != nil }

// DisplayKind enumerates the terminal display modes of "awa run". Display is a
// per-invocation presentation concern: it never affects capture, the cache key,
// stored payloads, or the wrapped command's exit code.
type DisplayKind int

const (
	// DisplayFull prints captured output normally (the default).
	DisplayFull DisplayKind = iota
	// DisplaySummary prints run metadata plus a bounded, non-authoritative excerpt.
	DisplaySummary
	// DisplayTail prints only the last n lines of captured output.
	DisplayTail
	// DisplayNone prints only awa diagnostics/metadata, no child output.
	DisplayNone
)

// DisplayMode is the parsed --display value: a kind plus, for DisplayTail, the
// line limit. The zero value is DisplayFull, matching the default.
type DisplayMode struct {
	kind DisplayKind
	tail TailLimit
}

// DefaultDisplayMode returns the default mode (full).
func DefaultDisplayMode() DisplayMode { return DisplayMode{kind: DisplayFull} }

// Kind reports the display kind.
func (m DisplayMode) Kind() DisplayKind { return m.kind }

// Tail reports the tail limit; it is meaningful only when Kind is DisplayTail.
func (m DisplayMode) Tail() TailLimit { return m.tail }

// String renders the mode as the stable token used in JSON output.
func (m DisplayMode) String() string {
	switch m.kind {
	case DisplayFull:
		return "full"
	case DisplaySummary:
		return "summary"
	case DisplayTail:
		return "tail"
	case DisplayNone:
		return "none"
	default:
		return "full"
	}
}

// ParseDisplayMode parses a --display value: "full", "summary", "none", or
// "tail:<n>". An unknown name or a malformed tail count returns an error the
// caller maps to a usage error.
func ParseDisplayMode(s string) (DisplayMode, error) {
	switch s {
	case "full":
		return DisplayMode{kind: DisplayFull}, nil
	case "summary":
		return DisplayMode{kind: DisplaySummary}, nil
	case "none":
		return DisplayMode{kind: DisplayNone}, nil
	}
	if rest, ok := strings.CutPrefix(s, "tail:"); ok {
		limit, err := ParseTailLimit(rest)
		if err != nil {
			return DisplayMode{}, err
		}
		return DisplayMode{kind: DisplayTail, tail: limit}, nil
	}
	return DisplayMode{}, fmt.Errorf("invalid display mode %q: want full, summary, tail:<n>, or none", s)
}
