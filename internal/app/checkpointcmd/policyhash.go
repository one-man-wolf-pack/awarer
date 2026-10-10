package checkpointcmd

import (
	"bytes"
	"encoding/binary"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/pattern"
)

// policyHashVersion leads the canonical encoding so future policy fields can be
// added without colliding with older, shorter encodings.
const policyHashVersion byte = 0x01

// hashOnlyPatternsField tags the hash_only_patterns field appended after the
// fixed-length predecessor encoding. 0x01 tagged the same list without a matching
// generation and must not be reused.
const hashOnlyPatternsField byte = 0x02

// checkpointPolicyHash computes a stable identifier for the checkpoint
// persistence policy: the configuration that affects how a checkpoint stores
// evidence rather than what the worktree contains. It covers store_file_contents
// and the ordered hash_only_patterns selection.
//
// The predecessor encoding is exactly two bytes (version, store flag). An empty
// selection appends nothing, so it keeps that encoding and hash; a non-empty one
// appends a field tag, the matching generation its patterns are read under, and
// the length-prefixed list in configured order (order is meaning: a later "!"
// exception un-selects). Because the predecessor encoding is
// fixed-length, the extended form can never collide with it.
//
// It is deliberately distinct from the scan config hash (scope + hashing) and
// from the tree hash (observed worktree). Changing a storage policy therefore
// changes this hash alone, leaving tree_hash and scan_config_hash stable.
func checkpointPolicyHash(cfg config.Config, h hashing.Hasher) hashing.ConfigHash {
	return checkpointPolicyHashAt(pattern.Generation, cfg, h)
}

func checkpointPolicyHashAt(generation uint64, cfg config.Config, h hashing.Hasher) hashing.ConfigHash {
	var b bytes.Buffer
	b.WriteByte(policyHashVersion)
	if cfg.Checkpoint.StoreFileContents {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	if patterns := cfg.Checkpoint.HashOnlyPatterns; len(patterns) > 0 {
		b.WriteByte(hashOnlyPatternsField)
		putUint(&b, generation)
		putUint(&b, uint64(len(patterns)))
		for _, p := range patterns {
			putUint(&b, uint64(len(p)))
			b.WriteString(p)
		}
	}
	return hashing.ConfigHashFromTree(h.HashBytes(b.Bytes()))
}

// putUint writes a fixed 8-byte big-endian number.
func putUint(b *bytes.Buffer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.Write(n[:])
}
