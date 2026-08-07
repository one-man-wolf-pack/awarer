package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	apprun "awarer/internal/app/run"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/evidence"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
	"awarer/internal/output"
	"awarer/internal/output/inspect"
)

// runHumanTime resolves the human time-display policy for a run subcommand: the
// --time flag value (empty for none) over the project's [ui].time default, plus a
// single "now" for relative formatting. Every subcommand that formats time accepts
// --config, so the [ui].time default is read from the same layered stack the rest
// of run uses — shared awa.toml, local .awa/config.toml, then an explicit --config
// override — via loadProjectConfigFull, not a config-ignoring subset.
func runHumanTime(opts GlobalOptions, timeFlag string) (domainconfig.TimeDisplay, time.Time, error) {
	_, _, loaded, err := loadProjectConfigFull(opts)
	if err != nil {
		return 0, time.Time{}, err
	}
	td, err := timeDisplayFromFlag(timeFlag, loaded.Config())
	if err != nil {
		return 0, time.Time{}, usageErrorf("%v", err)
	}
	return td, time.Now(), nil
}

// runShowHumanTime resolves the human --meta time display for "run show" without
// loading project config. run show is stored-only: a malformed awa.toml/config.toml
// must not block inspection of durable evidence, so the [ui].time default is not
// consulted. Without --time the default display is used; an explicit --time is honored
// (it was already validated at flag-parse time). The [ui].time preference is a display
// convenience, not part of the evidence, so it never gates access to it.
func runShowHumanTime(timeFlag string) (domainconfig.TimeDisplay, time.Time, error) {
	td, err := timeDisplayFromFlag(timeFlag, domainconfig.Defaults())
	if err != nil {
		return 0, time.Time{}, usageErrorf("%v", err)
	}
	return td, time.Now(), nil
}

// runRunLog handles "awa run log": list stored runs newest first.
func runRunLog(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	var limit int
	command := ""
	timeFlag := ""
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--limit", "-n":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return usageErrorf("invalid value %q for %s: want a positive integer", v, name)
			}
			limit = n
		case "--command":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			command = v
		case "--time":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			timeFlag = v
		default:
			return rejectExtra("run log", tok)
		}
	}

	// --time is a human display preference; JSON timestamps are always machine UTC, so
	// reject the combo instead of resolving and ignoring it.
	if inv.options.JSON && timeFlag != "" {
		return jsonTimeFlagError()
	}
	td, now, err := runHumanTime(inv.options, timeFlag)
	if err != nil {
		return err
	}

	svc, cleanup, err := buildRunService2(inv.invCtx(), inv.options)
	if err != nil {
		return err
	}
	defer cleanup()

	entries, err := svc.Log(inv.invCtx(), apprun.LogRequest{Limit: limit, Command: command})
	if err != nil {
		return classifyRunError(err)
	}

	if inv.options.JSON {
		return w.JSON("run.log", logDocs(entries))
	}
	if len(entries) == 0 {
		w.Line("no runs recorded")
		return nil
	}
	for _, e := range entries {
		w.Line(logLine(e, td, now))
	}
	return nil
}

// runRunLs handles "awa run ls": the current-state reuse view. Unlike "run log"
// (chronological history), it lists only runs that are reusable right now — a valid
// cache-hit candidate for the current project state — so an agent can skip rerunning
// checks already executed against the same inputs. With --all it also lists recent
// non-reusable runs, each carrying an explicit reason.
func runRunLs(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	var limit int
	command := ""
	all := false
	near := false
	timeFlag := ""
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--limit", "-n":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return usageErrorf("invalid value %q for %s: want a positive integer", v, name)
			}
			limit = n
		case "--command":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			command = v
		case "--all":
			if hasValue {
				return usageErrorf("%s takes no value", name)
			}
			all = true
		case "--near":
			if hasValue {
				return usageErrorf("%s takes no value", name)
			}
			near = true
		case "--time":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			timeFlag = v
		default:
			return rejectExtra("run ls", tok)
		}
	}

	// --time is a human display preference; JSON timestamps are always machine UTC, so
	// reject the combo instead of resolving and ignoring it.
	if inv.options.JSON && timeFlag != "" {
		return jsonTimeFlagError()
	}
	proj, layout, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}
	td, err := timeDisplayFromFlag(timeFlag, cfg)
	if err != nil {
		return usageErrorf("%v", err)
	}
	now := time.Now()
	// run ls re-observes each recent run's input state to say which of them would replay
	// right now. That is a cache decision, so it reads file content rather than trusting
	// a stat signature a same-size rewrite can leave untouched: a listing that promised a
	// hit the cache would not serve is worse than no listing. No index is wired — its
	// lookups would be skipped — and no presence lock is taken, since nothing here
	// writes.
	svc, cleanup, err := buildRunService(inv.invCtx(), layout, cfg, indexNone)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := svc.Ls(inv.invCtx(), apprun.LsRequest{
		Project: proj,
		Config:  cfg,
		Limit:   limit,
		Command: command,
		All:     all,
		Near:    near,
	})
	if err != nil {
		return classifyRunError(err)
	}

	if inv.options.JSON {
		// Plain and --near share one object family (lsView); both carry the latency
		// diagnostics structurally under data.diagnostics.performance, so machine output
		// never leaves performance facts on stderr. The corrupt-metadata count travels in
		// the payload; the near view still mirrors it to stderr as a human warning while
		// stdout stays a single JSON document.
		if near {
			warnCorruptRuns(w, res.Corrupt)
		}
		return w.JSON("run.ls", lsView(res, near))
	}

	// Human output: without --near, print the reusable rows as a flat list of lines (the
	// JSON object family above is machine-only; the human view stays a scannable list).
	if !near {
		if len(res.Reusable) == 0 {
			if all {
				w.Line("no runs recorded")
			} else {
				w.Line("no reusable runs for the current state")
			}
			emitPerformanceNotes(w, res.Performance)
			return nil
		}
		for _, e := range res.Reusable {
			w.Line(lsLine(e, td, now))
		}
		emitPerformanceNotes(w, res.Performance)
		return nil
	}

	// --near: a reusable-now section (identical meaning to plain run ls) followed by a
	// clearly separated near-miss section, so "no reusable runs" never dead-ends.
	w.Line("reusable now:")
	reusable := reusableNow(res.Reusable)
	if len(reusable) == 0 {
		w.Line("  none")
	}
	for _, e := range reusable {
		w.Line("  " + lsLine(e, td, now))
	}
	w.Line("")
	w.Line("near misses:")
	if len(res.NearMisses) == 0 {
		w.Line("  none")
	} else {
		for _, c := range res.NearMisses {
			for _, line := range nearLines(c, td, now) {
				w.Line(line)
			}
		}
	}
	warnCorruptRuns(w, res.Corrupt)
	emitPerformanceNotes(w, res.Performance)
	return nil
}

