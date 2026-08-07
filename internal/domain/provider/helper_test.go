package provider_test

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
)

// cpID builds a valid, distinct checkpoint id from a short label — the id is derived
// deterministically from the label, so equal labels yield equal ids and distinct labels
// distinct ids. It exists so tests exercise the constructors with real typed ids, never a
// hand-written string that could never be a genuine CheckpointID.
func cpID(t *testing.T, label string) checkpoint.CheckpointID {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(seededBytes(label, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID(%q): %v", label, err)
	}
	return id
}

// runID builds a valid, distinct run id from a short label, the run-id analogue of cpID.
func runID(t *testing.T, label string) runcache.RunID {
	t.Helper()
	id, err := runcache.ParseRunID(seededHex(label, 16))
	if err != nil {
		t.Fatalf("ParseRunID(%q): %v", label, err)
	}
	return id
}

// seededHex expands label into n deterministic bytes rendered as lowercase hex. It
// only needs to spread distinct labels onto distinct ids; nothing here is evidence,
// so a cheap non-cryptographic mix is exactly the right tool.
func seededHex(label string, n int) string {
	var out strings.Builder
	for i := 0; out.Len() < 2*n; i++ {
		f := fnv.New64a()
		fmt.Fprintf(f, "%s/%d", label, i)
		fmt.Fprintf(&out, "%016x", f.Sum64())
	}
	return out.String()[:2*n]
}

// seededBytes is seededHex's raw-byte counterpart, for constructors that read an
// io.Reader of random bytes.
func seededBytes(label string, n int) []byte {
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		f := fnv.New64a()
		fmt.Fprintf(f, "%s/%d", label, i)
		v := f.Sum64()
		for b := 0; b < 8 && len(out) < n; b++ {
			out = append(out, byte(v>>(8*b)))
		}
	}
	return out
}

// treeHash builds a valid tree hash from a one-character-repeat hex digest, so
// distinct nibbles yield distinct hashes.
func treeHash(t *testing.T, nibble string) hashing.TreeHash {
	t.Helper()
	th, err := hashing.ParseTreeHash(hashing.Namespace + ":" + strings.Repeat(nibble, 64))
	if err != nil {
		t.Fatalf("ParseTreeHash: %v", err)
	}
	return th
}

// configHash builds a valid config hash from a hex digest nibble.
func configHash(t *testing.T, nibble string) hashing.ConfigHash {
	t.Helper()
	ch, err := hashing.ParseConfigHash(hashing.Namespace + ":" + strings.Repeat(nibble, 64))
	if err != nil {
		t.Fatalf("ParseConfigHash: %v", err)
	}
	return ch
}

// validScan builds a fully-known scan identity.
func validScan(t *testing.T, cfgNibble string) provider.ScanIdentity {
	t.Helper()
	si, err := provider.NewScanIdentity(configHash(t, cfgNibble))
	if err != nil {
		t.Fatalf("NewScanIdentity: %v", err)
	}
	return si
}

// boundary builds a valid evidence boundary.
func boundary(t *testing.T, skipped int) provider.EvidenceBoundary {
	t.Helper()
	b, err := provider.NewEvidenceBoundary(skipped)
	if err != nil {
		t.Fatalf("NewEvidenceBoundary: %v", err)
	}
	return b
}
