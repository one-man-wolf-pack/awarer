package checkpoint

import (
	"testing"
	"time"
)

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
	header := CheckpointHeader{ID: idByte(t, 3)}

	cases := []struct {
		name    string
		counts  StoreReadCounts
		headers []CheckpointHeader
		want    StoreState
	}{
		{"empty", StoreReadCounts{}, nil, StoreEmpty},
		{"healthy", StoreReadCounts{Readable: 1}, []CheckpointHeader{header}, StoreHealthy},
		{"partial", StoreReadCounts{Readable: 1, Incompatible: 1}, []CheckpointHeader{header}, StorePartial},
		{"partial-with-corrupt", StoreReadCounts{Readable: 1, Corrupt: 1}, []CheckpointHeader{header}, StorePartial},
		{"incompatible-only", StoreReadCounts{Incompatible: 1}, nil, StoreIncompatible},
		{"corrupt-only", StoreReadCounts{Corrupt: 1}, nil, StoreCorrupt},
		{"mixed-unreadable-is-corrupt", StoreReadCounts{Incompatible: 1, Corrupt: 1}, nil, StoreCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewCheckpointStoreHealth(tc.counts, tc.headers)
			if got := h.State(); got != tc.want {
				t.Fatalf("State() = %v, want %v", got, tc.want)
			}
			if !h.State().Valid() {
				t.Fatalf("State() %v reports invalid", h.State())
			}
		})
	}
}

// TestStoreStateIgnoresRetainedWindowSize is the boundedness guard on the aggregate:
// a bounded caller retains one header out of a large, partially unreadable store, and
// the verdict must still describe the store rather than the window. Poison values are
// pairwise distinct so a verdict computed from the wrong field is visible.
func TestStoreStateIgnoresRetainedWindowSize(t *testing.T) {
	h := NewCheckpointStoreHealth(
		StoreReadCounts{Readable: 500, Incompatible: 7, Corrupt: 3},
		[]CheckpointHeader{{ID: idByte(t, 3)}},
	)
	if got := h.State(); got != StorePartial {
		t.Fatalf("State() = %v, want partial", got)
	}
	if h.Recorded() != 500 {
		t.Fatalf("Recorded() = %d, want the exact readable count 500", h.Recorded())
	}
	if len(h.NewestHeaders()) != 1 {
		t.Fatalf("NewestHeaders() length = %d, want the retained window 1", len(h.NewestHeaders()))
	}
}

// TestStoreHealthRejectsImpossibleConstruction guards the construction invariants. Each
// case is an aggregate that could not come from a real read pass and whose verdict
// would contradict its own evidence: a negative unreadable class cancels a real one, so
// the store reads healthy while records failed; a negative readable count reports a
// store smaller than empty; and a window wider than Readable would show more entries
// than the total it is drawn from. All fail at construction rather than downstream.
func TestStoreHealthRejectsImpossibleConstruction(t *testing.T) {
	cases := []struct {
		name    string
		counts  StoreReadCounts
		headers []CheckpointHeader
	}{
		{"negative readable", StoreReadCounts{Readable: -1}, nil},
		{"negative incompatible cancels a corrupt record", StoreReadCounts{Readable: 2, Incompatible: -1, Corrupt: 1}, nil},
		{"negative corrupt cancels an incompatible record", StoreReadCounts{Readable: 2, Incompatible: 1, Corrupt: -1}, nil},
		{"window wider than readable", StoreReadCounts{Readable: 1}, []CheckpointHeader{{ID: idByte(t, 3)}, {ID: idByte(t, 4)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("building a health aggregate from %+v with %d header(s) did not panic; the invariant is not enforced",
						tc.counts, len(tc.headers))
				}
			}()
			NewCheckpointStoreHealth(tc.counts, tc.headers)
		})
	}
}

// TestStoreHealthOrdersRetainedWindowNewestFirst proves the aggregate — not its
// producer — owns the returned order, including the id tie-break for equal creation
// times. The expected order is written out by hand rather than derived from the same
// comparison the constructor uses.
func TestStoreHealthOrdersRetainedWindowNewestFirst(t *testing.T) {
	older := time.Unix(1_700_000_000, 0).UTC()
	newer := older.Add(time.Minute)
	// Two headers share the newer timestamp, so the id tie-break decides between them.
	lowID, highID := idByte(t, 1), idByte(t, 2)
	if lowID.String() > highID.String() {
		lowID, highID = highID, lowID
	}
	h := NewCheckpointStoreHealth(
		StoreReadCounts{Readable: 3},
		[]CheckpointHeader{
			{ID: idByte(t, 3), CreatedAt: older},
			{ID: lowID, CreatedAt: newer},
			{ID: highID, CreatedAt: newer},
		},
	)
	got := h.NewestHeaders()
	want := []CheckpointID{highID, lowID, idByte(t, 3)}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("NewestHeaders()[%d] = %s, want %s", i, got[i].ID.Short(), id.Short())
		}
	}
	latest, ok := h.Latest()
	if !ok || latest.ID != highID {
		t.Fatalf("Latest() = (%s, %v), want (%s, true)", latest.ID.Short(), ok, highID.Short())
	}
}

func TestStoreHealthCounts(t *testing.T) {
	h := NewCheckpointStoreHealth(
		StoreReadCounts{Readable: 2, Incompatible: 1, Corrupt: 1},
		[]CheckpointHeader{{ID: idByte(t, 3)}, {ID: idByte(t, 4)}},
	)
	if h.Recorded() != 2 || h.Unreadable() != 2 || h.Incompatible() != 1 || h.Corrupt() != 1 {
		t.Fatalf("recorded=%d unreadable=%d incompatible=%d corrupt=%d, want 2/2/1/1",
			h.Recorded(), h.Unreadable(), h.Incompatible(), h.Corrupt())
	}
	if !h.AnyUnreadable() {
		t.Fatal("AnyUnreadable = false, want true")
	}
}
