// Package difftext detects whether content is text and renders unified diffs.
//
// It is infrastructure: a service over byte content, with no knowledge of
// checkpoints, blobs, or the comparison model. Text detection is conservative (a
// NUL byte or a large fraction of invalid UTF-8 marks content binary). The
// unified diff defaults to a bounded Histogram engine (see histogram.go), which
// falls back to Myers' O(ND) shortest-edit-script algorithm for regions without
// usable anchors and past its recursion bound; Myers is also selectable directly.
// Both are bounded so a representative text file does not degrade to quadratic
// behavior. Lines are split on "\n" only, so a CR is kept in the line content
// and a CRLF↔LF change shows up honestly as a modified line rather than being
// normalized away.
package difftext

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrTooLarge reports that an input exceeds the budget difftext will diff. The
// caller reports the change without a content diff rather than risking unbounded
// memory or time in the O(ND) algorithm.
var ErrTooLarge = errors.New("input too large to diff")

const (
	// binaryScanLimit bounds how many bytes IsBinary inspects; a NUL or enough
	// invalid UTF-8 within this window is sufficient to classify content.
	binaryScanLimit = 8192
	// maxDiffBytes caps the per-side input Unified will diff. Beyond it the line
	// count (and so the algorithm's working set) is unbounded, so the diff is
	// refused. It comfortably covers hand-written source files.
	maxDiffBytes = 4 << 20
	// maxEditDistance caps the edit-script length the search will compute. The
	// trace is stored sparsely at O(D²); the cap keeps a pathological all-different
	// pair from exhausting memory, at which point the diff is refused.
	maxEditDistance = 4000
)

// IsBinary reports whether data should be treated as binary rather than text. A
// NUL byte is a definitive signal; otherwise content with more than ~30% invalid
// UTF-8 in the scanned prefix is treated as binary.
func IsBinary(data []byte) bool {
	scan := data
	if len(scan) > binaryScanLimit {
		scan = scan[:binaryScanLimit]
	}
	if len(scan) == 0 {
		return false
	}
	invalid := 0
	for i := 0; i < len(scan); {
		if scan[i] == 0x00 {
			return true
		}
		r, size := utf8.DecodeRune(scan[i:])
		if r == utf8.RuneError && size == 1 {
			invalid++
		}
		i += size
	}
	return invalid*100/len(scan) > 30
}

// op is one operation in an edit script.
type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

// line is one line of input: its text (without the trailing "\n", but keeping a
// CR so CRLF differs from LF) and whether it was terminated by a newline. The
// hasNewline flag makes "a" and "a\n" distinct lines, so a change in the final
// newline is a real difference rather than vanishing into equal text.
type line struct {
	text       string
	hasNewline bool
}

type op struct {
	kind opKind
	line line
}

// diffFunc computes an ordered edit script between two line slices. Both engines
// (myers, histogram) satisfy it, so Unified and UnifiedHistogram share the same
// size guard and rendering and differ only in how the ops are produced.
type diffFunc func(a, b []line) ([]op, error)

// Unified renders a unified diff of old vs new using Myers' algorithm, the
// explicit Myers engine. oldName and newName label the "---"/"+++" headers.
// context is the number of unchanged lines kept around each change (clamped to
// >= 0). It returns the empty string when the inputs are identical — which it
// answers cheaply, before any size check — and ErrTooLarge when a differing
// input exceeds the diff budget.
func Unified(oldName, newName string, old, new []byte, context int) (string, error) {
	return unifiedWith(myers, oldName, newName, old, new, context)
}

// UnifiedHistogram renders the same unified diff as Unified but uses the hybrid
// Histogram engine, which anchors on rare lines for cleaner code-oriented hunks
// and falls back to Myers locally for regions it cannot safely anchor. Its
// contract — identical-is-empty, the size budget, and ErrTooLarge — matches
// Unified exactly, so the two are interchangeable from the caller's view.
func UnifiedHistogram(oldName, newName string, old, new []byte, context int) (string, error) {
	return unifiedWith(histogram, oldName, newName, old, new, context)
}

// unifiedWith is the engine-agnostic body: it applies the identical-is-empty
// fast path and the per-side size budget before any engine work, runs the
// chosen diff engine, and renders the ops as a unified diff.
func unifiedWith(diff diffFunc, oldName, newName string, old, new []byte, context int) (string, error) {
	if context < 0 {
		context = 0
	}
	// Identical content needs no diffing, so this wins over the size budget: equal
	// inputs are reported as "no diff" no matter how large they are.
	if bytes.Equal(old, new) {
		return "", nil
	}
	if len(old) > maxDiffBytes || len(new) > maxDiffBytes {
		return "", ErrTooLarge
	}
	a := splitLines(old)
	b := splitLines(new)
	ops, err := diff(a, b)
	if err != nil {
		return "", err
	}
	return render(oldName, newName, ops, context), nil
}

// render groups the flat op list into hunks and writes the unified-diff text,
// returning the empty string when there is no change to show.
func render(oldName, newName string, ops []op, context int) string {
	hunks := group(ops, context)
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n", oldName)
	fmt.Fprintf(&sb, "+++ %s\n", newName)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%s +%s @@\n", rangeSpec(h.oldStart, h.oldCount), rangeSpec(h.newStart, h.newCount))
		for _, o := range h.ops {
			switch o.kind {
			case opEqual:
				writeDiffLine(&sb, ' ', o.line)
			case opDelete:
				writeDiffLine(&sb, '-', o.line)
			case opInsert:
				writeDiffLine(&sb, '+', o.line)
			}
		}
	}
	return sb.String()
}

