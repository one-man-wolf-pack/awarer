package blob

import (
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
)

func hashWith(t *testing.T, hexPayload string) hashing.ContentHash {
	t.Helper()
	h, err := hashing.ParseContentHash(hashing.Namespace + ":" + hexPayload)
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	return h
}

const sampleHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPathForShardsByHexUnderTheIdentityNamespace(t *testing.T) {
	h := hashWith(t, sampleHex)
	p, err := PathFor(h)
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	want := "blake3/01/23/" + sampleHex
	if p.Rel() != want {
		t.Fatalf("Rel() = %q, want %q", p.Rel(), want)
	}
}

func TestPathForIsDeterministic(t *testing.T) {
	h := hashWith(t, sampleHex)
	a, _ := PathFor(h)
	b, _ := PathFor(h)
	if a.Rel() != b.Rel() {
		t.Fatalf("non-deterministic: %q vs %q", a.Rel(), b.Rel())
	}
}

// TestPathForNamespacesEveryBlob proves the content-addressed namespace is still a
// real path segment. A digest cannot choose it — the value comes from the hashing
// domain's fixed Namespace — so a future identity change cannot land its blobs in
// the same directory tree as today's.
func TestPathForNamespacesEveryBlob(t *testing.T) {
	p, err := PathFor(hashWith(t, sampleHex))
	if err != nil {
		t.Fatalf("PathFor: %v", err)
	}
	if !strings.HasPrefix(p.Rel(), hashing.Namespace+"/") {
		t.Fatalf("path %q does not start with the identity namespace %q", p.Rel(), hashing.Namespace)
	}
	if strings.HasPrefix(p.Rel(), sampleHex[:2]+"/") {
		t.Fatalf("path %q shards before the namespace segment", p.Rel())
	}
}

func TestPathForRejectsZeroHash(t *testing.T) {
	if _, err := PathFor(hashing.ContentHash{}); err == nil {
		t.Fatal("expected error for zero hash")
	}
}

func TestNewBlobRefRejectsZero(t *testing.T) {
	if _, err := NewBlobRef(hashing.ContentHash{}); err == nil {
		t.Fatal("expected error for zero hash")
	}
	ref, err := NewBlobRef(hashWith(t, sampleHex))
	if err != nil {
		t.Fatalf("NewBlobRef: %v", err)
	}
	if ref.IsZero() {
		t.Fatal("ref should not be zero")
	}
}
