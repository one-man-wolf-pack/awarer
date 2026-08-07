package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"awarer/internal/app/changes"
	"awarer/internal/app/configcmd"
	"awarer/internal/app/state"
	"awarer/internal/domain/compare"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/gitfresh"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/projfs"
	"awarer/internal/output"
)

// runChanges handles "awa changes". It compares two states (default latest..now),
// optionally narrowed by path arguments, and reports the change set. It records
// nothing: a "now" side is an ephemeral scan.
func runChanges(w *output.Writer, inv invocation) error {
	rangeTok, pathArgs, stat, nameOnly, noRenames, err := parseChangesArgs(inv.args)
	if err != nil {
		return err
	}
	if stat && nameOnly {
		return usageErrorf("--stat and --name-only cannot be combined")
	}
	// --stat and --name-only shape the human text output; they carry no machine
	// data --json does not already provide (the summary counts and the full change
	// list with paths are always in the JSON document). Combining them with --json
	// would silently no-op, so reject it rather than grow a confusing contract.
	if inv.options.JSON && stat {
		return jsonHumanModeError("--stat", "includes the summary")
	}
	if inv.options.JSON && nameOnly {
		return jsonHumanModeError("--name-only", "lists changes with paths")
	}

	rng, err := state.ParseRange(rangeTok)
	if err != nil {
		return usageErrorf("%v", err)
	}

	proj, layout, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}
	// Tokens after "--" are literal path filters, never ranges or flags — the way
	// to name a path that looks like a range ("a..b") or a flag ("-x.go").
	pathArgs = append(pathArgs, inv.operands...)
	filters, err := parsePathFilters(layout.Root(), pathArgs)
	if err != nil {
		return err
	}

	resolver, cleanup, err := buildStateResolver(layout, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := inv.invCtx()
	svc := changes.New(resolver)
	req := changes.Request{
		Range:         rng,
		Now:           state.NowContext{Project: proj, Config: cfg},
		DetectRenames: cfg.Checkpoint.RenameDetection && !noRenames,
		PathFilters:   filters,
	}

	// JSON streams the change set: fixed facts, then the changes array element-by-
	// element (never a materialized []changeView), then the aggregates computed while
	// draining. With rename detection on (the default) the comparison eagerly pairs the
	// whole set inside Stream, so a corrupt manifest fails before any byte is written —
	// a clean, non-partial failure. Under --no-renames the merge is lazy and a
	// mid-stream error leaves a partial document with a non-zero exit, exactly like the
	// human streaming path.
	if inv.options.JSON {
		sr, err := svc.Stream(ctx, req)
		if err != nil {
			return classifyStateError(err)
		}
		defer func() { _ = sr.Close() }()
		// Baseline freshness is resolved here at the command edge (git I/O lives here, not
		// in the view builder) and drives the JSON document.
		bg := baselineGitContextOf(ctx, layout.Root(), sr.Left, sr.Right)
		return streamChangesJSON(w, sr, filters, cfg, bg)
	}

	td, err := timeDisplayFromFlag("", cfg)
	if err != nil {
		return usageErrorf("%v", err)
	}
	// Human output streams: the change events are pulled one at a time and printed,
	// so a large drift never materializes as a full change set.
	sr, err := svc.Stream(ctx, req)
	if err != nil {
		return classifyStateError(err)
	}
	defer func() { _ = sr.Close() }()
	// Baseline freshness is resolved here at the command edge and passed into the
	// presentation layer, which performs no git I/O of its own.
	bg := baselineGitContextOf(ctx, layout.Root(), sr.Left, sr.Right)
	return renderChanges(w, sr, filters, stat, nameOnly, td, bg)
}

