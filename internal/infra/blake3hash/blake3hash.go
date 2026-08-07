// Package blake3hash implements the hashing.Hasher port.
//
// It is the only package that imports a concrete local hashing library: the
// BLAKE3 dependency is confined here, behind the domain Hasher interface, so
// upgrading the implementation never touches the scanner or domain. BLAKE3 is the
// single local content-identity primitive — there is nothing to select, so the
// constructor takes no argument and cannot fail. Files are streamed through
// io.Copy, so a large file is hashed in constant memory rather than being read
// whole.
package blake3hash

import (
	"fmt"
	"hash"
	"io"

	"github.com/zeebo/blake3"

	"awarer/internal/domain/hashing"
)

// Hasher computes content and tree identities. It holds no state: the primitive is
// fixed, so every instance behaves identically and one may be shared freely.
type Hasher struct{}

// New returns a Hasher. It is total: the primitive is fixed, so there is no
// miswired algorithm to reject and no error for a caller to handle.
func New() *Hasher { return &Hasher{} }

// newSum starts a fresh digest over the fixed primitive.
func (h *Hasher) newSum() hash.Hash { return blake3.New() }

// HashReader streams r into the digest and returns the content hash. It does
// not close r. A read failure is returned to the caller rather than yielding a
// partial digest.
func (h *Hasher) HashReader(r io.Reader) (hashing.ContentHash, error) {
	sum := h.newSum()
	if _, err := io.Copy(sum, r); err != nil {
		return hashing.ContentHash{}, fmt.Errorf("hashing content: %w", err)
	}
	ch, err := hashing.NewContentHash(sum.Sum(nil))
	if err != nil {
		return hashing.ContentHash{}, err
	}
	return ch, nil
}

// NewWriter returns a streaming digest: an io.Writer to feed bytes into and a
// finalize function that returns the content hash of everything written. It lets
// a caller hash and persist content in a single pass — for example tee-ing a file
// through both this writer and a temp blob file — without buffering the whole
// file or re-reading it. The returned writer never fails (an in-memory digest
// has nothing to fail on), and finalize is total for the same reason: a fixed
// primitive cannot produce an impossible digest, so an error there would be a
// programming error and panics rather than being hidden.
func (h *Hasher) NewWriter() (io.Writer, func() hashing.ContentHash) {
	sum := h.newSum()
	finalize := func() hashing.ContentHash {
		ch, err := hashing.NewContentHash(sum.Sum(nil))
		if err != nil {
			panic(fmt.Sprintf("blake3hash: impossible content hash digest: %v", err))
		}
		return ch
	}
	return sum, finalize
}

// NewTreeWriter returns a streaming tree digest: an io.Writer to feed the
// canonical record encoding into and a finalize function that returns the tree
// hash of everything written. It is the streaming counterpart of HashBytes, used
// by the tree reducer to fold a manifest stream into the hash without buffering
// the whole canonical encoding in memory. The writer never fails (an in-memory
// digest has nothing to fail on), and finalize panics rather than hiding a would-be
// programming error, for the same reason HashBytes does.
func (h *Hasher) NewTreeWriter() (io.Writer, func() hashing.TreeHash) {
	sum := h.newSum()
	finalize := func() hashing.TreeHash {
		th, err := hashing.NewTreeHash(sum.Sum(nil))
		if err != nil {
			panic(fmt.Sprintf("blake3hash: impossible tree hash digest: %v", err))
		}
		return th
	}
	return sum, finalize
}

// HashBytes digests an in-memory buffer into a tree hash. The digest length is
// fixed, so NewTreeHash cannot fail here; a construction error would mean the
// primitive produced an impossible digest, which we surface as a panic rather
// than hiding.
func (h *Hasher) HashBytes(b []byte) hashing.TreeHash {
	sum := h.newSum()
	// hash.Hash.Write never returns an error (documented invariant).
	_, _ = sum.Write(b)
	th, err := hashing.NewTreeHash(sum.Sum(nil))
	if err != nil {
		panic(fmt.Sprintf("blake3hash: impossible tree hash digest: %v", err))
	}
	return th
}
