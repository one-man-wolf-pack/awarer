package hashing

import (
	"encoding/hex"
	"fmt"
)

// ContentHash is the identity of a file's contents: the lowercase-hex BLAKE3
// digest of the bytes. It is a value object — two ContentHashes are equal exactly
// when their digest matches — and it always renders with the fixed blake3: prefix
// so a persisted hash is self-describing.
//
// The zero ContentHash is deliberately invalid (it has no parsed hex); IsZero
// reports it so callers distinguish "no hash recorded" from a real digest rather
// than treating an empty value as content identity.
type ContentHash struct {
	hex string
}

// NewContentHash builds a ContentHash from a raw digest produced by a hasher. The
// digest length is validated against the expected output, so a truncated or
// oversized digest cannot become a content identity.
func NewContentHash(digest []byte) (ContentHash, error) {
	digestHex := hex.EncodeToString(digest)
	if err := validateHex(digestHex); err != nil {
		return ContentHash{}, fmt.Errorf("invalid content hash: %w", err)
	}
	return ContentHash{hex: digestHex}, nil
}

// ParseContentHash parses a persisted "blake3:hex" content hash, rejecting any
// other prefix and malformed digests.
func ParseContentHash(s string) (ContentHash, error) {
	hex, err := parseDigest("content hash", s)
	if err != nil {
		return ContentHash{}, err
	}
	return ContentHash{hex: hex}, nil
}

// Hex returns the lowercase hex digest without the prefix, or the empty string
// for the zero value. The blob store shards on this payload, so it is exposed
// without forcing callers to re-split the persisted form.
func (h ContentHash) Hex() string { return h.hex }

// IsZero reports whether h is the zero value — no content hash recorded.
func (h ContentHash) IsZero() bool { return h.hex == "" }

// String renders the canonical "blake3:hex" form, e.g. "blake3:abcd…". The zero
// value renders as the empty string so it is never mistaken for a digest.
func (h ContentHash) String() string {
	if h.IsZero() {
		return ""
	}
	return formatDigest(h.hex)
}
