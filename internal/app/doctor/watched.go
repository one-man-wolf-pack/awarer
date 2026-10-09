package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	domconfig "awarer/internal/domain/config"
	"awarer/internal/domain/doctor"
	"awarer/internal/infra/fsx"
)

// WatchedLimits bounds the explicit watched-root audit (doctor --capacity). Each bound
// is separate because each answers a separate coverage question: the search entries
// and directories bound finding roots, the per-root entries and depth bound sizing one
// root, the aggregate entries bound sizing every root together, and the detail limit
// bounds the roots the report lists. They bound work and retained memory, not elapsed
// time; reaching one is reported as a typed gap, never as a smaller exact answer. A
// zero field takes the production default.
type WatchedLimits struct {
	SearchEntries    int64
	SearchDirs       int
	RootEntries      int64
	RootDepth        int
	AggregateEntries int64
	Detail           int
}

// The audit's default bounds. The search entry budget matches the run's effect
// discovery, so the audit can find the roots run observes; the directory budget caps
// the search frontier, the only search collection that could otherwise grow with the
// tree. A root is sized as far as the store walk would size .awa/, and all roots
// together four times that, so one huge dependency tree cannot leave every other root
// unmeasured. Output trees nest deeper than awa's own store, hence the deeper limit.
const (
	defaultWatchedSearchEntries    = 2_000_000
	defaultWatchedSearchDirs       = 100_000
	defaultWatchedRootEntries      = 1_000_000
	defaultWatchedRootDepth        = 64
	defaultWatchedAggregateEntries = 4_000_000
	defaultWatchedDetail           = 20
)

func (l WatchedLimits) withDefaults() WatchedLimits {
	if l.SearchEntries <= 0 {
		l.SearchEntries = defaultWatchedSearchEntries
	}
	if l.SearchDirs <= 0 {
		l.SearchDirs = defaultWatchedSearchDirs
	}
	if l.RootEntries <= 0 {
		l.RootEntries = defaultWatchedRootEntries
	}
	if l.RootDepth <= 0 {
		l.RootDepth = defaultWatchedRootDepth
	}
	if l.AggregateEntries <= 0 {
		l.AggregateEntries = defaultWatchedAggregateEntries
	}
	if l.Detail <= 0 {
		l.Detail = defaultWatchedDetail
	}
	return l
}

// listDirFunc is the watched-root search's listing seam: eachScanDirEntry in
// production, a counting wrapper where a test must prove that plain doctor searches
// nothing.
type listDirFunc func(root, dir string, fn func(os.DirEntry) error) error

// errStopSearch ends the root search once one of its budgets is spent.
var errStopSearch = errors.New("doctor: watched-root search budget reached")

// auditWatched runs the explicit watched-root capacity audit. It searches the project
// for real directories the effective selectors select — asking the selector owner,
// never matching on its own — sizes each found root stat-only, and records a
// non-repairable warning for each listed root whose observed bytes reach the
// configured threshold. It changes nothing on disk and shares no work with the
// integrity checks, which have already run. Only cancellation is returned.
func (s *Service) auditWatched(ctx context.Context, root string, cfg domconfig.Config, rep *report) (doctor.WatchedRootAudit, error) {
	selectors := domconfig.EffectiveEffectRoots(cfg.Run.WatchedEffectRoots)
	var threshold int64
	if warn := cfg.Doctor.WatchedRootWarningSize; warn.Enabled() {
		threshold = warn.Size().Bytes()
	}
	lim := s.watched
	tally, err := doctor.NewWatchedRootTally(selectors, threshold, lim.Detail)
	if err != nil {
		return doctor.WatchedRootAudit{}, err
	}
	a := &watchedAudit{
		ctx:       ctx,
		root:      root,
		lim:       lim,
		selects:   domconfig.NewEffectRootMatcher(selectors),
		protected: make(map[string]bool),
		tally:     tally,
		list:      s.listWatched,
		left:      lim.AggregateEntries,
	}
	for _, p := range domconfig.ProtectedExcludes() {
		a.protected[p] = true
	}
	if err := a.search(); err != nil {
		return doctor.WatchedRootAudit{}, err
	}
	audit := tally.Audit()

	warned := 0
	for _, r := range audit.Detail() {
		if audit.Reaches(r) {
			warned++
			rep.add(finding(doctor.CodeWatchedRootCapacityThreshold, doctor.SeverityWarning, doctor.SubsystemCapacity,
				filepath.Join(root, filepath.FromSlash(r.Path())), r.Path(), watchedMessage(r, threshold), false))
		}
	}
	// An exhaustive, exact audit with nothing at its threshold is a pass. Anything
	// less is neither a pass nor a finding: the audit facts disclose it.
	if warned == 0 && audit.ObservedBytesComplete() {
		rep.pass()
	}
	return audit, nil
}

// watchedAudit is one audit's traversal state: the search frontier, the budgets
// left, and the bounded tally. Nothing in it grows with the files measured; the
// frontier is capped by SearchDirs and the tally by the detail limit.
type watchedAudit struct {
	ctx       context.Context
	root      string
	lim       WatchedLimits
	selects   domconfig.EffectRootMatcher
	protected map[string]bool
	tally     *doctor.WatchedRootTally
	list      listDirFunc

	examined  int64 // search entries examined
	committed int   // directories listed or queued for listing
	left      int64 // aggregate sizing entries left
}

