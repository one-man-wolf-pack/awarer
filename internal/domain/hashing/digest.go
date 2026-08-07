// Package hashing defines the domain model for content and tree identity.
//
// It owns the local identity value objects — ContentHash, TreeHash, ConfigHash —
// and the Hasher port that produces them. BLAKE3 is the only local content
// identity primitive: a hash always renders with the fixed blake3: prefix so a
// persisted value is self-describing and a future identity change is a governed
// format decision rather than a silent reinterpretation of the same bytes. The
// prefix is self-description, not configurable state; nothing here selects an
// algorithm. The package is pure: it performs no I/O and depends on no other
// domain package. The concrete BLAKE3 implementation lives behind the Hasher
// port in infrastructure, so no third-party hashing type leaks into the domain.
//
// SHA-256 exists elsewhere in the project only as an external interoperability
// convention (release checksums, exported documentation manifests, site assets).
// It never produces local evidence identity and has no representation here.
package hashing

import (
	"fmt"
	"strings"
)

// Namespace is the fixed local identity marker. It is the digest prefix
// ("blake3:<hex>") and the content-addressed storage path segment, so a stored
// digest and the directory it lives under name the same primitive. It is a
// constant rather than a value carried per digest: a digest describes itself, it
// does not get to choose its own namespace.
const Namespace = "blake3"

// prefix is the persisted digest framing derived from Namespace.
const prefix = Namespace + ":"

// hexLen is the number of lowercase hex characters in a persisted digest. BLAKE3
// emits a 32-byte digest here, which is 64 hex characters. A different output
// length would be a different value type, so the domain pins the expected length
// rather than accepting any hex run.
const hexLen = 64

// parseDigest validates a persisted "blake3:hex" string and returns its hex
// payload. It rejects every other prefix — including sha256: and ad-hoc checksums
// such as crc32 — bare digests, malformed framing, and hex of the wrong length or
// alphabet, so an invalid identity can never enter the domain as raw data.
func parseDigest(kind, s string) (string, error) {
	hexPart, found := strings.CutPrefix(s, prefix)
	if !found {
		return "", fmt.Errorf("invalid %s %q: want %q", kind, s, prefix+"hex")
	}
	if err := validateHex(hexPart); err != nil {
		return "", fmt.Errorf("invalid %s %q: %w", kind, s, err)
	}
	return hexPart, nil
}

// validateHex enforces the lowercase, fixed-length hex contract shared by every
// persisted digest.
func validateHex(h string) error {
	if len(h) != hexLen {
		return fmt.Errorf("hex must be %d characters, got %d", hexLen, len(h))
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return fmt.Errorf("hex must be lowercase 0-9a-f, found %q", string(c))
		}
	}
	return nil
}

// formatDigest renders a hex payload as the canonical "blake3:hex" persisted form.
func formatDigest(hexPayload string) string {
	return prefix + hexPayload
}
