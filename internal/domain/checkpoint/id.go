// Package checkpoint is the pure domain model for worktree checkpoints.
//
// It owns the value objects a checkpoint produces — checkpoint id, message,
// manifest and skipped entries, git metadata, stats, and the Checkpoint aggregate —
// and the repository ports the application uses to persist and look them up. It
// performs no I/O and holds no JSON or filesystem details: those live in the
// checkpointjson infrastructure adapter behind the ports declared here.
package checkpoint

import (
	"encoding/base32"
	"fmt"
	"io"
)

// idAlphabet is Crockford base32, lowercased: digits plus the lowercase letters
// with the ambiguous i, l, o, and u removed. It is filename-safe and avoids
// glyphs a human would misread when retyping a short id.
const idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// idBytes is the number of random bytes in a checkpoint id. 20 bytes is 160 bits —
// far beyond any realistic collision risk — and encodes to exactly 32 base32
// characters with no padding.
const idBytes = 20

// idLen is the rendered length of a checkpoint id.
const idLen = 32

// shortLen is the number of leading characters shown in human output. The
// repository, which knows every id, is responsible for confirming a short form
// is unambiguous before relying on it; Short itself is display only.
const shortLen = 12

// idEncoding encodes id bytes into the Crockford-lowercase alphabet without
// padding, so an id is a fixed-length token with no "=" to escape in a filename.
var idEncoding = base32.NewEncoding(idAlphabet).WithPadding(base32.NoPadding)

// CheckpointID uniquely identifies a checkpoint. Unlike a scan id it carries no
// timestamp: checkpoint ordering comes from the recorded creation time, and the id
// exists only to name an immutable record collision-free. It is generated from
// cryptographic randomness and rejects any malformed persisted value.
type CheckpointID struct {
	s string
}

// NewCheckpointID draws idBytes of randomness from r and encodes them. r is
// injected so tests can supply a deterministic source; production passes
// crypto/rand.Reader.
func NewCheckpointID(r io.Reader) (CheckpointID, error) {
	var buf [idBytes]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return CheckpointID{}, fmt.Errorf("generating checkpoint id: %w", err)
	}
	return CheckpointID{s: idEncoding.EncodeToString(buf[:])}, nil
}

// ParseCheckpointID validates a persisted checkpoint id: exactly idLen characters,
// all drawn from the id alphabet. It rejects anything else so a stray filename in
// the checkpoints directory cannot masquerade as an id.
func ParseCheckpointID(s string) (CheckpointID, error) {
	if len(s) != idLen {
		return CheckpointID{}, fmt.Errorf("invalid checkpoint id %q: want %d characters", s, idLen)
	}
	for i := 0; i < len(s); i++ {
		if !inAlphabet(s[i]) {
			return CheckpointID{}, fmt.Errorf("invalid checkpoint id %q: bad character %q", s, string(s[i]))
		}
	}
	return CheckpointID{s: s}, nil
}

func inAlphabet(c byte) bool {
	for i := 0; i < len(idAlphabet); i++ {
		if idAlphabet[i] == c {
			return true
		}
	}
	return false
}

// ValidateIDPrefix checks that s is a syntactically valid short id prefix: a
// non-empty run of id-alphabet characters no longer than a full id. It exists so
// a repository lookup can tell malformed input (empty, too long, bad character)
// from a well-formed prefix that simply matches nothing — the two are different
// outcomes, and an empty or garbage prefix must never be treated as "match all".
func ValidateIDPrefix(s string) error {
	if s == "" {
		return fmt.Errorf("checkpoint id prefix is empty")
	}
	if len(s) > idLen {
		return fmt.Errorf("checkpoint id prefix %q is longer than a full id (%d)", s, idLen)
	}
	for i := 0; i < len(s); i++ {
		if !inAlphabet(s[i]) {
			return fmt.Errorf("checkpoint id prefix %q has invalid character %q", s, string(s[i]))
		}
	}
	return nil
}

// String returns the full id.
func (id CheckpointID) String() string { return id.s }

// Short returns the leading shortLen characters for human display. It is never
// used as a lookup key on its own — prefix resolution belongs to the repository,
// which alone can prove a prefix unambiguous.
func (id CheckpointID) Short() string {
	if len(id.s) <= shortLen {
		return id.s
	}
	return id.s[:shortLen]
}

// IsZero reports whether the id is unset.
func (id CheckpointID) IsZero() bool { return id.s == "" }
