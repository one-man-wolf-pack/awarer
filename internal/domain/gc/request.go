package gc

import (
	"fmt"
	"time"
)

// KeepLastDefault signals that no explicit --keep-last was given and GC should fall
// back to the configured retention count. A non-negative KeepLast overrides config.
const KeepLastDefault = -1

// GCRequest is the validated, immutable description of one GC invocation: whether
// it mutates, which subsystem it may delete from, and the retention knobs that
// tighten the default policy. It carries no project or config — those are passed to
// the planner separately — so it is a pure value the CLI builds from flags.
type GCRequest struct {
	dryRun    bool
	filter    SubsystemFilter
	keepLast  int
	olderThan time.Duration
	committed bool
}

// NewGCRequest builds a validated request. filter must be a known filter; keepLast
// must be KeepLastDefault or non-negative; olderThan must be non-negative (zero
// means "unset", falling back to the configured run-retention window).
func NewGCRequest(dryRun bool, filter SubsystemFilter, keepLast int, olderThan time.Duration, committed bool) (GCRequest, error) {
	if !filter.Valid() {
		return GCRequest{}, fmt.Errorf("invalid subsystem filter %d", filter)
	}
	if keepLast < 0 && keepLast != KeepLastDefault {
		return GCRequest{}, fmt.Errorf("keep-last must not be negative, got %d", keepLast)
	}
	if olderThan < 0 {
		return GCRequest{}, fmt.Errorf("older-than must not be negative, got %s", olderThan)
	}
	return GCRequest{
		dryRun:    dryRun,
		filter:    filter,
		keepLast:  keepLast,
		olderThan: olderThan,
		committed: committed,
	}, nil
}

func (r GCRequest) DryRun() bool             { return r.dryRun }
func (r GCRequest) Filter() SubsystemFilter  { return r.filter }
func (r GCRequest) Committed() bool          { return r.committed }
func (r GCRequest) OlderThan() time.Duration { return r.olderThan }

// HasOlderThan reports whether an explicit --older-than window was set.
func (r GCRequest) HasOlderThan() bool { return r.olderThan > 0 }

// KeepLast returns the effective keep-last count: the request value if it was set,
// otherwise the supplied configured default. The result is clamped to be
// non-negative.
func (r GCRequest) KeepLast(configDefault int) int {
	n := r.keepLast
	if n == KeepLastDefault {
		n = configDefault
	}
	if n < 0 {
		n = 0
	}
	return n
}
