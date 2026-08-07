package blobstore

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/blake3hash"
)

func benchMaterialize(b *testing.B, size int) {
	hasher := blake3hash.New()
	store := New(paths.New(b.TempDir()), hasher)

	base := make([]byte, size)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Vary the content per iteration so each write stores a new blob rather
		// than hitting the idempotent skip.
		content := make([]byte, size)
		copy(content, base)
		binary.LittleEndian.PutUint64(content, uint64(i))
		h, err := hasher.HashReader(bytes.NewReader(content))
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := store.Materialize(h, func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(content)), nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaterialize1KiB(b *testing.B) { benchMaterialize(b, 1<<10) }
func BenchmarkMaterialize1MiB(b *testing.B) { benchMaterialize(b, 1<<20) }
