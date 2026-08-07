// Package blob is the pure domain model for content-addressed blob storage.
//
// It owns the identity of a stored blob (BlobRef), the deterministic on-disk
// layout that addresses it (BlobPath), and the Store port the application uses
// to materialize and read blobs. It performs no I/O: the concrete filesystem
// store lives behind the Store port in infrastructure, so the application
// depends on this interface rather than on os/filepath details.
package blob

import (
	"errors"
	"fmt"
	"io"

	"awarer/internal/domain/hashing"
)

// ErrHashMismatch reports that bytes materialized for a blob did not hash back to
// the expected content hash. It means the source file changed between the scan
// and the blob write, so the caller must abort rather than publish a manifest
// that references bytes that were never stored.
var ErrHashMismatch = errors.New("blob content hash mismatch")

// ErrCorruptBlob reports that a blob already present in the store does not hash to
// the address it is stored under — the stored bytes are corrupt or truncated.
// Unlike ErrHashMismatch (a moving source file), this is on-disk corruption of
// the store itself, so the caller surfaces it as storage corruption.
var ErrCorruptBlob = errors.New("stored blob is corrupt")

// BlobRef is the durable identity of a stored blob: the content hash that
// addresses it. A manifest entry references content through a BlobRef, never a
// raw path, so the store remains free to change its on-disk layout.
type BlobRef struct {
	hash hashing.ContentHash
}

// NewBlobRef wraps a non-zero content hash. A zero hash is not a blob identity —
// it means "no content recorded" — so it is rejected rather than producing a ref
// that addresses nothing.
func NewBlobRef(h hashing.ContentHash) (BlobRef, error) {
	if h.IsZero() {
		return BlobRef{}, fmt.Errorf("blob ref requires a content hash")
	}
	return BlobRef{hash: h}, nil
}

// Hash returns the content hash that addresses the blob.
func (r BlobRef) Hash() hashing.ContentHash { return r.hash }

// IsZero reports whether the ref is unset.
func (r BlobRef) IsZero() bool { return r.hash.IsZero() }

// BlobPath is the store-relative location of a blob, computed deterministically
// from its content hash. It is "blake3/<ab>/<cd>/<hex>": the fixed identity
// namespace is a path segment so a stored blob names the primitive that addresses
// it and a future identity change cannot land in the same directory tree, and the
// two leading byte-pairs shard the space so no single directory holds every blob.
// The namespace is a constant from the hashing domain, never a per-digest value:
// a digest does not get to choose its own path segment.
type BlobPath struct {
	rel string
}

// shardLen is the number of leading hex characters used per shard directory.
const shardLen = 2

// PathFor computes the store-relative path for a content hash. It rejects the
// zero hash and any hash whose hex payload is too short to shard, so a malformed
// identity can never produce a path that aliases the shard directories.
func PathFor(h hashing.ContentHash) (BlobPath, error) {
	if h.IsZero() {
		return BlobPath{}, fmt.Errorf("blob path requires a content hash")
	}
	hex := h.Hex()
	if len(hex) < 2*shardLen {
		return BlobPath{}, fmt.Errorf("blob path: hex %q too short to shard", hex)
	}
	rel := hashing.Namespace + "/" + hex[:shardLen] + "/" + hex[shardLen:2*shardLen] + "/" + hex
	return BlobPath{rel: rel}, nil
}

// Rel returns the store-relative path using forward slashes. The infrastructure
// store joins it onto the blobs directory with the OS separator.
func (p BlobPath) Rel() string { return p.rel }

// Store is the port the application uses to persist and read blob content. It is
// defined here, on the consumer side, and implemented by a filesystem adapter so
// no os/filepath details enter the application service.
type Store interface {
	// Has reports whether the blob for h already exists in the store.
	Has(h hashing.ContentHash) (bool, error)
	// Materialize ensures the blob for expected exists. It streams bytes from
	// open() through the hasher and a temp file, verifies the computed hash equals
	// expected before publishing, and is idempotent: an already-present blob is a
	// no-op. written reports whether new bytes were written (false when the blob
	// already existed). It returns ErrHashMismatch when freshly read bytes do not
	// hash to expected (the source moved), and ErrCorruptBlob when an already-stored
	// blob does not hash to its address (store corruption). In both cases it leaves
	// no new blob behind, so a checkpoint never references bytes that were not stored.
	Materialize(expected hashing.ContentHash, open func() (io.ReadCloser, error)) (ref BlobRef, written bool, err error)
	// Open returns a reader for an existing blob's content. The caller closes it.
	Open(h hashing.ContentHash) (io.ReadCloser, error)
}
