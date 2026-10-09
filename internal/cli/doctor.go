package cli

import (
	"fmt"
	"strings"

	"awarer/internal/app/configcmd"
	doctorapp "awarer/internal/app/doctor"
	domdoctor "awarer/internal/domain/doctor"
	"awarer/internal/infra/lockfile"
	"awarer/internal/output"
)

// runDoctor handles "awa doctor". It inspects the project's durable .awa state and
// prints a concise report; with --repair it also performs the safe, mechanical
// repairs. Verification depth is raised by the global --strict (trust-strict) flag,
// which for doctor means recomputing hashes rather than trusting stat signatures.
// The command takes the local --repair and --capacity flags; --root/--config/--json/
// --strict are global. It does not consume operands: --capacity audits the
// configured watched roots, never a path named on the command line.
func runDoctor(w *output.Writer, inv invocation) error {
	repair, capacity := false, false
	args := inv.args
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, _, hasValue := splitFlag(tok)
		switch name {
		case "--repair":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			repair = true
		case "--capacity":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			capacity = true
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for doctor", tok)
			}
			return usageErrorf("unexpected argument %q for doctor", tok)
		}
	}

	proj, err := resolveProject(inv.options)
	if err != nil {
		return err
	}
	layout, err := proj.Paths()
	if err != nil {
		return err
	}

	// The project supplies the storage location; the shared awa.toml and local
	// .awa/config.toml layer under any --config override, so doctor validates the
	// same composed config the user points it at. An invalid layer is a config error
	// (exit 4), surfaced before any check runs.
	files, err := projectLayerFiles(layout, inv.options)
	if err != nil {
		return err
	}
	loaded, err := configcmd.LoadLayered(files, trustModeOverride(inv.options))
	if err != nil {
		return classifyConfigLoad(err)
	}
	cfg := loaded.Config()

	// Doctor's "strict" is the global trust-strict mode: do not trust stat-signature
	// reuse for integrity claims; recompute hashes where the scanner/index supports
	// it.
	strict := inv.options.has(capTrustMode) && inv.options.TrustMode == TrustStrict

	svc := doctorapp.New(doctorapp.Deps{
		Locks: newCollectorLocker(inv.invCtx(), layout, lockfile.OpDoctorRepair, cfg.Locks.Timeout.Duration()),
	})
	res, err := svc.Run(inv.invCtx(), doctorapp.Request{
		Project:  proj,
		Resolved: loaded,
		Strict:   strict,
		Repair:   repair,
		Capacity: capacity,
	})
	if err != nil {
		// A failure that prevented diagnosis or the repair pass at all (e.g. the layout
		// could not be resolved, or --repair could not acquire its lock before a
		// collector's timeout). Ordinary corruption is reported as findings, not here.
		if c := interruptError(err); c != nil {
			return c
		}
		if coded := lockErrorCode(err); coded != nil {
			return coded
		}
		return genericErrorf("%v", err)
	}

	if inv.options.JSON {
		if err := w.JSON("doctor", doctorViewOf(res)); err != nil {
			return err
		}
	} else {
		renderDoctor(w, res)
	}

	// Exit code is the diagnosis, not an awa failure: like "awa run", doctor's own
	// outcome drives the process exit with no "awa: " error line. Unrepaired errors
	// or unrepaired repairable findings mean local state needs an action (exit 5) —
	// which is not the same as damage, since an unrepaired guard file earns it too; a
	// healthy or warnings-only run exits 0.
	if res.NeedsAttention() {
		return &commandExit{code: int(ExitStateActionRequired)}
	}
	return nil
}

// doctorView is the stable JSON shape of a doctor report. Findings carry their own
// severity, so a single list serves machine consumers that filter by severity or
// repairability; the summary mirrors the derived counts.
type doctorView struct {
	Health        string              `json:"health"`
	Summary       doctorSummaryView   `json:"summary"`
	Findings      []doctorFindingView `json:"findings"`
	StoreCapacity storeCapacityView   `json:"store_capacity"`
	// WatchedRoots is null unless --capacity requested the watched-root audit, so an
	// unrequested audit never reads as one that found nothing.
	WatchedRoots *watchedRootsView `json:"watched_root_capacity"`
}

