package worktree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"awarer/internal/domain/hashing"
)

// treeDigestHasher is the optional capability a Hasher may offer to digest the
// canonical tree encoding as a stream rather than from a single buffer. When a
// hasher implements it, the reducer folds records straight into the digest in
// constant memory; otherwise it falls back to buffering the encoding and calling
// HashBytes once. The fallback keeps simple test hashers working without forcing
// the streaming method onto the Hasher port.
type treeDigestHasher interface {
	NewTreeWriter() (io.Writer, func() hashing.TreeHash)
}

// treeHashVersion prefixes the canonical encoding so the format can evolve
// without silently colliding with old hashes. v2 dropped the content-storage
// token: the tree hash answers "is the observed worktree the same?", which does
// not depend on whether a regular file's bytes are persisted as a blob or
// hash-only — that is a checkpoint storage policy, hashed separately. v3 added
// the traversal provenance (whether a node was reached by following symlinks, and
// through which source at what depth): a direct file and a file reached through
// follow_symlinks describe different worktree semantics even when path, content,
// and mode match, so they must not hash identically. v4 dropped the algorithm
// field: the local hash primitive is fixed and the rendered digest already carries
// the blake3: marker, so repeating it inside the encoding was redundant state.
const treeHashVersion byte = 0x04

// lenPrefixSize is the width, in bytes, of the big-endian length that prefixes every
// field and every record in the canonical encoding. Length-prefixing is what makes
// the encoding injective, so this width is part of the wire format and is fixed by
// treeHashVersion.
const lenPrefixSize = 8

// ReducedStats are the per-kind counts and total size folded from a manifest
// record stream. They are the source the checkpoint layer maps into its own
// CheckpointStats, so the counts are derived from the same record sequence as the
// tree hash and cannot drift from it. Blobs and HashOnly are counted from each
// regular entry's storage intent, which by checkpoint time is the final storage
// decision.
type ReducedStats struct {
	Files      int
	Dirs       int
	Symlinks   int
	Blobs      int
	HashOnly   int
	Skipped    int
	TotalBytes int64
}

// TreeReduction is the result of folding a canonical manifest stream: the
// deterministic tree hash plus the stats, taint flag, and record count derived
// from the same sequence. Holding them together keeps a caller from re-deriving
// any of them from a second pass that could disagree.
type TreeReduction struct {
	Hash    hashing.TreeHash
	Stats   ReducedStats
	Tainted bool
	Count   int
}

// TreeReducer folds canonical manifest records into a TreeReduction in one pass.
// It is the single source of truth for the tree hash, the per-kind stats, and the
// taint flag: every one of those facts comes from the same record sequence, so none
// can drift from another. Build one with NewTreeReducer and finish with Finish.
//
// Add records in ascending RelPath order (the ManifestCursor contract guarantees
// this). Add re-guards order and duplicate paths on its own — it is a live
// incremental boundary that persistence and verifying cursors feed record by
// record, so a caller violating the cursor contract must fail here rather than get
// a valid-looking hash for a noncanonical sequence. That the current source cursors
// are also Ordered does not make the guard redundant.
type TreeReducer struct {
	hasher   hashing.Hasher
	w        io.Writer
	finalize func() hashing.TreeHash
	have     bool
	prev     RelPath
	stats    ReducedStats
	tainted  bool
	count    int
	// scratch is the per-record encoding buffer, reused across Add calls so a large
	// manifest folds in constant per-record allocation rather than allocating a fresh
	// buffer (and growing it) for every record.
	scratch []byte
}

// NewTreeReducer starts a reducer that hashes with h, validating each entry as it
// is folded in. It folds the canonical encoding straight into a streaming digest
// when h offers one (constant memory), and otherwise buffers it and calls HashBytes
// at the end. Both paths write the version prefix up front and then the same
// per-record encoding, so a streaming and a buffering hasher agree byte for byte.
func NewTreeReducer(h hashing.Hasher) (*TreeReducer, error) {
	if h == nil {
		return nil, fmt.Errorf("tree: hasher must not be nil")
	}
	r := &TreeReducer{hasher: h}
	if tw, ok := h.(treeDigestHasher); ok {
		r.w, r.finalize = tw.NewTreeWriter()
	} else {
		buf := &bytes.Buffer{}
		r.w = buf
		r.finalize = func() hashing.TreeHash { return h.HashBytes(buf.Bytes()) }
	}
	// The writer is always an in-memory digest or buffer, neither of which can fail,
	// so this one-time version-byte write never errors.
	_, _ = r.w.Write([]byte{treeHashVersion})
	return r, nil
}