// parseChangesArgs parses the changes-local arguments: at most one range or
// "-N" shortcut token, zero or more path arguments, and the boolean flags.
func parseChangesArgs(args []string) (rangeTok string, paths []string, stat, nameOnly, noRenames bool, err error) {
	haveRange := false
	setRange := func(tok string) error {
		if haveRange {
			return usageErrorf("only one range may be given (got a second: %q)", tok)
		}
		rangeTok = tok
		haveRange = true
		return nil
	}
	fail := func(e error) (string, []string, bool, bool, bool, error) {
		return "", nil, false, false, false, e
	}
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, _, hasValue := splitFlag(tok)
		switch name {
		case "--stat":
			if err := noValue(name, hasValue); err != nil {
				return fail(err)
			}
			stat = true
		case "--name-only":
			if err := noValue(name, hasValue); err != nil {
				return fail(err)
			}
			nameOnly = true
		case "--no-renames":
			if err := noValue(name, hasValue); err != nil {
				return fail(err)
			}
			noRenames = true
		default:
			isPath, err := routePositional("changes", tok, name, hasValue, setRange)
			if err != nil {
				return fail(err)
			}
			if isPath {
				paths = append(paths, tok)
			}
		}
	}
	return rangeTok, paths, stat, nameOnly, noRenames, nil
}

// loadProjectConfig resolves the project, its layout, and the effective config
// (honoring --config and --trust-mode), shared by changes, diff, and the run
// commands. Callers that also need the per-layer facts (status) call
// loadProjectConfigFull to get the whole aggregate from the same read.
func loadProjectConfig(opts GlobalOptions) (projfs.Project, paths.Layout, domainconfig.Config, error) {
	p, l, loaded, err := loadProjectConfigFull(opts)
	return p, l, loaded.Config(), err
}

// loadProjectConfigFull resolves the project, its layout, and the single validated
// config aggregate — effective config, origins, and per-layer existence facts, all
// from one read. Returning the aggregate keeps the config and its provenance from
// being assembled separately, so a consumer never pairs a config with layer facts
// gathered by an independent stat that a concurrent edit could make disagree.
func loadProjectConfigFull(opts GlobalOptions) (projfs.Project, paths.Layout, domainconfig.ResolvedConfig, error) {
	p, err := resolveProject(opts)
	if err != nil {
		return projfs.Project{}, paths.Layout{}, domainconfig.ResolvedConfig{}, err
	}
	l, err := p.Paths()
	if err != nil {
		return projfs.Project{}, paths.Layout{}, domainconfig.ResolvedConfig{}, err
	}
	files, err := projectLayerFiles(l, opts)
	if err != nil {
		return projfs.Project{}, paths.Layout{}, domainconfig.ResolvedConfig{}, err
	}
	loaded, err := configcmd.LoadLayered(files, trustModeOverride(opts))
	if err != nil {
		return projfs.Project{}, paths.Layout{}, domainconfig.ResolvedConfig{}, classifyConfigLoad(err)
	}
	return p, l, loaded, nil
}

