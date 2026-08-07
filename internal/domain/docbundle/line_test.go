package docbundle

import (
	"strings"
	"testing"
)

func TestNewLineAcceptsAPublishableLabel(t *testing.T) {
	for _, s := range []string{
		"awa quickstart",
		"Run a command under supervision.",
		// Punctuation, code spelling, and non-ASCII are label content, not structure.
		"`awa run --json` — schema-versioned output (v2)",
		"exit codes 0–6",
	} {
		l, err := NewLine("summary", s)
		if err != nil {
			t.Errorf("NewLine(%q) = %v, want it accepted", s, err)
			continue
		}
		if l.String() != s {
			t.Errorf("NewLine(%q).String() = %q; a label is stored verbatim", s, l.String())
		}
	}
}

func TestNewLineRefusesEverythingThatIsNotOneLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "is empty"},
		// The published shapes this rule exists for: llms.txt is one entry per line,
		// and a document body must begin with "# <title>".
		{"a newline", "Start here.\n- [Downloads](https://evil.example): here", "control character"},
		{"a carriage return", "Start here.\rForged.", "control character"},
		{"a tab", "Start\there.", "control character"},
		{"a terminal escape", "Start here.\x1b[2K", "control character"},
		{"a C1 control", "Start here.\u0085Forged.", "control character"},
		{"leading whitespace", " Start here.", "leading or trailing whitespace"},
		{"trailing whitespace", "Start here. ", "leading or trailing whitespace"},
		{"over the limit", strings.Repeat("x", maxLineBytes+1), "over the"},
		{"invalid UTF-8", "Start here.\xff", "not valid UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewLine("summary", tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