// Add folds one record into the reduction. It validates the wrapped entry or skip
// (re-checking the same invariants the constructors enforce, as a persistence-
// boundary guard), rejects a record whose path is out of order or duplicates its
// predecessor, and writes the record's length-prefixed canonical encoding to the
// digest.
func (r *TreeReducer) Add(rec ManifestRecord) error {
	path := rec.Path()
	if path.IsZero() {
		return fmt.Errorf("tree: manifest record has no path")
	}
	if r.have {
		if path.Equal(r.prev) {
			return fmt.Errorf("duplicate path in tree: %q", path)
		}
		if path.Less(r.prev) {
			return fmt.Errorf("tree: record %q is out of canonical order after %q", path, r.prev)
		}
	}

	// Each record is itself a length-prefixed field in the canonical stream: reserve a
	// zeroed length placeholder, append the body, fill the prefix, then write once. The
	// scratch buffer is reused across the whole manifest instead of allocated per
	// record, and the encoding is byte-identical to wrapping each record with writeField.
	var zero [lenPrefixSize]byte
	r.scratch = append(r.scratch[:0], zero[:]...)
	switch {
	case !rec.IsSkipped():
		e, _ := rec.Entry()
		if err := e.Validate(); err != nil {
			return err
		}
		r.scratch = encodeEntry(r.scratch, e)
		switch e.Kind {
		case KindRegular:
			r.stats.Files++
			r.stats.TotalBytes += e.Stat.Size
			switch e.Storage {
			case StorageBlob:
				r.stats.Blobs++
			case StorageHashOnly:
				r.stats.HashOnly++
			}
		case KindDir:
			r.stats.Dirs++
		case KindSymlink:
			r.stats.Symlinks++
		}
	default:
		s, _ := rec.Skipped()
		if err := s.Validate(); err != nil {
			return err
		}
		r.scratch = encodeSkipped(r.scratch, s)
		r.stats.Skipped++
		if s.Reason.TaintsScan() {
			r.tainted = true
		}
	}
	binary.BigEndian.PutUint64(r.scratch[:lenPrefixSize], uint64(len(r.scratch)-lenPrefixSize))
	_, _ = r.w.Write(r.scratch)

	r.have = true
	r.prev = path
	r.count++
	return nil
}

// Finish finalizes the digest and returns the reduction. The reducer must not be
// used after Finish.
func (r *TreeReducer) Finish() TreeReduction {
	return TreeReduction{
		Hash:    r.finalize(),
		Stats:   r.stats,
		Tainted: r.tainted,
		Count:   r.count,
	}
}

// ReduceCursor drives a manifest cursor through a reducer and returns the
// reduction. It always closes the cursor (including when the reducer cannot be
// built), and a nil cursor is a wiring error reported loudly rather than a panic.
// An error from Add (invalid record, out-of-order, duplicate path) or from the
// cursor itself aborts and is returned.
func ReduceCursor(h hashing.Hasher, cur ManifestCursor) (TreeReduction, error) {
	if cur == nil {
		return TreeReduction{}, fmt.Errorf("tree: nil manifest cursor")
	}
	defer func() { _ = cur.Close() }()
	r, err := NewTreeReducer(h)
	if err != nil {
		return TreeReduction{}, err
	}
	for cur.Next() {
		if err := r.Add(cur.Record()); err != nil {
			return TreeReduction{}, err
		}
	}
	if err := cur.Err(); err != nil {
		return TreeReduction{}, err
	}
	return r.Finish(), nil
}

