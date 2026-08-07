package stateprovider

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// Comparing two states must never turn worktree cardinality into a full-memory collection.
// Three layers own that together, none alone: drainModified proves the merge stays lazy and
// completes with exact counts; TestMergeCountsUsesStreamingMerge proves mergeCounts does not
// materialize what the merge streams, as a claim about source rather than runtime; and the
// real-Assessor tests in assess_integration_test.go prove the on-disk path returns the right
// counts with rename pairing disabled, which is what leaves the merge lazy. No layer reads a
// heap counter, forces a GC, runs a sampler, or compares a byte band.

// genCursor generates n entries on the fly with ascending paths and index-derived content
// hashes, holding exactly one record at a time, so its retained fixture state is O(1) in n.
// The side byte perturbs every content hash, so a left/right pair of the same n yields all
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

// reads reports records successfully handed to the consumer: the index advances only on the
// path that returns true, so an exhausted or failed Next never inflates it.
func (g *genCursor) reads() int { return g.i }

// drainModified streams a left/right pair of n all-modified entries through the canonical
// merge into the same fixed-size Summary mergeCounts uses, and returns that Summary.
//
// While draining it holds the merge to its own shape. Both sides are all-modified pairs of
// the same n, so each successful source read is also exactly one emitted change and the two
// counts compare like with like. One bound states the whole rule: reading more than one record
// beyond the changes emitted is running ahead, and at the first change that permits at most
// two reads, so for any n above two it also proves neither source was consumed up front. The
// whole result is drained, because a comparison could emit one change eagerly and materialize
// everything after it.
func drainModified(t testing.TB, n int) compare.Summary {
	t.Helper()
	const acceptedLookahead = 1 // the merge's entire retained lookahead: one record per side

	left := &genCursor{n: n, side: 0}
	right := &genCursor{n: n, side: 1}
	cur, err := compare.CompareStream(left, right, compare.Options{DetectRenames: false})
	if err != nil {
		t.Fatalf("CompareStream: %v", err)
	}
	defer func() { _ = cur.Close() }()

	var sum compare.Summary
	emitted := 0
	for cur.Next() {
		emitted++
		sum.Add(cur.Change())
		if lead := max(left.reads(), right.reads()) - emitted; lead > acceptedLookahead {
			t.Fatalf("at change %d of %d the sources had read left=%d right=%d records: a lead of %d over the changes emitted, above the merge's %d-record lookahead",
				emitted, n, left.reads(), right.reads(), lead, acceptedLookahead)
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if left.reads() != n || right.reads() != n {
		t.Fatalf("sources ended at left=%d right=%d records, want %d each: the merge must consume both manifests in full",
			left.reads(), right.reads(), n)
	}
	return sum
}

// TestCompareEmitsChangesWhileSourcesAreStillStreaming proves the non-rename comparison is a
// lazy k=2 merge rather than one that consumes a side into memory first, at a cardinality
// where every step is inspectable and one far beyond any constant a lookahead could hide
// behind.
func TestCompareEmitsChangesWhileSourcesAreStillStreaming(t *testing.T) {
	for _, n := range []int{4, 20000} {
		if got, want := drainModified(t, n), (compare.Summary{Modified: n}); got != want {
			t.Errorf("n=%d: summary = %+v, want %+v", n, got, want)
		}
	}
}

// TestCompareDrainScales proves the drain completes with correct counts at a tree cardinality
// far larger than any per-path buffer would tolerate.
func TestCompareDrainScales(t *testing.T) {
	const n = 200000
	if got, want := drainModified(t, n), (compare.Summary{Modified: n}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

// TestMergeCountsUsesStreamingMerge pins the ownership rule no runtime observation can catch:
// a per-change slice that is appended to, counted, and dropped before mergeCounts returns is
// already collectable by the time any measurement could run, yet it is exactly the regression
// this boundary exists to prevent. So it is checked where it is decided, in the source: one
// method body, three facts — it calls compare.CompareStream, it does not reach for the
// materializing CompareToChangeSet adapter, and it builds no collection of its own. Everything
// else belongs to the production code and the behavioral tests above; this cannot prove every
// materialization shape and is not meant to.
//
// Mutation proof: declare `var acc []compare.Change` in mergeCounts, append cur.Change() to it
// inside the drain loop, and drop acc before the return — the append rule goes red even though
// the slice never outlives the call.
func TestMergeCountsUsesStreamingMerge(t *testing.T) {
	qualified := func(e ast.Expr, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == name && identName(sel.X) == "compare"
	}
	var streams, adapters, appends int
	ast.Inspect(mergeCountsBody(t), func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			// Any reference, not only a call: handing the adapter elsewhere materializes
			// the change set just the same.
			if qualified(node, "CompareToChangeSet") {
				adapters++
			}
		case *ast.CallExpr:
			if identName(node.Fun) == "append" {
				appends++
			} else if qualified(node.Fun, "CompareStream") {
				streams++
			}
		}
		return true
	})

	if streams == 0 {
		t.Error("mergeCounts must call compare.CompareStream: the counts come from the canonical streaming merge")
	}
	if adapters != 0 {
		t.Errorf("mergeCounts references compare.CompareToChangeSet %d times: that adapter materializes the whole change set", adapters)
	}
	if appends != 0 {
		t.Errorf("mergeCounts calls append %d times: a per-change collection here is worktree cardinality held in application memory, even if it is dropped before the function returns", appends)
	}
}

// identName returns e's identifier name, or "" when e is not a bare identifier.
func identName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// mergeCountsBody returns the body of the one (*Assessor).mergeCounts declaration in
// compare.go, failing loudly rather than passing vacuously when it is not found.
func mergeCountsBody(t *testing.T) *ast.BlockStmt {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "compare.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing compare.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "mergeCounts" || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok && identName(star.X) == "Assessor" {
			return fn.Body
		}
	}
	t.Fatal("compare.go declares no (*Assessor).mergeCounts body: this guard checks nothing if it cannot find the method")
	return nil
}

func BenchmarkCompareDrain(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if got := drainModified(b, 10000); got.Modified != 10000 {
			b.Fatalf("summary = %+v, want 10000 modifications", got)
		}
	}
}
