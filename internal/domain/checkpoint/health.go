package checkpoint

import (
	"fmt"
	"slices"
)

// StoreState is the derived, overall read state of a checkpoint store. It is the
// closed vocabulary the machine-readable surfaces must keep distinguishable so
// "empty", "unreadable", and "partial" never collapse into one shape.
type StoreState int

const (
	// StoreEmpty means no checkpoints exist. Distinct from a store that exists but
	// could not be read.
	StoreEmpty StoreState = iota + 1
	// StoreHealthy means every checkpoint read back cleanly.
	StoreHealthy
	// StorePartial means some checkpoints are readable and at least one is not.
	StorePartial
	// StoreIncompatible means no checkpoint is readable and every unreadable one
	// declares a schema this build does not speak.
	StoreIncompatible
	// StoreCorrupt means no checkpoint is readable and at least one is corrupt.
	StoreCorrupt
)

func (s StoreState) String() string {
	switch s {
	case StoreEmpty:
		return "empty"
	case StoreHealthy:
		return "healthy"
	case StorePartial:
		return "partial"
	case StoreIncompatible:
		return "incompatible"
	case StoreCorrupt:
		return "corrupt"
	default:
		return "unknown"
	}
}

// Valid reports whether s is a known store state.
func (s StoreState) Valid() bool {
	switch s {
	case StoreEmpty, StoreHealthy, StorePartial, StoreIncompatible, StoreCorrupt:
		return true
	default:
		return false
	}
}

// StoreReadCounts is the exact tally of one complete read pass over a checkpoint
// store: how many records read back cleanly, how many declare a schema this build
// does not speak, and how many are genuine durable corruption. Every committed
// record lands in exactly one of the three, so the counts describe the whole store
// regardless of how many headers the pass was asked to retain.
//
// The fields are named rather than positional so a producer cannot silently swap
// the incompatible and corrupt tallies — the two the store policy treats most
// differently.
type StoreReadCounts struct {
	Readable     int
	Incompatible int
	Corrupt      int
}

// CheckpointStoreHealth aggregates one read pass over a checkpoint store: the exact
// per-class counts of every committed record, plus the newest readable headers the
// pass was asked to retain. State is derived from the counts, never set by hand and
// never from the retained window, so a caller that asked for one header still gets
// the whole store's verdict rather than a verdict about its window.
//
// The retained window is a subset of the readable records — NewestHeaders is at most
// Recorded long — so a caller must read totals from the counters and only entries
// from the window.
type CheckpointStoreHealth struct {
	counts StoreReadCounts
	newest []CheckpointHeader
}

// NewCheckpointStoreHealth builds a health aggregate from the exact counts of a
// complete read pass and the readable headers that pass retained. The headers are
// sorted newest-first with the deterministic id tie-breaker and copied, so a later
// mutation of the input cannot change a built aggregate and the caller never has to
// order them itself.
//
// Two producer bugs are refused here rather than described. A count below zero is not
// a tally of anything: a negative unreadable class cancels a real one, so AnyUnreadable
// reads false and the verdict comes out healthy for a store whose records failed, and a
// negative readable count reports a store smaller than empty. And keeping more headers
// than Readable would let one aggregate report an empty store that still has a latest,
// or a "showing N of M" with M below N. The counts and the window come from the same
// pass, so either mismatch is a violated programmer invariant and fails loudly here
// rather than surfacing as a contradictory verdict downstream.
func NewCheckpointStoreHealth(counts StoreReadCounts, newest []CheckpointHeader) CheckpointStoreHealth {
	if counts.Readable < 0 || counts.Incompatible < 0 || counts.Corrupt < 0 {
		panic(fmt.Sprintf("checkpoint store health: read counts cannot be negative (readable %d, incompatible %d, corrupt %d)",
			counts.Readable, counts.Incompatible, counts.Corrupt))
	}
	if len(newest) > counts.Readable {
		panic(fmt.Sprintf("checkpoint store health: %d retained headers exceed the %d readable records they are a window over",
			len(newest), counts.Readable))
	}
	hs := slices.Clone(newest)
	slices.SortFunc(hs, NewestFirst)
	return CheckpointStoreHealth{counts: counts, newest: hs}
}

// NewestHeaders returns the retained readable headers, newest-first. It is the
// requested window, not the whole store: its length is bounded by what the operation
// was asked to keep, so a total must come from Recorded rather than from this slice.
// The slice is a copy.
func (h CheckpointStoreHealth) NewestHeaders() []CheckpointHeader {
	return slices.Clone(h.newest)
}

// Recorded reports how many checkpoints read back cleanly across the whole store. It
// is deliberately the readable count, not the on-disk count, so a caller cannot
// report an unreadable store as an empty one by reading this alone — Unreadable
// carries the rest.
func (h CheckpointStoreHealth) Recorded() int { return h.counts.Readable }

// Unreadable reports how many checkpoints could not be read.
func (h CheckpointStoreHealth) Unreadable() int { return h.counts.Incompatible + h.counts.Corrupt }

// Incompatible reports how many unreadable checkpoints declare a schema this build
// does not speak.
func (h CheckpointStoreHealth) Incompatible() int { return h.counts.Incompatible }

// Corrupt reports how many unreadable checkpoints are corrupt.
func (h CheckpointStoreHealth) Corrupt() int { return h.counts.Corrupt }

// AnyUnreadable reports whether any checkpoint could not be read.
func (h CheckpointStoreHealth) AnyUnreadable() bool { return h.Unreadable() > 0 }

// Latest returns the newest readable checkpoint header, or ok false when the pass
// retained none. A caller must not treat ok false as "no checkpoints" when
// AnyUnreadable is true: the newest record may itself be unreadable, and ordering
// unreadable records against readable ones is impossible without their headers.
func (h CheckpointStoreHealth) Latest() (CheckpointHeader, bool) {
	if len(h.newest) == 0 {
		return CheckpointHeader{}, false
	}
	return h.newest[0], true
}

// State derives the overall store verdict from the exact counts: empty when nothing
// exists, healthy when everything read cleanly, partial when some read and some did
// not, and — when nothing readable remains — incompatible only if no unreadable
// record is corrupt, otherwise corrupt. The incompatible case fails closed: a single
// corrupt record degrades a store to corrupt, never to the milder verdict.
func (h CheckpointStoreHealth) State() StoreState {
	switch {
	case h.counts.Readable == 0 && h.Unreadable() == 0:
		return StoreEmpty
	case h.Unreadable() == 0:
		return StoreHealthy
	case h.counts.Readable > 0:
		return StorePartial
	case h.counts.Corrupt == 0:
		return StoreIncompatible
	default:
		return StoreCorrupt
	}
}
