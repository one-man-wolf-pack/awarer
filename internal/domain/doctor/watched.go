package doctor

import (
	"fmt"
	"slices"
	"sort"
)

// WatchedRootScope names the boundary the explicit watched-root audit covers: the
// real directories the effective effect-root selectors select, not the whole worktree,
// the disk, or .awa/.
const WatchedRootScope = "effect-root-selectors"

// WatchedRoot is one audited directory's measurement: its native slash-separated
// project-relative path and a logical-byte figure that is exact, partial (a lower
// bound with typed gaps), or unmeasured (no figure). An exact zero is a known-empty
// root. Fields are unexported so only the coherent combinations exist.
type WatchedRoot struct {
	path        string
	measurement CapacityMeasurement
	bytes       int64
	gaps        []CapacityGap
}

// MeasuredWatchedRoot is a root whose sizing walk established observed bytes. With no
// gaps it is exact; any gap makes it a partial lower bound. GapUnavailable belongs to
// an unmeasured root only.
func MeasuredWatchedRoot(path string, observed int64, gaps ...CapacityGap) (WatchedRoot, error) {
	if path == "" {
		return WatchedRoot{}, fmt.Errorf("watched root needs a path")
	}
	if observed < 0 {
		return WatchedRoot{}, fmt.Errorf("observed bytes %d of %q must not be negative", observed, path)
	}
	present := make(map[CapacityGap]bool, len(gaps))
	for _, g := range gaps {
		if g == GapUnavailable || !g.valid() {
			return WatchedRoot{}, fmt.Errorf("%q is not a gap of a measured watched root", g)
		}
		present[g] = true
	}
	r := WatchedRoot{path: path, measurement: CapacityExact, bytes: observed}
	for _, g := range capacityGaps {
		if present[g] {
			r.gaps = append(r.gaps, g)
		}
	}
	if len(r.gaps) > 0 {
		r.measurement = CapacityPartial
	}
	return r, nil
}

// UnmeasuredWatchedRoot is a root that was found but not sized: it could not be
// opened (GapUnavailable) or the audit's shared budget was already spent
// (GapAggregateBudget). It establishes nothing about the root's size.
func UnmeasuredWatchedRoot(path string, reason CapacityGap) (WatchedRoot, error) {
	if path == "" {
		return WatchedRoot{}, fmt.Errorf("watched root needs a path")
	}
	if reason != GapUnavailable && reason != GapAggregateBudget {
		return WatchedRoot{}, fmt.Errorf("%q is not a reason a watched root is unmeasured", reason)
	}
	return WatchedRoot{path: path, measurement: CapacityUnmeasured, gaps: []CapacityGap{reason}}, nil
}

// Path is the root's native slash-separated project-relative path.
func (r WatchedRoot) Path() string { return r.path }

// Measurement reports how much of the root was established.
func (r WatchedRoot) Measurement() CapacityMeasurement { return r.measurement }

// ObservedBytes returns the observed sum and whether one exists (exact or partial).
func (r WatchedRoot) ObservedBytes() (int64, bool) {
	return r.bytes, r.measurement == CapacityExact || r.measurement == CapacityPartial
}

// Gaps returns the reasons the measurement is not exact, in canonical order.
func (r WatchedRoot) Gaps() []CapacityGap { return append([]CapacityGap(nil), r.gaps...) }

// reaches reports whether the root's observed bytes reach threshold. A partial
// figure at or above it suffices; below it, it proves nothing.
func (r WatchedRoot) reaches(threshold int64) bool {
	b, ok := r.ObservedBytes()
	return ok && threshold > 0 && b >= threshold
}

// WatchedRootAudit is the result of an explicit watched-root capacity audit. It keeps
// three coverage facts apart: whether the search for matching roots was exhaustive,
// how completely each found root was measured, and how many found roots the bounded
// detail list omits. Counts and the byte sum are over the roots found; only an
// exhaustive search makes them counts over every matching root. The zero value is an
// unrequested audit.
type WatchedRootAudit struct {
	requested   bool
	selectors   []string
	threshold   int64
	searchGaps  []CapacityGap
	found       int
	exact       int
	partial     int
	unmeasured  int
	observed    int64
	reaching    int
	detail      []WatchedRoot
	detailLimit int
}

// Requested reports whether the audit ran at all; an unrequested audit carries no
// other fact.
func (a WatchedRootAudit) Requested() bool { return a.requested }

// Selectors returns the effective selectors the audit searched for.
func (a WatchedRootAudit) Selectors() []string { return append([]string(nil), a.selectors...) }

// Threshold returns the per-root warning threshold and whether one is configured.
func (a WatchedRootAudit) Threshold() (int64, bool) { return a.threshold, a.threshold > 0 }

// SearchComplete reports whether every directory that could hold a matching root
// was searched.
func (a WatchedRootAudit) SearchComplete() bool { return len(a.searchGaps) == 0 }

// SearchGaps returns why the search was not exhaustive, in canonical order.
func (a WatchedRootAudit) SearchGaps() []CapacityGap {
	return append([]CapacityGap(nil), a.searchGaps...)
}

// Found is the number of matching roots the search found.
func (a WatchedRootAudit) Found() int { return a.found }

