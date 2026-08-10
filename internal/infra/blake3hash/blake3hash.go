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
	"io"
	"sync"

	"github.com/zeebo/blake3"

	"awarer/internal/domain/hashing"
)

// Hasher computes content and tree identities. It holds no state: the primitive is
// fixed, so every instance behaves identically and one may be shared freely.
type Hasher struct{}

// New returns a Hasher. It is total: the primitive is fixed, so there is no
// miswired algorithm to reject and no error for a caller to handle.
func New() *Hasher { return &Hasher{} }

// newSum starts a fresh digest over the fixed primitive. The return type is the
// concrete one this package already owns rather than hash.Hash, so that the copy
// adapter below stays a single pointer and costs nothing to box as an io.Writer.
// It stays package-private: every exported signature still speaks in standard
// interfaces, so the confined dependency is confined exactly as before.
func (h *Hasher) newSum() *blake3.Hasher { return blake3.New() }

// HashReader streams r into the digest and returns the content hash. It does
// not close r. A read failure is returned to the caller rather than yielding a
// partial digest.
func (h *Hasher) HashReader(r io.Reader) (hashing.ContentHash, error) {
	sum := h.newSum()
	if _, err := io.Copy(digestSink{digest: sum}, r); err != nil {
		return hashing.ContentHash{}, fmt.Errorf("hashing content: %w", err)
	}
	ch, err := hashing.NewContentHash(sum.Sum(nil))
	if err != nil {
		return hashing.ContentHash{}, err
	}
	return ch, nil
}

// copyBufferSize is io.Copy's own default. A larger buffer is not the point — not
// allocating a new one for every file is.
const copyBufferSize = 32 * 1024

// copyBuffers holds cleared copy scratch. What it retains is proportional to how
// many callers hash at once (and to the runtime's internal pool shards), never to
// how many files a scan visits, and the runtime may drop every entry at any
// collection. Nothing here is a cache: a lost buffer costs one allocation.
var copyBuffers = sync.Pool{
	New: func() any {
		buf := make([]byte, copyBufferSize)
		// Pooling the pointer, not the slice, keeps Put from boxing a header per call.
		return &buf
	},
}

// digestSink gives the digest an io.ReaderFrom so io.Copy feeds it through reusable
// scratch instead of a buffer allocated per call.
//
// io.Copy still prefers the source's WriteTo, so an in-memory reader writes its
// whole slice straight into the digest and touches no buffer at all. An *os.File
// has WriteTo too, but its zero-copy send needs a socket destination and an in-memory
// digest is not one, so it falls back to an inner io.Copy over itself with WriteTo
// hidden. That inner copy is the one that allocated 32 KiB per hashed file: it found
// no ReadFrom on the bare digest, and now finds this one.
//
// The single pointer field is deliberate: it makes the sink pointer-shaped, so
// handing it to io.Copy as an io.Writer stores the pointer inline and allocates
// nothing. Wrapping an interface here instead would put a 16-byte box on every
// call, including the in-memory ones this adapter never needs to touch.
type digestSink struct{ digest *blake3.Hasher }

func (s digestSink) Write(p []byte) (int, error) { return s.digest.Write(p) }

func (s digestSink) ReadFrom(r io.Reader) (int64, error) {
	buf := copyBuffers.Get().(*[]byte)
	defer releaseCopyBuffer(buf)
	// The destination is the digest itself, never this wrapper, so this does not
	// re-enter ReadFrom.
	return io.CopyBuffer(s.digest, r, *buf)
}

// releaseCopyBuffer zeroes the whole buffer before it becomes reusable, on success
// and on failure alike, so no file content outlives the call that read it — including
// the bytes past a final short read, which the copy loop never overwrites.
func releaseCopyBuffer(buf *[]byte) {
	clear(*buf)
	copyBuffers.Put(buf)
}

// NewWriter returns a streaming digest: an io.Writer to feed bytes into and a
// finalize function that returns the content hash of everything written. It lets
// a caller hash and persist content in a single pass — for example tee-ing a file
// through both this writer and a temp blob file — without buffering the whole
// file or re-reading it. The returned writer never fails (an in-memory digest
// has nothing to fail on), and finalize is total for the same reason: a fixed
// primitive cannot produce an impossible digest, so an error there would be a
// programming error and panics rather than being hidden.
//
// Unlike HashReader, this writer carries no io.ReaderFrom of its own, so a caller
// that copies into it still allocates its own copy buffer. That is deliberate: the
// single-pass tee above is the shape this method exists for, and an io.MultiWriter
// is not reachable by ReaderFrom dispatch at all, so scratch for such a copy has to
// be owned where the copy is written rather than hidden behind this return value.
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
