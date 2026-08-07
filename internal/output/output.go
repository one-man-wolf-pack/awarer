// Package output owns the primitives for writing command results.
//
// It enforces a single rule that the rest of awa depends on: machine-readable
// payload goes to stdout, while diagnostics, progress, and errors go to stderr.
// This keeps stdout suitable for piping even when awa is chatty.
//
// Output carries no global state; callers pass the destination streams
// explicitly, which keeps tests deterministic and parallel-safe.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

// SchemaVersion identifies the machine-output envelope this binary emits under
// --json: the generation a consumer is reading, carried as "schema_version" on
// every payload. It names the one current shape, not a history — awa ships a single
// envelope generation and no reader for any other.
const SchemaVersion = 1

// Writer routes human and machine output to the correct stream.
//
// Payload writes (Line, Linef, JSON) target stdout and are fallible: a closed
// pipe must not be lost. Rather than force a check after every line, the Writer
// retains the first stdout write error (like bufio.Writer) and exposes it via
// Err. Command handlers therefore write freely and do not thread the error
// back; cli.Run inspects Err once after successful routing and turns a failed
// payload write into a non-zero exit. JSON additionally returns a marshalling
// error directly, since that is a data-shape bug in the caller, not a write
// failure. Diagnostic and Error target stderr and are best-effort — if the
// error stream itself is broken there is nowhere left to report.
//
// The zero value is not usable; construct one with New.
type Writer struct {
	out  io.Writer
	err  io.Writer
	werr error
}

// New returns a Writer that sends payload to out and diagnostics to err.
func New(out, err io.Writer) *Writer {
	return &Writer{out: out, err: err}
}

// Line writes a human-readable payload line to stdout.
func (w *Writer) Line(s string) {
	w.write(s + "\n")
}

// Linef writes a formatted human-readable payload line to stdout.
func (w *Writer) Linef(format string, args ...any) {
	w.write(fmt.Sprintf(format+"\n", args...))
}

// Raw writes bytes to stdout verbatim — no added or trimmed newline. Use it for
// commands that must reproduce content exactly, such as "config show", where a
// rewritten trailing newline would corrupt the output. It shares the same
// sticky-error handling as Line, so failures surface centrally via Err.
func (w *Writer) Raw(p []byte) {
	if w.werr != nil {
		return
	}
	if _, err := w.out.Write(p); err != nil {
		w.werr = err
	}
}

// write sends s to stdout, retaining the first error. Once a write has failed,
// later writes are skipped: the stream is broken and the first error is the
// one worth reporting.
func (w *Writer) write(s string) {
	if w.werr != nil {
		return
	}
	if _, err := io.WriteString(w.out, s); err != nil {
		w.werr = err
	}
}

// Err reports the first error encountered while writing payload to stdout, or
// nil. cli.Run checks it after routing so a failed stdout write is not silently
// swallowed; a streaming handler may also check it to stop writing early.
func (w *Writer) Err() error {
	return w.werr
}

// Stdout and Stderr expose the underlying payload and diagnostic streams for
// commands that must stream raw bytes through them — such as "awa run" teeing a
// wrapped command's output, or replaying a cached run. Ordinary output should use
// Line/Linef/Raw/JSON (stdout) and Diagnostic/Error (stderr); these are the
// escape hatch for streaming, and they honor the streams passed to New so output
// never leaks to the process globals. Writes here are not tracked by Err; a
// streaming caller surfaces its own copy error instead.
func (w *Writer) Stdout() io.Writer { return w.out }
func (w *Writer) Stderr() io.Writer { return w.err }

// Diagnostic writes a human-readable diagnostic line to stderr.
// Use it for cache-hit notices, warnings, and progress that must not pollute
// piped stdout. Writes to stderr are best-effort.
func (w *Writer) Diagnostic(s string) {
	_, _ = fmt.Fprintln(w.err, s)
}

// Error writes a user-facing error line to stderr, prefixed with "awa: ".
// Writes to stderr are best-effort.
func (w *Writer) Error(s string) {
	_, _ = fmt.Fprintln(w.err, "awa: "+s)
}

// envelope is the stable shape of every --json response.
type envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	Data          any    `json:"data"`
}

// JSON writes a schema-versioned envelope wrapping data to stdout.
//
// It marshals into a buffer first, then writes through the same stdout path as
// Line/Linef, so a write failure is retained in the Writer and surfaced
// centrally by cli.Run — exactly like every other payload write. (Encode
// already buffers a whole document before writing, so this adds no streaming
// cost.) A marshalling failure is an invariant violation in callers (they
// control the data shape), so that — and only that — is returned here.
func (w *Writer) JSON(command string, data any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	env := envelope{
		SchemaVersion: SchemaVersion,
		Command:       command,
		Data:          data,
	}
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("encoding %s json output: %w", command, err)
	}
	w.write(buf.String())
	return nil
}

