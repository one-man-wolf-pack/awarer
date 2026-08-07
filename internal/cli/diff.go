package cli

import (
	"fmt"
	"strconv"

	appdiff "awarer/internal/app/diff"
	"awarer/internal/app/state"
	"awarer/internal/domain/compare"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/output"
)

// runDiff handles "awa diff". It compares two states (default latest..now) and
// renders a content diff for each change where content is available, falling back
// to clear notices for binary, hash-only, skipped, and unavailable content.
func runDiff(w *output.Writer, inv invocation) error {
	da, err := parseDiffArgs(inv.args)
	if err != nil {
		return err
	}
	// --stat is a human output mode; --json already carries the summary counts, so
	// combining them would silently no-op. Reject it rather than ignore the flag.
	// (diff has no --name-only.)
	if inv.options.JSON && da.stat {
		return jsonHumanModeError("--stat", "includes the summary")
	}

	rng, err := state.ParseRange(da.rangeTok)
	if err != nil {
		return usageErrorf("%v", err)
	}

	proj, layout, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}
	// Tokens after "--" are literal path filters, never ranges or flags — the way
	// to name a path that looks like a range ("a..b") or a flag ("-x.go").
	pathArgs := append(da.paths, inv.operands...)
	filters, err := parsePathFilters(layout.Root(), pathArgs)
	if err != nil {
		return err
	}

	contextLines := cfg.Checkpoint.DiffContext
	if da.ctxOverride >= 0 {
		contextLines = da.ctxOverride
	}

	// Precedence: a CLI algorithm flag overrides the config [diff].algorithm,
	// which itself defaults to Histogram (Myers remains explicit and the fallback).
	algorithm := cfg.Diff.Algorithm
	if da.algorithmSet {
		algorithm = da.algorithm
	}

	resolver, cleanup, err := buildStateResolver(layout, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := inv.invCtx()
	svc := appdiff.New(resolver)
	req := appdiff.Request{
		Range:         rng,
		Now:           state.NowContext{Project: proj, Config: cfg, NeedContent: true},
		DetectRenames: cfg.Checkpoint.RenameDetection && !da.noRenames,
		PathFilters:   filters,
		Context:       contextLines,
		Algorithm:     algorithm,
	}

	// JSON streams the per-file diffs: fixed facts, then each file's diff element (with
	// its unified body) pulled one at a time — never a materialized []fileDiffView with
	// every diff body at once, the worst of the old memory offenders — then the
	// aggregates. With rename detection on (the default) the comparison pairs eagerly in
	// Stream, so a corrupt manifest fails before any output; under --no-renames a
	// mid-stream error is partial output with a non-zero exit, like the human path.
	if inv.options.JSON {
		sr, err := svc.Stream(ctx, req)
		if err != nil {
			return classifyStateError(err)
		}
		defer func() { _ = sr.Close() }()
		bg := baselineGitContextOf(ctx, layout.Root(), sr.Left, sr.Right)
		return streamDiffJSON(w, sr, filters, cfg, bg)
	}

	td, err := timeDisplayFromFlag("", cfg)
	if err != nil {
		return usageErrorf("%v", err)
	}
	// --stat reports only the change counts, so it takes the change-only stream: no file
	// content is read and the text diff engine never runs just to count. It also needs no
	// "now" content sources, so it resolves without them.
	if da.stat {
		req.Now.NeedContent = false
		csr, err := svc.StreamChanges(ctx, req)
		if err != nil {
			return classifyStateError(err)
		}
		defer func() { _ = csr.Close() }()
		bg := baselineGitContextOf(ctx, layout.Root(), csr.Left, csr.Right)
		return renderDiffStat(w, csr, filters, td, bg)
	}
	// Human output streams: each file's content diff is rendered as it is pulled, so
	// peak memory is one file's bounded diff, not the whole set.
	sr, err := svc.Stream(ctx, req)
	if err != nil {
		return classifyStateError(err)
	}
	defer func() { _ = sr.Close() }()
	bg := baselineGitContextOf(ctx, layout.Root(), sr.Left, sr.Right)
	return renderDiff(w, sr, filters, td, bg)
}

