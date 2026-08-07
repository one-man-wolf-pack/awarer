package manifestsort

import (
	"fmt"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

func benchHasher(tb testing.TB) hashing.Hasher {
	tb.Helper()
	h := blake3hash.New()
	return h
}

// genRecord builds one directory record at index i with a nested path. Callers feed
// the indices descending, so the input order is not canonical and the sort has real
// work to do.
func genRecord(tb testing.TB, i int) worktree.ManifestRecord {
	p, err := worktree.ParseRelPath(fmt.Sprintf("d%04d/s%02d/f%08d", i%1000, i%40, i))
	if err != nil {
		tb.Fatal(err)
	}
	e, err := worktree.NewDirEntry(p, worktree.TraversalInfo{})
	if err != nil {
		tb.Fatal(err)
	}
	return worktree.EntryRecord(e)
}

// benchSort times one accumulate-sort-finish cycle over n records with the given
// buffer cap. The records are built before the loop so the measurement reflects the
// sorter rather than path parsing; a ManifestRecord is an immutable value, so the
// same input is safe to replay across iterations. It is a manual diagnostic for the
// scan hot path, not an acceptance gate: it asserts no time, allocation, or memory
// budget.
func benchSort(b *testing.B, n, bufferMax int) {
	h := benchHasher(b)
	records := make([]worktree.ManifestRecord, n)
	for i := range records {
		records[i] = genRecord(b, n-1-i)
	}
	b.ReportAllocs()
	for b.Loop() {
		s := New(bufferMax, "")
		for _, rec := range records {
			if err := s.Add(rec); err != nil {
				b.Fatal(err)
			}
		}
		sorted, err := s.Finish(h)
		if err != nil {
			b.Fatal(err)
		}
		_ = sorted.Close()
	}
}

func BenchmarkSortInMemory(b *testing.B) { benchSort(b, 50_000, 0) } // no spill

func BenchmarkSortSpilling(b *testing.B) { benchSort(b, 50_000, 2000) } // many spilled runs
