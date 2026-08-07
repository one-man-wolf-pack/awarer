package checkpointjson

import (
	"encoding/json"
	"fmt"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// The on-disk layout separates a checkpoint's small metadata from its potentially
// large manifest, so headers and listings are read without materializing every
// record:
//
//	.awa/checkpoints/<id>/header.json     (schema 1, small metadata)
//	.awa/checkpoints/<id>/manifest.jsonl  (one canonical record per line)
//
// header.json is the commit point: it is published last and exclusively, so a
// crashed write that left only a manifest is invisible to readers, and a
// colliding id fails loudly. The checkpoint directory is the only address a
// checkpoint has; a node of any other shape under the checkpoints directory is
// structural corruption, never a document to be classified (see listIDs).
const (
	headerSchemaVersion = 1
	headerName          = "header.json"
	manifestName        = "manifest.jsonl"
)

// headerDoc is the persisted form of a checkpoint header: a checkpoint's metadata
// and its record count, but no manifest.
type headerDoc struct {
	SchemaVersion         int                        `json:"schema_version"`
	ID                    string                     `json:"id"`
	CreatedAt             string                     `json:"created_at"`
	Root                  string                     `json:"root"`
	CommandCwd            string                     `json:"command_cwd"`
	AwaVersion            string                     `json:"awa_version"`
	Message               string                     `json:"message,omitempty"`
	ScanConfigHash        string                     `json:"scan_config_hash"`
	CheckpointPolicyHash  string                     `json:"checkpoint_policy_hash"`
	TreeHash              string                     `json:"tree_hash"`
	TrustMode             string                     `json:"trust_mode"`
	FastModeWeakSignature bool                       `json:"fast_mode_weak_signature"`
	OmittedStatFields     []string                   `json:"omitted_stat_fields"`
	Git                   *gitDoc                    `json:"git"`
	Stats                 checkpoint.CheckpointStats `json:"stats"`
	RecordCount           int                        `json:"record_count"`
}

// encodeHeader renders a checkpoint's header as current-schema JSON bytes.
func encodeHeader(h checkpoint.CheckpointHeader) ([]byte, error) {
	doc := headerDoc{
		SchemaVersion:         headerSchemaVersion,
		ID:                    h.ID.String(),
		CreatedAt:             h.CreatedAt.UTC().Format(time.RFC3339Nano),
		Root:                  h.Root,
		CommandCwd:            h.CommandCwd,
		AwaVersion:            h.AwaVersion,
		Message:               h.Message.String(),
		ScanConfigHash:        h.ScanConfigHash.String(),
		CheckpointPolicyHash:  h.CheckpointPolicyHash.String(),
		TreeHash:              h.TreeHash.String(),
		TrustMode:             h.TrustMode.String(),
		FastModeWeakSignature: h.FastModeWeakSignature,
		OmittedStatFields:     h.OmittedStatFields.Tokens(),
		Git:                   encodeGit(h.Git),
		Stats:                 h.Stats,
		RecordCount:           h.RecordCount,
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding checkpoint header %s: %w", h.ID, err)
	}
	return append(out, '\n'), nil
}

// decodeHeader parses current-schema header bytes into a validated CheckpointHeader,
// rebuilding every value object through its constructor so a malformed or hand-edited
// header fails here rather than being trusted.
//
// It draws the incompatible/corrupt line first and only once. A lenient probe reads
// the document's own schema_version: a readable version other than the current one is
// a format this build does not speak (ErrIncompatibleFormat) and is never decoded
// through any other shape, while data that is not even a schema-carrying JSON object
// is corruption. Only a document claiming the current schema reaches the strict
// reader, where an unknown field, a missing invariant, or trailing bytes are
// corruption too.
func decodeHeader(data []byte) (checkpoint.CheckpointHeader, error) {
	switch v, err := probeSchemaVersion(data); {
	case err != nil:
		return checkpoint.CheckpointHeader{}, fmt.Errorf("decoding checkpoint header: %w", err)
	case v != headerSchemaVersion:
		return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: checkpoint header schema version %d, want %d",
			checkpoint.ErrIncompatibleFormat, v, headerSchemaVersion)
	}
	// A header is a hostile-input boundary on its own: an unknown field (a hand-edit,
	// a typo, or a record from another format) and trailing bytes are both rejected by
	// the shared strict reader rather than silently accepted.
	var doc headerDoc
	if err := decodeStrictJSON(data, &doc); err != nil {
		return checkpoint.CheckpointHeader{}, fmt.Errorf("decoding checkpoint header: %w", err)
	}
	id, err := checkpoint.ParseCheckpointID(doc.ID)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	createdAt, err := time.Parse(time.RFC3339Nano, doc.CreatedAt)
	if err != nil {
		return checkpoint.CheckpointHeader{}, fmt.Errorf("checkpoint %s: invalid created_at: %w", doc.ID, err)
	}
	msg, err := checkpoint.ParseCheckpointMessage(doc.Message)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	scanCfgHash, err := hashing.ParseConfigHash(doc.ScanConfigHash)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	policyHash, err := hashing.ParseConfigHash(doc.CheckpointPolicyHash)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	treeHash, err := hashing.ParseTreeHash(doc.TreeHash)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	trust, err := config.ParseTrustMode(doc.TrustMode)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	omitted, err := worktree.ParseFieldSet(doc.OmittedStatFields)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	git, err := decodeGit(doc.Git)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	h := checkpoint.CheckpointHeader{
		ID:                    id,
		CreatedAt:             createdAt,
		Root:                  doc.Root,
		CommandCwd:            doc.CommandCwd,
		AwaVersion:            doc.AwaVersion,
		Message:               msg,
		ScanConfigHash:        scanCfgHash,
		CheckpointPolicyHash:  policyHash,
		TreeHash:              treeHash,
		TrustMode:             trust,
		FastModeWeakSignature: doc.FastModeWeakSignature,
		OmittedStatFields:     omitted,
		Git:                   git,
		Stats:                 doc.Stats,
		RecordCount:           doc.RecordCount,
	}
	if err := h.Validate(); err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	return h, nil
}