// watchedRootsView projects the explicit watched-root audit. Its three coverage facts
// stay apart: search says whether every matching root was looked for, each root's
// measurement says how completely it was sized, and detail says how many found roots
// the list leaves out. RootsTotal is null unless the search was exhaustive, and
// ObservedBytes is a lower bound unless ObservedBytesComplete. RootsReachingThreshold
// is null without a threshold.
type watchedRootsView struct {
	Scope                  string                 `json:"scope"`
	Metric                 string                 `json:"metric"`
	Selectors              []string               `json:"selectors"`
	ThresholdBytes         *int64                 `json:"threshold_bytes"`
	Search                 watchedSearchView      `json:"search"`
	RootsFound             int                    `json:"roots_found"`
	RootsTotal             *int                   `json:"roots_total"`
	Measurements           watchedCountsView      `json:"measurements"`
	ObservedBytes          int64                  `json:"observed_bytes"`
	ObservedBytesComplete  bool                   `json:"observed_bytes_complete"`
	RootsReachingThreshold *int                   `json:"roots_reaching_threshold"`
	Roots                  []watchedRootView      `json:"roots"`
	Detail                 watchedRootsDetailView `json:"roots_detail"`
}

type watchedSearchView struct {
	Complete          bool     `json:"complete"`
	IncompleteReasons []string `json:"incomplete_reasons"`
}

type watchedCountsView struct {
	Exact      int `json:"exact"`
	Partial    int `json:"partial"`
	Unmeasured int `json:"unmeasured"`
}

type watchedRootView struct {
	Path              string   `json:"path"`
	Measurement       string   `json:"measurement"`
	ObservedBytes     *int64   `json:"observed_bytes"`
	IncompleteReasons []string `json:"incomplete_reasons"`
}

type watchedRootsDetailView struct {
	Complete bool `json:"complete"`
	Shown    int  `json:"shown"`
	Limit    int  `json:"limit"`
	Omitted  int  `json:"omitted"`
}

// storeCapacityView projects the store capacity advisory's measurement as facts, not
// prose. ObservedBytes is null unless the measurement is exact or partial (then a
// lower bound over the inspected files); ThresholdBytes is null when the advisory is
// disabled; IncompleteReasons is empty exactly when the figure is exact or disabled.
type storeCapacityView struct {
	Scope             string   `json:"scope"`
	Metric            string   `json:"metric"`
	Measurement       string   `json:"measurement"`
	ObservedBytes     *int64   `json:"observed_bytes"`
	ThresholdBytes    *int64   `json:"threshold_bytes"`
	IncompleteReasons []string `json:"incomplete_reasons"`
}

type doctorSummaryView struct {
	Passed     int `json:"passed"`
	Warnings   int `json:"warnings"`
	Errors     int `json:"errors"`
	Repairable int `json:"repairable"`
	Repaired   int `json:"repaired"`
}

type doctorFindingView struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	Subsystem  string `json:"subsystem"`
	Path       string `json:"path,omitempty"`
	Subject    string `json:"subject"`
	Message    string `json:"message"`
	Repairable bool   `json:"repairable"`
	Repaired   bool   `json:"repaired"`
}