// renderChanges streams the human change report: it pulls change events one at a
// time from the cursor and prints each, accumulating only bounded summary counts,
// so a large drift is never held in memory. On a mid-stream cursor error it returns
// a partial-output error — the command exits non-zero and the diagnostic goes to
// stderr, so the already-printed lines are never mistaken for a complete report.
func renderChanges(w *output.Writer, sr changes.StreamResult, filters []worktree.RelPath, stat, nameOnly bool, td domainconfig.TimeDisplay, bg baselineGitContext) error {
	// The caller owns the StreamResult's lifecycle and closes it (cursor + states); this
	// renderer only consumes the cursor.
	cur := sr.Changes

	// --name-only is the scripting form (its lines are fed to other tools), so it stays
	// header-free; every other human view leads with the baseline/range context.
	headerPrinted := false
	if !nameOnly {
		renderRangeHeader(w, state.NewStateRangeSummary(sr.Left, sr.Right, filters), td, bg)
		headerPrinted = true
	}

	switch {
	case stat:
		// --stat prints one summary line, which needs the full counts. Draining the
		// cursor into bounded Summary counters (not the change list) keeps it memory-
		// bounded while still one line of output.
		var sum compare.Summary
		for cur.Next() {
			sum.Add(cur.Change())
		}
		if err := cur.Err(); err != nil {
			return partialOutputError(headerPrinted, err)
		}
		if sum.Total() == 0 {
			w.Line("no changes")
			emitReviewCoverage(w)
			emitScopeNote(w, 0)
			return nil
		}
		renderChangesStat(w, sum)
		emitScopeNote(w, sum.Skipped)
	case nameOnly:
		wrote := false
		for cur.Next() {
			w.Line(cur.Change().PrimaryPath().String())
			wrote = true
		}
		if err := cur.Err(); err != nil {
			return partialOutputError(wrote, err)
		}
	default:
		printed, skipped := 0, 0
		for cur.Next() {
			c := cur.Change()
			w.Line(formatChangeLine(c))
			printed++
			if c.Status == compare.Skipped {
				skipped++
			}
		}
		if err := cur.Err(); err != nil {
			return partialOutputError(headerPrinted || printed > 0, err)
		}
		if printed == 0 {
			w.Line("no changes")
			emitReviewCoverage(w)
		}
		emitScopeNote(w, skipped)
	}
	// A clean drain: if rename detection was requested but skipped for size, say so.
	renameSkippedNote(w, sr.RenameDetection)
	return nil
}

// emitReviewCoverage prints the review-coverage honesty note to stderr: an empty awa
// delta means "no changes since this checkpoint", never "the whole worktree was
// reviewed". It is placed exactly where a reviewer is most tempted to over-trust an
// empty result. It goes to stderr as an advisory (matching the note contract) so
// stdout stays a clean "no changes".
func emitReviewCoverage(w *output.Writer) {
	var rc gitfresh.ReviewCoverage
	w.Diagnostic("note: " + rc.Message())
}

// emitScopeNote prints the honest scope caveat for a human (non-pipe) changes/diff
// report: ignored paths are outside awa's evidence, so a "deleted" path may in fact be
// one that became ignored (awa's scan prunes ignored paths before any tally, so it never
// claims a per-path deleted-vs-ignored distinction). It is emitted on EVERY non-pipe
// report — including an empty "no changes" delta, the case a reviewer is most tempted to
// read as "nothing to review" — so the caveat is never hidden in the JSON alone. Any
// known skipped-in-scope inputs are folded into the same line. Advisory, so it goes to
// stderr and never pollutes a piped stdout (and --name-only omits it entirely).
func emitScopeNote(w *output.Writer, skipped int) {
	if skipped > 0 {
		w.Diagnostic(fmt.Sprintf("note: %d input(s) skipped inside scope; ignored paths are outside awa evidence, and a deleted path may be one that became ignored", skipped))
		return
	}
	w.Diagnostic("note: ignored paths are outside awa evidence; deleted paths may include paths now excluded by ignore policy")
}

// partialOutputError frames a mid-stream failure honestly. The underlying error is
// classified first, so it keeps its real exit code — a corrupt-store error
// mid-stream still reports the state-action-required code, not a generic failure. When output has
// already reached stdout the report is incomplete, so the message is prefixed
// "partial output:" (preserving that classified code) — the router prints it to
// stderr, and the exit status marks the truncated stdout as not a clean success.
// Before any output is written it is the ordinary classified failure.
func partialOutputError(wrote bool, err error) error {
	classified := classifyStateError(err)
	if !wrote {
		return classified
	}
	if ce, ok := classified.(*codedError); ok {
		return &codedError{code: ce.code, msg: "partial output: " + ce.msg}
	}
	return genericErrorf("partial output: %v", err)
}

