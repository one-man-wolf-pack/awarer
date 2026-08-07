package checkpointcmd

import (
	"bytes"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// policyHashVersion leads the canonical encoding so future policy fields can be
// added without colliding with older, shorter encodings.
const policyHashVersion byte = 0x01

// checkpointPolicyHash computes a stable identifier for the checkpoint
// persistence policy: the configuration that affects how a checkpoint stores
// evidence rather than what the worktree contains. It currently covers
// store_file_contents; later storage-affecting policy is appended to the same
// canonical encoding.
//
// It is deliberately distinct from the scan config hash (scope + hashing) and
// from the tree hash (observed worktree). Toggling a storage policy therefore
// changes this hash alone, leaving tree_hash and scan_config_hash stable.
func checkpointPolicyHash(cfg config.Config, h hashing.Hasher) hashing.ConfigHash {
	var b bytes.Buffer
	b.WriteByte(policyHashVersion)
	if cfg.Checkpoint.StoreFileContents {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	return hashing.ConfigHashFromTree(h.HashBytes(b.Bytes()))
}
