package runcache_test

import (
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/scantest"
)

func blakeTree(t *testing.T, nibble string) hashing.TreeHash {
	t.Helper()
	th, err := hashing.ParseTreeHash("blake3:" + strings.Repeat(nibble, 64))
	if err != nil {
		t.Fatalf("ParseTreeHash: %v", err)
	}
	return th
}

func cfgHash(t *testing.T, nibble string) hashing.ConfigHash {
	t.Helper()
	ch, err := hashing.ParseConfigHash(hashing.Namespace + ":" + strings.Repeat(nibble, 64))
	if err != nil {
		t.Fatalf("ParseConfigHash: %v", err)
	}
	return ch
}

// TestNewRunObservationReadGuards proves the value object rejects every malformed shape
// the old exported field bag could express.
//
// Mutation proofs:
//   - drop the nil-manifest guard -> the nil-manifest case goes red (a nil stream is
//     accepted).
//   - drop the tree-hash guard -> the zero-tree case goes red.
//   - drop the scan-config guard -> the zero-scan-config case goes red.
func TestNewRunObservationReadGuards(t *testing.T) {
	tree := blakeTree(t, "a")
	cfg := cfgHash(t, "b")
	reuse := runcache.ReusableCacheEntry()

	// Happy path: non-nil stream, complete identity.
	read, err := runcache.NewRunObservationRead(scantest.CanonicalStream(nil, nil), tree, cfg, reuse)
	if err != nil {
		t.Fatalf("NewRunObservationRead happy path: %v", err)
	}
	if read.Manifest() == nil || read.TreeHash() != tree || read.ScanConfigHash() != cfg {
		t.Error("accessors did not round-trip the constructed values")
	}

	if _, err := runcache.NewRunObservationRead(nil, tree, cfg, reuse); err == nil {
		t.Error("must reject a nil manifest stream")
	}
	if _, err := runcache.NewRunObservationRead(scantest.CanonicalStream(nil, nil), hashing.TreeHash{}, cfg, reuse); err == nil {
		t.Error("must reject a zero tree hash")
	}
	if _, err := runcache.NewRunObservationRead(scantest.CanonicalStream(nil, nil), tree, hashing.ConfigHash{}, reuse); err == nil {
		t.Error("must reject a zero scan config hash")
	}
}