// renameSkippedNote prints the compact human diagnostic for the bounded rename
// presentation policy: when rename detection was requested but the change set exceeded
// the buffer limit, pairing was skipped and changes are shown as plain add/delete. It
// goes to stderr so it never pollutes piped stdout (including --name-only), yet keeps
// the skipped detection non-silent. The command still exits 0 — this is a presentation
// policy, not a failure — so the note is emitted only after a clean drain. The message is
// specific to the limit-exceeded reason, so it matches that reason exactly rather than
// any not-applied outcome: a future not-applied reason must bring its own wording.
func renameSkippedNote(w *output.Writer, rd compare.RenameDetection) {
	if rd.Reason == compare.RenameReasonLimitExceeded {
		w.Diagnostic("note: rename detection skipped: change set exceeded limit; showing adds/deletes")
	}
}

// renderRangeHeader prints the review context for a changes/diff report: the
// effective range, the baseline checkpoint (with its message and age) when the
// baseline is an explicit checkpoint, the current git HEAD and a baseline-freshness
// note when the baseline predates or diverges from it, and the path filter in effect.
// It is the answer to "what is this compared against, and does it still map onto
// git?" that a reviewer would otherwise have to infer. It goes to stdout as the
// leading lines of the human report. The git context is computed by the caller (no I/O
// here); a zero bg renders no git line or note.
func renderRangeHeader(w *output.Writer, sum state.StateRangeSummary, td domainconfig.TimeDisplay, bg baselineGitContext) {
	now := time.Now()
	w.Linef("range: %s -> %s", rangeEndpointLabel(sum.Left), rangeEndpointLabel(sum.Right))
	if base := sum.Left; base.Kind == state.KindCheckpoint && base.HasCheckpoint {
		// base.CreatedAt is the zero time when HasCreatedAt is false, which baselineLabel
		// renders as "no age", so it needs no separate presence flag.
		w.Line("baseline: " + baselineLabel(base.CheckpointID.Short(), base.Message, base.CreatedAt, td, now))
		if bg.head.ShortCommit != "" {
			w.Line("git: " + formatGitLine(bg.head.Branch, bg.head.ShortCommit, bg.head.Subject, ""))
		}
		if note := freshnessNote(bg.freshness, bg.head); note != "" {
			w.Diagnostic("note: " + note)
		}
	}
	if len(sum.PathFilters) > 0 {
		w.Linef("paths: %s", strings.Join(sum.PathFilters, ", "))
	}
}

// baselineLabel renders a checkpoint as "checkpoint <short> "<message>" (<age>)",
// the one way a checkpoint baseline is named across the review surfaces (the
// changes/diff header and the status dashboard) so the two can never drift. The
// message and age are optional: an empty message and a zero createdAt are simply
// omitted.
func baselineLabel(shortID, message string, createdAt time.Time, td domainconfig.TimeDisplay, now time.Time) string {
	label := "checkpoint " + shortID
	if message != "" {
		label += " " + strconv.Quote(message)
	}
	if !createdAt.IsZero() {
		label += " (" + FormatTime(td, createdAt, now) + ")"
	}
	return label
}

// rangeEndpointLabel renders one side for the "range:" line as the ref the user
// asked for (or the default resolved to) — "latest", "@-2", a name, or an explicit
// id prefix — so the header reveals whether the baseline came from the shared,
// movable "latest" or from an explicit checkpoint. The "baseline:" line below
// carries the resolved detail (id, message, age); the model is "range = what you
// asked for, baseline = what it resolved to". It falls back to the resolved label
// when no requested ref is recorded.
func rangeEndpointLabel(e state.StateEndpoint) string {
	if e.RequestedRef != "" {
		return e.RequestedRef
	}
	return endpointLabel(e)
}

// endpointLabel renders one side of a range as a compact label: "now", an explicit
// checkpoint ("checkpoint <short>"), or a run observation ("run <short>
// before/after"). It falls back to the requested/canonical ref token when a kind
// carries no id.
func endpointLabel(e state.StateEndpoint) string {
	switch e.Kind {
	case state.KindNow:
		return "now"
	case state.KindCheckpoint:
		if e.HasCheckpoint {
			return "checkpoint " + e.CheckpointID.Short()
		}
	case state.KindRunObservation:
		if e.HasRun {
			return "run " + shortRunID(e.RunID) + " " + e.RunSel.String()
		}
	}
	if e.CanonicalRef != "" {
		return e.CanonicalRef
	}
	return e.RequestedRef
}