// nearLines renders one near-miss candidate: a header line naming the run and why it
// is not reusable, then — for an effect-state miss whose root was observed — one compact
// watched-state detail, and — for an input-tree-differs miss — a bounded changed-path
// sample and any advisory hints. Near misses are visually indented under the section and
// always carry their not-reusable reason, so they can never read as a cache hit.
func nearLines(c apprun.RunReuseCandidate, td domainconfig.TimeDisplay, now time.Time) []string {
	en := c.Entry
	lines := []string{
		fmt.Sprintf("  %s  %s  exit=%s  %s  not-reusable(%s)",
			en.ID.Short(),
			FormatTime(td, en.StartedAt, now),
			exitLabel(en.Exit),
			strings.Join(en.KeyInput.Command.Argv, " "),
			c.Reason,
		),
	}
	if detail := effectDetail(c.Effect); detail != "" {
		lines = append(lines, "    effect: "+detail)
	}
	if c.Changed != nil {
		lines = append(lines, changedSampleLines(*c.Changed)...)
	}
	for _, h := range c.Hints {
		lines = append(lines, "    hint: "+h.Message)
	}
	return lines
}

// changedSampleLines renders a bounded changed-path sample, declaring the true total
// and the omitted count when the sample is a truncated prefix, so the reader is never
// misled about how much it shows. Callers render nothing when a candidate carries no
// sample; a sample that exists always shows a computed prefix.
func changedSampleLines(s runcache.ChangedPathSample) []string {
	paths := s.Paths()
	if len(paths) == 0 {
		return nil
	}
	parts := make([]string, 0, len(paths))
	for _, p := range paths {
		parts = append(parts, p.Status+" "+p.Path)
	}
	head := fmt.Sprintf("    changed inputs (%d): %s", s.Total(), strings.Join(parts, ", "))
	if s.Truncated() {
		head += fmt.Sprintf(" (+%d more)", s.Omitted())
	}
	return []string{head}
}

// lsLine renders a single run for "run ls", ending with the reusability verdict so
// an agent can see at a glance whether the shown invocation is replayable now.
func lsLine(e apprun.LsEntry, td domainconfig.TimeDisplay, now time.Time) string {
	if e.Corrupt {
		return fmt.Sprintf("%s  [corrupt metadata]  not-reusable(%s)", e.ID.Short(), runcache.ReasonCorrupt)
	}
	en := e.Entry
	status := "reusable"
	if !e.Reusable() {
		status = fmt.Sprintf("not-reusable(%s)", e.Reusability.Reason())
	}
	// Mark a partial stored payload, matching run log: the shown invocation's evidence
	// is incomplete, so run show of it will not be the full command output.
	trunc := ""
	if en.Stdout.Truncated || en.Stderr.Truncated {
		trunc = "  [truncated]"
	}
	return fmt.Sprintf("%s  %s  exit=%s  %s  key=%s  cwd=%s  %s%s  %s",
		en.ID.Short(),
		FormatTime(td, en.StartedAt, now),
		exitLabel(en.Exit),
		durLabel(en),
		shortHex(en.Key.Hex()),
		en.KeyInput.CWD.String(),
		strings.Join(en.KeyInput.Command.Argv, " "),
		trunc,
		status,
	)
}