func doctorViewOf(res domdoctor.DoctorResult) doctorView {
	s := res.Summary()
	v := doctorView{
		Health: res.Health().String(),
		Summary: doctorSummaryView{
			Passed:     s.Passed,
			Warnings:   s.Warnings,
			Errors:     s.Errors,
			Repairable: s.Repairable,
			Repaired:   s.Repaired,
		},
		Findings:      []doctorFindingView{},
		StoreCapacity: storeCapacityViewOf(res.StoreCapacity()),
		WatchedRoots:  watchedRootsViewOf(res.WatchedRoots()),
	}
	for _, f := range res.Findings() {
		v.Findings = append(v.Findings, doctorFindingView{
			Code:       f.Code().String(),
			Severity:   f.Severity().String(),
			Subsystem:  string(f.Subsystem()),
			Path:       f.Path(),
			Subject:    f.Subject(),
			Message:    f.Message(),
			Repairable: f.Repairable(),
			Repaired:   f.Repaired(),
		})
	}
	return v
}

func storeCapacityViewOf(c domdoctor.StoreCapacity) storeCapacityView {
	v := storeCapacityView{
		Scope:             domdoctor.StoreCapacityScope,
		Metric:            domdoctor.StoreCapacityMetric,
		Measurement:       c.Measurement().String(),
		IncompleteReasons: []string{},
	}
	if b, ok := c.ObservedBytes(); ok {
		v.ObservedBytes = &b
	}
	if th, ok := c.Threshold(); ok {
		v.ThresholdBytes = &th
	}
	for _, g := range c.Gaps() {
		v.IncompleteReasons = append(v.IncompleteReasons, string(g))
	}
	return v
}

func watchedRootsViewOf(a domdoctor.WatchedRootAudit) *watchedRootsView {
	if !a.Requested() {
		return nil
	}
	exact, partial, unmeasured := a.Counts()
	v := &watchedRootsView{
		Scope:                 domdoctor.WatchedRootScope,
		Metric:                domdoctor.StoreCapacityMetric,
		Selectors:             a.Selectors(),
		Search:                watchedSearchView{Complete: a.SearchComplete(), IncompleteReasons: gapStrings(a.SearchGaps())},
		RootsFound:            a.Found(),
		Measurements:          watchedCountsView{Exact: exact, Partial: partial, Unmeasured: unmeasured},
		ObservedBytes:         a.ObservedBytes(),
		ObservedBytesComplete: a.ObservedBytesComplete(),
		Roots:                 []watchedRootView{},
		Detail: watchedRootsDetailView{
			Complete: a.Omitted() == 0,
			Shown:    len(a.Detail()),
			Limit:    a.DetailLimit(),
			Omitted:  a.Omitted(),
		},
	}
	if total, ok := a.Total(); ok {
		v.RootsTotal = &total
	}
	if th, ok := a.Threshold(); ok {
		reaching := a.Reaching()
		v.ThresholdBytes = &th
		v.RootsReachingThreshold = &reaching
	}
	for _, r := range a.Detail() {
		rv := watchedRootView{Path: r.Path(), Measurement: r.Measurement().String(), IncompleteReasons: gapStrings(r.Gaps())}
		if b, ok := r.ObservedBytes(); ok {
			rv.ObservedBytes = &b
		}
		v.Roots = append(v.Roots, rv)
	}
	return v
}

func gapStrings(gaps []domdoctor.CapacityGap) []string {
	out := make([]string, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, string(g))
	}
	return out
}

// watchedRootsLines words the watched-root audit for the human report: one summary
// line keeping search, measurement, and detail coverage apart, then one line per
// listed root. It prints nothing for an unrequested audit.
func watchedRootsLines(a domdoctor.WatchedRootAudit) []string {
	if !a.Requested() {
		return nil
	}
	search := "search complete"
	if !a.SearchComplete() {
		search = "search incomplete: " + strings.Join(gapStrings(a.SearchGaps()), ", ")
	}
	warn := "no size warning (doctor.watched_root_warning_size off)"
	if th, ok := a.Threshold(); ok {
		warn = fmt.Sprintf("%d at or above %s", a.Reaching(), humanizeBytes(th))
	}
	if a.Found() == 0 {
		found := "none found"
		if !a.SearchComplete() {
			found = "none found so far"
		}
		return []string{fmt.Sprintf("watched roots: %s (%s); %s", found, search, warn)}
	}
	exact, partial, unmeasured := a.Counts()
	lines := []string{fmt.Sprintf("watched roots: %d found (%s), %s; %d exact, %d partial, %d unmeasured; %s",
		a.Found(), search, logicalBytes(a.ObservedBytes(), a.ObservedBytesComplete()), exact, partial, unmeasured, warn)}
	for _, r := range a.Detail() {
		lines = append(lines, "  "+watchedRootLine(r))
	}
	if n := a.Omitted(); n > 0 {
		lines = append(lines, fmt.Sprintf("  ... %d more found root(s) not listed (%d with the largest observed figures shown)", n, len(a.Detail())))
	}
	return lines
}