// diffArgs holds the parsed diff-local arguments. ctxOverride is -1 when
// --context was not given; algorithmSet reports whether a CLI algorithm flag
// selected an engine, so runDiff can fall back to config when it did not.
type diffArgs struct {
	rangeTok     string
	paths        []string
	stat         bool
	noRenames    bool
	ctxOverride  int
	algorithm    domainconfig.DiffAlgorithm
	algorithmSet bool
}

// parseDiffArgs parses the diff-local arguments: at most one range or "-N"
// shortcut token, zero or more path arguments, --stat, --no-renames,
// --context <n>, and --algorithm <name>.
func parseDiffArgs(args []string) (diffArgs, error) {
	da := diffArgs{ctxOverride: -1}
	haveRange := false
	setRange := func(tok string) error {
		if haveRange {
			return usageErrorf("only one range may be given (got a second: %q)", tok)
		}
		da.rangeTok = tok
		haveRange = true
		return nil
	}
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--stat":
			if err := noValue(name, hasValue); err != nil {
				return diffArgs{}, err
			}
			da.stat = true
		case "--no-renames":
			if err := noValue(name, hasValue); err != nil {
				return diffArgs{}, err
			}
			da.noRenames = true
		case "--context":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return diffArgs{}, err
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return diffArgs{}, usageErrorf("invalid value %q for --context: want a non-negative integer", v)
			}
			da.ctxOverride = n
		case "--algorithm":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return diffArgs{}, err
			}
			a, err := domainconfig.ParseDiffAlgorithm(v)
			if err != nil {
				return diffArgs{}, usageErrorf("%v", err)
			}
			// Repeating the same engine is harmless; naming two different ones is a
			// usage error, because the user expressed a genuine conflict.
			if da.algorithmSet && a != da.algorithm {
				return diffArgs{}, usageErrorf("conflicting diff algorithms: --algorithm selects at most one engine")
			}
			da.algorithm = a
			da.algorithmSet = true
		default:
			isPath, err := routePositional("diff", tok, name, hasValue, setRange)
			if err != nil {
				return diffArgs{}, err
			}
			if isPath {
				da.paths = append(da.paths, tok)
			}
		}
	}

	return da, nil
}

// renderDiff streams the human diff report: it pulls one file diff at a time and
// prints it, so only a single file's bounded diff is held in memory. On a mid-stream
// failure (a comparison error or a missing/corrupt blob a diff needs) it returns a
// partial-output error — the command exits non-zero with a stderr diagnostic, so the
// files already printed are never mistaken for a complete diff.
func renderDiff(w *output.Writer, sr appdiff.StreamResult, filters []worktree.RelPath, td domainconfig.TimeDisplay, bg baselineGitContext) error {
	// The caller owns the StreamResult's lifecycle and closes it (cursor + states); this
	// renderer only consumes the cursor.
	cur := sr.Files

	renderRangeHeader(w, state.NewStateRangeSummary(sr.Left, sr.Right, filters), td, bg)

	printed, skipped := 0, 0
	for cur.Next() {
		fd := cur.FileDiff()
		renderFileDiff(w, fd)
		printed++
		if fd.Change.Status == compare.Skipped {
			skipped++
		}
	}
	if err := cur.Err(); err != nil {
		return partialOutputError(true, err)
	}
	if printed == 0 {
		w.Line("no changes")
		emitReviewCoverage(w)
	}
	emitScopeNote(w, skipped)
	// A clean drain: if rename detection was requested but skipped for size, say so.
	renameSkippedNote(w, sr.RenameDetection)
	return nil
}