// runRunShow handles "awa run show <id>": show one run's metadata and, on request,
// its stored output — optionally filtered with --tail/--grep, or the latest run via
// --last.
func runRunShow(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	var ref string
	var showStdout, showStderr, showMeta, last bool
	var tailSet, grepSet bool
	var tail inspect.TailLimit
	var grep inspect.GrepPattern
	timeFlag := ""
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--stdout":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			showStdout = true
		case "--stderr":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			showStderr = true
		case "--meta":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			showMeta = true
		case "--last":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			last = true
		case "--tail":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			tl, err := inspect.ParseTailLimit(v)
			if err != nil {
				return usageErrorf("%v", err)
			}
			tail, tailSet = tl, true
		case "--grep":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			gp, err := inspect.ParseGrepPattern(v)
			if err != nil {
				return usageErrorf("%v", err)
			}
			grep, grepSet = gp, true
		case "--time":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			timeFlag = v
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for run show", tok)
			}
			if ref != "" {
				return usageErrorf("run show takes a single run id (got a second: %q)", tok)
			}
			ref = tok
		}
	}
	// Exactly one run selector: an explicit id or --last.
	if ref == "" && !last {
		return usageErrorf("run show requires a run id or --last")
	}
	if ref != "" && last {
		return usageErrorf("run show: provide either a run id or --last, not both")
	}
	// The stream selectors pick exactly one payload to write to stdout. Mixing them
	// would interleave human metadata with raw bytes (--meta --stdout) or concatenate
	// two payloads with no framing (--stdout --stderr), silently corrupting a pipe.
	if modes := boolCount(showMeta, showStdout, showStderr); modes > 1 {
		return usageErrorf("run show: --meta, --stdout, and --stderr are mutually exclusive")
	}
	// --meta is metadata-only; a filter would have nothing to apply to.
	if showMeta && (tailSet || grepSet) {
		return usageErrorf("run show: --meta cannot be combined with --tail or --grep")
	}
	// A raw --stdout/--stderr dump and the --json envelope cannot share stdout, so
	// reject that combo rather than silently honor only one. With a filter the bytes
	// do not go to stdout — they become bounded samples inside the JSON — so there
	// --stdout/--stderr is allowed and selects which stream to sample.
	if inv.options.JSON && (showStdout || showStderr) && !tailSet && !grepSet {
		return usageErrorf("run show --json cannot be combined with --stdout or --stderr")
	}
	// --time is a human metadata-display preference. JSON always uses machine UTC
	// timestamps, so reject the combo rather than validate-and-ignore the value
	// (matching log/run log/run ls). Only the human-metadata view consults --time; the
	// raw-stream and filtered views do not, but an explicit bad value must still fail
	// loudly there rather than slip through, so validate it on those non-JSON paths.
	if timeFlag != "" {
		if inv.options.JSON {
			return jsonTimeFlagError()
		}
		if _, err := domainconfig.ParseTimeDisplay(timeFlag); err != nil {
			return usageErrorf("%v", err)
		}
	}

	// run show is a stored-only operation: it discovers the .awa/ store without loading
	// or validating current project config, so a malformed awa.toml/config.toml never
	// blocks inspection of durable run evidence, and it never scans the current worktree.
	svc, evidence, cleanup, err := buildRunShowContext(inv.invCtx(), inv.options)
	if err != nil {
		return classifyRunError(err)
	}
	defer cleanup()

	id, skipped, err := resolveShowID(inv.invCtx(), svc, ref, last)
	if err != nil {
		return classifyRunError(err)
	}
	// The human diagnostics carry the skip; the JSON path carries it structurally on
	// the document below, so neither surface presents the answer as the newest run
	// when it is not.
	if !inv.options.JSON {
		for _, warn := range apprun.SkippedLatestWarnings(skipped) {
			w.Diagnostic("warning: " + warn)
		}
	}

	if inv.options.JSON {
		// The JSON metadata document is bounded and byte-free: it composes the validated
		// record, the two before/after state assessments, and stat-only output
		// inspectability. It never opens or hashes a payload unless an explicit --tail/
		// --grep sample is requested below.
		ev, err := evidence.Build(inv.invCtx(), id)
		if err != nil {
			return classifyRunError(err)
		}
		// Integrity starts at the aggregate's metadata inspection (unverified) and is
		// upgraded per stream only after an explicit --tail/--grep read opens and
		// hash-verifies it below — the strongest verification this invocation performed.
		stdoutIntegrity, stderrIntegrity := ev.Stdout().Integrity(), ev.Stderr().Integrity()
		var samples *showSamplesDoc
		if tailSet || grepSet {
			samples, err = buildShowSamples(svc, id, showStdout, showStderr, tailSet, tail, grepSet, grep)
			if err != nil {
				// A tampered selected stream fails loud here, so no document with a false
				// "verified" is ever emitted.
				return classifyRunError(err)
			}
			readStdout, readStderr := selectedShowStreams(showStdout, showStderr)
			if readStdout {
				stdoutIntegrity = runevidence.IntegrityVerified
			}
			if readStderr {
				stderrIntegrity = runevidence.IntegrityVerified
			}
		}
		doc := evidenceDocOf(ev, stdoutIntegrity, stderrIntegrity)
		doc.Samples = samples
		doc.SkippedLatest = skippedLatestDocOf(skipped)
		return w.JSON("run.show", doc)
	}

	// Non-JSON paths need the record's metadata (for the truncation banner and the meta
	// view). Load it without verifying payload bytes; explicit stream selection below
	// performs the byte verification, and --meta is metadata-only.
	entry, err := svc.Get(id)
	if err != nil {
		return classifyRunError(err)
	}

	// A single stream selector writes that stream (filtered or raw) to stdout, safe
	// for pipes.
	if showStdout || showStderr {
		open := svc.OpenStdout
		if showStderr {
			open = svc.OpenStderr
		}
		// The banner goes to stderr so it never corrupts a piped --stdout/--stderr dump,
		// while still warning that the bytes about to be shown are only part of what the
		// command produced — a tail or grep window may exclude the stored marker entirely.
		emitStoredTruncationBanner(w, entry.Stdout, entry.Stderr)
		if err := dumpSelectedStream(w, open, id, tailSet, tail, grepSet, grep); err != nil {
			return classifyRunError(err)
		}
		return nil
	}
	// No stream selector but a filter: inspect both streams in a clearly labeled
	// human view. The store keeps separate payloads, so this does not claim to
	// reproduce the original stdout/stderr interleaving.
	if tailSet || grepSet {
		emitStoredTruncationBanner(w, entry.Stdout, entry.Stderr)
		if err := renderShowFilterBoth(w, svc, id, tailSet, tail, grepSet, grep); err != nil {
			return classifyRunError(err)
		}
		return nil
	}
	// --meta or no selector at all: metadata view. Stored-only, so the time display is
	// resolved without loading project config.
	td, now, err := runShowHumanTime(timeFlag)
	if err != nil {
		return err
	}
	renderShowMeta(w, entry, td, now)
	return nil
}

