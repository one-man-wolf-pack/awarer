package blake3hash

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkHashFile measures the path the scanner actually takes: an *os.File
// streamed through io.Copy. The in-memory benchmark below cannot stand in for it —
// a bytes.Reader hands its whole slice to the digest through WriteTo and never
// reaches a copy buffer, so it cannot show the per-file scratch cost at all.
func BenchmarkHashFile(b *testing.B) {
	cases := []struct {
		name string
		size int
	}{
		{"tiny-256B", 256},
		{"small-8KiB", 8 << 10},
		{"large-20MiB", 20 << 20},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			h := New()
			f := benchFile(b, c.size)
			b.ReportAllocs()
			b.SetBytes(int64(c.size))
			for b.Loop() {
				// Rewinding is fixture bookkeeping, not part of hashing; it costs one
				// lseek and no allocation.
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					b.Fatalf("rewind fixture: %v", err)
				}
				if _, err := h.HashReader(f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkHashReaderInMemory keeps the in-memory source visible. It takes io.Copy's
// source-WriteTo branch, so it must stay free of copy-buffer cost no matter what
// the writer side does.
func BenchmarkHashReaderInMemory(b *testing.B) {
	cases := []struct {
		name string
		size int
	}{
		{"tiny-256B", 256},
		{"large-20MiB", 20 << 20},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			h := New()
			data := benchPayload(c.size)
			r := bytes.NewReader(data)
			b.ReportAllocs()
			b.SetBytes(int64(c.size))
			for b.Loop() {
				r.Reset(data)
				if _, err := h.HashReader(r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchFile writes size bytes into a test-owned temporary file and returns it open
// at offset zero. Creating and opening the fixture stays outside every timed loop so
// its allocation is not amortized into the per-hash numbers.
func benchFile(b *testing.B, size int) *os.File {
	b.Helper()
	path := filepath.Join(b.TempDir(), "payload")
	if err := os.WriteFile(path, benchPayload(size), 0o600); err != nil {
		b.Fatalf("write fixture: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		b.Fatalf("open fixture: %v", err)
	}
	b.Cleanup(func() { _ = f.Close() })
	return f
}

func benchPayload(size int) []byte {
	filler := []byte("the quick brown fox\n")
	return bytes.Repeat(filler, size/len(filler)+1)[:size]
}