// encodeEntry appends an entry's canonical encoding to buf and returns the extended
// slice. The fields contributed per kind are exactly those that constitute observed
// identity, plus the traversal
// provenance (how the node was reached). Two things are deliberately excluded:
// volatile stat fields (mtime/ctime/dev/ino/nlink), so the same content hashes
// identically across machines; and the content-storage class, so that choosing to
// persist a regular file as a blob versus hash-only does not change the worktree's
// identity (that is a checkpoint storage policy). Entry.Validate has already
// guaranteed the kind is regular, dir, or symlink.
func encodeEntry(buf []byte, e Entry) []byte {
	buf = writeField(buf, e.Path.String())
	switch e.Kind {
	case KindRegular:
		buf = writeField(buf, "file")
		buf = writeField(buf, e.Content.String())
		// Only the permission bits contribute to identity; the type is already
		// encoded by the record token and the high bits are not portable across
		// platforms.
		buf = writeUint(buf, uint64(e.Stat.Mode&0o777))
	case KindDir:
		buf = writeField(buf, "dir")
	case KindSymlink:
		// A symlink's identity is its target; the kind token already says it is a
		// symlink, so no storage class is encoded.
		buf = writeField(buf, "symlink")
		buf = writeField(buf, e.Symlink.String())
	default:
		panic(fmt.Sprintf("worktree: unvalidated entry kind %v for %q", e.Kind, e.Path))
	}
	return writeTraversal(buf, e.Traversal)
}

// encodeSkipped appends a skipped input's canonical encoding to buf and returns the
// extended slice. The symlink target is always included (empty for non-symlinks), so
// re-pointing a skipped symlink
// changes the tree hash even when the skip reason stays the same. The traversal
// provenance is included too, so a skip reached by following symlinks is distinct
// from an otherwise-identical direct skip.
//
// A skipped input has no content hash, so — unlike a regular entry, whose identity
// is its content and which deliberately excludes volatile stat — the
// change-signaling stat subset (size, mtime, mode) is part of its identity. If a
// skipped file changes (its mtime moves, its mode flips) the tree hash changes,
// which matters for the run cache. The non-portable inode fields (dev/ino/nlink)
// are still excluded so the same skipped file hashes identically across machines.
func encodeSkipped(buf []byte, s SkippedInput) []byte {
	buf = writeField(buf, s.Path.String())
	buf = writeField(buf, "skipped")
	buf = writeField(buf, s.Kind.String())
	buf = writeField(buf, s.Reason.String())
	buf = writeUint(buf, uint64(s.Size))
	buf = writeField(buf, s.Symlink.String())
	buf = writeUint(buf, uint64(s.Stat.MtimeNs))
	// Only the permission bits, as for a regular entry: the kind is already
	// encoded, and the raw st_mode's type/high bits are platform-specific and
	// would break cross-machine determinism.
	buf = writeUint(buf, uint64(s.Stat.Mode&0o777))
	return writeTraversal(buf, s.Traversal)
}

// writeTraversal appends the encoding of how a node was reached to buf: a followed
// flag, the source symlink path (empty when not followed), and the hop depth. A
// fixed three-field layout keeps the encoding injective, and the flag makes a direct
// node (not followed) distinct from any followed node.
func writeTraversal(buf []byte, t TraversalInfo) []byte {
	var followed uint64
	if t.Followed {
		followed = 1
	}
	buf = writeUint(buf, followed)
	buf = writeField(buf, t.SourcePath.String())
	return writeUint(buf, uint64(t.Depth))
}

// writeField appends a length-prefixed field to buf: an 8-byte big-endian length
// followed by the raw bytes of s. Length-prefixing makes the encoding injective, so
// a path or target containing a separator byte can never collide with a different
// field layout. It takes a string so callers can pass a value's String() form
// directly, avoiding a []byte(s) conversion before the bytes are copied into buf.
func writeField(buf []byte, s string) []byte {
	var n [lenPrefixSize]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	buf = append(buf, n[:]...)
	return append(buf, s...)
}

// writeUint appends a fixed 8-byte big-endian number to buf.
func writeUint(buf []byte, v uint64) []byte {
	var n [lenPrefixSize]byte
	binary.BigEndian.PutUint64(n[:], v)
	return append(buf, n[:]...)
}