// resolveShowID resolves the run id for "run show", from --last (newest run with
// readable metadata) or an explicit id/prefix. Both selectors are metadata-only;
// payload health is projected as inspectability and explicit output reads verify
// their bytes.
//
// It returns what --last passed over as well as what it found. A newer record awa
// could not read means the answer is not the newest stored run, and the caller has to
// say so — in the human diagnostics and in the JSON document — rather than let a
// partial answer read as a complete one.
func resolveShowID(ctx context.Context, svc *apprun.Service, ref string, last bool) (runcache.RunID, apprun.SkippedLatest, error) {
	if last {
		return svc.LatestRunID(ctx)
	}
	id, err := svc.Resolve(ctx, ref)
	return id, apprun.SkippedLatest{}, err
}

// skippedLatestDoc is the structural form of what a --last selection passed over, so
// a machine consumer learns the answer is not the newest stored run without parsing
// the human warning. Counts are exact; the id samples are bounded.
type skippedLatestDoc struct {
	Corrupt      *skippedRunsDoc `json:"corrupt,omitempty"`
	Incompatible *skippedRunsDoc `json:"incompatible,omitempty"`
}

type skippedRunsDoc struct {
	Count  int      `json:"count"`
	Sample []string `json:"sample"`
}

// skippedLatestDocOf projects the skipped facts, or nil when nothing was skipped so
// the field stays absent on the ordinary path.
func skippedLatestDocOf(s apprun.SkippedLatest) *skippedLatestDoc {
	if !s.Any() {
		return nil
	}
	out := &skippedLatestDoc{}
	if s.Corrupt.Count > 0 {
		out.Corrupt = skippedRunsDocOf(s.Corrupt)
	}
	if s.Incompatible.Count > 0 {
		out.Incompatible = skippedRunsDocOf(s.Incompatible)
	}
	return out
}

func skippedRunsDocOf(s apprun.SkippedRuns) *skippedRunsDoc {
	d := &skippedRunsDoc{Count: s.Count, Sample: make([]string, 0, len(s.Sample))}
	for _, id := range s.Sample {
		d.Sample = append(d.Sample, id.String())
	}
	return d
}

// streamOpener opens one verified stored stream by run id.
type streamOpener = func(runcache.RunID) (io.ReadCloser, error)

// dumpSelectedStream writes one stored stream to stdout: raw bytes when no filter
// is set (byte-exact, pipe-safe), or the filtered lines otherwise. grep runs
// before tail when both are given.
func dumpSelectedStream(w *output.Writer, open streamOpener, id runcache.RunID, tailSet bool, tail inspect.TailLimit, grepSet bool, grep inspect.GrepPattern) error {
	if !tailSet && !grepSet {
		return dumpStream(w.Stdout(), open, id)
	}
	rc, err := open(id)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	switch {
	case grepSet && tailSet:
		lines, _, err := inspect.GrepTail(rc, grep, tail)
		if err != nil {
			return err
		}
		writeLines(w.Stdout(), lines)
	case grepSet:
		_, err := inspect.GrepStream(rc, grep, w.Stdout())
		return err
	default: // tailSet
		lines, _, err := inspect.TailLines(rc, tail)
		if err != nil {
			return err
		}
		writeLines(w.Stdout(), lines)
	}
	return nil
}

// renderShowFilterBoth prints a filtered view of both streams, each under a clear
// label. Output goes to stdout as the command's deliverable; pipe-clean single
// streams are available via --stdout/--stderr. grep without tail streams every
// match (bounded memory, no accumulation); a tailed view collects only its bounded
// window so it can show the truncation note.
func renderShowFilterBoth(w *output.Writer, svc *apprun.Service, id runcache.RunID, tailSet bool, tail inspect.TailLimit, grepSet bool, grep inspect.GrepPattern) error {
	for _, st := range []struct {
		name string
		open streamOpener
	}{{"stdout", svc.OpenStdout}, {"stderr", svc.OpenStderr}} {
		if grepSet && !tailSet {
			w.Line("== " + st.name + " ==")
			if err := streamGrep(st.open, id, grep, w.Stdout()); err != nil {
				return err
			}
			continue
		}
		lines, overflow, err := collectFilteredTail(st.open, id, tail, grepSet, grep)
		if err != nil {
			return err
		}
		header := "== " + st.name + " =="
		if overflow {
			header += " (tail — earlier lines omitted)"
		}
		w.Line(header)
		for _, l := range lines {
			w.Line(l)
		}
	}
	return nil
}

// streamGrep streams every line of one stored stream matching grep to out.
func streamGrep(open streamOpener, id runcache.RunID, grep inspect.GrepPattern, out io.Writer) error {
	rc, err := open(id)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = inspect.GrepStream(rc, grep, out)
	return err
}

