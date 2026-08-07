package output

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestJSON_Envelope(t *testing.T) {
	// Arrange
	var out, errBuf bytes.Buffer
	w := New(&out, &errBuf)

	// Act
	if err := w.JSON("status", map[string]any{"clean": true}); err != nil {
		t.Fatalf("JSON returned error: %v", err)
	}

	// Assert: stdout holds a valid, schema-versioned envelope; stderr is clean.
	if errBuf.Len() != 0 {
		t.Errorf("stderr = %q, want empty", errBuf.String())
	}
	var env struct {
		SchemaVersion int            `json:"schema_version"`
		Command       string         `json:"command"`
		Data          map[string]any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not valid json: %v\n%s", err, out.String())
	}
	if env.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, SchemaVersion)
	}
	if env.Command != "status" {
		t.Errorf("command = %q, want %q", env.Command, "status")
	}
	if env.Data["clean"] != true {
		t.Errorf("data.clean = %v, want true", env.Data["clean"])
	}
}

// failWriter fails every write, simulating a closed stdout pipe.
type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

// failAfterWriter succeeds for the first ok writes, then fails every write with a closed
// pipe. It models a pipe that breaks partway through a stream (e.g. the reader ran head).
type failAfterWriter struct {
	ok int
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.ok > 0 {
		f.ok--
		return len(p), nil
	}
	return 0, io.ErrClosedPipe
}

// TestStreamJSON_ArrayErrStopsDrainOnBrokenSink proves the early-stop contract: once
// stdout breaks mid-array, a drain loop that consults JSONArrayWriter.Err stops pulling
// its size-proportional source instead of draining it to the end, and the document is left
// unterminated so it can never parse as a complete JSON result.
func TestStreamJSON_ArrayErrStopsDrainOnBrokenSink(t *testing.T) {
	// Arrange: the sink accepts the envelope prefix and a couple of elements, then breaks.
	sink := &failAfterWriter{ok: 6}
	var errBuf bytes.Buffer
	w := New(sink, &errBuf)

	const available = 1000
	pulled := 0

	// Act: the loop mirrors the CLI streamers — Add an element, then break on Err.
	err := w.StreamJSON("changes", func(o *JSONObjectWriter) error {
		o.Array("changes", func(arr *JSONArrayWriter) {
			for pulled < available {
				pulled++
				arr.Add(map[string]int{"n": pulled})
				if arr.Err() != nil {
					return
				}
			}
		})
		return nil
	})

	// Assert: StreamJSON reports no build/marshal error (the failure is a sink error), the
	// drain stopped far short of the whole source, and the retained write error surfaces.
	if err != nil {
		t.Fatalf("StreamJSON returned %v, want nil (sink error is retained on Err, not returned)", err)
	}
	if pulled >= available {
		t.Fatalf("drained %d of %d elements; want an early stop once the sink broke", pulled, available)
	}
	if w.Err() == nil {
		t.Fatal("w.Err() = nil, want the retained stdout write error")
	}
}

func TestErr_SurfacesStdoutWriteFailure(t *testing.T) {
	// Arrange: stdout is broken, stderr is fine.
	var errBuf bytes.Buffer
	w := New(failWriter{}, &errBuf)

	// Act
	w.Line("payload")

	// Assert: the write error is retained, not swallowed.
	if w.Err() == nil {
		t.Fatal("Err() = nil, want the stdout write error")
	}
}

func TestRaw_ByteForByte(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no trailing newline", "a = 1"},
		{"single trailing newline", "a = 1\n"},
		{"multiple trailing newlines", "a = 1\n\n\n"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errBuf bytes.Buffer
			w := New(&out, &errBuf)

			w.Raw([]byte(tt.in))

			if out.String() != tt.in {
				t.Errorf("Raw wrote %q, want %q (byte for byte)", out.String(), tt.in)
			}
		})
	}
}

func TestRaw_SurfacesWriteFailure(t *testing.T) {
	var errBuf bytes.Buffer
	w := New(failWriter{}, &errBuf)

	w.Raw([]byte("payload"))

	if w.Err() == nil {
		t.Fatal("Err() = nil, want the stdout write error")
	}
}

func TestErr_NilOnSuccess(t *testing.T) {
	var out, errBuf bytes.Buffer
	w := New(&out, &errBuf)

	w.Line("payload")

	if err := w.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

func TestStreamSeparation(t *testing.T) {
	// Arrange
	var out, errBuf bytes.Buffer
	w := New(&out, &errBuf)

	// Act
	w.Line("payload")
	w.Diagnostic("note")
	w.Error("boom")

	// Assert: payload only on stdout; diagnostics and errors only on stderr.
	if got := out.String(); got != "payload\n" {
		t.Errorf("stdout = %q, want %q", got, "payload\n")
	}
	if got := errBuf.String(); got != "note\nawa: boom\n" {
		t.Errorf("stderr = %q, want %q", got, "note\nawa: boom\n")
	}
}