// renderDiffStat prints the --stat summary from the change-only stream. It folds each
// pulled change into bounded Summary counters — reading no file content and running no
// text diff engine — so the summary stays as cheap as its counts imply.
func renderDiffStat(w *output.Writer, csr appdiff.ChangeStreamResult, filters []worktree.RelPath, td domainconfig.TimeDisplay, bg baselineGitContext) error {
	renderRangeHeader(w, state.NewStateRangeSummary(csr.Left, csr.Right, filters), td, bg)

	cur := csr.Changes
	var sum compare.Summary
	for cur.Next() {
		sum.Add(cur.Change())
	}
	if err := cur.Err(); err != nil {
		return partialOutputError(true, err)
	}
	if sum.Total() == 0 {
		w.Line("no changes")
		emitReviewCoverage(w)
		emitScopeNote(w, 0)
		return nil
	}
	renderChangesStat(w, sum)
	emitScopeNote(w, sum.Skipped)
	renameSkippedNote(w, csr.RenameDetection)
	return nil
}

// renderFileDiff prints a per-file header and, for a text diff, the unified body.
// Every non-text change prints one descriptive line carrying its reason, so the
// output stays parseable and the reason is the single source of the wording.
func renderFileDiff(w *output.Writer, fd appdiff.FileDiff) {
	c := fd.Change
	if fd.Availability == appdiff.Text {
		w.Line(diffHeader(c))
		w.Raw([]byte(fd.Text))
		return
	}
	w.Linef("%s %s (%s)", c.Status.Code(), primaryPath(c), fd.Reason)
}

func diffHeader(c compare.Change) string {
	header := "diff " + primaryPath(c)
	if c.Status == compare.Renamed {
		header = "diff " + c.OldPath.String() + " -> " + c.NewPath.String()
	}
	// Surface a permission change in the header so it stays visible even when the
	// content also changed and a textual body follows.
	if c.ModeChanged() {
		header += fmt.Sprintf(" (mode %04o -> %04o)", c.OldMode, c.NewMode)
	}
	return header
}

func primaryPath(c compare.Change) string { return c.PrimaryPath().String() }

// --- JSON views ---

type fileDiffView struct {
	changeView
	Availability string `json:"availability"`
	Reason       string `json:"reason,omitempty"`
	Diff         string `json:"diff,omitempty"`
}

// streamDiffJSON writes diff --json as a streamed envelope: the fixed facts and the
// rename-detection outcome, then the per-file diffs pulled one at a time (each with its
// unified body), then the fixed-size aggregates (summary, scope) folded during the drain.
// Peak memory is one file's bounded diff, not every diff body at once — the per-file
// availability facts (availability, reason, content_diff_available) live on each element,
// so there is no unbounded aggregate warnings list. A mid-stream cursor error is framed
// as partial output with the classified exit code.
func streamDiffJSON(w *output.Writer, sr appdiff.StreamResult, filters []worktree.RelPath, cfg domainconfig.Config, bg baselineGitContext) error {
	cur := sr.Files
	var summary compare.Summary
	err := w.StreamJSON("diff", func(o *output.JSONObjectWriter) error {
		o.Field("left", stateView(sr.Left))
		o.Field("right", stateView(sr.Right))
		o.Field("path_filters", filterStrings(filters))
		o.Field("rename_detection", toRenameDetectionView(sr.RenameDetection))
		if bg.view != nil {
			o.Field("freshness", bg.view)
		}
		o.Field("review_coverage", reviewCoverageViewOf())
		o.Array("files", func(arr *output.JSONArrayWriter) {
			for cur.Next() {
				fd := cur.FileDiff()
				summary.Add(fd.Change)
				v := fileDiffView{
					changeView:   toChangeView(fd.Change),
					Availability: fd.Availability.String(),
					Reason:       fd.Reason,
				}
				if fd.Availability == appdiff.Text {
					v.Diff = fd.Text
				}
				arr.Add(v)
				// Stop once the sink breaks: each pull is a blob read + diff render, so draining
				// a broken pipe is size-proportional waste. See JSONArrayWriter.Err's contract.
				if arr.Err() != nil {
					return
				}
			}
		})
		if cerr := cur.Err(); cerr != nil {
			return cerr
		}
		o.Field("summary", summary)
		o.Field("scope", scopeViewOf(summary.Skipped, cfg))
		return nil
	})
	if err != nil {
		return partialOutputError(true, err)
	}
	return nil
}
