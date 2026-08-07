package worktree_test

import (
	"fmt"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// BenchmarkReduceCursorManyRecords folds 50k records through the stream-first
// reducer, the scale path checkpoint reads and tree hashing share. It guards against
// accidental quadratic behavior in the merge/reduce boundary.
func BenchmarkReduceCursorManyRecords(b *testing.B) {
	h := fakeHasher{}
	entries := make([]worktree.Entry, 0, 50000)
	ch, err := hashing.ParseContentHash("blake3:" + hexA)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 50000; i++ {
		path, err := worktree.ParseRelPath(fmt.Sprintf("pkg/dir%04d/file%06d.go", i%500, i))
		if err != nil {
			b.Fatal(err)
		}
		entries = append(entries, worktree.Entry{
			Path: path, Kind: worktree.KindRegular, Content: ch, Storage: worktree.StorageBlob,
		})
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := worktree.ReduceCursor(h, scantest.CanonicalCursor(entries, nil)); err != nil {
			b.Fatal(err)
		}
	}
}
