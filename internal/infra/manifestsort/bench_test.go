package manifestsort

import (
	"context"
	"fmt"
	"runtime"
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

func benchCtx() context.Context { return context.Background() }

// genRecord builds one directory record at index i with a nested path in an order
// that is not canonical (the caller feeds indices descending), so the sort has real
// work to do. Records are generated on the fly and never retained by the caller, so a
// memory measurement reflects the sorter's own footprint, not an input slice.
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

// sortN feeds n generated records through a Sorter with the given buffer cap and
// returns the sorter's retained heap after Finish, measured with the input garbage
// collected — so it reflects the sorter's own footprint (buffer + run list + merge
// front), not the records fed in. The result stream is drained and closed.
func sortN(tb testing.TB, n, bufferMax int) uint64 {
	tb.Helper()
	h := benchHasher(tb)

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	s := New(bufferMax, "")
	for i := n - 1; i >= 0; i-- {
		if err := s.Add(genRecord(tb, i)); err != nil {
			tb.Fatal(err)
		}
	}
	sorted, err := s.Finish(h)
	if err != nil {
		tb.Fatal(err)
	}
	// Collect the generation garbage so the reading below reflects retained state, not
	// the transient records fed in.
	runtime.GC()
	var peak runtime.MemStats
	runtime.ReadMemStats(&peak)

	cur, err := sorted.Stream().Open(benchCtx())
	if err != nil {
		tb.Fatal(err)
	}
	count := 0
	for cur.Next() {
		count++
	}
	if err := cur.Err(); err != nil {
		tb.Fatal(err)
	}
	_ = cur.Close()
	_ = sorted.Close()
	if count != n {
		tb.Fatalf("drained %d records, want %d", count, n)
	}
	if peak.HeapAlloc < before.HeapAlloc {
		return 0
	}
	return peak.HeapAlloc - before.HeapAlloc
}

// TestSpillMemoryStaysBoundedAcrossSizes is the acceptance evidence for the scanner
// bound: with a fixed small buffer, retained memory must NOT grow proportionally with
// the record count — a spilling sort of 4x the records must not retain ~4x the memory.
// It is a coarse guard (heap sampling is noisy), so it only asserts the spilling path's
// peak stays within a generous constant factor as the input grows an order of magnitude,
// which a regression back to full in-memory accumulation would blow past.
func TestSpillMemoryStaysBoundedAcrossSizes(t *testing.T) {
	if testing.Short() {
		t.Skip("memory-shape test skipped in -short")
	}
	const buffer = 2000
	small := sortN(t, 20_000, buffer)
	large := sortN(t, 200_000, buffer)
	t.Logf("spill-bounded retained heap: 20k=%d bytes, 200k=%d bytes (buffer=%d)", small, large, buffer)

	// Full in-memory accumulation would make 200k retain ~10x what 20k does. With a
	// fixed buffer the peak is dominated by the buffer, so the ratio must stay well under
	// that. Guard generously (heap sampling is noisy) but tight enough to catch a
	// regression to O(records).
	if small == 0 {
		return // sampling produced no signal; do not fail on noise
	}
	ratio := float64(large) / float64(small)
	if ratio > 4.0 {
		t.Fatalf("retained heap grew %.1fx from 20k to 200k records with a fixed buffer; want it bounded (< 4x)", ratio)
	}
}

func benchSort(b *testing.B, n, bufferMax int) {
	h := benchHasher(b)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := New(bufferMax, "")
		for j := n - 1; j >= 0; j-- {
			if err := s.Add(genRecord(b, j)); err != nil {
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
