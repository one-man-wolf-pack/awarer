package inspect

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// defaultRingBytes bounds the memory a LineRing retains while streaming, so a
// pathological single very long line cannot grow without limit. It is generous
// relative to a terminal-sized tail.
const defaultRingBytes = 1 << 20 // 1 MiB

// maxLineBytes caps how many bytes of a single line the read scanner retains; the
// rest of an over-long (or newline-free) line is consumed and discarded, so one
// huge line can never exhaust memory or blow a JSON size budget. A line clipped
// here is reported as truncated. It mirrors LineRing's per-line bound on the live
// path.
const maxLineBytes = 1 << 20 // 1 MiB

// noByteCap disables a window's byte cap, leaving only its line cap in force. It is
// the largest int.
const noByteCap = int(^uint(0) >> 1)

// lineWindow retains the most recent lines pushed to it, bounded by both a line
// count and a total byte size; older lines are evicted as either cap is reached, so
// memory stays bounded regardless of how much is pushed. It is the shared core of
// LineRing (byte-fed) and the line-fed collectors below.
type lineWindow struct {
	maxLines int
	maxBytes int
	lines    []string
	bytes    int
	dropped  bool
}

func newLineWindow(maxLines, maxBytes int) *lineWindow {
	return &lineWindow{maxLines: maxLines, maxBytes: maxBytes}
}

// push appends a line and evicts the oldest lines until both caps hold. maxLines is
// always positive, so the line cap never empties the window; the byte cap keeps at
// least the most recent line.
func (w *lineWindow) push(line string) {
	w.lines = append(w.lines, line)
	w.bytes += len(line)
	for len(w.lines) > w.maxLines || (w.bytes > w.maxBytes && len(w.lines) > 1) {
		w.bytes -= len(w.lines[0])
		w.lines = w.lines[1:]
		w.dropped = true
	}
}

func (w *lineWindow) checkpoint() []string {
	out := make([]string, len(w.lines))
	copy(out, w.lines)
	return out
}

// LineRing is an io.Writer that retains only the last lines (bounded by line count
// and bytes) of everything written through it. It backs the live tail of a cache
// miss: capture tees full output to the store while this ring keeps just the window
// the terminal will show, so display stays bounded regardless of how much the
// command prints.
//
// Lines are split on '\n'; a trailing '\r' is dropped so CRLF output renders
// cleanly. A trailing chunk without a newline is retained as the final line.
type LineRing struct {
	win      *lineWindow
	maxBytes int // cap on a single in-progress (newline-free) line
	partial  []byte
	overflow bool
}

// NewLineRing returns a ring retaining the last maxLines lines. maxLines must be
// positive.
func NewLineRing(maxLines int) *LineRing {
	return newLineRing(maxLines, defaultRingBytes)
}

func newLineRing(maxLines, maxBytes int) *LineRing {
	return &LineRing{win: newLineWindow(maxLines, maxBytes), maxBytes: maxBytes}
}

// Write consumes p, splitting it into lines and retaining only the most recent
// ones. It always reports the full length as written (it never blocks capture) and
// never returns an error.
func (r *LineRing) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			r.appendPartial(p)
			break
		}
		r.appendPartial(p[:i])
		r.finishLine()
		p = p[i+1:]
	}
	return n, nil
}

// appendPartial extends the in-progress line with b, retaining only the last
// maxBytes bytes of the result. It never materializes more than maxBytes, so even a
// single huge newline-free Write stays bounded: when b alone overflows the cap only
// its tail is copied; otherwise just enough of the existing partial is dropped from
// the front to make room.
func (r *LineRing) appendPartial(b []byte) {
	if len(b) >= r.maxBytes {
		if len(r.partial) > 0 || len(b) > r.maxBytes {
			r.overflow = true
		}
		r.partial = append(r.partial[:0], b[len(b)-r.maxBytes:]...)
		return
	}
	if excess := len(r.partial) + len(b) - r.maxBytes; excess > 0 {
		r.partial = append(r.partial[:0], r.partial[excess:]...) // left-shift, drop the front
		r.overflow = true
	}
	r.partial = append(r.partial, b...)
}

// finishLine commits the in-progress bytes as a completed line.
func (r *LineRing) finishLine() {
	line := strings.TrimSuffix(string(r.partial), "\r")
	r.partial = r.partial[:0]
	r.win.push(line)
}

