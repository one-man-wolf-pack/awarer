package hashing

import (
	"encoding/hex"
	"fmt"
)

// TreeHash is the identity of an entire scanned tree: the digest of the tree's
// canonical encoding. It is a distinct type from ContentHash so a file digest can
// never be assigned where a tree digest is expected, even though both share the
// "blake3:hex" persisted form.
//
// The zero TreeHash is invalid; IsZero reports it.
type TreeHash struct {
	hex string
}

// NewTreeHash builds a TreeHash from a raw digest produced by a hasher.
func NewTreeHash(digest []byte) (TreeHash, error) {
	digestHex := hex.EncodeToString(digest)
	if err := validateHex(digestHex); err != nil {
		return TreeHash{}, fmt.Errorf("invalid tree hash: %w", err)
	}
	return TreeHash{hex: digestHex}, nil
}

// ParseTreeHash parses a persisted "blake3:hex" tree hash.
func ParseTreeHash(s string) (TreeHash, error) {
	hex, err := parseDigest("tree hash", s)
	if err != nil {
		return TreeHash{}, err
	}
	return TreeHash{hex: hex}, nil
}

// Hex returns the lowercase hex digest without the prefix, or the empty string
// for the zero value.
func (h TreeHash) Hex() string { return h.hex }

// IsZero reports whether h is the zero value — no tree hash computed.
func (h TreeHash) IsZero() bool { return h.hex == "" }

// String renders the canonical "blake3:hex" form. The zero value renders as the
// empty string.
func (h TreeHash) String() string {
	if h.IsZero() {
		return ""
	}
	return formatDigest(h.hex)
}
