package difftext

// This file implements the hybrid Histogram diff engine. Within a region it
// collects, in a single pass, every line that is unique on both sides — the
// rarest, least ambiguous anchors — confirms each by real line equality, and
// keeps the longest non-crossing chain of them; it then recurses only into the
// gaps between anchors and falls back to Myers locally for any region it cannot
// anchor. Hashes only narrow the search for candidate anchors; an anchor is
// never accepted until real line equality confirms it, so a hash collision can
// never create a false match. Every step is bounded: anchoring only on lines
// unique to both sides excludes over-common lines (braces, blank lines,
// boilerplate) outright, a depth cap stops pathological recursion, and Myers'
// own edit-distance guard bounds any region it handles.

const (
	// maxAnchorFrequency saturates the per-line occurrence counters so a
	// pathologically repeated line cannot drive a counter without bound. It does
	// not select anchors: the engine anchors only on lines that occur exactly once
	// on each side, so any line reaching count 2 is already excluded. The cap sits
	// well above that uniqueness threshold and exists purely to keep counting work
	// bounded for inputs full of one repeated line.
	maxAnchorFrequency = 64
	// maxHistogramDepth bounds recursion. The anchor chain captures every usable
	// anchor in a region at once, so depth is normally shallow; the cap turns a
	// degenerate input into a local Myers diff rather than unbounded recursion.
	maxHistogramDepth = 50
)

// lineHash maps a line to a 64-bit hash used only to group equal lines while
// counting occurrences. It is a package variable so an internal test can force
// collisions and prove that real line equality — not the hash — decides every
// anchor. It is not part of the product API.
var lineHash = fnv1aLine

// fnv1aLine is the default line hash: FNV-1a over the line text with the
// newline flag mixed in, so "a" and "a\n" hash apart and are never confused as
// the same anchor candidate.
func fnv1aLine(l line) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(l.text); i++ {
		h ^= uint64(l.text[i])
		h *= prime64
	}
	var nl byte
	if l.hasNewline {
		nl = 1
	}
	h ^= uint64(nl)
	h *= prime64
	return h
}

// histogram is the diffFunc entry point for the Histogram engine. It returns an
// ordered edit script for a vs b, propagating ErrTooLarge if a Myers-handled
// region exceeds the edit-distance budget.
func histogram(a, b []line) ([]op, error) {
	return histogramRegion(a, b, 0, len(a), 0, len(b), 0)
}

// histogramRegion diffs a[lowA:highA] against b[lowB:highB] and returns the ops
// in order. It trims the shared prefix and suffix, then anchors the interior.
func histogramRegion(a, b []line, lowA, highA, lowB, highB, depth int) ([]op, error) {
	var ops []op

	// Trim the common prefix: shared leading lines are equal ops emitted now.
	for lowA < highA && lowB < highB && a[lowA] == b[lowB] {
		ops = append(ops, op{kind: opEqual, line: a[lowA]})
		lowA++
		lowB++
	}
	// Trim the common suffix, collecting it reversed to append after the middle.
	var suffix []op
	for highA > lowA && highB > lowB && a[highA-1] == b[highB-1] {
		suffix = append(suffix, op{kind: opEqual, line: a[highA-1]})
		highA--
		highB--
	}

	mid, err := histogramMiddle(a, b, lowA, highA, lowB, highB, depth)
	if err != nil {
		return nil, err
	}
	ops = append(ops, mid...)
	// Append the trimmed suffix in forward order.
	for i := len(suffix) - 1; i >= 0; i-- {
		ops = append(ops, suffix[i])
	}
	return ops, nil
}