// collectFilteredTail collects the bounded tail of one stored stream: the last
// tail.Lines() lines, optionally restricted to those matching grep. Both forms keep
// only the tail window in memory. The caller guarantees a tail is requested.
func collectFilteredTail(open streamOpener, id runcache.RunID, tail inspect.TailLimit, grepSet bool, grep inspect.GrepPattern) ([]string, bool, error) {
	rc, err := open(id)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rc.Close() }()
	if grepSet {
		return inspect.GrepTail(rc, grep, tail)
	}
	return inspect.TailLines(rc, tail)
}

// writeLines writes each line followed by a newline.
func writeLines(out io.Writer, lines []string) {
	for _, l := range lines {
		_, _ = io.WriteString(out, l+"\n")
	}
}

// runRunRm handles "awa run rm": delete stored runs by id or by filter.
func runRunRm(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	var req apprun.RmRequest
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--command":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			req.Command = v
		case "--older-than":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			d, err := domainconfig.ParseDuration(v)
			if err != nil {
				return usageErrorf("invalid value %q for --older-than: %v", v, err)
			}
			req.OlderThan = d.Duration()
			req.HasOlderThan = true
		case "--dry-run":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			req.DryRun = true
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for run rm", tok)
			}
			req.IDs = append(req.IDs, tok)
		}
	}
	if len(req.IDs) == 0 && req.Command == "" && !req.HasOlderThan {
		return usageErrorf("run rm requires a run id or a filter (--command or --older-than)")
	}
	if len(req.IDs) > 0 && (req.Command != "" || req.HasOlderThan) {
		return usageErrorf("run rm takes either run ids or filters (--command/--older-than), not both")
	}

	svc, cleanup, err := buildRunService2(inv.invCtx(), inv.options)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := svc.Rm(inv.invCtx(), req)
	if err != nil {
		return classifyRunError(err)
	}

	if inv.options.JSON {
		return w.JSON("run.rm", rmDoc(res))
	}
	verb := "removed"
	if res.DryRun {
		verb = "would remove"
	}
	if len(res.Removed) == 0 && len(res.RemovedUnreadable) == 0 {
		w.Line("no runs matched")
		return nil
	}
	for _, e := range res.Removed {
		w.Linef("%s %s  %s", verb, e.ID.Short(), strings.Join(e.KeyInput.Command.Argv, " "))
	}
	for _, u := range res.RemovedUnreadable {
		w.Linef("%s %s  (%s)", verb, u.ID.Short(), unreadableRunPhrase(u.Reason))
	}
	return nil
}

// buildRunService2 resolves the project from global options and builds the run
// service for the non-scanning subcommands (log, show, rm). They never scan the
// worktree, so the service is wired with no worktree index (the mutable index belongs
// only to the run write path, opened under its presence lock).
func buildRunService2(ctx context.Context, opts GlobalOptions) (*apprun.Service, func(), error) {
	_, layout, cfg, err := loadProjectConfig(opts)
	if err != nil {
		return nil, nil, err
	}
	// buildRunService2 backs the non-scanning subcommands (run log/show/rm); they list
	// or mutate stored runs and never look a worktree path up, so they need no index.
	return buildRunService(ctx, layout, cfg, indexNone)
}

// dumpStream copies a verified stored output stream to out.
func dumpStream(out io.Writer, open func(runcache.RunID) (io.ReadCloser, error), id runcache.RunID) error {
	rc, err := open(id)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	_, err = io.Copy(out, rc)
	return err
}

// logLine renders a single run for "run log".
func logLine(le apprun.LogEntry, td domainconfig.TimeDisplay, now time.Time) string {
	if le.Corrupt {
		// Metadata is unreadable: show only the id (resolved from the directory) so the
		// user can still inspect or delete it.
		return fmt.Sprintf("%s  [corrupt metadata]", le.ID.Short())
	}
	e := le.Entry
	flags := ""
	// The mutation outcome is the headline diagnostic of a recorded run, so a command
	// that changed observed state (or whose post-state could not be observed) is marked
	// even when the reuse reason is a policy one like record-only.
	switch e.Mutation.Outcome() {
	case runcache.MutationChanged:
		flags += " [mutated]"
	case runcache.MutationScanFailed:
		flags += " [post-scan-failed]"
	}
	if e.Stdout.Truncated || e.Stderr.Truncated {
		flags += " [truncated]"
	}
	if !le.Healthy {
		flags += " [corrupt]"
	}
	// Surface the reuse classification so run history distinguishes a reusable cache
	// entry from record-only/mutating/unknown-post-state history at a glance.
	reuse := e.Reuse.String()
	if r := e.Reuse.Reason().String(); r != "" {
		reuse += ":" + r
	}
	return fmt.Sprintf("%s  %s  exit=%s  %s  key=%s  %s  %s%s",
		e.ID.Short(),
		FormatTime(td, e.StartedAt, now),
		exitLabel(e.Exit),
		durLabel(e),
		shortHex(e.Key.Hex()),
		reuse,
		strings.Join(e.KeyInput.Command.Argv, " "),
		flags,
	)
}

