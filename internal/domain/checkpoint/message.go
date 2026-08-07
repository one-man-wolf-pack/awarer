package checkpoint

import (
	"fmt"
	"strings"
)

// CheckpointMessage is an optional free-text note attached to a checkpoint. Unlike a
// name it is not a reference and is not constrained for the filesystem, so it may
// contain spaces and newlines. The zero value means "no message".
type CheckpointMessage struct {
	s string
}

// ParseCheckpointMessage validates an optional message. An empty string yields the
// zero (absent) message. A NUL byte is rejected because it cannot round-trip
// through text storage and almost always signals a mistake; other control
// characters are allowed so multi-line messages work.
func ParseCheckpointMessage(raw string) (CheckpointMessage, error) {
	if raw == "" {
		return CheckpointMessage{}, nil
	}
	if strings.ContainsRune(raw, 0) {
		return CheckpointMessage{}, fmt.Errorf("invalid checkpoint message: contains NUL")
	}
	return CheckpointMessage{s: raw}, nil
}

// String returns the message text, empty when absent.
func (m CheckpointMessage) String() string { return m.s }

// IsZero reports whether no message is set.
func (m CheckpointMessage) IsZero() bool { return m.s == "" }

// FirstLine returns the message's first line, for compact one-line log output.
func (m CheckpointMessage) FirstLine() string {
	if i := strings.IndexByte(m.s, '\n'); i >= 0 {
		return m.s[:i]
	}
	return m.s
}
