package doctor

import (
	"context"
	"fmt"
	"os"
	"strings"

	domconfig "awarer/internal/domain/config"
	"awarer/internal/domain/doctor"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/fsx"
)

// The store sizing walk's default work bounds. They bound work and retained
// resources, not elapsed time or a minimum measured byte count. The entry budget caps
// the no-follow stats one doctor run spends on sizing; a store with more entries
// reports a partial lower bound, which warns only if it already reaches the
// threshold and is otherwise disclosed as incomplete. The depth limit is far below
// anything awa writes (.awa/ nests about six levels), so it only bounds descriptors
// on a store something else has filled with a deep tree; the deeper levels are
// reported unmeasured, never as empty.
const (
	defaultSizingMaxEntries = 1_000_000
	defaultSizingMaxDepth   = 32
)

// sizeTreeFunc is the sizing walk's seam: fsx.SizeTreeWithin in production, a
// counting stand-in where a test must prove that a disabled advisory walks nothing.
type sizeTreeFunc func(ctx context.Context, dir *os.File, name string, lim fsx.TreeLimits) (fsx.BoundedTreeSize, error)

// measureStore runs the store capacity advisory: it sizes .awa/ under the sizing
// bounds and records a non-repairable warning when the observed logical bytes reach
// the threshold. It runs after the integrity checks and shares none of their work, so
// a disabled, failed, or budget-stopped sizing never narrows what they inspected. Only
// cancellation is returned; every other sizing failure is an unmeasured store, never
// a zero.
func (s *Service) measureStore(ctx context.Context, layout paths.Layout, warn domconfig.ByteThreshold, rep *report) (doctor.StoreCapacity, error) {
	if !warn.Enabled() {
		return doctor.DisabledStoreCapacity(), nil
	}
	threshold := warn.Size().Bytes()
	size, err := s.sizeStore(ctx, layout)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return doctor.StoreCapacity{}, cerr
		}
		return doctor.UnmeasuredStoreCapacity(threshold)
	}
	var gaps []doctor.CapacityGap
	if size.EntryBudgetReached {
		gaps = append(gaps, doctor.GapWorkBudget)
	}
	if size.DepthLimitReached {
		gaps = append(gaps, doctor.GapDepthLimit)
	}
	if size.Unreadable {
		gaps = append(gaps, doctor.GapUnreadableEntries)
	}
	c, err := doctor.MeasuredStoreCapacity(size.Bytes, threshold, gaps...)
	if err != nil {
		return doctor.StoreCapacity{}, err
	}
	switch {
	case c.AtOrAboveThreshold():
		rep.add(finding(doctor.CodeStoreCapacityThreshold, doctor.SeverityWarning, doctor.SubsystemCapacity,
			layout.AwaDir(), "store", capacityMessage(c), false))
	case c.Measurement() == doctor.CapacityExact:
		rep.pass()
	}
	// A partial sum below the threshold is neither a pass nor a finding: it does not
	// establish a small store, and the report's capacity facts disclose it.
	return c, nil
}

// sizeStore sizes .awa no-follow beneath the trusted project root — the same anchor
// every store repository uses — under the configured bounds. The tree walk reads an
// absent or non-directory name as a known zero, which is right for a record but not
// for the store itself: a .awa that is missing or not a real directory leaves the
// store unmeasured.
func (s *Service) sizeStore(ctx context.Context, layout paths.Layout) (fsx.BoundedTreeSize, error) {
	root, err := os.Open(layout.Root())
	if err != nil {
		return fsx.BoundedTreeSize{}, err
	}
	defer func() { _ = root.Close() }()
	mode, _, err := fsx.LstatAt(root, paths.Dir)
	if err != nil {
		return fsx.BoundedTreeSize{}, err
	}
	if !mode.IsDir() {
		return fsx.BoundedTreeSize{}, fmt.Errorf("%s is not a directory", paths.Dir)
	}
	return s.sizeTree(ctx, root, paths.Dir, s.sizing)
}

// capacityMessage words the advisory finding: what crossed which threshold, how
// complete the figure is, what it is not, and the inspection to run next.
func capacityMessage(c doctor.StoreCapacity) string {
	observed, _ := c.ObservedBytes()
	threshold, _ := c.Threshold()
	amount := fmt.Sprintf("%d", observed)
	if c.Measurement() == doctor.CapacityPartial {
		gaps := make([]string, 0, len(c.Gaps()))
		for _, g := range c.Gaps() {
			gaps = append(gaps, string(g))
		}
		amount = fmt.Sprintf("at least %d (partial: %s)", observed, strings.Join(gaps, ", "))
	}
	return fmt.Sprintf("regular files under .awa/ hold %s logical bytes, reaching doctor.store_warning_size (%d bytes); "+
		"this is not disk usage or what gc would reclaim, and doctor deletes nothing for it — "+
		"review retention with `awa gc --dry-run` or adjust the threshold", amount, threshold)
}