// Total returns the number of matching roots in the project and whether it is known:
// only an exhaustive search knows it.
func (a WatchedRootAudit) Total() (int, bool) { return a.found, a.SearchComplete() }

// Counts returns how many found roots were measured exactly, partially, or not at all.
func (a WatchedRootAudit) Counts() (exact, partial, unmeasured int) {
	return a.exact, a.partial, a.unmeasured
}

// ObservedBytes is the sum of the observed figures of the found roots.
func (a WatchedRootAudit) ObservedBytes() int64 { return a.observed }

// ObservedBytesComplete reports whether ObservedBytes is the exact logical size of
// every matching root: the search was exhaustive and every found root exact.
// Otherwise the sum is a lower bound over what was inspected.
func (a WatchedRootAudit) ObservedBytesComplete() bool {
	return a.SearchComplete() && a.partial == 0 && a.unmeasured == 0
}

// Reaching is the number of found roots whose observed bytes reach the threshold
// (zero without one). Partial figures below the threshold are not counted either way.
func (a WatchedRootAudit) Reaching() int { return a.reaching }

// Detail returns the retained roots, largest observed figure first (unmeasured roots
// last, ties by path). It is a selection from the roots found, not a ranking of every
// directory in the project.
func (a WatchedRootAudit) Detail() []WatchedRoot { return append([]WatchedRoot(nil), a.detail...) }

// DetailLimit is the most roots Detail retains.
func (a WatchedRootAudit) DetailLimit() int { return a.detailLimit }

// Omitted is how many found roots Detail leaves out. It is always known, because
// every found root is counted; it says nothing about roots the search never reached.
func (a WatchedRootAudit) Omitted() int { return a.found - len(a.detail) }

// Reaches reports whether r reaches the audit's threshold.
func (a WatchedRootAudit) Reaches(r WatchedRoot) bool { return r.reaches(a.threshold) }

// WatchedRootTally accumulates an audit in a bounded amount of memory: scalar counts
// and sums over every root added, and at most the detail limit of retained roots.
type WatchedRootTally struct {
	a WatchedRootAudit
}

// NewWatchedRootTally starts an audit over selectors. threshold is the per-root
// warning size, or zero for none; detailLimit bounds the retained roots and must be
// positive.
func NewWatchedRootTally(selectors []string, threshold int64, detailLimit int) (*WatchedRootTally, error) {
	if threshold < 0 {
		return nil, fmt.Errorf("watched root threshold %d must not be negative", threshold)
	}
	if detailLimit <= 0 {
		return nil, fmt.Errorf("watched root detail limit %d must be positive", detailLimit)
	}
	// A configured selector may repeat a built-in; it still names one boundary.
	var unique []string
	for _, s := range selectors {
		if !slices.Contains(unique, s) {
			unique = append(unique, s)
		}
	}
	return &WatchedRootTally{a: WatchedRootAudit{
		requested:   true,
		selectors:   unique,
		threshold:   threshold,
		detailLimit: detailLimit,
	}}, nil
}

// Add counts one found root and keeps it in the detail list if it ranks among the
// retained roots.
func (t *WatchedRootTally) Add(r WatchedRoot) {
	a := &t.a
	a.found++
	switch r.measurement {
	case CapacityExact:
		a.exact++
	case CapacityPartial:
		a.partial++
	default:
		a.unmeasured++
	}
	if b, ok := r.ObservedBytes(); ok {
		a.observed += b
	}
	if r.reaches(a.threshold) {
		a.reaching++
	}
	i := sort.Search(len(a.detail), func(i int) bool { return ranksBefore(r, a.detail[i]) })
	if i >= a.detailLimit {
		return
	}
	if len(a.detail) < a.detailLimit {
		a.detail = append(a.detail, WatchedRoot{})
	}
	copy(a.detail[i+1:], a.detail[i:len(a.detail)-1])
	a.detail[i] = r
}

// SearchIncomplete records why the search for roots was not exhaustive. Only work
// and read gaps describe a search.
func (t *WatchedRootTally) SearchIncomplete(g CapacityGap) error {
	if g != GapWorkBudget && g != GapUnreadableEntries {
		return fmt.Errorf("%q is not a gap of a watched-root search", g)
	}
	present := map[CapacityGap]bool{g: true}
	for _, have := range t.a.searchGaps {
		present[have] = true
	}
	t.a.searchGaps = t.a.searchGaps[:0]
	for _, k := range capacityGaps {
		if present[k] {
			t.a.searchGaps = append(t.a.searchGaps, k)
		}
	}
	return nil
}

// Audit returns the accumulated audit.
func (t *WatchedRootTally) Audit() WatchedRootAudit {
	a := t.a
	a.selectors = append([]string(nil), a.selectors...)
	a.searchGaps = append([]CapacityGap(nil), a.searchGaps...)
	a.detail = append([]WatchedRoot(nil), a.detail...)
	return a
}

// ranksBefore orders the detail list: a larger observed figure first, unmeasured
// roots after every measured one, and the path as a deterministic tie-break.
func ranksBefore(x, y WatchedRoot) bool {
	xb, xok := x.ObservedBytes()
	yb, yok := y.ObservedBytes()
	switch {
	case xok != yok:
		return xok
	case xb != yb:
		return xb > yb
	}
	return x.path < y.path
}
