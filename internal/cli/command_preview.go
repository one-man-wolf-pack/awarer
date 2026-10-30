package cli

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// commandPreviewLimit bounds a human command preview, counted in runes of the rendered
// text, escapes and the elision marker included.
const commandPreviewLimit = 160

// commandPreviewElision marks a preview shortened for display. It is not ASCII "..."
// (which real argv such as "go test ./..." contains) and not the "[truncated]" marker
// of a stored output stream.
const commandPreviewElision = "…"

// commandPreview renders argv as a single bounded line for human scanning rows. Arguments
// are joined by single spaces like strings.Join, so an ordinary short command reads
// unchanged. Line breaks, other control characters, Unicode line separators, explicit
// bidirectional controls and invalid UTF-8 are written as visible escapes, so the preview
// never starts another report row, reaches the terminal as a control sequence, or carries
// a direction override or isolate into the rest of the row. Other format characters,
// such as a zero-width joiner, are printed unchanged. A command over the limit keeps a
// prefix of whole escapes and runes followed by commandPreviewElision.
//
// The preview is presentation only: it is not executable, not reversible, and never an
// input to storage, keys, filters or removal. Full argv stays in run show and JSON.
func commandPreview(argv []string) string {
	var b strings.Builder
	count := 0 // runes written to b
	cut := 0   // byte length of the longest prefix that leaves room for the marker
	add := func(tok string, n int) bool {
		if count+n > commandPreviewLimit {
			return false
		}
		b.WriteString(tok)
		count += n
		if count <= commandPreviewLimit-1 {
			cut = b.Len()
		}
		return true
	}
	for i, arg := range argv {
		if i > 0 && !add(" ", 1) {
			return b.String()[:cut] + commandPreviewElision
		}
		for j := 0; j < len(arg); {
			r, size := utf8.DecodeRuneInString(arg[j:])
			tok := arg[j : j+size]
			n := 1
			if esc := previewEscape(r, size, arg[j]); esc != "" {
				tok, n = esc, len(esc) // escapes are ASCII
			}
			if !add(tok, n) {
				return b.String()[:cut] + commandPreviewElision
			}
			j += size
		}
	}
	return b.String()
}

// previewEscape returns the visible escape for a rune that must not reach the terminal
// as is, or "" when the rune is printed unchanged. first is the rune's first byte, used
// to name an invalid UTF-8 byte.
func previewEscape(r rune, size int, first byte) string {
	switch {
	case r == utf8.RuneError && size == 1:
		return fmt.Sprintf(`\x%02x`, first)
	case r == '\n':
		return `\n`
	case r == '\r':
		return `\r`
	case r == '\t':
		return `\t`
	case r < utf8.RuneSelf && unicode.IsControl(r):
		return fmt.Sprintf(`\x%02x`, r)
	case unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || unicode.Is(unicode.Bidi_Control, r):
		return fmt.Sprintf(`\u%04x`, r)
	}
	return ""
}
