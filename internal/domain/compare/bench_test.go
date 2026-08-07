package compare_test

import (
	"fmt"
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// benchEntries builds n regular entries; every k-th entry gets a distinct hash so
// the right side differs from the left in a realistic fraction of paths. The entries
// are built once and turned into a fresh canonical cursor per iteration, so the
// measurement covers the comparison rather than fixture construction.
func benchEntries(b *testing.B, n int, distinctHex string) []worktree.Entry {
	b.Helper()
	entries := make([]worktree.Entry, 0, n)
	base, err := hashing.ParseContentHash("blake3:" + hexA)
	if err != nil {
		b.Fatal(err)
	}
	alt, err := hashing.ParseContentHash("blake3:" + distinctHex)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		p, err := worktree.ParseRelPath(fmt.Sprintf("pkg/dir%03d/file%05d.go", i%200, i))
		if err != nil {
			b.Fatal(err)
		}
		h := base
		if i%10 == 0 {
			h = alt
		}
		e, err := worktree.NewRegularEntry(p, h, worktree.StorageBlob, worktree.StatSignature{Size: 10, Mode: 0o644, MtimeNs: 1}, worktree.TraversalInfo{})
		if err != nil {
			b.Fatal(err)
		}
		entries = append(entries, e)
	}
	return entries
}

// benchCompare drains one comparison of the two entry sets through the production
// streaming path without accumulating the changes.
func benchCompare(b *testing.B, left, right []worktree.Entry, opts compare.Options) {
	b.Helper()
	cur, err := compare.CompareStream(scantest.CanonicalCursor(left, nil), scantest.CanonicalCursor(right, nil), opts)
	if err != nil {
		b.Fatal(err)
	}
	for cur.Next() {
		_ = cur.Change()
	}
	if err := cur.Err(); err != nil {
		b.Fatal(err)
	}
	if err := cur.Close(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkCompareManyEntries(b *testing.B) {
	left := benchEntries(b, 5000, hexA)
	right := benchEntries(b, 5000, hexB)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchCompare(b, left, right, compare.Options{})
	}
}

func BenchmarkCompareWithRenames(b *testing.B) {
	left := benchEntries(b, 5000, hexA)
	right := benchEntries(b, 5000, hexB)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchCompare(b, left, right, compare.Options{DetectRenames: true})
	}
}

func BenchmarkCompareWithPathFilter(b *testing.B) {
	left := benchEntries(b, 5000, hexA)
	right := benchEntries(b, 5000, hexB)
	filter, err := worktree.ParseRelPath("pkg/dir001")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchCompare(b, left, right, compare.Options{PathFilters: []worktree.RelPath{filter}})
	}
}

// BenchmarkCompareStreamLarge merges two 50k-record manifest streams and drains the
// change cursor without accumulating the results, exercising the map-free
// sorted-merge path that does not hold the full change set in memory — the real
// streaming contract changes/diff rely on.
func BenchmarkCompareStreamLarge(b *testing.B) {
	left := benchEntries(b, 50000, hexA)
	right := benchEntries(b, 50000, hexB)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchCompare(b, left, right, compare.Options{})
	}
}