// writeDiffLine writes one unified-diff line with its prefix, following it with
// the conventional "\ No newline at end of file" marker when the line had no
// trailing newline.
func writeDiffLine(sb *strings.Builder, prefix byte, l line) {
	sb.WriteByte(prefix)
	sb.WriteString(l.text)
	sb.WriteByte('\n')
	if !l.hasNewline {
		sb.WriteString("\\ No newline at end of file\n")
	}
}

// splitLines splits data into lines on "\n". A trailing "\n" does not produce an
// empty final line; instead the final line records hasNewline=false when the
// input does not end with a newline, so "a" and "a\n" differ. A CR before the
// newline stays in the line text, so CRLF differs from LF.
func splitLines(data []byte) []line {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\n")
	// A trailing "\n" yields a final empty part: drop it and mark the preceding
	// lines as newline-terminated. Otherwise the last part is an unterminated line.
	trailingNewline := parts[len(parts)-1] == ""
	if trailingNewline {
		parts = parts[:len(parts)-1]
	}
	lines := make([]line, len(parts))
	for i, p := range parts {
		lines[i] = line{text: p, hasNewline: true}
	}
	if !trailingNewline && len(lines) > 0 {
		lines[len(lines)-1].hasNewline = false
	}
	return lines
}

// myers computes the shortest edit script between a and b using the greedy
// O(ND) algorithm, then backtracks to an ordered op list. The per-depth trace is
// stored sparsely (only the reachable diagonal window [-d, d]), so the trace
// costs O(D²) rather than O(D·(N+M)); the search is capped at maxEditDistance and
// returns ErrTooLarge beyond it.
func myers(a, b []line) ([]op, error) {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, nil
	}
	max := n + m
	offset := max
	v := make([]int, 2*max+1)
	limit := max
	if limit > maxEditDistance {
		limit = maxEditDistance
	}
	trace := make([][]int, 0, limit+1)

	for d := 0; d <= limit; d++ {
		// Checkpoint only the reachable window: at depth d, prior values live on
		// diagonals k in [-d, d]. Index a diagonal k as row[k+d].
		row := make([]int, 2*d+1)
		for k := -d; k <= d; k++ {
			row[k+d] = v[offset+k]
		}
		trace = append(trace, row)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1] // move down: an insertion from b
			} else {
				x = v[offset+k-1] + 1 // move right: a deletion from a
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace, d), nil
			}
		}
	}
	// The algorithm always completes by d == max, so reaching here means the cap
	// (limit < max) stopped it: the inputs differ too much to diff.
	return nil, ErrTooLarge
}

// backtrack walks the sparse trace from depth dEnd to the origin, emitting the
// edit operations in forward order. At depth d a diagonal k is read as
// trace[d][k+d].
func backtrack(a, b []line, trace [][]int, dEnd int) []op {
	var ops []op
	x, y := len(a), len(b)
	for d := dEnd; d > 0; d-- {
		row := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && row[k-1+d] < row[k+1+d]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := row[prevK+d]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			ops = append(ops, op{kind: opEqual, line: a[x-1]})
			x--
			y--
		}
		if x == prevX {
			ops = append(ops, op{kind: opInsert, line: b[prevY]})
		} else {
			ops = append(ops, op{kind: opDelete, line: a[prevX]})
		}
		x, y = prevX, prevY
	}
	for x > 0 && y > 0 {
		ops = append(ops, op{kind: opEqual, line: a[x-1]})
		x--
		y--
	}
	// reverse into forward order
	for i, j := 0, len(ops)-1; i < j; i, j = i+1, j-1 {
		ops[i], ops[j] = ops[j], ops[i]
	}
	return ops
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	ops                []op
}

// group splits the flat op list into hunks, keeping context unchanged lines
// around each run of changes and merging runs that are within 2*context of each
// other.
func group(ops []op, context int) []hunk {
	hasChange := false
	for _, o := range ops {
		if o.kind != opEqual {
			hasChange = true
			break
		}
	}
	if !hasChange {
		return nil
	}

	// Mark which ops are within context of a change.
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.kind != opEqual {
			lo := i - context
			if lo < 0 {
				lo = 0
			}
			hi := i + context
			if hi >= len(ops) {
				hi = len(ops) - 1
			}
			for j := lo; j <= hi; j++ {
				keep[j] = true
			}
		}
	}

	var hunks []hunk
	oldLine, newLine := 1, 1
	i := 0
	// Track line numbers as we walk every op, opening a hunk on the first kept op.
	lineOld, lineNew := make([]int, len(ops)), make([]int, len(ops))
	for idx, o := range ops {
		lineOld[idx] = oldLine
		lineNew[idx] = newLine
		switch o.kind {
		case opEqual:
			oldLine++
			newLine++
		case opDelete:
			oldLine++
		case opInsert:
			newLine++
		}
	}

	for i < len(ops) {
		if !keep[i] {
			i++
			continue
		}
		start := i
		for i < len(ops) && keep[i] {
			i++
		}
		seg := ops[start:i]
		h := hunk{oldStart: lineOld[start], newStart: lineNew[start], ops: seg}
		for _, o := range seg {
			switch o.kind {
			case opEqual:
				h.oldCount++
				h.newCount++
			case opDelete:
				h.oldCount++
			case opInsert:
				h.newCount++
			}
		}
		hunks = append(hunks, h)
	}
	return hunks
}

// rangeSpec renders a unified-diff range. A zero-count range is reported at
// start-1 with count 0, matching the conventional empty-side notation.
func rangeSpec(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}