// shortRunID abbreviates a run id to its short form for a range label, tolerating a
// value that is already short.
func shortRunID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// formatChangeLine renders one change as "<code> <path>", with a rename arrow
// and parenthesized annotations (a mode change, a note) where relevant.
func formatChangeLine(c compare.Change) string {
	var b strings.Builder
	b.WriteString(c.Status.Code())
	b.WriteByte(' ')
	if c.Status == compare.Renamed {
		b.WriteString(c.OldPath.String())
		b.WriteString(" -> ")
		b.WriteString(c.NewPath.String())
	} else {
		b.WriteString(c.PrimaryPath().String())
	}
	if annots := changeAnnotations(c); len(annots) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(annots, ", "))
		b.WriteByte(')')
	}
	return b.String()
}

// changeAnnotations collects the parenthesized notes for a change: a permission
// change (carried structurally, so it shows even alongside a content edit) and
// any note the comparison recorded (hash-only, skipped, traversal).
func changeAnnotations(c compare.Change) []string {
	var annots []string
	if c.ModeChanged() {
		annots = append(annots, fmt.Sprintf("mode %04o -> %04o", c.OldMode, c.NewMode))
	}
	if c.Note != "" {
		annots = append(annots, c.Note)
	}
	return annots
}

func renderChangesStat(w *output.Writer, s compare.Summary) {
	w.Linef("%d changed: %d added, %d modified, %d deleted, %d renamed, %d type-changed, %d skipped",
		s.Total(), s.Added, s.Modified, s.Deleted, s.Renamed, s.TypeChanged, s.Skipped)
}

// --- JSON views ---

type stateRefView struct {
	Ref            string `json:"ref"`
	Kind           string `json:"kind"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	RunObservation string `json:"run_observation,omitempty"`
	// RunReuse and RunReuseReason report the recorded run's reuse classification when
	// a side is a run observation, so a comparison is self-describing: an agent sees
	// it compared, say, a record-only run's observations, not a reusable cache entry's.
	RunReuse       string `json:"run_reuse,omitempty"`
	RunReuseReason string `json:"run_reuse_reason,omitempty"`
	TreeHash       string `json:"tree_hash"`
	// Message and CreatedAt expose the resolved checkpoint's label and creation time
	// for a checkpoint side, so the JSON carries the same baseline facts the human
	// header names ("baseline: checkpoint <short> <message> (<age>)") — an agent never has to
	// parse the human text to learn which checkpoint a comparison ran against.
	Message   string `json:"message,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type changeView struct {
	Status               string `json:"status"`
	OldPath              string `json:"old_path,omitempty"`
	NewPath              string `json:"new_path,omitempty"`
	Kind                 string `json:"kind"`
	OldContentHash       string `json:"old_content_hash,omitempty"`
	NewContentHash       string `json:"new_content_hash,omitempty"`
	OldSize              int64  `json:"old_size"`
	NewSize              int64  `json:"new_size"`
	OldMode              string `json:"old_mode,omitempty"`
	NewMode              string `json:"new_mode,omitempty"`
	ContentDiffAvailable bool   `json:"content_diff_available"`
	Note                 string `json:"note,omitempty"`
}

// renameDetectionView is the structural rename-detection fact in changes/diff JSON: a
// machine-readable record of whether rename pairing was requested and applied, so an
// agent learns that a large comparison was shown as add/delete without parsing prose.
type renameDetectionView struct {
	Attempted bool   `json:"attempted"`
	Applied   bool   `json:"applied"`
	Reason    string `json:"reason,omitempty"`
	Limit     int    `json:"limit"`
}

func toRenameDetectionView(rd compare.RenameDetection) renameDetectionView {
	return renameDetectionView{
		Attempted: rd.Attempted,
		Applied:   rd.Applied,
		Reason:    rd.Reason,
		Limit:     rd.Limit,
	}
}

