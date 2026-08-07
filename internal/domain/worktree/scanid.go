package worktree

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

// scanIDHexLen is the rendered length of a ScanID: 8 bytes of big-endian
// timestamp plus 8 random bytes, hex-encoded.
const scanIDHexLen = 32

// ScanID uniquely identifies a scan. It is time-prefixed so ids sort in scan
// order, with a random suffix so concurrent scans never collide. It avoids a
// ULID dependency: the timestamp and randomness are injected, keeping the domain
// pure and the value testable.
type ScanID struct {
	hex string
}

// NewScanID builds a ScanID from a unix-nanosecond timestamp and a source of
// randomness. The timestamp leads so lexical order matches chronological order.
func NewScanID(unixNano int64, rand io.Reader) (ScanID, error) {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(unixNano))
	if _, err := io.ReadFull(rand, buf[8:]); err != nil {
		return ScanID{}, fmt.Errorf("generating scan id: %w", err)
	}
	return ScanID{hex: hex.EncodeToString(buf[:])}, nil
}

// ParseScanID validates a persisted scan id.
func ParseScanID(s string) (ScanID, error) {
	if len(s) != scanIDHexLen {
		return ScanID{}, fmt.Errorf("invalid scan id %q: want %d hex characters", s, scanIDHexLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return ScanID{}, fmt.Errorf("invalid scan id %q: non-hex character", s)
		}
	}
	return ScanID{hex: s}, nil
}

// String returns the hex id.
func (id ScanID) String() string { return id.hex }

// IsZero reports whether the id is unset.
func (id ScanID) IsZero() bool { return id.hex == "" }
