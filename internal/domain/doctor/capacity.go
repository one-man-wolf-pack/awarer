package doctor

import "fmt"

// StoreCapacityScope is the boundary the store capacity metric covers: the project's
// own .awa/ directory, never the worktree or generated-output roots.
const StoreCapacityScope = ".awa"

// StoreCapacityMetric names what the measured number is: the sum of the observed
// lengths of the regular files inside the scope. It is not allocated blocks, physical
// disk usage, or bytes gc could reclaim.
const StoreCapacityMetric = "logical-bytes"

// CapacityMeasurement is how much of the store the capacity advisory established.
// Exact covers every enumerated file (a known zero included), partial carries an
// observed lower bound with at least one gap, unmeasured established nothing, and
// disabled means the advisory and its sizing walk were switched off.
type CapacityMeasurement uint8

const (
	CapacityUnmeasured CapacityMeasurement = iota
	CapacityDisabled
	CapacityExact
	CapacityPartial
)

// CapacityMeasurements returns every measurement state in declaration order, so the
// documentation that names their tokens can be held to the full set.
func CapacityMeasurements() []CapacityMeasurement {
	return []CapacityMeasurement{CapacityUnmeasured, CapacityDisabled, CapacityExact, CapacityPartial}
}

func (m CapacityMeasurement) String() string {
	switch m {
	case CapacityDisabled:
		return "disabled"
	case CapacityExact:
		return "exact"
	case CapacityPartial:
		return "partial"
	default:
		return "unmeasured"
	}
}

// CapacityGap is a typed reason a capacity measurement is not exact. The set is
// closed and ordered; a measurement reports its gaps in this order.
type CapacityGap string

const (
	// GapUnavailable: the store itself could not be opened for sizing.
	GapUnavailable CapacityGap = "unavailable"
	// GapWorkBudget: the sizing walk examined its budgeted number of entries and
	// stopped.
	GapWorkBudget CapacityGap = "work-budget"
	// GapDepthLimit: directories nested past the sizing depth limit were not entered.
	GapDepthLimit CapacityGap = "depth-limit"
	// GapUnreadableEntries: some entries could not be stat'ed, opened, or listed.
	GapUnreadableEntries CapacityGap = "unreadable-entries"
	// GapAggregateBudget: a watched-root audit had spent the work budget it shares
	// across roots, so this root was sized only partly or not at all. The store
	// measurement, a single tree, never reports it.
	GapAggregateBudget CapacityGap = "aggregate-budget"
)

var capacityGaps = []CapacityGap{GapUnavailable, GapWorkBudget, GapAggregateBudget, GapDepthLimit, GapUnreadableEntries}

// CapacityGaps returns the closed gap vocabulary in canonical order.
func CapacityGaps() []CapacityGap { return append([]CapacityGap(nil), capacityGaps...) }

// StoreCapacity is the doctor store capacity advisory's measurement. Its fields are
// unexported and its constructors admit only the coherent combinations: a disabled
// advisory has no threshold and no bytes, an unmeasured one has a threshold but no
// bytes, and only exact and partial carry an observed sum. So a zero is always a
// known zero and never stands in for "not measured". The zero value is an unmeasured
// store with no threshold.
type StoreCapacity struct {
	measurement CapacityMeasurement
	bytes       int64
	threshold   int64
	gaps        []CapacityGap
}

// DisabledStoreCapacity is the advisory switched off: no sizing walk ran.
func DisabledStoreCapacity() StoreCapacity {
	return StoreCapacity{measurement: CapacityDisabled}
}

// UnmeasuredStoreCapacity is an enabled advisory whose store could not be sized at
// all; it establishes nothing about the store's size.
func UnmeasuredStoreCapacity(threshold int64) (StoreCapacity, error) {
	if threshold <= 0 {
		return StoreCapacity{}, fmt.Errorf("store capacity threshold %d must be positive", threshold)
	}
	return StoreCapacity{measurement: CapacityUnmeasured, threshold: threshold, gaps: []CapacityGap{GapUnavailable}}, nil
}

// MeasuredStoreCapacity is an enabled advisory's observed sum. With no gaps it is
// exact; any gap makes it a partial lower bound over the inspected files.
// GapUnavailable belongs to unmeasured only and is rejected here.
func MeasuredStoreCapacity(observed, threshold int64, gaps ...CapacityGap) (StoreCapacity, error) {
	if threshold <= 0 {
		return StoreCapacity{}, fmt.Errorf("store capacity threshold %d must be positive", threshold)
	}
	if observed < 0 {
		return StoreCapacity{}, fmt.Errorf("observed store bytes %d must not be negative", observed)
	}
	present := make(map[CapacityGap]bool, len(gaps))
	for _, g := range gaps {
		if g == GapUnavailable || g == GapAggregateBudget || !g.valid() {
			return StoreCapacity{}, fmt.Errorf("%q is not a gap of a measured store", g)
		}
		present[g] = true
	}
	c := StoreCapacity{measurement: CapacityExact, bytes: observed, threshold: threshold}
	for _, g := range capacityGaps {
		if present[g] {
			c.gaps = append(c.gaps, g)
		}
	}
	if len(c.gaps) > 0 {
		c.measurement = CapacityPartial
	}
	return c, nil
}

func (g CapacityGap) valid() bool {
	for _, k := range capacityGaps {
		if g == k {
			return true
		}
	}
	return false
}

// Measurement reports how much of the store was established.
func (c StoreCapacity) Measurement() CapacityMeasurement { return c.measurement }

// ObservedBytes returns the observed logical-byte sum and whether one exists (exact
// or partial). A partial sum is a lower bound over the inspected files.
func (c StoreCapacity) ObservedBytes() (int64, bool) {
	return c.bytes, c.measurement == CapacityExact || c.measurement == CapacityPartial
}

// Threshold returns the effective warning threshold and whether the advisory is
// enabled with one.
func (c StoreCapacity) Threshold() (int64, bool) {
	return c.threshold, c.threshold > 0
}

// Gaps returns the reasons the measurement is not exact, in canonical order.
func (c StoreCapacity) Gaps() []CapacityGap { return append([]CapacityGap(nil), c.gaps...) }

// AtOrAboveThreshold reports whether the observed bytes reach the threshold. A
// partial sum that already reaches it is sufficient, since the store holds at least
// that much; a partial sum below it proves nothing and reports false.
func (c StoreCapacity) AtOrAboveThreshold() bool {
	observed, ok := c.ObservedBytes()
	threshold, enabled := c.Threshold()
	return ok && enabled && observed >= threshold
}
