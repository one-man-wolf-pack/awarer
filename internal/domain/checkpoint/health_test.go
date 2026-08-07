package checkpoint

import "testing"

func idByte(t *testing.T, b byte) CheckpointID {
	t.Helper()
	// A 32-char id built from a repeated alphabet character is a valid parseable id.
	s := make([]byte, idLen)
	for i := range s {
		s[i] = idAlphabet[int(b)%len(idAlphabet)]
	}
	id, err := ParseCheckpointID(string(s))
	if err != nil {
		t.Fatalf("ParseCheckpointID: %v", err)
	}
	return id
}

func TestStoreStateDerivation(t *testing.T) {
	incompatible := NewIncompatibleReadFinding(idByte(t, 1), "old field")
	corrupt := NewCorruptReadFinding(idByte(t, 2), "bad json")
	header := CheckpointHeader{ID: idByte(t, 3)}

	cases := []struct {
		name     string
		headers  []CheckpointHeader
		findings []ReadFinding
		want     StoreState
	}{
		{"empty", nil, nil, StoreEmpty},
		{"healthy", []CheckpointHeader{header}, nil, StoreHealthy},
		{"partial", []CheckpointHeader{header}, []ReadFinding{incompatible}, StorePartial},
		{"partial-with-corrupt", []CheckpointHeader{header}, []ReadFinding{corrupt}, StorePartial},
		{"incompatible-only", nil, []ReadFinding{incompatible}, StoreIncompatible},
		{"corrupt-only", nil, []ReadFinding{corrupt}, StoreCorrupt},
		{"mixed-unreadable-is-corrupt", nil, []ReadFinding{incompatible, corrupt}, StoreCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewCheckpointStoreHealth(tc.headers, tc.findings)
			if got := h.State(); got != tc.want {
				t.Fatalf("State() = %v, want %v", got, tc.want)
			}
			if !h.State().Valid() {
				t.Fatalf("State() %v reports invalid", h.State())
			}
		})
	}
}

// TestStoreStateUnknownHealthFailsClosed guards the defensive derivation: the public
// constructors make an unclassified read-health unrepresentable, but if one ever
// reached State() (built here in-package via the raw struct), a store of only such
// findings must degrade to corrupt — fail closed — never to disposable incompatible.
func TestStoreStateUnknownHealthFailsClosed(t *testing.T) {
	unknown := ReadFinding{id: idByte(t, 1), health: 0, detail: "unclassified"}
	h := NewCheckpointStoreHealth(nil, []ReadFinding{unknown})
	if got := h.State(); got != StoreCorrupt {
		t.Fatalf("State() with an unclassified finding = %v, want corrupt (fail closed)", got)
	}
}

func TestStoreHealthCounts(t *testing.T) {
	h := NewCheckpointStoreHealth(
		[]CheckpointHeader{{ID: idByte(t, 3)}, {ID: idByte(t, 4)}},
		[]ReadFinding{
			NewIncompatibleReadFinding(idByte(t, 1), "a"),
			NewCorruptReadFinding(idByte(t, 2), "b"),
		},
	)
	if h.Recorded() != 2 || h.Unreadable() != 2 || h.Incompatible() != 1 || h.Corrupt() != 1 {
		t.Fatalf("recorded=%d unreadable=%d incompatible=%d corrupt=%d, want 2/2/1/1",
			h.Recorded(), h.Unreadable(), h.Incompatible(), h.Corrupt())
	}
	if !h.AnyUnreadable() {
		t.Fatal("AnyUnreadable = false, want true")
	}
}
