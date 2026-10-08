package gc

import "fmt"

// Measurement says how much of a candidate's logical byte count was established.
// The three states stay distinct through every summary and projection: a known zero
// is a fact about the files, while an unmeasured value says nothing about them.
type Measurement uint8

const (
	// MeasureUnmeasured: no length was established — the candidate is outside a
	// selected deletion boundary (retained, filtered, blocked) or its boundary could
	// not be inspected at all. Its byte value is zero and means nothing.
	MeasureUnmeasured Measurement = iota
	// MeasurePartial: enumeration of the boundary was incomplete, so the byte value
	// holds only the lengths that were established and is a lower bound.
	MeasurePartial
	// MeasureExact: every regular file in the boundary was measured. A boundary that
	// was already absent is an exact zero.
	MeasureExact
)

// String returns the stable wire token for the measurement.
func (m Measurement) String() string {
	switch m {
	case MeasureUnmeasured:
		return "unmeasured"
	case MeasurePartial:
		return "partial"
	case MeasureExact:
		return "exact"
	default:
		return fmt.Sprintf("measurement(%d)", uint8(m))
	}
}

// Valid reports whether m is one of the defined measurements.
func (m Measurement) Valid() bool { return m <= MeasureExact }

// ByteMeasure is a candidate's logical byte count together with how completely it
// was measured. Build one through ExactBytes, PartialBytes, or UnmeasuredBytes so an
// unmeasured value can never carry bytes it did not establish.
type ByteMeasure struct {
	bytes int64
	state Measurement
}

// ExactBytes is a complete measurement of n bytes; zero is a known zero.
func ExactBytes(n int64) ByteMeasure { return ByteMeasure{bytes: n, state: MeasureExact} }

// PartialBytes is an incomplete measurement whose n established bytes are a lower
// bound.
func PartialBytes(n int64) ByteMeasure { return ByteMeasure{bytes: n, state: MeasurePartial} }

// UnmeasuredBytes is the absence of a measurement.
func UnmeasuredBytes() ByteMeasure { return ByteMeasure{state: MeasureUnmeasured} }

// Bytes returns the established byte count: exact, a lower bound, or zero when
// unmeasured.
func (b ByteMeasure) Bytes() int64 { return b.bytes }

// Measurement returns how completely the bytes were measured.
func (b ByteMeasure) Measurement() Measurement { return b.state }

// Exact reports whether the byte count is a complete measurement.
func (b ByteMeasure) Exact() bool { return b.state == MeasureExact }
