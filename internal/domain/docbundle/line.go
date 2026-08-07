package docbundle

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Line is one line of publishable label text: a document title or a summary.
//
// It exists because both are published into formats where a line break is
// structure, not content. A summary carrying a newline splits one entry of
// llms.txt into two, and the second half — attacker-chosen text at the start of a
// line — reads as a new list item pointing wherever it likes; a title carrying one
// stops matching the "# <title>" first line every document is required to have. A
// tab does the same to any column-aligned listing. Refusing those spellings at
// construction is what lets every consumer treat the value as a label.
//
// It is deliberately not a general string sanitiser: nothing is trimmed, folded,
// or replaced. A producer that emits an edge space or a control character has a
// bug, and quietly repairing it here would hide the bug and make two spellings of
// one label compare unequal in a manifest that must be byte-stable.
type Line struct{ s string }

// maxLineBytes bounds a label. The real corpus's longest is well under a hundred
// bytes; the cap is far above anything a legible label needs and exists so a
// hostile manifest cannot turn one summary into the bulk of a published listing.
const maxLineBytes = 512

// NewLine validates and constructs a publishable label. what names the field for
// the diagnostic, because the same rule guards several of them.
func NewLine(what, s string) (Line, error) {
	switch {
	case s == "":
		return Line{}, fmt.Errorf("docbundle: %s is empty", what)
	case len(s) > maxLineBytes:
		return Line{}, fmt.Errorf("docbundle: %s is %d bytes, over the %d-byte limit", what, len(s), maxLineBytes)
	case !utf8.ValidString(s):
		return Line{}, fmt.Errorf("docbundle: %s is not valid UTF-8", what)
	case s != strings.TrimSpace(s):
		return Line{}, fmt.Errorf("docbundle: %s %q has leading or trailing whitespace", what, s)
	}
	for i, r := range s {
		// Unicode's control category, which covers the C0 and C1 ranges: the line
		// breaks and separators that would give a label structure it must not have,
		// and the terminal escapes that would give it behaviour.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return Line{}, fmt.Errorf("docbundle: %s %q carries a control character at byte %d", what, s, i)
		}
	}
	return Line{s: s}, nil
}

// String returns the label.
func (l Line) String() string { return l.s }