// marshalIndentNoHTML encodes v as indented JSON with HTML escaping off, matching
// JSON's envelope style. prefix is prepended to every line after the first, so the
// value can be embedded inline after a field name at that indent level. The trailing
// newline json.Encoder appends is trimmed.
func marshalIndentNoHTML(v any, prefix string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// StreamJSON writes a schema-versioned envelope whose data object is produced field by
// field by build, so a large array field can be streamed element-by-element instead of
// being materialized into a slice and a whole-document buffer. The output matches the
// indented {schema_version, command, data} envelope of JSON.
//
// Unlike JSON, StreamJSON writes to stdout as build runs, so memory stays bounded. The
// cost is the streaming contract the human changes/diff renderers already use: a
// mid-build error can leave a partial document on stdout. build must return any source
// error so the caller surfaces it as a non-zero exit — a partial JSON document plus a
// non-zero exit is the honest signal, never a clean-looking truncated document. A build
// error is returned as is; a marshalling error in a field is captured and returned; a
// stdout write failure is retained on the Writer and surfaced centrally via Err.
func (w *Writer) StreamJSON(command string, build func(*JSONObjectWriter) error) error {
	cmd, err := marshalIndentNoHTML(command, "")
	if err != nil {
		return fmt.Errorf("encoding %s json command: %w", command, err)
	}
	w.write("{\n")
	w.write("  \"schema_version\": " + strconv.Itoa(SchemaVersion) + ",\n")
	w.write("  \"command\": " + string(cmd) + ",\n")
	w.write("  \"data\": {")
	o := &JSONObjectWriter{w: w, indent: "    "}
	buildErr := build(o)
	if buildErr == nil && o.err == nil {
		if o.wrote {
			w.write("\n  }\n}\n")
		} else {
			w.write("}\n}\n")
		}
		return nil
	}
	// A mid-build error: leave the document deliberately unterminated so it cannot parse
	// as a complete JSON document. Combined with the non-zero exit the caller returns,
	// that is the honest signal — a truncated stream, never a clean-looking result. The
	// build error takes precedence over a later field-marshalling error.
	if buildErr != nil {
		return buildErr
	}
	return o.err
}

// JSONObjectWriter writes the fields of a StreamJSON data object one at a time. Field
// order is exactly the call order. It accumulates the first marshalling error; once set,
// later writes are skipped so a broken value cannot corrupt the rest of the document.
type JSONObjectWriter struct {
	w      *Writer
	indent string
	wrote  bool
	err    error
}

func (o *JSONObjectWriter) key(name string) string {
	b, err := marshalIndentNoHTML(name, "")
	if err != nil {
		// Field names are static ASCII identifiers, so this cannot fail in practice;
		// fall back to a quoted form rather than panic.
		return strconv.Quote(name)
	}
	return string(b)
}

func (o *JSONObjectWriter) sep() {
	if o.wrote {
		o.w.write(",\n")
	} else {
		o.w.write("\n")
	}
	o.wrote = true
}

// Field writes one scalar/struct/slice field, marshalled at the object's indent level.
func (o *JSONObjectWriter) Field(name string, v any) {
	if o.err != nil {
		return
	}
	b, err := marshalIndentNoHTML(v, o.indent)
	if err != nil {
		o.err = fmt.Errorf("encoding field %q: %w", name, err)
		return
	}
	o.sep()
	o.w.write(o.indent + o.key(name) + ": " + string(b))
}

// Array writes a streamed array field: it opens the array, runs build — which pushes
// elements one at a time via Add — and always closes the array before returning, so a
// large collection is never materialized. The closure form makes an unclosed array
// unrepresentable and scopes the nesting visibly: a caller cannot leak the array open
// or interleave a sibling field into it. An empty array is written compactly as [].
func (o *JSONObjectWriter) Array(name string, build func(*JSONArrayWriter)) {
	if o.err != nil {
		return
	}
	o.sep()
	o.w.write(o.indent + o.key(name) + ": [")
	a := &JSONArrayWriter{o: o, indent: o.indent + "  "}
	build(a)
	if a.wrote {
		o.w.write("\n" + o.indent + "]")
	} else {
		o.w.write("]")
	}
}

// JSONArrayWriter streams the elements of one array field. It is only valid inside the
// build closure Array passes it to.
type JSONArrayWriter struct {
	o      *JSONObjectWriter
	indent string
	wrote  bool
}

// Err reports the first condition that makes further element work pointless: a field
// marshalling error already recorded on the object, or a broken stdout sink retained on
// the Writer. A drain loop that produces elements from a size-proportional source (a large
// comparison, a per-element blob read + diff render) should check it after each Add and
// stop early — once the sink is broken, continuing only burns work that is discarded. The
// honesty contract is unaffected: StreamJSON still leaves the document unterminated and
// cli.Run still surfaces the retained write error as a non-zero exit.
func (a *JSONArrayWriter) Err() error {
	if a.o.err != nil {
		return a.o.err
	}
	return a.o.w.werr
}

// Add marshals and writes one element at the array's indent level. It is a no-op once a
// field-marshalling error has been recorded or the stdout sink has broken, so a broken
// element cannot corrupt the rest and a dead sink stops costing per-element marshalling.
func (a *JSONArrayWriter) Add(v any) {
	if a.o.err != nil || a.o.w.werr != nil {
		return
	}
	b, err := marshalIndentNoHTML(v, a.indent)
	if err != nil {
		a.o.err = fmt.Errorf("encoding array element: %w", err)
		return
	}
	if a.wrote {
		a.o.w.write(",")
	}
	a.wrote = true
	a.o.w.write("\n" + a.indent + string(b))
}
