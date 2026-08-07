package checkpoint

import "sort"

// ReadHealth classifies how one checkpoint record reads back from the store. The
// zero value is not valid; a ReadFinding can only be built through the two named
// constructors, so a finding never carries an unclassified health.
type ReadHealth int

const (
	// ReadIncompatible is a record whose declared schema this build does not speak. It
	// is intact, self-describing local evidence awa has no reader for, not damage: the
	// user resets the store explicitly.
	ReadIncompatible ReadHealth = iota + 1
	// ReadCorrupt is a record that will not decode or validate for any reason other
	// than an incompatible declared schema — genuine durable corruption.
	ReadCorrupt
)

func (h ReadHealth) String() string {
	switch h {
	case ReadIncompatible:
		return "incompatible"
	case ReadCorrupt:
		return "corrupt"
	default:
		return "unknown"
	}
}

// ReadFinding records one unreadable checkpoint: its id (always known, since the id
// is the on-disk address the listing walks), whether it is incompatible or corrupt,
// and a human detail. Its fields are unexported and there is no open, health-taking
// constructor: a finding is built through NewIncompatibleReadFinding or
// NewCorruptReadFinding, so an invalid or unclassified read-health is unrepresentable
// rather than a caller-discipline hazard — a store's derived state can never key off
// a zero-value health.
type ReadFinding struct {
	id     CheckpointID
	health ReadHealth
	detail string
}

// NewIncompatibleReadFinding records a checkpoint whose declared schema this build
// does not speak. detail carries the underlying decode error.
func NewIncompatibleReadFinding(id CheckpointID, detail string) ReadFinding {
	return ReadFinding{id: id, health: ReadIncompatible, detail: detail}
}

// NewCorruptReadFinding records a checkpoint that reads back as genuine durable
// corruption: malformed data, a broken invariant, or data claiming the current schema
// that the strict reader rejected. detail carries the underlying decode error.
func NewCorruptReadFinding(id CheckpointID, detail string) ReadFinding {
	return ReadFinding{id: id, health: ReadCorrupt, detail: detail}
}

func (f ReadFinding) ID() CheckpointID   { return f.id }
func (f ReadFinding) Health() ReadHealth { return f.health }
func (f ReadFinding) Detail() string     { return f.detail }

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

// CheckpointStoreHealth aggregates one read pass over a checkpoint store: the
// headers that read back cleanly (newest-first) and a finding for each record that
// did not. State is derived from these, never set by hand, so the reported verdict
// always matches the evidence. It is the resilient counterpart to ListHeaders: a
// single unreadable record becomes a finding here rather than collapsing the whole
// listing.
type CheckpointStoreHealth struct {
	headers  []CheckpointHeader
	findings []ReadFinding
}

// NewCheckpointStoreHealth builds a health aggregate from the readable headers and
// the per-record findings. Headers are sorted newest-first with a deterministic
// id tie-breaker (matching ListHeaders) and both slices are copied, so a later
// mutation of the inputs cannot change a built aggregate.
func NewCheckpointStoreHealth(headers []CheckpointHeader, findings []ReadFinding) CheckpointStoreHealth {
	hs := make([]CheckpointHeader, len(headers))
	copy(hs, headers)
	sort.Slice(hs, func(i, j int) bool {
		if !hs[i].CreatedAt.Equal(hs[j].CreatedAt) {
			return hs[i].CreatedAt.After(hs[j].CreatedAt)
		}
		return hs[i].ID.String() > hs[j].ID.String()
	})
	fs := make([]ReadFinding, len(findings))
	copy(fs, findings)
	return CheckpointStoreHealth{headers: hs, findings: fs}
}

// Headers returns the readable checkpoint headers, newest-first. The slice is a copy.
func (h CheckpointStoreHealth) Headers() []CheckpointHeader {
	out := make([]CheckpointHeader, len(h.headers))
	copy(out, h.headers)
	return out
}

// Findings returns the per-record findings for unreadable checkpoints. The slice is a copy.
func (h CheckpointStoreHealth) Findings() []ReadFinding {
	out := make([]ReadFinding, len(h.findings))
	copy(out, h.findings)
	return out
}

// Recorded reports how many checkpoints read back cleanly. It is deliberately the
// readable count, not the on-disk count, so a caller cannot report an unreadable
// store as an empty one by reading this alone — Unreadable carries the rest.
func (h CheckpointStoreHealth) Recorded() int { return len(h.headers) }

// Unreadable reports how many checkpoints could not be read.
func (h CheckpointStoreHealth) Unreadable() int { return len(h.findings) }

// Incompatible reports how many unreadable checkpoints declare a schema this build
// does not speak.
func (h CheckpointStoreHealth) Incompatible() int { return h.countHealth(ReadIncompatible) }

// Corrupt reports how many unreadable checkpoints are corrupt.
func (h CheckpointStoreHealth) Corrupt() int { return h.countHealth(ReadCorrupt) }

func (h CheckpointStoreHealth) countHealth(want ReadHealth) int {
	n := 0
	for _, f := range h.findings {
		if f.health == want {
			n++
		}
	}
	return n
}

// AnyUnreadable reports whether any checkpoint could not be read.
func (h CheckpointStoreHealth) AnyUnreadable() bool { return len(h.findings) > 0 }

// Latest returns the newest readable checkpoint header, or ok false when none read
// back cleanly. A caller must not treat ok false as "no checkpoints" when
// AnyUnreadable is true: the newest record may itself be unreadable, and ordering
// unreadable records against readable ones is impossible without their headers.
func (h CheckpointStoreHealth) Latest() (CheckpointHeader, bool) {
	if len(h.headers) == 0 {
		return CheckpointHeader{}, false
	}
	return h.headers[0], true
}

// State derives the overall store verdict from the counts: empty when nothing
// exists, healthy when everything read cleanly, partial when some read and some did
// not, and — when nothing readable remains — incompatible only if every failure is an
// incompatible schema, otherwise corrupt. The incompatible case fails closed: it
// requires every finding to be classified incompatible rather than merely the absence
// of a corrupt one, so an unclassified finding (unrepresentable through the
// constructors, but guarded here anyway) degrades a store to corrupt, never to the
// milder verdict.
func (h CheckpointStoreHealth) State() StoreState {
	switch {
	case len(h.headers) == 0 && len(h.findings) == 0:
		return StoreEmpty
	case len(h.findings) == 0:
		return StoreHealthy
	case len(h.headers) > 0:
		return StorePartial
	case h.Incompatible() == len(h.findings):
		return StoreIncompatible
	default:
		return StoreCorrupt
	}
}