func watchedRootLine(r domdoctor.WatchedRoot) string {
	gaps := strings.Join(gapStrings(r.Gaps()), ", ")
	b, ok := r.ObservedBytes()
	switch {
	case !ok:
		return fmt.Sprintf("%s: unmeasured (%s)", r.Path(), gaps)
	case r.Measurement() == domdoctor.CapacityPartial:
		return fmt.Sprintf("%s: at least %s (partial: %s)", r.Path(), humanizeBytes(b), gaps)
	case b == 0:
		return r.Path() + ": empty"
	}
	return fmt.Sprintf("%s: %s", r.Path(), humanizeBytes(b))
}

// storeCapacityLine words the store measurement for the human report: the figure
// with its completeness, the threshold it is compared with, or why there is none.
func storeCapacityLine(c domdoctor.StoreCapacity) string {
	threshold, _ := c.Threshold()
	gaps := make([]string, 0, len(c.Gaps()))
	for _, g := range c.Gaps() {
		gaps = append(gaps, string(g))
	}
	switch c.Measurement() {
	case domdoctor.CapacityDisabled:
		return "store: .awa/ size advisory off (doctor.store_warning_size)"
	case domdoctor.CapacityUnmeasured:
		return fmt.Sprintf("store: .awa/ size unmeasured (%s); warning at %s", strings.Join(gaps, ", "), humanizeBytes(threshold))
	}
	observed, _ := c.ObservedBytes()
	line := fmt.Sprintf("store: %s in .awa/", logicalBytes(observed, c.Measurement() == domdoctor.CapacityExact))
	if len(gaps) > 0 {
		line += " (partial: " + strings.Join(gaps, ", ") + ")"
	}
	return line + "; warning at " + humanizeBytes(threshold)
}

// renderDoctor prints the human report: a one-line verdict for a healthy project,
// or the verdict plus one line per finding grouped by the order they were found.
// The tag favors the action a reader should take — [repaired] for a fixed finding,
// [repairable] for one that --repair would fix, otherwise the severity.
func renderDoctor(w *output.Writer, res domdoctor.DoctorResult) {
	s := res.Summary()
	w.Linef("health: %s", res.Health().String())

	findings := res.Findings()
	if len(findings) > 0 {
		w.Line("")
		for _, f := range findings {
			w.Linef("[%s] %s %s — %s", findingTag(f), f.Code(), f.Subject(), f.Message())
		}
		w.Line("")
	}
	w.Line(storeCapacityLine(res.StoreCapacity()))
	for _, line := range watchedRootsLines(res.WatchedRoots()) {
		w.Line(line)
	}
	w.Linef("checks: %d passed, %d warnings, %d errors", s.Passed, s.Warnings, s.Errors)
	// Summary.Repairable counts every repairable finding including those already
	// fixed, so the still-fixable tally subtracts the repaired ones.
	if s.Repairable > 0 {
		w.Linef("repairs: %d repaired, %d repairable", s.Repaired, s.Repairable-s.Repaired)
	}
}

// findingTag chooses the bracketed label for a finding line.
func findingTag(f domdoctor.Finding) string {
	switch {
	case f.Repaired():
		return "repaired"
	case f.Repairable():
		return "repairable"
	default:
		return f.Severity().String()
	}
}
