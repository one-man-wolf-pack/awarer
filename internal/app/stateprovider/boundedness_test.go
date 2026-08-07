package stateprovider

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// genCursor generates n entries on the fly with ascending paths and index-derived
// content hashes, holding exactly one record at a time — an O(1) fixture, so a memory
// measurement isolates the merge and accumulator rather than a backing slice. The side
// byte perturbs every content hash, so a left/right pair of the same n yields all
// modifications.
type genCursor struct {
	n    int
	i    int
	side byte
	cur  worktree.ManifestRecord
	err  error
}

func (g *genCursor) Next() bool {
	if g.err != nil || g.i >= g.n {
		return false
	}
	path, err := worktree.ParseRelPath(fmt.Sprintf("f%09d.txt", g.i))
	if err != nil {
		g.err = err
		return false
	}
	// A distinct 32-byte digest per (side, index): 64 hex chars, valid lowercase hex.
	hex := fmt.Sprintf("%02x%062x", g.side, uint64(g.i))
	ch, err := hashing.ParseContentHash("blake3:" + hex)
	if err != nil {
		g.err = err
		return false
	}
	omitted := worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)
	e, err := worktree.NewRegularEntry(path, ch, worktree.StorageBlob,
		worktree.StatSignature{Size: 1, MtimeNs: 1, Mode: 0o644, Omitted: omitted}, worktree.TraversalInfo{})
	if err != nil {
		g.err = err
		return false
	}
	g.cur = worktree.EntryRecord(e)
	g.i++
	return true
}

func (g *genCursor) Record() worktree.ManifestRecord { return g.cur }
func (g *genCursor) Err() error                      { return g.err }
func (g *genCursor) Close() error                    { return nil }

// drainModified streams a left/right pair of n all-modified entries through the same
// canonical merge and bounded Summary accumulator mergeCounts uses, returning the
// modified count. It retains only the fixed-size Summary regardless of n.
func drainModified(t testing.TB, n int) int {
	left := &genCursor{n: n, side: 0}
	right := &genCursor{n: n, side: 1}
	cur, err := compare.CompareStream(left, right, compare.Options{DetectRenames: false})
	if err != nil {
		t.Fatalf("CompareStream: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var sum compare.Summary
	for cur.Next() {
		sum.Add(cur.Change())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if sum.Added != 0 || sum.Deleted != 0 || sum.TypeChanged != 0 {
		t.Fatalf("expected only modifications, got %+v", sum)
	}
	return sum.Modified
}

// TestMergeCountsUsesStreamingMerge is the structural boundedness guarantee: the compare
// path must obtain counts from the canonical streaming merge (CompareStream) and never
// from CompareToChangeSet, the named adapter that buffers the whole change set. This is
// the invariant a runtime memory sample after the drain cannot catch — a buffered slice
// would be counted and then collected — so it is pinned here at the source.
func TestMergeCountsUsesStreamingMerge(t *testing.T) {
	src, err := os.ReadFile("compare.go")
	if err != nil {
		t.Fatalf("reading compare.go: %v", err)
	}
	s := string(src)
	if !strings.Contains(s, "compare.CompareStream(") {
		t.Error("mergeCounts must drive compare.CompareStream")
	}
	if strings.Contains(s, "CompareToChangeSet") {
		t.Error("compare path must not reference CompareToChangeSet (it materializes the whole change set)")
	}
}

// TestCompareDrainScales proves the streaming drain produces correct counts at a tree
// cardinality far larger than any per-path buffer would tolerate, using the O(1)
// generator fixture so success depends only on the merge and accumulator being bounded.
func TestCompareDrainScales(t *testing.T) {
	for _, n := range []int{1000, 200000} {
		if got := drainModified(t, n); got != n {
			t.Errorf("drainModified(%d) = %d, want %d", n, got, n)
		}
	}
}

// retainedLiveBytes drains n differing paths through the same streaming merge and bounded
// Summary accumulator mergeCounts uses, then returns the post-drain live-heap delta while
// the Summary is still referenced. Only the fixed-size Summary survives the drain, so the
// figure is the *retained* cost of the merge — flat in n. An accumulator that keeps a
// per-path slice live past the drain grows this with n.
func retainedLiveBytes(t testing.TB, n int) int64 {
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	left := &genCursor{n: n, side: 0}
	right := &genCursor{n: n, side: 1}
	cur, err := compare.CompareStream(left, right, compare.Options{DetectRenames: false})
	if err != nil {
		t.Fatalf("CompareStream: %v", err)
	}
	var sum compare.Summary
	for cur.Next() {
		sum.Add(cur.Change())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	_ = cur.Close()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// The Summary must be live at the measurement so its retained cost is counted; the
	// cursors and merge are all O(1) and may be collected.
	runtime.KeepAlive(&sum)
	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

// TestCompareDrainRetainsBoundedMemory is the *retained* boundedness guarantee that the
// completion test (TestCompareDrainScales) cannot give: after draining n differing paths
// into the Summary accumulator, the live heap must be flat in n, because only the
// fixed-size Summary survives the drain. It is measured as the post-drain live-heap delta
// at two cardinalities 100x apart; an accumulator that retains a per-path slice grows the
// delta with n and fails. This is the runtime complement to the structural
// TestMergeCountsUsesStreamingMerge, which pins the source but cannot observe retention.
//
// Mutation proof: add `var acc []compare.Change` to retainedLiveBytes,
// `acc = append(acc, cur.Change())` in the loop, and `runtime.KeepAlive(&acc)` before the
// return — the large-cardinality delta jumps by tens of MiB and this test goes red.
func TestCompareDrainRetainsBoundedMemory(t *testing.T) {
	// O(1) retention sits in KiB; a retained 200k-path slice is tens of MiB, so the 100x
	// cardinality gap separates streaming from accumulating with orders of magnitude to
	// spare, and this band never trips on GC noise.
	const band = 2 << 20 // 2 MiB
	small := retainedLiveBytes(t, 2000)
	large := retainedLiveBytes(t, 200000)
	if large-small > band {
		t.Errorf("retained live heap grew by %d bytes across a 100x cardinality increase (band %d): the drain is accumulating, not streaming", large-small, band)
	}
}

func BenchmarkCompareDrain(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := drainModified(b, 10000); got != 10000 {
			b.Fatalf("got %d", got)
		}
	}
}
