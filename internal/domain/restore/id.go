package restore

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

// operationIDHexLen is the rendered length of an OperationID: 8 bytes of
// big-endian timestamp plus 8 random bytes, hex-encoded. Time-prefixing makes ids
// sort in operation order, so a timeline view and a newest-first listing fall out
// of the id alone, while the random suffix keeps concurrent operations on
// different hosts from colliding. It matches the run id shape deliberately: both
// are system-owned records, and one id grammar is easier to reason about than two.
const operationIDHexLen = 32

// operationIDShortLen is the number of leading characters shown in human output.
// The record store, which knows every id, confirms a short form is unambiguous
// before acting on it; Short itself is display only.
const operationIDShortLen = 12

// OperationID uniquely identifies one applied restore and the pre-restore
// recovery observation it published. It is what `restore:<id>:before` names.
type OperationID struct {
	hex string
}

// NewOperationID builds an OperationID from a unix-nanosecond timestamp and a
// source of randomness. The timestamp leads so lexical order matches
// chronological order. rand is injected so tests can supply a deterministic
// source.
func NewOperationID(unixNano int64, rand io.Reader) (OperationID, error) {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(unixNano))
	if _, err := io.ReadFull(rand, buf[8:]); err != nil {
		return OperationID{}, fmt.Errorf("generating restore operation id: %w", err)
	}
	return OperationID{hex: hex.EncodeToString(buf[:])}, nil
}

// ParseOperationID validates a persisted id: exactly operationIDHexLen lowercase
// hex characters, so a stray directory name under the restore store cannot pass
// as an id.
func ParseOperationID(s string) (OperationID, error) {
	if len(s) != operationIDHexLen {
		return OperationID{}, fmt.Errorf("invalid restore operation id %q: want %d hex characters", s, operationIDHexLen)
	}
	if !isLowerHex(s) {
		return OperationID{}, fmt.Errorf("invalid restore operation id %q: non-hex character", s)
	}
	return OperationID{hex: s}, nil
}

// ValidateOperationIDPrefix checks that s is a syntactically valid short id
// prefix: a non-empty run of lowercase hex no longer than a full id. It lets the
// record store tell malformed input from a well-formed prefix that simply matches
// nothing, so a blank or garbage reference is never treated as "match all".
func ValidateOperationIDPrefix(s string) error {
	if s == "" {
		return fmt.Errorf("invalid restore operation id prefix: empty")
	}
	if len(s) > operationIDHexLen {
		return fmt.Errorf("invalid restore operation id prefix %q: longer than a full id (%d)", s, operationIDHexLen)
	}
	if !isLowerHex(s) {
		return fmt.Errorf("invalid restore operation id prefix %q: non-hex character", s)
	}
	return nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// String returns the full hex id. Output always publishes this form, never the
// short one, so an id printed by awa is always directly usable as a reference.
func (id OperationID) String() string { return id.hex }

// Short returns the leading operationIDShortLen characters for human display. It
// is never a lookup key on its own — prefix resolution belongs to the store.
func (id OperationID) Short() string {
	if len(id.hex) <= operationIDShortLen {
		return id.hex
	}
	return id.hex[:operationIDShortLen]
}

// IsZero reports whether the id is unset.
func (id OperationID) IsZero() bool { return id.hex == "" }

// BeforeRef returns the state reference that names this operation's pre-restore
// recovery observation. It is the one place the `restore:<id>:before` spelling is
// constructed, so output, the state-reference parser, and documentation cannot
// drift on it.
func (id OperationID) BeforeRef() string {
	if id.IsZero() {
		return ""
	}
	return "restore:" + id.hex + ":before"
}