func stateView(s *state.ResolvedState) stateRefView {
	v := stateRefView{Ref: s.RequestedRef, TreeHash: s.TreeHash.String()}
	switch s.Kind {
	case state.KindNow:
		v.Kind = "now"
	case state.KindCheckpoint:
		v.Kind = "checkpoint"
		if id, ok := s.CheckpointID(); ok {
			v.CheckpointID = id.String()
		}
		if s.Header != nil {
			v.Message = s.Header.Message.FirstLine()
			v.CreatedAt = s.Header.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
	case state.KindRunObservation:
		v.Kind = "run-observation"
		if id, sel, reuse, reason, ok := s.RunObservation(); ok {
			v.RunID = id
			v.RunObservation = sel.String()
			v.RunReuse = reuse
			v.RunReuseReason = reason
		}
	}
	return v
}

// changeKind returns the kind to display for a change: the old kind for a
// delete, otherwise the new kind.
func changeKind(c compare.Change) worktree.FileKind {
	if c.Status == compare.Deleted {
		return c.OldKind
	}
	return c.NewKind
}

func toChangeView(c compare.Change) changeView {
	return changeView{
		Status:               c.Status.String(),
		OldPath:              c.OldPath.String(),
		NewPath:              c.NewPath.String(),
		Kind:                 changeKind(c).String(),
		OldContentHash:       c.OldContent.String(),
		NewContentHash:       c.NewContent.String(),
		OldSize:              c.OldSize,
		NewSize:              c.NewSize,
		OldMode:              octalMode(c.OldMode, c.OldModeSet),
		NewMode:              octalMode(c.NewMode, c.NewModeSet),
		ContentDiffAvailable: c.ContentDiffAvailable,
		Note:                 c.Note,
	}
}

// octalMode renders permission bits as a 4-digit octal string, or "" when the
// side carries no mode (absent or non-regular), so the JSON omits it. The set
// flag — not a zero value — decides presence, since 0000 is valid permissions.
func octalMode(mode uint32, set bool) string {
	if !set {
		return ""
	}
	return fmt.Sprintf("%04o", mode)
}

// streamChangesJSON writes changes --json as a streamed envelope: the fixed facts and
// the rename-detection outcome (known once Stream returns), then the changes array
// pulled one at a time from the cursor, then the fixed-size aggregates (summary, scope)
// folded during the drain. Peak memory is one change, not the whole set — the per-change
// content-availability facts (status, note, content_diff_available) live on each array
// element, so there is no unbounded aggregate warnings list to accumulate. A mid-stream
// cursor error is framed as partial output with the classified exit code, so a truncated
// document is never mistaken for a clean report.
func streamChangesJSON(w *output.Writer, sr changes.StreamResult, filters []worktree.RelPath, cfg domainconfig.Config, bg baselineGitContext) error {
	cur := sr.Changes
	var summary compare.Summary
	err := w.StreamJSON("changes", func(o *output.JSONObjectWriter) error {
		o.Field("left", stateView(sr.Left))
		o.Field("right", stateView(sr.Right))
		o.Field("path_filters", filterStrings(filters))
		o.Field("rename_detection", toRenameDetectionView(sr.RenameDetection))
		if bg.view != nil {
			o.Field("freshness", bg.view)
		}
		o.Field("review_coverage", reviewCoverageViewOf())
		o.Array("changes", func(arr *output.JSONArrayWriter) {
			for cur.Next() {
				c := cur.Change()
				summary.Add(c)
				arr.Add(toChangeView(c))
				// Stop draining once the sink breaks (e.g. piped to head): see Err's contract.
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
		// The envelope prefix is already on stdout, so any error here yields a partial
		// document; frame it as partial output (preserving the classified exit code).
		return partialOutputError(true, err)
	}
	return nil
}

func filterStrings(filters []worktree.RelPath) []string {
	out := make([]string, 0, len(filters))
	for _, f := range filters {
		out = append(out, f.String())
	}
	return out
}