// renderShowMeta prints a run's metadata and a short output summary.
func renderShowMeta(w *output.Writer, e runcache.RunEntry, td domainconfig.TimeDisplay, now time.Time) {
	w.Linef("id:        %s", e.ID.String())
	w.Linef("command:   %s", strings.Join(e.KeyInput.Command.Argv, " "))
	w.Linef("cwd:       %s", e.KeyInput.CWD.String())
	w.Linef("exit:      %s", exitLabel(e.Exit))
	w.Linef("started:   %s", FormatTime(td, e.StartedAt, now))
	w.Linef("finished:  %s", FormatTime(td, e.FinishedAt, now))
	w.Linef("duration:  %s", durLabel(e))
	reason := ""
	if !e.Decision.Cacheable && e.Decision.Reason != "" {
		reason = " (" + e.Decision.Reason + ")"
	}
	w.Linef("cacheable: %t%s", e.Decision.Cacheable, reason)
	reuse := e.Reuse.String()
	if r := e.Reuse.Reason().String(); r != "" {
		reuse += " (" + r + ")"
	}
	w.Linef("reuse:     %s", reuse)
	w.Linef("mutation:  %s", e.Mutation.Outcome().String())
	w.Linef("key:       %s", e.Key.String())
	w.Linef("inputs:    tree=%s trust=%s scope=%s", e.KeyInput.InputTreeHash.String(), e.KeyInput.TrustMode.String(), strings.Join(e.KeyInput.IncludeScope, ","))
	w.Linef("stdout:    %s", streamSummary(e.Stdout))
	w.Linef("stderr:    %s", streamSummary(e.Stderr))
	if e.Skipped.Count > 0 {
		w.Linef("skipped:   %d (allowed=%t)", e.Skipped.Count, e.Skipped.Allowed)
	}
}

func streamSummary(c runcache.OutputCapture) string {
	s := fmt.Sprintf("%d bytes", c.OriginalBytes)
	if c.Truncated {
		s += fmt.Sprintf(" (stored %d, %d omitted)", c.StoredBytes, c.OmittedBytes)
	}
	return s
}

func exitLabel(e runcache.ExitStatus) string {
	if e.Kind == runcache.ExitSignaled {
		return "signal:" + e.Signal
	}
	return strconv.Itoa(e.Code)
}

// durLabel renders a stored run's elapsed time in the one canonical duration form
// (durationLabel), so run ls / log / show read like the run footer and timeline.
func durLabel(e runcache.RunEntry) string {
	return durationLabel(e.FinishedAt.Sub(e.StartedAt))
}

func shortHex(hex string) string {
	if len(hex) <= 12 {
		return hex
	}
	return hex[:12]
}

// --- JSON views for subcommands ---

type logEntryDoc struct {
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	CWD        string   `json:"cwd"`
	ExitCode   int      `json:"exit_code"`
	Signal     string   `json:"signal"`
	DurationMs int64    `json:"duration_ms"`
	Key        string   `json:"key"`
	StartedAt  string   `json:"started_at"`
	Cacheable  bool     `json:"cacheable"`
	// Reuse is the typed reuse classification (reusable, non-reusable, record-only,
	// unknown-post-state) and ReuseReason the machine-stable why for a non-reusable
	// run, so an agent can tell a reusable cache entry from mutating/record-only
	// history without inferring it from cacheable alone.
	Reuse       string `json:"reuse"`
	ReuseReason string `json:"reuse_reason,omitempty"`
	// Mutation is the run mutation guard's outcome (unobserved, unchanged, changed,
	// scan-failed), the headline diagnostic fact of a recorded run: an agent reading
	// the ledger sees that a command changed observed state, not just that it was
	// record-only.
	Mutation  string `json:"mutation"`
	Truncated bool   `json:"truncated"`
	// Healthy is false when the run's stored payloads are missing or fail
	// verification, so a consumer can tell a degraded entry from a replayable one.
	Healthy bool `json:"healthy"`
	// Corrupt is true when the run's metadata could not be decoded; only id is
	// populated and the metadata fields are left empty.
	Corrupt bool `json:"corrupt,omitempty"`
}

func logDocs(entries []apprun.LogEntry) []logEntryDoc {
	out := make([]logEntryDoc, 0, len(entries))
	for _, le := range entries {
		if le.Corrupt {
			out = append(out, logEntryDoc{ID: le.ID.String(), Corrupt: true})
			continue
		}
		e := le.Entry
		out = append(out, logEntryDoc{
			ID:          e.ID.String(),
			Command:     e.KeyInput.Command.Argv,
			CWD:         e.KeyInput.CWD.String(),
			ExitCode:    e.Exit.Code,
			Signal:      e.Exit.Signal,
			DurationMs:  e.DurationMs(),
			Key:         e.Key.String(),
			StartedAt:   e.StartedAt.UTC().Format(time.RFC3339Nano),
			Cacheable:   e.Decision.Cacheable,
			Reuse:       e.Reuse.String(),
			ReuseReason: e.Reuse.Reason().String(),
			Mutation:    e.Mutation.Outcome().String(),
			Truncated:   e.Stdout.Truncated || e.Stderr.Truncated,
			Healthy:     le.Healthy,
		})
	}
	return out
}

// lsEntryDoc is the stable JSON shape of one "run ls" row. The command and cwd are
// exact replay guidance (argv as executed, cwd relative to the project root), so an
// agent can copy the invocation that produced a reusable entry. Reusable rows omit
// reason; non-reusable rows (only present with --all) always carry one.
type lsEntryDoc struct {
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	CWD        string   `json:"cwd"`
	ExitCode   int      `json:"exit_code"`
	Signal     string   `json:"signal"`
	DurationMs int64    `json:"duration_ms"`
	Key        string   `json:"key"`
	StartedAt  string   `json:"started_at"`
	Reusable   bool     `json:"reusable"`
	Reason     string   `json:"reason,omitempty"`
	// Truncated is true when either stored stream was cut at the capture limit, so a
	// consumer knows this entry's stored output is incomplete evidence before fetching
	// it — matching the run log JSON field.
	Truncated bool `json:"truncated"`
	// Corrupt is true when the run's metadata could not be decoded; only id, reusable
	// (false), and reason ("corrupt") are populated.
	Corrupt bool `json:"corrupt,omitempty"`
}

