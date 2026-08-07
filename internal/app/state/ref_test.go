package state

import (
	"strings"
	"testing"
)

func TestParseRangeDefault(t *testing.T) {
	rng, err := ParseRange("")
	if err != nil {
		t.Fatalf("ParseRange(\"\"): %v", err)
	}
	if rng.Left.Kind != RefLatest {
		t.Errorf("left kind = %v, want RefLatest", rng.Left.Kind)
	}
	if rng.Right.Kind != RefNow {
		t.Errorf("right kind = %v, want RefNow", rng.Right.Kind)
	}
}

func TestParseRangeNumericShortcut(t *testing.T) {
	rng, err := ParseRange("-2")
	if err != nil {
		t.Fatalf("ParseRange(-2): %v", err)
	}
	if rng.Left.Kind != RefAtN || rng.Left.N != 2 {
		t.Errorf("left = %+v, want @-2", rng.Left)
	}
	if rng.Right.Kind != RefNow {
		t.Errorf("right kind = %v, want RefNow", rng.Right.Kind)
	}
}

func TestParseRangeShortcutOneIsLatest(t *testing.T) {
	rng, err := ParseRange("-1")
	if err != nil {
		t.Fatalf("ParseRange(-1): %v", err)
	}
	// -1 expands to @-1..now; @-1 resolves to the newest checkpoint, the same as latest.
	if rng.Left.Kind != RefAtN || rng.Left.N != 1 {
		t.Errorf("left = %+v, want @-1", rng.Left)
	}
}

func TestParseRangeExplicit(t *testing.T) {
	rng, err := ParseRange("@-2..@-1")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if rng.Left.Kind != RefAtN || rng.Left.N != 2 {
		t.Errorf("left = %+v", rng.Left)
	}
	if rng.Right.Kind != RefAtN || rng.Right.N != 1 {
		t.Errorf("right = %+v", rng.Right)
	}
}

// TestParseRangeRejectsNonPrefixToken proves the parser owns id-prefix syntax: a
// token that is not a reserved reference and cannot be an id prefix is rejected
// here, so it never reaches the checkpoint repository as a lookup that could have
// succeeded.
func TestParseRangeRejectsNonPrefixToken(t *testing.T) {
	// "-" and "u" are outside the id alphabet; the last token is longer than a full id.
	for _, tok := range []string{"before-refactor..now", "unusual..now", "now..README.md", strings.Repeat("a", 33) + "..now"} {
		if _, err := ParseRange(tok); err == nil {
			t.Errorf("ParseRange(%q) = nil error, want an invalid-reference usage error", tok)
		}
	}
}

// TestParseRangeReservedWinsOverPrefix proves the reserved vocabulary is matched
// before prefix syntax is enforced. "latest" carries an "l", which the id alphabet
// deliberately excludes, so a parser that validated first would reject the most
// common reference in the product.
func TestParseRangeReservedWinsOverPrefix(t *testing.T) {
	rng, err := ParseRange("latest..now")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if rng.Left.Kind != RefLatest {
		t.Errorf("latest must parse as reserved, got %+v", rng.Left)
	}
}

func TestParseRangeMalformed(t *testing.T) {
	for _, tok := range []string{"a..b..c", "..now", "latest..", "..", "notarange", "@x", "@-0", "-0"} {
		if _, err := ParseRange(tok); err == nil {
			t.Errorf("ParseRange(%q) should fail", tok)
		}
	}
}

func TestParseRangeIDPrefixSide(t *testing.T) {
	rng, err := ParseRange("abc123..def456")
	if err != nil {
		t.Fatalf("ParseRange: %v", err)
	}
	if rng.Left.Kind != RefCheckpointPrefix || rng.Left.Raw != "abc123" {
		t.Errorf("left = %+v", rng.Left)
	}
	if rng.Right.Kind != RefCheckpointPrefix || rng.Right.Raw != "def456" {
		t.Errorf("right = %+v", rng.Right)
	}
}
