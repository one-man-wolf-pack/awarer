package blake3hash

import (
	"bytes"
	"testing"
)

func BenchmarkHashSmallFile(b *testing.B) {
	h := New()
	data := bytes.Repeat([]byte("x"), 256)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := h.HashReader(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashLargeFileStreaming(b *testing.B) {
	h := New()
	data := bytes.Repeat([]byte("the quick brown fox\n"), 1<<20) // ~20 MiB
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := h.HashReader(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
