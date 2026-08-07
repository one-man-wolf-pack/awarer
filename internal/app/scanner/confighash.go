package scanner

import (
	"bytes"
	"encoding/binary"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
)

// configHash computes a stable identifier for the configuration fields that
// constitute the scan policy: what is scanned, and how (including how strongly the
// evidence is trusted). It is persisted as a scan's scan_config_hash, so it must be
// honest about every input that changes the meaning of the scan — including trust
// mode, since strict, normal, and fast scans produce evidence of different strength
// even over the same worktree. It excludes unrelated sections (checkpoint, run,
// gc), which are hashed separately or not at all.
//
// Every field is length-prefixed, so the encoding is injective: lists such as
// ["a", "b"] and ["a,b"] can never collide, and a value containing a separator
// byte cannot be confused for a field boundary.
func configHash(cfg config.Config, scope config.ScanScope, h hashing.Hasher) hashing.ConfigHash {
	var b bytes.Buffer
	// Scope inputs come from the resolved ScanScope the walker actually used.
	// Include is canonicalized so semantically identical scopes (["src"] vs
	// ["./src"]) hash identically — the same canonical list the walker scans.
	// Exclude entries are gitignore patterns the walker uses verbatim, so they are
	// hashed verbatim too.
	putList(&b, scope.CanonicalInclude())
	putList(&b, scope.Exclude)
	putBool(&b, scope.UseGitignore)
	putBool(&b, scope.UseAwaignore)
	putBool(&b, scope.FollowSymlinks)
	putUint(&b, uint64(scope.SymlinkMaxDepth))
	putBool(&b, scope.AllowSymlinkRootEscape)
	// Hashing inputs, including trust mode: it is part of the scan policy, so a
	// strict scan and a fast scan of the same worktree get distinct scan_config_hash.
	putString(&b, cfg.Hashing.TrustMode.String())
	putUint(&b, uint64(cfg.Hashing.MaxFileSize.Bytes()))
	putString(&b, cfg.Hashing.LargeFilePolicy.String())
	return hashing.ConfigHashFromTree(h.HashBytes(b.Bytes()))
}

// putString writes an 8-byte big-endian length followed by the raw bytes.
func putString(b *bytes.Buffer, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	b.Write(n[:])
	b.WriteString(s)
}

// putList writes the element count followed by each element, length-prefixed.
func putList(b *bytes.Buffer, items []string) {
	putUint(b, uint64(len(items)))
	for _, it := range items {
		putString(b, it)
	}
}

// putUint writes a fixed 8-byte big-endian number.
func putUint(b *bytes.Buffer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.Write(n[:])
}

// putBool writes a single discriminating byte.
func putBool(b *bytes.Buffer, v bool) {
	if v {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
}
