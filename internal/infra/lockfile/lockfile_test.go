package lockfile

import (
	"errors"
	"testing"
	"time"
)

func TestParseRoundTrip(t *testing.T) {
	created := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	data, err := Encode(Record{Operation: "run", PID: 12345, Hostname: "devbox", CreatedAt: created})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Operation != "run" || got.PID != 12345 || got.Hostname != "devbox" || !got.CreatedAt.Equal(created) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"unknown field", `{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z","extra":true}`},
		{"trailing object", `{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}{}`},
		// A valid record followed by non-JSON garbage: the second decode fails (not
		// EOF), which the old err==nil check let slip through. It must be rejected so a
		// hand-appended or truncated lock is unknown, not trusted.
		{"trailing garbage", `{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"} then junk`},
		{"trailing token", `{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}]`},
		{"bad timestamp", `{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"not-a-time"}`},
		{"missing operation", `{"schema_version":1,"operation":"","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}`},
		{"non-positive pid", `{"schema_version":1,"operation":"run","pid":0,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}`},
		{"garbage", `not json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.data)); err == nil {
				t.Fatalf("expected parse error")
			}
		})
	}
}

func TestParseAcceptsTrailingWhitespace(t *testing.T) {
	// Trailing whitespace after the object is not trailing data: the second decode
	// skips it and reports EOF. This keeps Encode's newline-terminated output (and any
	// editor that adds a final newline) parseable.
	data := `{"schema_version":1,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}` + "\n\t \n"
	if _, err := Parse([]byte(data)); err != nil {
		t.Fatalf("trailing whitespace must parse: %v", err)
	}
}

func TestParseRejectsUnknownOperation(t *testing.T) {
	// A file using awa's schema but an operation this build does not recognize must
	// be rejected, so a third-party (or newer-awa) lock is classified unknown rather
	// than trusted and auto-removed.
	_, err := Parse([]byte(`{"schema_version":1,"operation":"third-party","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}`))
	if err == nil {
		t.Fatalf("expected parse error for unrecognized operation")
	}
}

func TestParseUnsupportedSchema(t *testing.T) {
	_, err := Parse([]byte(`{"schema_version":2,"operation":"run","pid":1,"hostname":"h","created_at":"2026-06-23T10:00:00Z"}`))
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("expected ErrUnsupportedSchema, got %v", err)
	}
}

func TestClassify(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute)
	old := now.Add(-48 * time.Hour)

	dead := func(int) bool { return false }
	live := func(int) bool { return true }

	t.Run("same host dead owner is stale", func(t *testing.T) {
		r := Record{Hostname: "h", PID: 5, CreatedAt: recent}
		if got := r.Classify(now, DefaultStaleAfter, "h", dead); got != StatusStale {
			t.Fatalf("got %v, want stale", got)
		}
	})
	t.Run("same host live owner is active even if old", func(t *testing.T) {
		r := Record{Hostname: "h", PID: 5, CreatedAt: old}
		if got := r.Classify(now, DefaultStaleAfter, "h", live); got != StatusActive {
			t.Fatalf("got %v, want active", got)
		}
	})
	t.Run("other host recent is active", func(t *testing.T) {
		r := Record{Hostname: "other", PID: 5, CreatedAt: recent}
		if got := r.Classify(now, DefaultStaleAfter, "h", dead); got != StatusActive {
			t.Fatalf("got %v, want active", got)
		}
	})
	t.Run("other host old is stale", func(t *testing.T) {
		r := Record{Hostname: "other", PID: 5, CreatedAt: old}
		if got := r.Classify(now, DefaultStaleAfter, "h", live); got != StatusStale {
			t.Fatalf("got %v, want stale", got)
		}
	})
}
