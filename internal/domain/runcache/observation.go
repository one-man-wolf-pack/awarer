package runcache

import (
	"fmt"

	"awarer/internal/domain/hashing"
)

// Observation names a recorded run's pre- or post-execution observed state. The
// canonical artifact is a manifest file (the same JSONL format checkpoints use)
// stored alongside the entry; ManifestRef holds the file name and the derived
// facts that verify it on read, so a run-observation reference can re-derive the
// tree hash and record count and reject a tampered or truncated manifest.

// ManifestRef addresses one stored observation manifest and the facts that verify
// it. The manifest bytes are the source of truth; TreeHash and RecordCount are
// recomputed by the reducer at read time and must match.
type ManifestRef struct {
	// File is the fixed manifest file name inside the entry directory.
	File string
	// TreeHash is the reduced tree hash of the manifest's records.
	TreeHash hashing.TreeHash
	// RecordCount is the number of manifest records (entries plus skipped inputs).
	RecordCount int
}

// Validate checks a manifest ref is well-formed: a non-empty file name, a
// non-negative record count, and a tree hash that is set. The exact file name is a
// layout concern the store enforces; the domain only requires it be present so a
// ref can never address an empty path.
func (r ManifestRef) Validate() error {
	if r.File == "" {
		return fmt.Errorf("manifest ref has no file name")
	}
	if r.RecordCount < 0 {
		return fmt.Errorf("manifest ref has a negative record count %d", r.RecordCount)
	}
	if r.TreeHash.IsZero() {
		return fmt.Errorf("manifest ref has no tree hash")
	}
	return nil
}

// Observation is one pre/post observed-state identity attached to a recorded run.
// It bundles the manifest ref (which carries the observed tree hash) with the
// scanner's complete policy identity (ScanConfigHash: scope, excludes, symlink and
// large-file policy, and trust mode) as one typed value, so a stored observation can
// be resolved to a full external-state identity — not just a tree hash whose
// interpreting policy would have to be guessed.
type Observation struct {
	Manifest ManifestRef
	// ScanConfigHash is the scanner-produced scan_config_hash captured with this
	// observation. It is produced by the same project hasher as the manifest's tree
	// hash, so the two are read and validated as one identity. It is never
	// reconstructed from current config: an observation without it is not a
	// resolvable identity.
	ScanConfigHash hashing.ConfigHash
}

// Validate checks the observation's manifest ref and that its scan-policy identity
// is present: a non-zero scan_config_hash. A zero policy hash makes the observation
// unresolvable rather than a weakened partial identity.
func (o Observation) Validate() error {
	if err := o.Manifest.Validate(); err != nil {
		return err
	}
	if o.ScanConfigHash.IsZero() {
		return fmt.Errorf("observation has no scan config hash")
	}
	return nil
}