// search walks the project depth-first for selected directories. It never descends
// into a selected root (the outer root owns its subtree, so a nested match is not a
// second root), a protected name, or a symlink. The root is opened as the trusted
// anchor the caller resolved and every directory below it no-follow, under the same
// native paths the nested-marker scan uses, so a name store path grammar could not
// express is still searched. An unreadable directory leaves the search incomplete and
// the walk continues; a spent budget stops it.
func (a *watchedAudit) search() error {
	stack := []string{a.root}
	a.committed = 1
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		err := a.list(a.root, dir, func(e os.DirEntry) error {
			if err := a.ctx.Err(); err != nil {
				return err
			}
			if a.examined >= a.lim.SearchEntries {
				return errStopSearch
			}
			a.examined++
			name := e.Name()
			if a.protected[name] || !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				return nil
			}
			full := filepath.Join(dir, name)
			rel := a.relOf(full)
			if a.selects.Selects(name, rel) {
				r, err := a.measure(full, rel)
				if err != nil {
					return err
				}
				a.tally.Add(r)
				return nil
			}
			if a.committed >= a.lim.SearchDirs {
				return errStopSearch
			}
			a.committed++
			stack = append(stack, full)
			return nil
		})
		switch {
		case err == nil:
		case a.ctx.Err() != nil:
			return a.ctx.Err()
		case errors.Is(err, errStopSearch):
			return a.tally.SearchIncomplete(doctor.GapWorkBudget)
		default:
			if terr := a.tally.SearchIncomplete(doctor.GapUnreadableEntries); terr != nil {
				return terr
			}
		}
	}
	return nil
}

// relOf renders a path the search built under root as its slash-separated
// project-relative form, the form exact selectors are written in. Only the platform
// separator is converted, so a Unix name containing a backslash keeps it.
func (a *watchedAudit) relOf(full string) string {
	rel, err := filepath.Rel(a.root, full)
	if err != nil {
		return filepath.ToSlash(full)
	}
	return filepath.ToSlash(rel)
}

// measure sizes one found root stat-only under the per-root budget, drawn from the
// aggregate budget, and returns its measurement. A root the aggregate budget no
// longer reaches, or one that cannot be opened as a directory, is unmeasured; only
// cancellation is an error.
func (a *watchedAudit) measure(full, rel string) (doctor.WatchedRoot, error) {
	if a.left <= 0 {
		return doctor.UnmeasuredWatchedRoot(rel, doctor.GapAggregateBudget)
	}
	budget := min(a.lim.RootEntries, a.left)
	size, err := sizeWatchedRoot(a.ctx, full, fsx.TreeLimits{MaxEntries: budget, MaxDepth: a.lim.RootDepth}, a.protected)
	if err != nil {
		if cerr := a.ctx.Err(); cerr != nil {
			return doctor.WatchedRoot{}, cerr
		}
		return doctor.UnmeasuredWatchedRoot(rel, doctor.GapUnavailable)
	}
	a.left -= size.Entries
	var gaps []doctor.CapacityGap
	if size.EntryBudgetReached {
		if budget < a.lim.RootEntries {
			gaps = append(gaps, doctor.GapAggregateBudget)
		} else {
			gaps = append(gaps, doctor.GapWorkBudget)
		}
	}
	if size.DepthLimitReached {
		gaps = append(gaps, doctor.GapDepthLimit)
	}
	if size.Unreadable {
		gaps = append(gaps, doctor.GapUnreadableEntries)
	}
	return doctor.MeasuredWatchedRoot(rel, size.Bytes, gaps...)
}

// sizeWatchedRoot opens a found root no-follow, confirms it is still a directory,
// and sizes its contents, leaving protected subtrees out at any depth just as the
// search does.
func sizeWatchedRoot(ctx context.Context, full string, lim fsx.TreeLimits, protected map[string]bool) (fsx.BoundedTreeSize, error) {
	d, err := fsx.OpenNoFollow(full)
	if err != nil {
		return fsx.BoundedTreeSize{}, err
	}
	defer func() { _ = d.Close() }()
	info, err := d.Stat()
	if err != nil {
		return fsx.BoundedTreeSize{}, err
	}
	if !info.IsDir() {
		return fsx.BoundedTreeSize{}, fmt.Errorf("%s is no longer a directory", full)
	}
	return fsx.SizeDirWithin(ctx, d, lim, protected)
}

// watchedMessage words one root's threshold finding: the figure with its
// completeness, the threshold, what the figure is not, and that doctor removes
// nothing.
func watchedMessage(r doctor.WatchedRoot, threshold int64) string {
	observed, _ := r.ObservedBytes()
	amount := fmt.Sprintf("%d", observed)
	if r.Measurement() == doctor.CapacityPartial {
		gaps := make([]string, 0, len(r.Gaps()))
		for _, g := range r.Gaps() {
			gaps = append(gaps, string(g))
		}
		amount = fmt.Sprintf("at least %d (partial: %s)", observed, strings.Join(gaps, ", "))
	}
	return fmt.Sprintf("regular files under this watched root hold %s logical bytes, reaching doctor.watched_root_warning_size (%d bytes); "+
		"this is not disk usage, gc does not manage project output, and doctor deletes nothing for it — "+
		"review what produces it or adjust the threshold", amount, threshold)
}
