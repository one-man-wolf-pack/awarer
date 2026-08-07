package checkpoint

import (
	"fmt"
	"path/filepath"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// CheckpointBuild is the non-derived metadata a checkpoint is written with: everything
// a caller supplies, but none of the fields derived from the manifest (the tree
// hash, the stats, the record count). It is the input to the streaming write
// contract (Repository.PutManifest), which consumes a manifest stream and derives
// the rest itself — so the write boundary never requires the caller to own a fully
// materialized manifest aggregate.
type CheckpointBuild struct {
	ID                    CheckpointID
	CreatedAt             time.Time
	Root                  string
	CommandCwd            string
	AwaVersion            string
	Message               CheckpointMessage
	ScanConfigHash        hashing.ConfigHash
	CheckpointPolicyHash  hashing.ConfigHash
	TrustMode             config.TrustMode
	FastModeWeakSignature bool
	OmittedStatFields     worktree.FieldSet
	Git                   *GitMetadata
}

// Validate checks the non-derived metadata invariants. It is run before a write
// consumes the manifest stream, so malformed metadata fails fast rather than after
// the I/O of streaming a manifest to disk.
func (b CheckpointBuild) Validate() error { return validateMeta(b) }

// Header completes the build into a full header by adding the fields derived from
// the manifest: the tree hash, the stats, and the record count.
func (b CheckpointBuild) Header(treeHash hashing.TreeHash, stats CheckpointStats, recordCount int) CheckpointHeader {
	return CheckpointHeader{
		ID:                    b.ID,
		CreatedAt:             b.CreatedAt,
		Root:                  b.Root,
		CommandCwd:            b.CommandCwd,
		AwaVersion:            b.AwaVersion,
		Message:               b.Message,
		ScanConfigHash:        b.ScanConfigHash,
		CheckpointPolicyHash:  b.CheckpointPolicyHash,
		TreeHash:              treeHash,
		TrustMode:             b.TrustMode,
		FastModeWeakSignature: b.FastModeWeakSignature,
		OmittedStatFields:     b.OmittedStatFields,
		Git:                   b.Git,
		Stats:                 stats,
		RecordCount:           recordCount,
	}
}

// validateMeta checks the metadata a checkpoint carries independent of its
// manifest. It is shared by CheckpointBuild.Validate and CheckpointHeader.Validate so
// the two cannot drift on what well-formed metadata means.
func validateMeta(b CheckpointBuild) error {
	if b.ID.IsZero() {
		return fmt.Errorf("checkpoint has no id")
	}
	if b.CreatedAt.IsZero() {
		return fmt.Errorf("checkpoint %s has no creation time", b.ID)
	}
	if b.Root == "" || !filepath.IsAbs(b.Root) || filepath.Clean(b.Root) != b.Root {
		return fmt.Errorf("checkpoint %s root %q must be an absolute, cleaned path", b.ID, b.Root)
	}
	if b.CommandCwd == "" {
		return fmt.Errorf("checkpoint %s has no command cwd", b.ID)
	}
	if b.AwaVersion == "" {
		return fmt.Errorf("checkpoint %s has no awa version", b.ID)
	}
	if !b.TrustMode.Valid() {
		return fmt.Errorf("checkpoint %s has invalid trust mode %v", b.ID, b.TrustMode)
	}
	if b.ScanConfigHash.IsZero() {
		return fmt.Errorf("checkpoint %s has no scan config hash", b.ID)
	}
	if b.CheckpointPolicyHash.IsZero() {
		return fmt.Errorf("checkpoint %s has no checkpoint policy hash", b.ID)
	}
	if !b.OmittedStatFields.Valid() {
		return fmt.Errorf("checkpoint %s has unknown omitted stat fields", b.ID)
	}
	if b.FastModeWeakSignature != (b.TrustMode == config.TrustFast) {
		return fmt.Errorf("checkpoint %s fast-mode weak signature %v contradicts trust mode %v", b.ID, b.FastModeWeakSignature, b.TrustMode)
	}
	if b.Git != nil {
		if err := b.Git.Validate(); err != nil {
			return fmt.Errorf("checkpoint %s: %w", b.ID, err)
		}
	}
	return nil
}

// CheckpointHeader is the small, durable metadata of a checkpoint — everything except
// the manifest of entries and skipped inputs. It is the unit a repository can load
// and list without materializing a checkpoint's whole manifest, which is what lets
// log, status, and listing scale independently of manifest size.
//
// RecordCount is the number of manifest records (entries plus skipped inputs) the
// checkpoint's manifest holds. It is a durable second source of truth: a streaming
// manifest read checks the records it yields against it, so a truncated manifest
// is caught rather than silently shortened.
type CheckpointHeader struct {
	ID                    CheckpointID
	CreatedAt             time.Time
	Root                  string
	CommandCwd            string
	AwaVersion            string
	Message               CheckpointMessage
	ScanConfigHash        hashing.ConfigHash
	CheckpointPolicyHash  hashing.ConfigHash
	TreeHash              hashing.TreeHash
	TrustMode             config.TrustMode
	FastModeWeakSignature bool
	OmittedStatFields     worktree.FieldSet
	Git                   *GitMetadata
	Stats                 CheckpointStats
	RecordCount           int
}

// Build projects the header's non-derived metadata back into a CheckpointBuild,
// dropping the manifest-derived fields. It is the inverse of CheckpointBuild.Header
// and lets the header reuse the shared metadata validation.
func (h CheckpointHeader) Build() CheckpointBuild {
	return CheckpointBuild{
		ID:                    h.ID,
		CreatedAt:             h.CreatedAt,
		Root:                  h.Root,
		CommandCwd:            h.CommandCwd,
		AwaVersion:            h.AwaVersion,
		Message:               h.Message,
		ScanConfigHash:        h.ScanConfigHash,
		CheckpointPolicyHash:  h.CheckpointPolicyHash,
		TrustMode:             h.TrustMode,
		FastModeWeakSignature: h.FastModeWeakSignature,
		OmittedStatFields:     h.OmittedStatFields,
		Git:                   h.Git,
	}
}

// Validate checks the header's invariants: the shared metadata, plus the derived
// fields it duplicates (tree hash present, stats mechanically possible, record
// count consistent). Per-manifest checks (entry validity, stats/tree-hash
// derivation) are not done here because a header carries no manifest; the
// repository verifies those when it reads the records.
func (h CheckpointHeader) Validate() error {
	if err := validateMeta(h.Build()); err != nil {
		return err
	}
	if h.TreeHash.IsZero() {
		return fmt.Errorf("checkpoint %s has no tree hash", h.ID)
	}
	// The durable stats must be mechanically possible on their own — a header is a
	// hostile-input boundary that can be read without the manifest.
	if err := h.Stats.Validate(); err != nil {
		return fmt.Errorf("checkpoint %s: %w", h.ID, err)
	}
	// RecordCount is a durable count; it must agree with the per-kind stats the
	// header also carries, so a hand-edited header whose count and stats disagree is
	// rejected before any manifest read trusts either.
	if h.RecordCount < 0 {
		return fmt.Errorf("checkpoint %s has negative record count %d", h.ID, h.RecordCount)
	}
	if want := h.Stats.Files + h.Stats.Dirs + h.Stats.Symlinks + h.Stats.Skipped; h.RecordCount != want {
		return fmt.Errorf("checkpoint %s record count %d does not match stats total %d", h.ID, h.RecordCount, want)
	}
	return nil
}
