package hashing

import (
	"strings"
	"testing"
)

// sixtyFour is a valid 64-character lowercase-hex digest used across the tests.
const sixtyFour = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseContentHashRoundTrips(t *testing.T) {
	in := Namespace + ":" + sixtyFour
	h, err := ParseContentHash(in)
	if err != nil {
		t.Fatalf("ParseContentHash(%q) error: %v", in, err)
	}
	if got := h.String(); got != in {
		t.Errorf("String() = %q, want %q", got, in)
	}
	if h.Hex() != sixtyFour {
		t.Errorf("Hex() = %q, want %q", h.Hex(), sixtyFour)
	}
	if h.IsZero() {
		t.Errorf("parsed hash reports IsZero")
	}
}

// TestParseRejectsEveryPrefixButBlake3 is the fixed-prefix guard. BLAKE3 is the only
// local content-identity primitive, so a digest naming any other one — most pointedly
// the sha256: that external release and checksum contracts speak — belongs to a
// foreign namespace: invalid local evidence, never a value to reinterpret as BLAKE3.
// Each local digest type parses through the same boundary, so all of them are
// checked: a guard that held for ContentHash alone would leave the others open.
func TestParseRejectsEveryPrefixButBlake3(t *testing.T) {
	parsers := map[string]func(string) error{
		"ContentHash": func(s string) error { _, err := ParseContentHash(s); return err },
		"TreeHash":    func(s string) error { _, err := ParseTreeHash(s); return err },
		"ConfigHash":  func(s string) error { _, err := ParseConfigHash(s); return err },
	}
	bad := []string{
		"",                                     // empty
		sixtyFour,                              // bare digest, no identity prefix
		"sha256:" + sixtyFour,                  // external contracts' primitive
		"crc32:" + sixtyFour,                   // forbidden ad-hoc checksum
		"md5:" + sixtyFour,                     // unknown primitive
		"BLAKE3:" + sixtyFour,                  // uppercase prefix
		"blake3:" + sixtyFour[:63],             // too short
		"blake3:" + sixtyFour + "0",            // too long
		"blake3:" + strings.ToUpper(sixtyFour), // uppercase hex
		"blake3:" + strings.Repeat("g", 64),    // non-hex alphabet
		"blake3:",                              // missing hex
		"blake3" + sixtyFour,                   // missing separator
	}
	for name, parse := range parsers {
		for _, in := range bad {
			if err := parse(in); err == nil {
				t.Errorf("Parse%s(%q) = nil error, want rejection", name, in)
			}
		}
	}
}

func TestParseTreeHashRoundTrips(t *testing.T) {
	in := Namespace + ":" + sixtyFour
	h, err := ParseTreeHash(in)
	if err != nil {
		t.Fatalf("ParseTreeHash(%q) error: %v", in, err)
	}
	if got := h.String(); got != in {
		t.Errorf("String() = %q, want %q", got, in)
	}
	if h.Hex() != sixtyFour {
		t.Errorf("Hex() = %q, want %q", h.Hex(), sixtyFour)
	}
}

func TestConfigHash(t *testing.T) {
	in := Namespace + ":" + sixtyFour
	c, err := ParseConfigHash(in)
	if err != nil {
		t.Fatalf("ParseConfigHash(%q): %v", in, err)
	}
	if c.String() != in || c.IsZero() {
		t.Errorf("round trip = %q isZero=%v, want %q", c.String(), c.IsZero(), in)
	}
	// A config hash cannot be built from an arbitrary string.
	if _, err := ParseConfigHash("abc"); err == nil {
		t.Errorf("ParseConfigHash accepted \"abc\"")
	}
	// ConfigHashFromTree carries the digest of the tree hash.
	th, err := ParseTreeHash(in)
	if err != nil {
		t.Fatal(err)
	}
	if ConfigHashFromTree(th).String() != in {
		t.Errorf("ConfigHashFromTree mismatch")
	}
	if (ConfigHash{}).String() != "" || !(ConfigHash{}).IsZero() {
		t.Errorf("zero ConfigHash should render empty and report IsZero")
	}
}

func TestNewContentHashValidatesDigestLength(t *testing.T) {
	if _, err := NewContentHash(make([]byte, 32)); err != nil {
		t.Errorf("NewContentHash(32 bytes) error: %v", err)
	}
	if _, err := NewContentHash(make([]byte, 16)); err == nil {
		t.Errorf("NewContentHash(16 bytes) = nil error, want rejection")
	}
	if _, err := NewContentHash(make([]byte, 64)); err == nil {
		t.Errorf("NewContentHash(64 bytes) = nil error, want rejection")
	}
}

func TestNewContentHashHexEncoding(t *testing.T) {
	digest := []byte{0x00, 0x01, 0x0a, 0xff}
	digest = append(digest, make([]byte, 28)...) // pad to 32 bytes
	h, err := NewContentHash(digest)
	if err != nil {
		t.Fatalf("NewContentHash error: %v", err)
	}
	if !strings.HasPrefix(h.String(), "blake3:00010aff") {
		t.Errorf("String() = %q, want blake3:00010aff… prefix", h.String())
	}
}

// TestNamespaceIsTheRenderedPrefix pins the one fixed label to what digests
// actually render, so the storage path segment and the persisted digest syntax
// cannot drift apart.
func TestNamespaceIsTheRenderedPrefix(t *testing.T) {
	if Namespace != "blake3" {
		t.Fatalf("Namespace = %q, want blake3", Namespace)
	}
	h, err := NewTreeHash(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.String(), Namespace+":") {
		t.Errorf("TreeHash renders %q, which does not start with %q", h.String(), Namespace+":")
	}
}

func TestZeroContentHash(t *testing.T) {
	var h ContentHash
	if !h.IsZero() {
		t.Errorf("zero ContentHash should report IsZero")
	}
	if h.String() != "" {
		t.Errorf("zero ContentHash String() = %q, want empty", h.String())
	}
}

func TestZeroTreeHash(t *testing.T) {
	var h TreeHash
	if !h.IsZero() {
		t.Errorf("zero TreeHash should report IsZero")
	}
	if h.String() != "" {
		t.Errorf("zero TreeHash String() = %q, want empty", h.String())
	}
}
