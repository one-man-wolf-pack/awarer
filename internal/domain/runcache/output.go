package runcache

import (
	"fmt"

	"awarer/internal/domain/hashing"
)

// TruncationPolicy names how a captured stream was bounded. Head-only is the only
// truncation implemented; the type exists so a future head+tail policy can be
// recorded without changing the metadata shape.
type TruncationPolicy int

const (
	// TruncationNone means the whole stream was stored.
	TruncationNone TruncationPolicy = iota
	// TruncationHead means the leading bytes up to the limit were stored and the
	// remainder was dropped, with a marker recording the exact omitted byte count.
	TruncationHead
)

// ParseTruncationPolicy resolves a persisted token.
func ParseTruncationPolicy(s string) (TruncationPolicy, error) {
	switch s {
	case "none":
		return TruncationNone, nil
	case "head":
		return TruncationHead, nil
	default:
		return 0, fmt.Errorf("invalid truncation policy %q: want none or head", s)
	}
}

func (p TruncationPolicy) String() string {
	switch p {
	case TruncationNone:
		return "none"
	case TruncationHead:
		return "head"
	default:
		return "unknown"
	}
}

// Valid reports whether p is a known policy.
func (p TruncationPolicy) Valid() bool { return p == TruncationNone || p == TruncationHead }

// OutputCapture is the metadata for one stored output stream. It records the
// original byte count observed from the process, the bytes actually stored (which
// includes any truncation marker), whether and how the stream was truncated, the
// exact omitted byte count, the payload hash used to verify the stored bytes
// before replay, and the payload's file name within the run entry.
type OutputCapture struct {
	OriginalBytes    int64
	StoredBytes      int64
	Truncated        bool
	OmittedBytes     int64
	TruncationPolicy TruncationPolicy
	Hash             hashing.ContentHash
	File             string
}

// Validate checks that a capture's fields are mutually consistent. An untruncated
// stream is stored in full, so StoredBytes equals OriginalBytes and nothing is
// omitted. A truncated stream omits a positive count under the head policy and
// stores the retained head plus a non-empty truncation marker, so StoredBytes
// strictly exceeds the retained head (OriginalBytes - OmittedBytes) and may exceed
// OriginalBytes. A stored stream must carry a hash and a file name so a hit can
// verify the payload before replay.
func (c OutputCapture) Validate() error {
	if !c.TruncationPolicy.Valid() {
		return fmt.Errorf("invalid truncation policy %v", c.TruncationPolicy)
	}
	if c.OriginalBytes < 0 || c.StoredBytes < 0 || c.OmittedBytes < 0 {
		return fmt.Errorf("byte counts must not be negative")
	}
	if c.Truncated {
		if c.TruncationPolicy == TruncationNone {
			return fmt.Errorf("truncated capture must not use the none policy")
		}
		if c.OmittedBytes <= 0 {
			return fmt.Errorf("truncated capture must omit a positive byte count")
		}
		if c.OriginalBytes <= 0 {
			return fmt.Errorf("truncated capture must have a positive original byte count")
		}
		// Truncation keeps a positive head: the capture limit is positive, so a
		// truncated stream always stored at least one byte before the limit was hit.
		// Omitting the whole stream (a marker-only payload) is impossible.
		if c.OmittedBytes >= c.OriginalBytes {
			return fmt.Errorf("truncated capture omits all %d of %d original bytes, leaving no head", c.OmittedBytes, c.OriginalBytes)
		}
		// The stored payload retains the head (original minus omitted) plus a non-empty
		// truncation marker, so it must be strictly longer than the retained head. A
		// stored size equal to the head means the marker is missing — a truncated=true
		// payload that would replay as a silent, unmarked cut.
		if c.StoredBytes <= c.OriginalBytes-c.OmittedBytes {
			return fmt.Errorf("truncated capture stored %d bytes, not more than the retained head %d (missing truncation marker)", c.StoredBytes, c.OriginalBytes-c.OmittedBytes)
		}
	} else {
		if c.OmittedBytes != 0 {
			return fmt.Errorf("untruncated capture must not omit bytes")
		}
		if c.TruncationPolicy != TruncationNone {
			return fmt.Errorf("untruncated capture must use the none policy")
		}
		// An untruncated stream is stored in full, byte for byte.
		if c.StoredBytes != c.OriginalBytes {
			return fmt.Errorf("untruncated capture stored %d of %d bytes", c.StoredBytes, c.OriginalBytes)
		}
	}
	if c.File == "" {
		return fmt.Errorf("capture has no payload file name")
	}
	if c.Hash.IsZero() {
		return fmt.Errorf("capture has no payload hash")
	}
	return nil
}