// histogramMiddle diffs the region left after prefix/suffix trimming. One side
// being empty is a pure insertion or deletion; otherwise it splits on the anchor
// chain and recurses into the gaps, or falls back to Myers when the region has
// no anchors or recursion is too deep.
func histogramMiddle(a, b []line, lowA, highA, lowB, highB, depth int) ([]op, error) {
	if lowA == highA {
		ops := make([]op, 0, highB-lowB)
		for j := lowB; j < highB; j++ {
			ops = append(ops, op{kind: opInsert, line: b[j]})
		}
		return ops, nil
	}
	if lowB == highB {
		ops := make([]op, 0, highA-lowA)
		for i := lowA; i < highA; i++ {
			ops = append(ops, op{kind: opDelete, line: a[i]})
		}
		return ops, nil
	}

	// Depth limit or no safe anchor: diff this region with Myers. Fallback is the
	// correct behavior for unsuitable input, not an error.
	if depth >= maxHistogramDepth {
		return myers(a[lowA:highA], b[lowB:highB])
	}
	chain := anchorChain(a, b, lowA, highA, lowB, highB)
	if len(chain) == 0 {
		return myers(a[lowA:highA], b[lowB:highB])
	}

	// Walk the anchors in order, diffing each gap before its anchor and emitting
	// the confirmed-equal anchor line, then diff the final tail gap.
	var ops []op
	prevA, prevB := lowA, lowB
	for _, an := range chain {
		gap, err := histogramRegion(a, b, prevA, an.aIdx, prevB, an.bIdx, depth+1)
		if err != nil {
			return nil, err
		}
		ops = append(ops, gap...)
		ops = append(ops, op{kind: opEqual, line: a[an.aIdx]})
		prevA, prevB = an.aIdx+1, an.bIdx+1
	}
	tail, err := histogramRegion(a, b, prevA, highA, prevB, highB, depth+1)
	if err != nil {
		return nil, err
	}
	ops = append(ops, tail...)
	return ops, nil
}

// anchor is a confirmed-equal line pair splitting a region.
type anchor struct {
	aIdx, bIdx int
}

// lineInfo records, per hash, how many times a line occurs in a region (capped)
// and the index of its first occurrence.
type lineInfo struct {
	count int
	idx   int
}

// anchorChain returns an ordered, non-crossing chain of anchors for a region:
// the longest increasing (by b index) subsequence of lines that are unique on
// both sides and equal by real bytes. Uniqueness makes each anchor unambiguous
// and collision-safe — a hash shared by two different lines is never unique, and
// even a hash shared by one line per side is rejected by the equality check. An
// empty result tells the caller to fall back to Myers.
func anchorChain(a, b []line, lowA, highA, lowB, highB int) []anchor {
	countA := make(map[uint64]lineInfo)
	for i := lowA; i < highA; i++ {
		h := lineHash(a[i])
		e := countA[h]
		if e.count == 0 {
			e.idx = i
		}
		if e.count <= maxAnchorFrequency {
			e.count++
		}
		countA[h] = e
	}
	countB := make(map[uint64]lineInfo)
	for j := lowB; j < highB; j++ {
		h := lineHash(b[j])
		e := countB[h]
		if e.count == 0 {
			e.idx = j
		}
		if e.count <= maxAnchorFrequency {
			e.count++
		}
		countB[h] = e
	}

	// Candidates in ascending a index: lines unique on both sides whose bytes
	// truly match. A unique line occurs once, so its first-occurrence index is its
	// only index.
	var cands []anchor
	for i := lowA; i < highA; i++ {
		h := lineHash(a[i])
		if countA[h].count != 1 {
			continue
		}
		eb, ok := countB[h]
		if !ok || eb.count != 1 {
			continue
		}
		if a[i] != b[eb.idx] {
			continue // hashes collided but the lines differ: not an anchor
		}
		cands = append(cands, anchor{aIdx: i, bIdx: eb.idx})
	}
	if len(cands) == 0 {
		return nil
	}
	return longestIncreasingByB(cands)
}

// longestIncreasingByB returns the longest subsequence of cands (already in
// ascending a-index order) whose b indices strictly increase, so the chosen
// anchors never cross. With a moved block this keeps the larger aligned run as
// anchors and lets the smaller one diff as a move.
func longestIncreasingByB(cands []anchor) []anchor {
	n := len(cands)
	// tails[k] is the cands index of the smallest possible b-tail of an
	// increasing chain of length k+1; prev links each element to its predecessor.
	tails := make([]int, 0, n)
	prev := make([]int, n)
	for i := range prev {
		prev[i] = -1
	}
	for i := 0; i < n; i++ {
		bi := cands[i].bIdx
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if cands[tails[mid]].bIdx < bi {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo > 0 {
			prev[i] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, i)
		} else {
			tails[lo] = i
		}
	}
	// Reconstruct from the last tail back through prev, then reverse.
	chain := make([]anchor, 0, len(tails))
	for k := tails[len(tails)-1]; k >= 0; k = prev[k] {
		chain = append(chain, cands[k])
	}
	for l, r := 0, len(chain)-1; l < r; l, r = l+1, r-1 {
		chain[l], chain[r] = chain[r], chain[l]
	}
	return chain
}
