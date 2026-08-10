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

// BenchmarkMaterializeExistingBlob measures the idempotent path: a blob that is
// already published is read back from disk and verified before it is reused. The
// benchmark above cannot stand in for it — it feeds new content from a bytes.Reader,
// so it never reaches the stored-file read this path performs. Here the source is the
// published blob itself, an actual *os.File, which is what decides how the verifying
// copy dispatches.
func BenchmarkMaterializeExistingBlob(b *testing.B) {
	cases := []struct {
		name string
		size int
	}{
		{"4KiB", 4 << 10},
		{"1MiB", 1 << 20},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			hasher := blake3hash.New()
			store := New(paths.New(b.TempDir()), hasher)
			content := benchContent(c.size)
			h, err := hasher.HashReader(bytes.NewReader(content))
			if err != nil {
				b.Fatalf("hashing fixture: %v", err)
			}
			// Publishing the blob is fixture setup, so it stays outside the timed loop:
			// every measured iteration must take the already-present branch.
			if _, written, err := store.Materialize(h, opener(content)); err != nil || !written {
				b.Fatalf("publishing fixture blob: written=%v err=%v", written, err)
			}
			// An existing blob is verified from the store, never from the source, so
			// reaching this opener would mean the benchmark measures the wrong path.
			noSource := func() (io.ReadCloser, error) {
				b.Fatal("source opened for an already-present blob")
				return nil, nil
			}
			b.ReportAllocs()
			b.SetBytes(int64(c.size))
			for b.Loop() {
				if _, written, err := store.Materialize(h, noSource); err != nil || written {
					b.Fatalf("reusing existing blob: written=%v err=%v", written, err)
				}
			}
		})
	}
}

func benchContent(size int) []byte {
	filler := []byte("the quick brown fox\n")
	return bytes.Repeat(filler, size/len(filler)+1)[:size]
}