// lsListDoc is the single object shape of "run ls --json", for both the plain listing
// (near:false) and the near-miss view (near:true). One data family, distinguished only by
// the near marker and whether near_misses is populated, so a consumer branches on a field
// rather than on the document's top-level type. It always carries the diagnostics block
// (performance is a stable array, empty when nothing crossed the interactive threshold),
// so machine-readable latency facts never live only on stderr.
type lsListDoc struct {
	Near        bool               `json:"near"`
	Reusable    []lsEntryDoc       `json:"reusable"`
	NearMisses  []nearMissDoc      `json:"near_misses"`
	Corrupt     int                `json:"corrupt"`
	Diagnostics perfDiagnosticsDoc `json:"diagnostics"`
}

// reusableNow returns only the entries reusable for the current state. Under --all
// the Ls result also carries non-reusable and corrupt rows; the "reusable" section
// — plain run ls and the top of run ls --near — is exactly the reusable-now rows,
// so this is the single place that rule lives.
func reusableNow(entries []apprun.LsEntry) []apprun.LsEntry {
	out := make([]apprun.LsEntry, 0, len(entries))
	for _, e := range entries {
		if e.Reusable() {
			out = append(out, e)
		}
	}
	return out
}

// warnCorruptRuns emits the standard stderr diagnostic for stored runs skipped
// because their metadata could not be decoded. It no-ops for a zero count, so
// callers need no guard, and is the single source of the wording shared by run ls
// --near and the status dashboard.
func warnCorruptRuns(w *output.Writer, n int) {
	if n <= 0 {
		return
	}
	w.Diagnostic(fmt.Sprintf("warning: %d stored run(s) skipped due to corrupt metadata", n))
}

// lsView builds the run.ls JSON object for either the plain listing (near=false) or the
// near-miss view (near=true). Plain lists the rows the command produced as-is (reusable
// now, or every row under --all); near narrows the reusable section to reusable-now and
// adds the ranked near misses. Both carry the same latency diagnostics and corrupt count.
func lsView(res apprun.LsResult, near bool) lsListDoc {
	reusable := res.Reusable
	misses := []nearMissDoc{}
	if near {
		reusable = reusableNow(res.Reusable)
		misses = make([]nearMissDoc, 0, len(res.NearMisses))
		for _, c := range res.NearMisses {
			misses = append(misses, nearMissView(c))
		}
	}
	return lsListDoc{
		Near:        near,
		Reusable:    lsDocs(reusable),
		NearMisses:  misses,
		Corrupt:     res.Corrupt,
		Diagnostics: perfDiagnosticsBlock(res.Performance),
	}
}

func lsDocs(entries []apprun.LsEntry) []lsEntryDoc {
	out := make([]lsEntryDoc, 0, len(entries))
	for _, e := range entries {
		if e.Corrupt {
			out = append(out, lsEntryDoc{ID: e.ID.String(), Reusable: false, Reason: string(runcache.ReasonCorrupt), Corrupt: true})
			continue
		}
		en := e.Entry
		out = append(out, lsEntryDoc{
			ID:         en.ID.String(),
			Command:    en.KeyInput.Command.Argv,
			CWD:        en.KeyInput.CWD.String(),
			ExitCode:   en.Exit.Code,
			Signal:     en.Exit.Signal,
			DurationMs: en.DurationMs(),
			Key:        en.Key.String(),
			StartedAt:  en.StartedAt.UTC().Format(time.RFC3339Nano),
			Reusable:   e.Reusable(),
			Reason:     string(e.Reusability.Reason()),
			Truncated:  en.Stdout.Truncated || en.Stderr.Truncated,
		})
	}
	return out
}

// emitStoredTruncationBanner warns, on stderr, that a stored run's payload is
// incomplete because the command exceeded the capture limit. Used by run show's
// byte-dump and filtered views, where a tail or grep window can otherwise present a
// clean-looking cut with no sign the underlying evidence is partial. It is distinct
// from display overflow ("earlier lines omitted"), which only bounds the view.
func emitStoredTruncationBanner(w *output.Writer, stdout, stderr runcache.OutputCapture) {
	if stdout.Truncated {
		w.Diagnostic(truncationNotice("stdout", stdout.OmittedBytes))
	}
	if stderr.Truncated {
		w.Diagnostic(truncationNotice("stderr", stderr.OmittedBytes))
	}
}

// showStreamDoc is the public per-stream evidence for "run show --json": byte
// accounting, truncation facts, and the content hash (a user-meaningful identity).
// It deliberately omits the internal store filename — the on-disk payload name is
// .awa/ layout, not a public contract, so exposing it would freeze an
// implementation detail.
type showStreamDoc struct {
	OriginalBytes    int64  `json:"original_bytes"`
	StoredBytes      int64  `json:"stored_bytes"`
	Truncated        bool   `json:"truncated"`
	OmittedBytes     int64  `json:"omitted_bytes"`
	TruncationPolicy string `json:"truncation_policy"`
	Hash             string `json:"hash"`
}