// Lines returns the retained lines, including any trailing newline-free remainder
// as the final line.
func (r *LineRing) Lines() []string {
	out := r.win.checkpoint()
	if len(r.partial) > 0 {
		out = append(out, strings.TrimSuffix(string(r.partial), "\r"))
	}
	return out
}

// Overflowed reports whether any earlier output was dropped — i.e. the retained
// lines are a tail, not the complete log.
func (r *LineRing) Overflowed() bool { return r.win.dropped || r.overflow }

// scanLines reads r line by line, calling fn with each line (stripped of its
// trailing newline and any '\r') and whether that line was clipped at lineCap
// bytes. It reads in bounded chunks and retains at most lineCap bytes per line —
// discarding the rest of an over-long line up to the next newline — so neither a
// single huge line nor a newline-free stream can exhaust memory.
func scanLines(r io.Reader, lineCap int, fn func(line string, clipped bool) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var line []byte
	clipped := false
	pending := false // bytes accumulated for a line not yet emitted
	emit := func() error {
		err := fn(strings.TrimSuffix(string(line), "\r"), clipped)
		line, clipped, pending = line[:0], false, false
		return err
	}
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			pending = true
			data := chunk
			complete := err == nil // ReadSlice returns nil only when '\n' was found
			if complete {
				data = data[:len(data)-1] // drop the '\n'
			}
			if room := lineCap - len(line); room > 0 {
				if len(data) > room {
					line = append(line, data[:room]...)
					clipped = true
				} else {
					line = append(line, data...)
				}
			} else if len(data) > 0 {
				clipped = true // line already at the cap; discard the overflow
			}
			if complete {
				if e := emit(); e != nil {
					return e
				}
			}
		}
		if err != nil {
			if err == bufio.ErrBufferFull {
				continue // more of this line remains; keep reading it in chunks
			}
			if err == io.EOF {
				if pending {
					return emit()
				}
				return nil
			}
			return err
		}
	}
}

// lineCapFor picks the per-line retention cap: the smaller of the caller's total
// byte budget and the default per-line bound, so a single line can never exceed the
// budget yet an unbounded (noByteCap) caller still clips pathological lines.
func lineCapFor(maxBytes int) int {
	if maxBytes < maxLineBytes {
		return maxBytes
	}
	return maxLineBytes
}

// CollectSample scans r and returns the most recent lines matching keep (every line
// when keep is nil), bounded to maxLines and maxBytes with the oldest evicted first.
// truncated reports whether earlier lines were dropped. Memory stays bounded to the
// caps during the scan, so a large log never materializes in full.
func CollectSample(r io.Reader, keep func(string) bool, maxLines, maxBytes int) ([]string, bool, error) {
	win := newLineWindow(maxLines, maxBytes)
	lineClipped := false
	if err := scanLines(r, lineCapFor(maxBytes), func(line string, clipped bool) error {
		if keep != nil && !keep(line) {
			return nil
		}
		if clipped {
			lineClipped = true
		}
		win.push(line)
		return nil
	}); err != nil {
		return nil, false, err
	}
	return win.checkpoint(), win.dropped || lineClipped, nil
}

// TailLines reads the last limit.Lines() lines of r, bounded by line count.
func TailLines(r io.Reader, limit TailLimit) ([]string, bool, error) {
	return CollectSample(r, nil, limit.Lines(), noByteCap)
}

// GrepStream writes each line of r matching p to w (newline-terminated) and returns
// the number of matches. It streams, holding at most one line in memory.
func GrepStream(r io.Reader, p GrepPattern, w io.Writer) (int, error) {
	count := 0
	err := scanLines(r, maxLineBytes, func(line string, _ bool) error {
		if !p.Match(line) {
			return nil
		}
		count++
		if _, e := io.WriteString(w, line+"\n"); e != nil {
			return e
		}
		return nil
	})
	return count, err
}

// GrepTail returns the last limit.Lines() lines of r that match p, and whether
// earlier matches were dropped. It is memory-bounded to the tail window. Callers
// needing every match (not just the last n) should stream via GrepStream.
func GrepTail(r io.Reader, p GrepPattern, limit TailLimit) ([]string, bool, error) {
	return CollectSample(r, p.Match, limit.Lines(), noByteCap)
}
