package runcache_test

import (
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

// Mutation proofs:
//   - drop the ScanConfigHash.IsZero() check in Observation.Validate ->
//     TestObservationRejectsZeroScanConfig goes red.
//   - drop the before==after scan-config check in validateObservations ->
//     TestValidateRejectsDivergentObservationPolicy goes red.

func blake3Cfg(t *testing.T, s string) hashing.ConfigHash {
	t.Helper()
	return hashing.ConfigHashFromTree(hashOf(t, s))
}

func TestObservationValidatesWithScanConfig(t *testing.T) {
	tree := hashOf(t, "tree")
	obs := runcache.Observation{
		Manifest:       runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: tree, RecordCount: 0},
		ScanConfigHash: hashing.ConfigHashFromTree(tree),
	}
	if err := obs.Validate(); err != nil {
		t.Fatalf("a well-formed observation was rejected: %v", err)
	}
}

func TestObservationRejectsZeroScanConfig(t *testing.T) {
	// A missing scan-config identity makes the observation unresolvable, not a weakened
	// partial identity: it is rejected outright.
	obs := runcache.Observation{
		Manifest: runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: hashOf(t, "tree")},
	}
	if err := obs.Validate(); err == nil {
		t.Error("an observation with no scan config hash must be rejected")
	}
}

func TestValidateRejectsDivergentObservationPolicy(t *testing.T) {
	// One run observes both states under one immutable effective config, so before and
	// after must carry the same scan policy identity. Divergent identities are rejected.
	e := validEntry(t)
	e.Before = &runcache.Observation{
		Manifest:       runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: e.KeyInput.InputTreeHash},
		ScanConfigHash: blake3Cfg(t, "policy-a"),
	}
	e.After = &runcache.Observation{
		Manifest:       runcache.ManifestRef{File: "after.manifest.jsonl", TreeHash: e.KeyInput.InputTreeHash},
		ScanConfigHash: blake3Cfg(t, "policy-b"), // a different scan policy identity
	}
	if err := e.Validate(); err == nil {
		t.Error("a run whose before/after observations carry different scan policy identities must be rejected")
	}
}
