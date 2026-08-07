package runcache

import "fmt"

// SampleCompleteness declares how a bounded changed-path sample relates to the
// full set of changes it was drawn from. It is a required, explicit fact so a
// rendered near miss can never imply it showed every changed path when it only
// showed a prefix, and can never silently present an empty sample as "no changes"
// when the truth is "the sample could not be computed".
type SampleCompleteness int

const (
	// SampleComplete: the sample lists every changed path (len(Paths) == Total).
	SampleComplete SampleCompleteness = iota
	// SampleTruncated: the sample is a bounded prefix of a larger set
	// (len(Paths) < Total); Total names how many changes there really are.
	SampleTruncated
	// SampleUnavailable: no sample was computed at all — the near miss carries no
	// ChangedPathSample. It is the projection vocabulary for an absent sample, never
	// a state a constructed sample is in, and is distinct from a complete empty one.
	SampleUnavailable
)

// String returns a stable token for the completeness state.
func (c SampleCompleteness) String() string {
	switch c {
	case SampleComplete:
		return "complete"
	case SampleTruncated:
		return "truncated"
	case SampleUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// ChangedPath is one changed input path in a sample: the path and a stable
// single-letter status code (A/M/D/R/T/S) produced by the compare vocabulary at
// construction. The code is stored as a plain string — mapped once, at the source,
// from compare.Status.Code() — so the run domain neither depends on the compare
// package nor forces a second copy of the token→code table onto its renderers.
type ChangedPath struct {
	Path   string
	Status string
}

// ChangedPathSample is a bounded, self-describing sample of the input paths that
// differ between a stored run and the current state. It exists to explain an
// input-tree-differs near miss without loading or printing an unbounded diff: the
// sample is a prefix, and it always declares whether that prefix is the whole story
// (SampleComplete) or a bounded excerpt (SampleTruncated). A constructed sample has
// exactly those two states; absence is carried by an absent *ChangedPathSample, not
// by a value claiming it holds nothing. Construct it only through
// NewChangedPathSample, which forbids the contradictory shapes.
type ChangedPathSample struct {
	paths        []ChangedPath
	total        int
	completeness SampleCompleteness
}

// NewChangedPathSample builds a sample from a bounded slice of changed paths and
// the true total number of changes. It rejects an oversized sample (more paths than
// the total) and every path missing its fields, and derives the completeness from
// the sample size: complete when it holds every change, truncated when it holds a
// prefix. It is the only constructor, so a value that exists was always computed.
func NewChangedPathSample(paths []ChangedPath, total int) (ChangedPathSample, error) {
	if total < 0 {
		return ChangedPathSample{}, fmt.Errorf("changed-path sample has negative total %d", total)
	}
	if len(paths) > total {
		return ChangedPathSample{}, fmt.Errorf("changed-path sample has %d paths but a total of only %d", len(paths), total)
	}
	for i, p := range paths {
		if p.Path == "" {
			return ChangedPathSample{}, fmt.Errorf("changed-path sample entry %d has no path", i)
		}
		if p.Status == "" {
			return ChangedPathSample{}, fmt.Errorf("changed-path sample entry %d (%s) has no status", i, p.Path)
		}
	}
	completeness := SampleComplete
	if len(paths) < total {
		completeness = SampleTruncated
	}
	return ChangedPathSample{
		paths:        append([]ChangedPath(nil), paths...),
		total:        total,
		completeness: completeness,
	}, nil
}

// Paths returns the bounded sample of changed paths.
func (s ChangedPathSample) Paths() []ChangedPath { return s.paths }

// Total returns the true number of changed paths the sample was drawn from.
func (s ChangedPathSample) Total() int { return s.total }

// Completeness returns whether the sample is complete or truncated.
func (s ChangedPathSample) Completeness() SampleCompleteness { return s.completeness }

// Truncated reports whether the sample is a bounded prefix of a larger set.
func (s ChangedPathSample) Truncated() bool { return s.completeness == SampleTruncated }

// Omitted returns how many changed paths are not shown in the sample.
func (s ChangedPathSample) Omitted() int {
	if n := s.total - len(s.paths); n > 0 {
		return n
	}
	return 0
}
