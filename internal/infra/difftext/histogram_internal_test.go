package difftext

import (
	"strings"
	"testing"
)

// TestHistogramHashCollisionNoFalseAnchor maps two distinct lines ("beta" and
// "BETA") to the same hash while every other line hashes normally. Each collides
// once per side, so they reach the equality check as a unique-hash candidate;
// real line equality must reject the pair so the change is rendered rather than
// silently anchored away.
func TestHistogramHashCollisionNoFalseAnchor(t *testing.T) {
	orig := lineHash
	lineHash = func(l line) uint64 {
		if l.text == "beta" || l.text == "BETA" {
			return 0xdead // a forced collision between two different lines
		}
		return orig(l)
	}
	defer func() { lineHash = orig }()

	old := splitLines([]byte("alpha\nbeta\ngamma\ndelta\n"))
	new := splitLines([]byte("alpha\nBETA\ngamma\ndelta\n"))
	ops, err := histogram(old, new)
	if err != nil {
		t.Fatalf("histogram: %v", err)
	}

	// Reconstruct both sides from the op stream and confirm they match the
	// inputs exactly — a false anchor between "beta" and "BETA" would corrupt
	// one side.
	var gotOld, gotNew []string
	for _, o := range ops {
		switch o.kind {
		case opEqual:
			gotOld = append(gotOld, o.line.text)
			gotNew = append(gotNew, o.line.text)
		case opDelete:
			gotOld = append(gotOld, o.line.text)
		case opInsert:
			gotNew = append(gotNew, o.line.text)
		}
	}
	if strings.Join(gotOld, "\n") != "alpha\nbeta\ngamma\ndelta" {
		t.Errorf("old side corrupted by collision: %q", gotOld)
	}
	if strings.Join(gotNew, "\n") != "alpha\nBETA\ngamma\ndelta" {
		t.Errorf("new side corrupted by collision: %q", gotNew)
	}
	// The change must actually appear: beta deleted, BETA inserted.
	var del, ins bool
	for _, o := range ops {
		if o.kind == opDelete && o.line.text == "beta" {
			del = true
		}
		if o.kind == opInsert && o.line.text == "BETA" {
			ins = true
		}
	}
	if !del || !ins {
		t.Errorf("expected beta->BETA change under collision; del=%v ins=%v", del, ins)
	}
}

// TestAnchorChainPrefersUniqueLine confirms the chain anchors on the line that
// is unique on both sides, ignoring the repeated "x" lines around it.
func TestAnchorChainPrefersUniqueLine(t *testing.T) {
	a := splitLines([]byte("x\nx\nUNIQUE\nx\nx\n"))
	b := splitLines([]byte("x\nUNIQUE\nx\n"))
	chain := anchorChain(a, b, 0, len(a), 0, len(b))
	if len(chain) != 1 {
		t.Fatalf("want exactly the UNIQUE anchor, got %d anchors", len(chain))
	}
	if a[chain[0].aIdx].text != "UNIQUE" || b[chain[0].bIdx].text != "UNIQUE" {
		t.Errorf("anchor = (%q,%q), want the UNIQUE line on both sides", a[chain[0].aIdx].text, b[chain[0].bIdx].text)
	}
}

// TestAnchorChainRepeatedFallsBack confirms that a region of one repeated line
// offers no unique anchors, so the caller falls back to Myers. The count is
// chosen above maxAnchorFrequency so the same input also exercises the
// counter-saturation path.
func TestAnchorChainRepeatedFallsBack(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < maxAnchorFrequency+5; i++ {
		sb.WriteString("dup\n")
	}
	lines := splitLines([]byte(sb.String()))
	if chain := anchorChain(lines, lines, 0, len(lines), 0, len(lines)); len(chain) != 0 {
		t.Errorf("a region with no unique lines must offer no anchors, got %d", len(chain))
	}
}

// TestAnchorChainNonCrossingOnMovedBlock confirms that when two unique runs swap
// order, the chain keeps a single non-crossing (monotonic) run of anchors rather
// than crossing anchors that would corrupt the diff.
func TestAnchorChainNonCrossingOnMovedBlock(t *testing.T) {
	a := splitLines([]byte("A1\nA2\nA3\nB1\nB2\n"))
	b := splitLines([]byte("B1\nB2\nA1\nA2\nA3\n"))
	chain := anchorChain(a, b, 0, len(a), 0, len(b))
	if len(chain) == 0 {
		t.Fatal("expected a non-empty anchor chain")
	}
	for i := 1; i < len(chain); i++ {
		if chain[i].aIdx <= chain[i-1].aIdx || chain[i].bIdx <= chain[i-1].bIdx {
			t.Errorf("anchors must strictly increase on both axes: %+v", chain)
		}
	}
}