// showSamplesDoc is a bounded, structured selection of a run's output lines for
// "run show --json --tail/--grep". It is capped in both line count and total bytes
// so the JSON document never grows with the underlying log.
type showSamplesDoc struct {
	// Filter is "tail", "grep", or "grep+tail".
	Filter string `json:"filter"`
	// TailLines is the requested tail window, present for tail and grep+tail.
	TailLines int `json:"tail_lines,omitempty"`
	// Pattern is the grep pattern, present for grep and grep+tail.
	Pattern string `json:"pattern,omitempty"`
	// Truncated is true when a tail dropped earlier lines or the sample caps clipped
	// the selection.
	Truncated bool     `json:"truncated"`
	Stdout    []string `json:"stdout,omitempty"`
	Stderr    []string `json:"stderr,omitempty"`
}

// JSON sample bounds: a structured selection must stay small regardless of the
// underlying payload size.
const (
	jsonSampleMaxLines = 1000
	jsonSampleMaxBytes = 256 * 1024
)

// buildShowSamples gathers bounded structured output samples for a JSON show. When
// a stream selector is set only that stream is sampled; otherwise both are.
func buildShowSamples(svc *apprun.Service, id runcache.RunID, showStdout, showStderr, tailSet bool, tail inspect.TailLimit, grepSet bool, grep inspect.GrepPattern) (*showSamplesDoc, error) {
	doc := &showSamplesDoc{Filter: filterLabel(tailSet, grepSet)}
	if tailSet {
		doc.TailLines = tail.Lines()
	}
	if grepSet {
		doc.Pattern = grep.String()
	}
	// Bound the sample to the requested tail window but never beyond the JSON caps,
	// and collect it streaming so a large log never materializes in memory.
	maxLines := jsonSampleMaxLines
	if tailSet && tail.Lines() < maxLines {
		maxLines = tail.Lines()
	}
	var keep func(string) bool
	if grepSet {
		keep = grep.Match
	}
	// sample collects one stream's bounded samples, folding its truncation into doc.
	sample := func(open streamOpener) ([]string, error) {
		rc, err := open(id)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		lines, dropped, err := inspect.CollectSample(rc, keep, maxLines, jsonSampleMaxBytes)
		if err != nil {
			return nil, err
		}
		if dropped {
			doc.Truncated = true
		}
		return lines, nil
	}
	readStdout, readStderr := selectedShowStreams(showStdout, showStderr)
	if readStdout {
		s, err := sample(svc.OpenStdout)
		if err != nil {
			return nil, err
		}
		doc.Stdout = s
	}
	if readStderr {
		s, err := sample(svc.OpenStderr)
		if err != nil {
			return nil, err
		}
		doc.Stderr = s
	}
	return doc, nil
}

// selectedShowStreams resolves which streams an explicit --tail/--grep view reads: the
// one named by --stdout/--stderr, or both when neither is named. It is the single source
// of truth shared by sample collection and the verified-integrity upgrade, so the two
// cannot disagree on which stream was actually opened and hash-verified.
func selectedShowStreams(showStdout, showStderr bool) (stdout, stderr bool) {
	if !showStdout && !showStderr {
		return true, true
	}
	return showStdout, showStderr
}

// filterLabel names the active filter combination for JSON output.
func filterLabel(tailSet, grepSet bool) string {
	switch {
	case grepSet && tailSet:
		return "grep+tail"
	case grepSet:
		return "grep"
	default:
		return "tail"
	}
}

func showStreamView(c runcache.OutputCapture) showStreamDoc {
	return showStreamDoc{
		OriginalBytes:    c.OriginalBytes,
		StoredBytes:      c.StoredBytes,
		Truncated:        c.Truncated,
		OmittedBytes:     c.OmittedBytes,
		TruncationPolicy: c.TruncationPolicy.String(),
		Hash:             c.Hash.String(),
	}
}

type rmRemovedDoc struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
}

type rmResultDoc struct {
	DryRun  bool           `json:"dry_run"`
	Removed []rmRemovedDoc `json:"removed"`
	// RemovedUnreadable lists the records removed despite metadata that would not
	// decode, each with the reason it would not, so a consumer can tell a clean
	// deletion from damage cleanup from discarding evidence in an unreadable schema.
	RemovedUnreadable []rmUnreadableDoc `json:"removed_unreadable,omitempty"`
}

type rmUnreadableDoc struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

func rmDoc(res apprun.RmResult) rmResultDoc {
	out := rmResultDoc{DryRun: res.DryRun, Removed: make([]rmRemovedDoc, 0, len(res.Removed))}
	for _, e := range res.Removed {
		out.Removed = append(out.Removed, rmRemovedDoc{ID: e.ID.String(), Command: e.KeyInput.Command.Argv})
	}
	for _, u := range res.RemovedUnreadable {
		out.RemovedUnreadable = append(out.RemovedUnreadable, rmUnreadableDoc{ID: u.ID.String(), Reason: u.Reason.String()})
	}
	return out
}

// boolCount returns how many of the given flags are set, for enforcing
// mutually-exclusive options.
func boolCount(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// unreadableRunPhrase renders why a removed run's metadata would not decode. The
// distinction is the user's, not an internal one: damage is something to investigate,
// while an unreadable schema is intact evidence this binary has no reader for and is
// exactly what the incompatible-evidence guidance tells them to clear from here.
func unreadableRunPhrase(reason evidence.DiagnosticToken) string {
	switch reason {
	case evidence.TokenMetadataIncompatible:
		return "incompatible schema"
	case evidence.TokenMetadataCorrupt:
		return "corrupt metadata"
	default:
		// Defaulting to either phrase would state something about the store that was
		// never established. Show the token instead: unhelpful is recoverable, wrong is
		// what sent a user chasing damage that was not there.
		return reason.String()
	}
}
