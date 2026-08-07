package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"awarer/internal/app/changes"
	"awarer/internal/app/configcmd"
	"awarer/internal/app/initcmd"
	apprun "awarer/internal/app/run"
	"awarer/internal/app/state"
	"awarer/internal/app/status"
	"awarer/internal/domain/compare"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/configref"
	"awarer/internal/domain/gitfresh"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/projfs"
	"awarer/internal/output"
)

// resolveProject resolves the project a root-aware command operates on: an
// explicit --root if given, otherwise discovery from the current directory.
// Either path returns a projfs.Project, which exists only when its root owns a
// .awa directory — so ownership is proven here, not assumed by downstream code.
// A missing project is an ExitNotFound error; an I/O failure while resolving is a
// generic error, never silently reported as "not a project".
func resolveProject(opts GlobalOptions) (projfs.Project, error) {
	if opts.Root != "" {
		// Open normalizes root to absolute; we compute abs only to phrase the
		// error in terms the user can locate.
		p, err := projfs.Open(opts.Root)
		if err == nil {
			return p, nil
		}
		abs := opts.Root
		if a, aerr := filepath.Abs(opts.Root); aerr == nil {
			abs = a
		}
		if errors.Is(err, projfs.ErrRootNotFound) {
			return projfs.Project{}, notFoundErrorf("not an awa project: %s does not contain a %s directory\nrun 'awa init' to create one", abs, paths.Dir)
		}
		return projfs.Project{}, genericErrorf("opening project at %s: %v", abs, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		return projfs.Project{}, genericErrorf("determining current directory: %v", err)
	}
	p, err := projfs.Find(wd)
	if errors.Is(err, projfs.ErrRootNotFound) {
		return projfs.Project{}, notFoundErrorf("not an awa project: no %s directory found in this or any parent directory\nrun 'awa init' to create one", paths.Dir)
	}
	if err != nil {
		return projfs.Project{}, genericErrorf("locating project root: %v", err)
	}
	return p, nil
}

// requireAccepted fails when a global option was provided that the command does
// not act on. Each command declares the capabilities it honors; anything else
// that was set is a loud usage error rather than a silently ignored flag. This
// is the single rule that keeps "the parser accepts it but the command ignores
// it" from recurring across commands.
func requireAccepted(cmd string, opts GlobalOptions, accepted ...capability) error {
	allow := make(map[capability]bool, len(accepted))
	for _, c := range accepted {
		allow[c] = true
	}
	for _, c := range allCapabilities {
		if opts.has(c) && !allow[c] {
			return usageErrorf("%s is not valid for %s", c, cmd)
		}
	}
	return nil
}

// configFlagOverride returns the absolute --config path and true when --config
// was given, so callers can honor the override before falling back to a root.
func configFlagOverride(opts GlobalOptions) (string, bool, error) {
	if opts.Config == "" {
		return "", false, nil
	}
	abs, err := filepath.Abs(opts.Config)
	if err != nil {
		return "", false, genericErrorf("resolving --config: %v", err)
	}
	// An absent project config is valid (the defaults apply), but a --config
	// override is an explicit request for a specific file: if it is missing,
	// fail loudly rather than silently falling back to the defaults.
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, notFoundErrorf("--config file not found: %s", abs)
		}
		return "", false, genericErrorf("reading --config: %v", err)
	}
	return abs, true, nil
}

// projectLayerFiles assembles the layered config file set for a project command:
// the shared, committable awa.toml, the private local .awa/config.toml, and an
// explicit --config override when given. It is the single place project commands
// build the layer stack, so they all honor the same shared->local->override
// precedence.
func projectLayerFiles(layout paths.Layout, opts GlobalOptions) (configcmd.LayerFiles, error) {
	files := configcmd.LayerFiles{Shared: layout.SharedConfigFile(), Local: layout.ConfigFile()}
	override, ok, err := configFlagOverride(opts)
	if err != nil {
		return configcmd.LayerFiles{}, err
	}
	if ok {
		files.Override = override
	}
	return files, nil
}

// runInit handles "awa init". It parses init-local flags from the leftover
// arguments — these are intentionally not global flags — and creates a project.
func runInit(w *output.Writer, inv invocation) error {
	profile := initcmd.ProfileDefault

	args := inv.args
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--profile":
			// Shares requireValue with the global parser, so --profile= and
			// --profile "" obey the same "non-empty value" rule as --root etc.
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			p, err := initcmd.ParseProfile(v)
			if err != nil {
				return usageErrorf("%v", err)
			}
			profile = p
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for init", tok)
			}
			return usageErrorf("unexpected argument %q for init", tok)
		}
	}

	root, err := initRoot(inv.options)
	if err != nil {
		return err
	}

	res, err := initcmd.Run(initcmd.Request{Root: root, Profile: profile})
	if err != nil {
		if errors.Is(err, initcmd.ErrAlreadyInitialized) {
			return genericErrorf("project already initialized: %s exists\nremove it or choose another directory to reinitialize", paths.New(root).AwaDir())
		}
		return genericErrorf("initializing project: %v", err)
	}

	if inv.options.JSON {
		return w.JSON("init", struct {
			Root        string   `json:"root"`
			ConfigPath  string   `json:"config_path"`
			CreatedDirs []string `json:"created_dirs"`
		}{res.Root, res.ConfigPath, res.CreatedDirs})
	}
	w.Linef("initialized awa project at %s", res.Root)
	if res.ConfigPath != "" {
		w.Linef("config: %s", res.ConfigPath)
	} else {
		w.Line("config: using built-in defaults (no config file written)")
	}
	return nil
}

// initRoot resolves where to initialize: an explicit --root, else the current
// directory. Unlike resolveRoot it does not walk upward, since init creates a
// new project rather than discovering an existing one.
func initRoot(opts GlobalOptions) (string, error) {
	if opts.Root != "" {
		abs, err := filepath.Abs(opts.Root)
		if err != nil {
			return "", genericErrorf("resolving --root: %v", err)
		}
		return abs, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", genericErrorf("determining current directory: %v", err)
	}
	return wd, nil
}

// runStatus handles "awa status" (and the bare "awa"). It accepts no arguments.
func runStatus(w *output.Writer, inv invocation) error {
	if len(inv.args) > 0 {
		return rejectExtra("status", inv.args[0])
	}

	// Load the project, layout, and the whole config aggregate once (config + origins
	// + per-layer facts from one read); the durable-facts pass and the review
	// dashboard both reuse it, and status's config facts come from the same read as
	// the config rather than a separate stat that a concurrent edit could contradict.
	proj, layout, loaded, err := loadProjectConfigFull(inv.options)
	if err != nil {
		return err
	}
	cfg := loaded.Config()
	// status writes no durable state, so it only warns about a damaged git guard
	// rather than failing; the restore, when the guard is merely missing, is silent.
	if err := guardPreflight(w, proj, false); err != nil {
		return err
	}

	res, err := status.Run(inv.invCtx(), proj, loaded)
	if err != nil {
		if c := interruptError(err); c != nil {
			return c
		}
		return classifyConfigLoad(err)
	}

	// The review dashboard (checkpoint/baseline, dirty summary, git, reusable/near
	// runs, suggested next commands) is best-effort: it scans the worktree and queries
	// git, and any part that cannot be computed becomes a stderr diagnostic rather than
	// failing status — status is the command a user runs to diagnose trouble, so it
	// must degrade honestly to a partial picture, never a lie. It is computed once and
	// consumed by both the JSON payload and the human dashboard so the two agree.
	review := computeStatusReview(inv.invCtx(), w, proj, layout, cfg, res)

	// JSON carries the durable facts and the review dashboard as one document, so an
	// agent sees the same facts a human does (baseline, dirty summary, cacheability,
	// near misses, follow-up commands) without parsing text.
	if inv.options.JSON {
		return w.JSON("status", statusDoc{Result: res, Review: review})
	}
	w.Linef("root:        %s", res.Root)
	if present := presentLayerPaths(res.ConfigLayers); len(present) > 0 {
		w.Linef("config:      %s", strings.Join(present, ", "))
	} else {
		w.Line("config:      built-in defaults (no awa.toml or .awa/config.toml)")
	}
	w.Line("initialized: yes")
	switch {
	case res.Checkpoints.Populated:
		w.Linef("checkpoints: %d", res.Checkpoints.Recorded)
		if l := res.Checkpoints.Latest; l != nil {
			w.Linef("  latest:    %s at %s (%d skipped)", l.ShortID, l.CreatedAt, l.Skipped)
		}
	case res.Checkpoints.Degraded():
		// A store that is incompatible or corrupt (including one whose listing failed
		// structurally) has no readable checkpoint but is not empty. Say so rather than
		// "none yet"; the warning below carries the reason.
		w.Line("checkpoints: 0 readable")
	default:
		w.Line("checkpoints: none yet")
	}
	if n := res.Checkpoints.Unreadable; n > 0 {
		// The breakdown is added only when there is one to give: a store whose records
		// are all corrupt should not read "(0 incompatible schema)", which invites the
		// reader to wonder what the zero is telling them.
		if inc := res.Checkpoints.Incompatible; inc > 0 {
			w.Linef("  unreadable: %d (%d in an incompatible schema)", n, inc)
		} else {
			w.Linef("  unreadable: %d", n)
		}
	}
	switch {
	case res.RunCache.Populated:
		w.Linef("run cache:   %d runs", res.RunCache.Recorded)
		if l := res.RunCache.Latest; l != nil {
			w.Linef("  latest:       %s exit=%d at %s", l.ShortID, l.ExitCode, l.StartedAt)
		}
		// One label per read state, each naming the state itself. The column is padded to
		// the longest so a reader scans the counts, not the ragged labels.
		if res.RunCache.CorruptInSample > 0 {
			w.Linef("  corrupt:      %d (in newest %d)", res.RunCache.CorruptInSample, res.RunCache.SampleSize)
		}
		if res.RunCache.IncompatibleInSample > 0 {
			w.Linef("  incompatible: %d (in newest %d)", res.RunCache.IncompatibleInSample, res.RunCache.SampleSize)
		}
		if res.RunCache.UnhealthyInSample > 0 {
			w.Linef("  unhealthy:    %d (in newest %d)", res.RunCache.UnhealthyInSample, res.RunCache.SampleSize)
		}
	case res.RunCache.Degraded():
		// A missing, unreadable, or permission-denied run store has no readable runs but
		// is not an honestly-empty cache. Say so rather than "none yet"; the degradation
		// warning below carries the reason.
		w.Line("run cache:   0 readable")
	default:
		w.Line("run cache:   none yet")
	}
	w.Linef("store:       %s", res.Store.Path)
	// The exact blob footprint is intentionally omitted from the ordinary dashboard —
	// computing it walks the whole blob store — so status stays cheap. The typed next
	// commands below point at the explicit diagnostics that report it.
	//
	// A degradation renders as a calm "note" when it is advisory (a bounded, still-clean
	// read) and a "warning" when it is a correctness signal (corrupt/incompatible/unreadable),
	// so an agent's exhaustive-check reminder never reads as an alarm.
	for _, d := range res.Degradations {
		if d.Detail == "" {
			continue
		}
		if d.Advisory() {
			w.Diagnostic("note: " + d.Detail)
		} else {
			w.Diagnostic("warning: " + d.Detail)
		}
	}

	td, err := timeDisplayFromFlag("", cfg)
	if err != nil {
		return usageErrorf("%v", err)
	}
	renderStatusReview(w, res, review, td)
	return nil
}

// --- status review dashboard: JSON shapes ---

// statusDoc is the JSON payload of "awa status": the durable facts (embedded, so
// every existing status field stays at its top-level path) plus the review
// dashboard. Embedding status.Result keeps the durable-facts contract intact while
// adding the review object additively.
type statusDoc struct {
	status.Result
	Review reviewDoc `json:"review"`
}

// reviewDoc is the machine-readable review dashboard: the same facts the human
// dashboard renders — the latest checkpoint/baseline, the dirty summary since it,
// live git state, which recent runs are reusable now (or the nearest miss), and the
// suggested follow-up commands.
type reviewDoc struct {
	Checkpoint reviewCheckpointDoc `json:"checkpoint"`
	Dirty      reviewDirtyDoc      `json:"dirty"`
	Git        reviewGitDoc        `json:"git"`
	Runs       reviewRunsDoc       `json:"runs"`
	Next       []reviewNextDoc     `json:"next"`
	// Partial is true when a section could not be fully computed; Warnings then
	// explains which section degraded and why, so a consumer distinguishes a normal
	// absent section (e.g. no checkpoint, not a git repo) from a failure without
	// reading the stderr diagnostics.
	// Freshness relates the current baseline checkpoint to the live git HEAD; omitted
	// when there is no baseline or it captured no git commit. ReviewCoverage is the
	// always-present honesty note that this dashboard's checkpoint delta is not a
	// whole-repo git review — status is the acceptance surface, so it always carries it.
	Freshness      *freshnessView     `json:"freshness,omitempty"`
	ReviewCoverage reviewCoverageView `json:"review_coverage"`
	Partial        bool               `json:"partial"`
	Warnings       []reviewWarningDoc `json:"warnings"`
	// Diagnostics carries the dashboard's latency diagnostics (diagnostics.performance[])
	// — currently a huge watched effect root that dominated the run-reuse observation —
	// present only when a known cause crossed the interactive threshold. performance is
	// the same facts in their domain form for the human renderer, unexported so it stays
	// out of JSON (Diagnostics is the machine projection).
	Diagnostics *perfDiagnosticsDoc   `json:"diagnostics,omitempty"`
	performance []perfdiag.Diagnostic `json:"-"`

	// freshness and head carry the classified baseline freshness and current HEAD for
	// the human note, unexported so they stay out of the JSON doc (Freshness holds the
	// machine projection) and the renderer never re-parses its own serialized token.
	freshness gitfresh.Freshness
	head      gitfresh.GitHead
}

// reviewNextDoc is one suggested follow-up command. Kind and Reason are stable
// machine tokens so an agent acts on the suggestion without parsing text; Argv is
// the tokenized invocation (with <...> placeholders where the user must supply a
// value); Display is the human line the dashboard prints (it may carry a trailing
// "# hint").
type reviewNextDoc struct {
	Kind    string   `json:"kind"`
	Reason  string   `json:"reason"`
	Argv    []string `json:"argv"`
	Display string   `json:"display"`
}

// reviewWarningDoc explains one degraded review section: which section, a stable
// code, and the human message (identical to the stderr diagnostic).
type reviewWarningDoc struct {
	Section string `json:"section"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// reviewCheckpointDoc names the latest checkpoint (checkpoint) the dirty summary is
// measured against. Present is false when no checkpoint exists yet.
type reviewCheckpointDoc struct {
	Present      bool   `json:"present"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	ShortID      string `json:"short_id,omitempty"`
	Message      string `json:"message,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	// createdAt carries the checkpoint time for human age rendering; it is unexported
	// so it stays out of the JSON doc (CreatedAt holds the machine timestamp) and the
	// renderer never re-parses its own serialized string. Zero when absent.
	createdAt time.Time
}

// reviewDirtyDoc is the working-tree change summary since the checkpoint. Computed
// is false when there is no checkpoint or the summary could not be computed; Dirty
// is true only when there is at least one change.
type reviewDirtyDoc struct {
	Computed bool            `json:"computed"`
	Dirty    bool            `json:"dirty"`
	Summary  compare.Summary `json:"summary"`
}

// Git state tokens for reviewGitDoc.State. The set is closed, so a consumer keys off
// one token instead of inferring the state from an available/reason field combination.
const (
	gitStateAvailable   = "available"   // a readable git worktree
	gitStateNonGit      = "non-git"     // the project is not a git worktree (normal)
	gitStateUnavailable = "unavailable" // git is a worktree but a command failed
)

// reasonGitCommandFailed is the stable token for a genuine git failure, shared by the
// status dashboard and the timeline so an agent reads one token, never a raw
// err.Error(). The human-readable detail travels on the accompanying warning line.
const reasonGitCommandFailed = "git-command-failed"

// reviewGitDoc is the live git branch and dirty state. State is the single closed token
// an agent keys off: "available" carries branch/short_commit/dirty; "non-git" is a
// normal non-worktree project; "unavailable" is a genuine git failure, whose stable
// Reason token is set (its human detail rides the review warnings, not this doc). Every
// state-specific field is omitted outside its state — Dirty is a *bool present only when
// available — so a non-git object never carries a meaningless "dirty": false.
type reviewGitDoc struct {
	State       string `json:"state"`
	Branch      string `json:"branch,omitempty"`
	ShortCommit string `json:"short_commit,omitempty"`
	Dirty       *bool  `json:"dirty,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// reviewRunsDoc reports how many recent runs are reusable now and, when none are,
// how many near misses exist plus the nearest one. The nearest candidate carries its
// own optional effect diagnosis inside the shared near-miss object; status adds no
// second copy of that fact at its own level.
type reviewRunsDoc struct {
	Reusable int          `json:"reusable"`
	Near     int          `json:"near"`
	Nearest  *nearMissDoc `json:"nearest"`
	// nearestEffect is the same typed diagnosis Nearest.Effect was projected from, kept
	// unexported so the human dashboard renders the fact through the shared renderer
	// instead of reading it back out of the JSON shape. Both come from one candidate, so
	// the dashboard line and the machine object cannot disagree.
	nearestEffect *apprun.EffectDiagnosis
}

// --- status review dashboard: computation ---

// reviewDiag collects the review dashboard's best-effort warnings. A degraded
// section calls warn once, which records the structured warning for the JSON
// payload and mirrors it to stderr for humans — one call, both channels, so the two
// never disagree about what degraded.
type reviewDiag struct {
	w        *output.Writer
	warnings []reviewWarningDoc
}

func (d *reviewDiag) warn(section, code, message string) {
	d.warnings = append(d.warnings, reviewWarningDoc{Section: section, Code: code, Message: message})
	d.w.Diagnostic("warning: " + message)
}

// computeStatusReview gathers the review dashboard facts once, reusing the changes,
// git, and run services. Every sub-step is best-effort: a failure becomes a warning
// (structured in the payload, mirrored to stderr) and an empty/absent section,
// never a failed status. The returned doc drives both the human dashboard and JSON.
func computeStatusReview(ctx context.Context, w *output.Writer, proj projfs.Project, layout paths.Layout, cfg domainconfig.Config, res status.Result) reviewDoc {
	diag := &reviewDiag{w: w}
	// A degraded checkpoint store is evidence the review must not bury: surface it as a
	// structured warning (which also flips Partial), so an agent reading the dashboard
	// cannot miss that the store is incompatible, corrupt, or otherwise unreadable. Gating
	// on Degraded (not just a counted-unreadable total) means a structural listing
	// failure — which reports a corrupt state with no per-record counts — is flagged
	// too. It is recorded structurally only — the human stderr line is already emitted
	// once through the res.Degradations loop above, so mirroring here would double-print
	// it. The message is sourced from that same degradation so the two never drift.
	if res.Checkpoints.Degraded() {
		msg := "checkpoint store is degraded"
		if d, ok := res.Degradation("checkpoints"); ok {
			msg = d.Detail
		}
		diag.warnings = append(diag.warnings, reviewWarningDoc{
			Section: "checkpoints",
			Code:    "checkpoint-store-partial",
			Message: msg,
		})
	}
	// Accepted boundary: computeStatusChanges (dirty summary, history scope) and
	// computeStatusRuns (run reuse, per-run input scope + the shared effect observation)
	// are two independent current-state scans. They deliberately do not share one
	// observed state — the scopes differ, so they cannot collapse into a single scan.
	// They also cost differently. computeStatusChanges keeps the mutable history-scope
	// resolver and reuses committed hashes for unchanged inputs. computeStatusRuns
	// cannot: it answers which stored runs would replay right now, which is a cache
	// decision, and every cache decision reads content rather than trusting a stat
	// signature that a same-size rewrite can leave untouched. It is wired with no index
	// at all (indexNone in buildRunService) because its scans would never consult one.
	// Both that cost and a huge effect root are surfaced through the run listing's
	// diagnostics below.
	checkpoint, dirty, baseline := computeStatusChanges(diag, ctx, layout, cfg, proj, res)
	git := computeStatusGit(diag, ctx, layout.Root())
	runs, runsPerf := computeStatusRuns(diag, ctx, layout, cfg, proj, res)
	next := computeStatusNext(res, dirty.Dirty, runs.Reusable, runs.Near)
	// Baseline freshness against the live git HEAD, best-effort: a git failure degrades
	// to no freshness note (never a failed status). Only classified when there is a
	// checkpoint baseline to relate.
	var freshView *freshnessView
	var fresh gitfresh.Freshness
	var head gitfresh.GitHead
	if baseline.Kind == state.KindCheckpoint && baseline.HasCheckpoint {
		fresh, head = classifyBaselineFreshness(ctx, layout.Root(), baseline)
		freshView = freshnessViewOf(fresh, baseline, head)
	}
	if diag.warnings == nil {
		diag.warnings = []reviewWarningDoc{}
	}
	return reviewDoc{
		Checkpoint:     checkpoint,
		Dirty:          dirty,
		Git:            git,
		Runs:           runs,
		Next:           next,
		Freshness:      freshView,
		ReviewCoverage: reviewCoverageViewOf(),
		Partial:        len(diag.warnings) > 0,
		Warnings:       diag.warnings,
		Diagnostics:    perfDiagnosticsView(runsPerf),
		performance:    runsPerf,
		freshness:      fresh,
		head:           head,
	}
}

// computeStatusChanges resolves the latest checkpoint and the dirty summary since it
// by comparing latest..now through the changes service. When there is no checkpoint
// (no checkpoints), it returns an absent checkpoint and an uncomputed dirty summary
// with no warning — that is normal, not a failure. A resolver/scan failure warns
// under the "dirty" section so the absent summary is explained.
func computeStatusChanges(diag *reviewDiag, ctx context.Context, layout paths.Layout, cfg domainconfig.Config, proj projfs.Project, res status.Result) (reviewCheckpointDoc, reviewDirtyDoc, state.StateEndpoint) {
	if !res.Checkpoints.Populated {
		return reviewCheckpointDoc{Present: false}, reviewDirtyDoc{Computed: false}, state.StateEndpoint{}
	}
	resolver, cleanup, err := buildStateResolver(layout, cfg)
	if err != nil {
		diag.warn("dirty", "state-resolver-failed", "cannot build state resolver: "+err.Error())
		return reviewCheckpointDoc{Present: false}, reviewDirtyDoc{Computed: false}, state.StateEndpoint{}
	}
	defer cleanup()
	// The default range is latest..now (the dirty-since-checkpoint comparison); the
	// zero Range would be now..now and always read clean, so parse the empty token.
	rng, err := state.ParseRange("")
	if err != nil {
		diag.warn("dirty", "range-resolve-failed", "cannot resolve the default range: "+err.Error())
		return reviewCheckpointDoc{Present: false}, reviewDirtyDoc{Computed: false}, state.StateEndpoint{}
	}
	cr, err := changes.New(resolver).Run(ctx, changes.Request{
		Range: rng,
		Now:   state.NowContext{Project: proj, Config: cfg},
	})
	if err != nil {
		diag.warn("dirty", "changes-scan-failed", "cannot compute changes since the latest checkpoint: "+err.Error())
		return reviewCheckpointDoc{Present: false}, reviewDirtyDoc{Computed: false}, state.StateEndpoint{}
	}
	defer func() { _ = cr.Close() }()
	var checkpoint reviewCheckpointDoc
	sum := state.NewStateRangeSummary(cr.Left, cr.Right, nil)
	base := sum.Left
	if base.Kind == state.KindCheckpoint && base.HasCheckpoint {
		checkpoint = reviewCheckpointDoc{
			Present:      true,
			CheckpointID: base.CheckpointID.String(),
			ShortID:      base.CheckpointID.Short(),
			Message:      base.Message,
		}
		if base.HasCreatedAt {
			checkpoint.CreatedAt = base.CreatedAt.UTC().Format(time.RFC3339Nano)
			checkpoint.createdAt = base.CreatedAt
		}
	}
	s := cr.ChangeSet.Summary
	return checkpoint, reviewDirtyDoc{Computed: true, Dirty: s.Total() > 0, Summary: s}, base
}

// computeStatusGit captures the live git branch and dirty state. A non-git project
// yields state "non-git" with no warning — that is normal; a genuine git failure yields
// "unavailable" with a stable reason token and warns under the "git" section (which
// carries the human detail) so the token never has to encode free-form error text.
func computeStatusGit(diag *reviewDiag, ctx context.Context, root string) reviewGitDoc {
	g, ok, err := gitmeta.New(root).Capture(ctx)
	if err != nil {
		diag.warn("git", "git-read-failed", "cannot read git state: "+err.Error())
		return reviewGitDoc{State: gitStateUnavailable, Reason: reasonGitCommandFailed}
	}
	if !ok || g == nil {
		return reviewGitDoc{State: gitStateNonGit}
	}
	dirty := !g.Dirty.Clean
	return reviewGitDoc{State: gitStateAvailable, Branch: g.Branch, ShortCommit: g.ShortCommit, Dirty: &dirty}
}

// computeStatusRuns reports which recent runs are reusable right now and, when none
// are, the nearest misses that explain why recent checks are stale. A single
// Ls(Near) call answers both from one bounded scan pass. Service/scan failures and
// the corrupt-metadata count warn under the "runs" section. It also returns the run
// listing's latency diagnostics (a huge watched effect root that dominated the shared
// effect observation), so the dashboard can surface an actionable note when this
// section is the slow one.
func computeStatusRuns(diag *reviewDiag, ctx context.Context, layout paths.Layout, cfg domainconfig.Config, proj projfs.Project, res status.Result) (reviewRunsDoc, []perfdiag.Diagnostic) {
	if !res.RunCache.Populated {
		return reviewRunsDoc{}, nil
	}
	svc, cleanup, err := buildRunService(ctx, layout, cfg, indexNone)
	if err != nil {
		diag.warn("runs", "run-service-failed", "cannot build run service: "+err.Error())
		return reviewRunsDoc{}, nil
	}
	defer cleanup()

	ls, err := svc.Ls(ctx, apprun.LsRequest{Project: proj, Config: cfg, Near: true})
	if err != nil {
		diag.warn("runs", "run-ls-failed", "cannot read reusable runs: "+err.Error())
		return reviewRunsDoc{}, nil
	}
	if len(ls.Reusable) > 0 {
		return reviewRunsDoc{Reusable: len(ls.Reusable)}, ls.Performance
	}
	if ls.Corrupt > 0 {
		diag.warn("runs", "corrupt", fmt.Sprintf("%d stored run(s) skipped due to corrupt metadata", ls.Corrupt))
	}
	if len(ls.NearMisses) == 0 {
		return reviewRunsDoc{}, ls.Performance
	}
	nearest := nearMissView(ls.NearMisses[0])
	return reviewRunsDoc{
		Near:          len(ls.NearMisses),
		Nearest:       &nearest,
		nearestEffect: ls.NearMisses[0].Effect,
	}, ls.Performance
}

// computeStatusNext builds the next commands most useful given the current state:
// focus a review when the tree is dirty, explain a stale check when only near misses
// exist, or checkpoint when nothing has been recorded yet. Each suggestion carries a
// machine token (kind/reason) and a tokenized argv alongside the human display line,
// so an agent acts on it without parsing text and the human sees the same commands.
func computeStatusNext(res status.Result, dirty bool, reusable, near int) []reviewNextDoc {
	// A non-nil slice so the JSON field is a stable empty array rather than null.
	next := []reviewNextDoc{}
	if !res.Checkpoints.Populated {
		next = append(next, reviewNextDoc{
			Kind: "create-checkpoint", Reason: "no-checkpoint",
			Argv:    []string{"awa", "checkpoint", "-m", "<specific-checkpoint-message>"},
			Display: "awa checkpoint -m \"<specific-checkpoint-message>\"  # replace the placeholder",
		})
	}
	if dirty {
		next = append(next,
			reviewNextDoc{
				Kind: "changes-stat", Reason: "dirty",
				Argv:    []string{"awa", "changes", "--stat"},
				Display: "awa changes --stat     # what changed since the checkpoint",
			},
			reviewNextDoc{
				Kind: "diff-path", Reason: "dirty",
				Argv:    []string{"awa", "diff", "src/"},
				Display: "awa diff src/          # focus the review on a path",
			},
		)
	}
	if near > 0 {
		next = append(next,
			reviewNextDoc{
				Kind: "run-ls-near", Reason: "near-misses",
				Argv:    []string{"awa", "run", "ls", "--near"},
				Display: "awa run ls --near      # why recent checks are stale",
			},
			reviewNextDoc{
				Kind: "run-explain", Reason: "near-misses",
				Argv:    []string{"awa", "run", "explain", "--", "<command>"},
				Display: "awa run explain -- <command>",
			},
		)
	} else if reusable == 0 && res.RunCache.Populated {
		next = append(next, reviewNextDoc{
			Kind: "run-record", Reason: "no-reusable-runs",
			Argv:    []string{"awa", "run", "--", "<command>"},
			Display: "awa run -- <command>   # record a check to reuse later",
		})
	}
	// When a checkpoint exists, the default latest..now baseline is shared
	// project-local state that may have moved if another agent checkpointed. Offer a
	// bounded look at the recent checkpoint timeline so a surprised reviewer can find
	// the right explicit id — appended last so it is never the leading suggestion.
	if res.Checkpoints.Populated {
		next = append(next, reviewNextDoc{
			Kind: "log-recent-checkpoints", Reason: "baseline-discovery",
			Argv:    []string{"awa", "log", "-n", "5"},
			Display: "awa log -n 5          # inspect recent checkpoints if this baseline is surprising",
		})
	}
	// status deliberately omits the exact blob-store footprint (a whole-store walk); when
	// there is a store worth measuring, point at the explicit diagnostic that reports it
	// without inlining an unbounded scan into the dashboard.
	if res.Checkpoints.Populated || res.RunCache.Populated {
		next = append(next, reviewNextDoc{
			Kind: "gc-dry-run", Reason: "store-footprint",
			Argv:    []string{"awa", "gc", "--dry-run", "--json"},
			Display: "awa gc --dry-run --json # store footprint and what cleanup would free",
		})
	}
	// Any degraded evidence (corrupt/incompatible/unreadable/permission) deserves the deeper,
	// exhaustive diagnostic that status intentionally does not run.
	if res.Degraded() {
		next = append(next, reviewNextDoc{
			Kind: "doctor", Reason: "degraded-evidence",
			Argv:    []string{"awa", "doctor", "--json"},
			Display: "awa doctor --json       # inspect and classify the degraded store",
		})
	}
	return next
}

// --- status review dashboard: human rendering ---

// renderStatusReview prints the review dashboard from the computed facts, so the
// human dashboard and the JSON payload are two renderings of one source of truth.
func renderStatusReview(w *output.Writer, res status.Result, r reviewDoc, td domainconfig.TimeDisplay) {
	if !r.Checkpoint.Present {
		if !res.Checkpoints.Populated {
			w.Line("checkpoint:  none yet — create one with: awa checkpoint -m \"<specific-checkpoint-message>\"")
		}
	} else {
		// The same baseline label the changes/diff header prints, so "which checkpoint
		// is this?" reads identically across the review surfaces.
		w.Line("checkpoint:  " + baselineLabel(r.Checkpoint.ShortID, r.Checkpoint.Message, r.Checkpoint.createdAt, td, time.Now()))
	}
	if r.Dirty.Computed {
		if !r.Dirty.Dirty {
			w.Line("  dirty:     clean since checkpoint")
		} else {
			s := r.Dirty.Summary
			w.Linef("  dirty:     %d changed (%dA %dM %dD %dR %dT %dS) — awa changes --stat",
				s.Total(), s.Added, s.Modified, s.Deleted, s.Renamed, s.TypeChanged, s.Skipped)
		}
	}

	// The git line uses the one canonical grammar, and the dashboard always names git
	// state — including a non-git project and an unavailable git — so a reviewer sees at
	// a glance whether a git cross-check applies.
	switch r.Git.State {
	case gitStateAvailable:
		clean := r.Git.Dirty == nil || !*r.Git.Dirty
		w.Linef("git:         %s", formatGitLine(r.Git.Branch, r.Git.ShortCommit, "", worktreeState(clean)))
	case gitStateUnavailable:
		w.Linef("git:         unavailable (%s)", r.Git.Reason)
	default:
		w.Line("git:         non-git")
	}
	// Baseline freshness against the live HEAD is a calm note (predates/diverges is
	// context, not a fault). It sources the enum and head from the same values the JSON
	// projection was built from, so the two never drift.
	if r.Freshness != nil {
		if note := freshnessNote(r.freshness, r.head); note != "" {
			w.Diagnostic("note: " + note)
		}
	}

	if res.RunCache.Populated {
		switch {
		case r.Runs.Reusable > 0:
			w.Linef("reusable:    %d run(s) replayable now — awa run ls", r.Runs.Reusable)
		case r.Runs.Near > 0:
			w.Linef("reusable:    none — %d near miss(es), inspect with: awa run ls --near", r.Runs.Near)
			if n := r.Runs.Nearest; n != nil {
				w.Linef("  nearest:   %s %s not-reusable(%s)", n.ShortID,
					strings.Join(n.Command, " "), n.Reason)
				// At most one compact detail line, and only where the candidate carries an
				// observed root the reason token above does not already state. The deeper
				// commands in "next:" carry the exclude/effect-root/record decision.
				if detail := effectDetail(r.Runs.nearestEffect); detail != "" {
					w.Linef("  effect:    %s", detail)
				}
			}
		default:
			w.Line("reusable:    none for the current state")
		}
	}

	if len(r.Next) > 0 {
		w.Line("next:")
		for _, n := range r.Next {
			w.Line("  " + n.Display)
		}
	}

	// A latency diagnostic (a huge watched effect root that dominated the run-reuse
	// observation) is a calm, bounded advisory before the closing coverage note. It
	// stays quiet when nothing crossed the interactive threshold.
	emitPerformanceNotes(w, r.performance)

	// status is the acceptance-decision surface, so it always closes with the honesty
	// note that this dashboard's checkpoint delta is not a whole-repo git review. It goes
	// to stderr as an advisory so stdout stays the machine-free dashboard body.
	w.Diagnostic("note: " + r.ReviewCoverage.Message)
}

// humanizeBytes renders a byte count in compact binary units for status output.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + "KMGTPE"[exp:exp+1] + "iB"
}

// configSubcommands lists the config subcommands, the single source for config's
// contract (summary, honored capabilities, authored help, handler), attached to
// the config command's registry entry (see newRouter). path/show/effective/validate
// honor --root/--config/--json; effective also applies a --trust-mode/--strict
// override. template renders the reference and needs no project; init writes a
// scaffold and needs a root. A zero help falls back to thin summary-derived help;
// author it for a subcommand with local flags or a positional the summary does not
// make discoverable (e.g. init's --shared/--local).
var configSubcommands = []subcommand{
	{name: "path", summary: "show the shared and local config paths and which exist", caps: []capability{capRoot, capConfig, capJSON}, run: runConfigPath},
	{name: "show", summary: "print a layer's raw contents (config show [shared|local])", caps: []capability{capRoot, capConfig, capJSON}, run: runConfigShow, help: commandHelp{
		usage:    []string{"config show [shared|local]"},
		long:     "Print one config layer's raw file contents. With no argument, prints the single existing layer, or asks you to name one when both exist.",
		examples: []string{"awa config show shared", "awa config show local --json"},
		topic:    "config",
	}},
	{name: "effective", summary: "print the composed config plus each value's origin layer", caps: []capability{capRoot, capConfig, capJSON, capTrustMode}, run: runConfigEffective},
	{name: "validate", summary: "check that every active config layer is valid", caps: []capability{capRoot, capConfig, capJSON}, run: runConfigValidate},
	{name: "template", summary: "print an annotated config template to stdout", caps: nil, run: runConfigTemplate},
	{name: "init", summary: "write a shared awa.toml or local .awa/config.toml scaffold", caps: []capability{capRoot, capJSON}, run: runConfigInit, help: commandHelp{
		usage: []string{"config init (--shared | --local) [--force]"},
		long:  "Write an annotated config scaffold. Choose exactly one layer: --shared writes a committable awa.toml; --local writes a private, untracked .awa/config.toml. An existing file is never overwritten without --force.",
		flags: []flagHelp{
			{"--shared", "write the shared, committable awa.toml", ""},
			{"--local", "write the private, untracked .awa/config.toml", ""},
			{"--force", "overwrite an existing config file", ""},
		},
		examples: []string{"awa config init --local", "awa config init --shared --force"},
		topic:    "config",
	}},
}

// configCapabilities is the coarse set route validates "awa config" against;
// runConfig then applies the precise per-subcommand set.
func configCapabilities() []capability { return unionCapabilities(configSubcommands) }

// configCmdHelp is the single source for the "awa config --help" frame (title and
// purpose). configHelp renders it and appends the subcommand list from
// configSubcommands, so the subcommand names live in exactly one place.
var configCmdHelp = commandHelp{
	usage: []string{"config <subcommand>"},
	long:  "Inspect or scaffold configuration. path, show, effective, and validate act on the layered stack (--root/--config); effective also applies a --trust-mode/--strict override. init writes a scaffold (--root, but no --config). template prints an annotated template to stdout and needs no project.",
	topic: "config",
}

// runConfig handles "awa config <subcommand>".
func runConfig(w *output.Writer, inv invocation) error {
	if inv.options.Help {
		return configHelp(w, inv)
	}
	if len(inv.args) == 0 {
		return usageErrorf("config requires a subcommand: path, show, effective, validate, template, or init")
	}
	sub := inv.args[0]

	spec := lookupSubcommand(configSubcommands, sub)
	if spec == nil {
		return usageErrorf("unknown config subcommand %q: want path, show, effective, validate, template, or init", sub)
	}
	return dispatchSubcommand(w, "config", spec, inv)
}

// configHelp renders "awa config --help" and "awa config <sub> --help". The bare
// form renders the configCmdHelp frame and lists the subcommands from their single
// source (configSubcommands); a named subcommand renders its thin help. It does no
// project or lock work.
func configHelp(w *output.Writer, inv invocation) error {
	if len(inv.args) > 0 {
		if spec := lookupSubcommand(configSubcommands, inv.args[0]); spec != nil {
			writeHelpBody(w, "awa config "+spec.name, subcommandHelp("config", *spec), hasCap(spec.caps, capJSON))
			return nil
		}
	}
	writeSubcommandOwnerHelp(w, "config", configCmdHelp, configSubcommands)
	return nil
}

// configLayerFiles resolves the layer files a config subcommand acts on: the
// shared awa.toml and local .awa/config.toml from the resolved project, plus an
// explicit --config override. A config subcommand may target a standalone --config
// file with no surrounding project, so a missing project is tolerated only when
// --config is given; otherwise the project-resolution error surfaces.
func configLayerFiles(opts GlobalOptions) (configcmd.LayerFiles, error) {
	var files configcmd.LayerFiles
	override, ok, err := configFlagOverride(opts)
	if err != nil {
		return configcmd.LayerFiles{}, err
	}
	if ok {
		files.Override = override
	}
	proj, perr := resolveProject(opts)
	if perr != nil {
		if ok {
			return files, nil
		}
		return configcmd.LayerFiles{}, perr
	}
	layout, err := proj.Paths()
	if err != nil {
		return configcmd.LayerFiles{}, err
	}
	files.Shared = layout.SharedConfigFile()
	files.Local = layout.ConfigFile()
	return files, nil
}

// fileExists reports whether path names an existing file. An empty path or a
// stat error other than not-exist is reported as absent, since the config surface
// only needs the yes/no.
func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// configLayerDoc names one config layer and whether its file exists. It is the
// machine shape shared by `config path` and `config validate`.
type configLayerDoc struct {
	Layer  string `json:"layer"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// originTokens projects a resolved config's per-key origins into the stable
// wire-token map (dotted key -> layer token) the config.effective JSON exposes.
func originTokens(resolved domainconfig.ResolvedConfig) map[string]string {
	origins := resolved.Origins()
	out := make(map[string]string, len(origins))
	for key, layer := range origins {
		out[key] = layer.String()
	}
	return out
}

// presentLayerPaths returns the paths of the config layers that actually exist, in
// precedence order, for the status dashboard's config line — so a shared-only
// project shows the shared awa.toml that exists, never an absent local path.
func presentLayerPaths(layers []status.ConfigLayerFact) []string {
	var out []string
	for _, l := range layers {
		if l.Exists {
			out = append(out, l.Path)
		}
	}
	return out
}

// configShowDoc is the machine shape of `config show`: which layer was shown, its
// path, whether it exists, and its raw content (empty when absent).
type configShowDoc struct {
	Layer   string `json:"layer"`
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Content string `json:"content"`
}

// configLayerDocs lists the named config layers a file set carries, in precedence
// order (shared, local, config). With onlyExisting it drops layers whose file is
// absent, so `config validate` reports only the layers it actually checked while
// `config path` lists every configured layer with its presence.
func configLayerDocs(files configcmd.LayerFiles, onlyExisting bool) []configLayerDoc {
	var out []configLayerDoc
	add := func(layer, path string) {
		if path == "" {
			return
		}
		exists := fileExists(path)
		if onlyExisting && !exists {
			return
		}
		out = append(out, configLayerDoc{layer, path, exists})
	}
	add("shared", files.Shared)
	add("local", files.Local)
	add("config", files.Override)
	return out
}

func runConfigPath(w *output.Writer, inv invocation) error {
	opts, args := inv.options, inv.args[1:]
	if len(args) > 0 {
		return rejectExtra("config path", args[0])
	}
	files, err := configLayerFiles(opts)
	if err != nil {
		return err
	}
	layers := configLayerDocs(files, false)
	if opts.JSON {
		return w.JSON("config.path", struct {
			Layers []configLayerDoc `json:"layers"`
		}{layers})
	}
	for _, l := range layers {
		mark := "absent"
		if l.Exists {
			mark = "present"
		}
		w.Linef("%s %s (%s)", pad(l.Layer+":", 9), l.Path, mark)
	}
	return nil
}

func runConfigShow(w *output.Writer, inv invocation) error {
	opts, args := inv.options, inv.args[1:]
	files, err := configLayerFiles(opts)
	if err != nil {
		return err
	}
	// Select which layer to show: an explicit `shared`/`local` token, else the
	// --config override, else the single existing layer. When both shared and local
	// exist and no selector was given, ask the user to pick so the shown bytes are
	// never ambiguous.
	var layer, path string
	if len(args) > 1 {
		return rejectExtra("config show", args[1])
	}
	if len(args) == 1 {
		switch args[0] {
		case "shared":
			layer, path = "shared", files.Shared
		case "local":
			layer, path = "local", files.Local
		default:
			return usageErrorf("config show: unknown layer %q: want shared or local", args[0])
		}
		if path == "" {
			return usageErrorf("config show %s: no project to resolve the %s config path", args[0], args[0])
		}
	} else if files.Override != "" {
		layer, path = "config", files.Override
	} else {
		sharedExists := fileExists(files.Shared)
		localExists := fileExists(files.Local)
		switch {
		case sharedExists && localExists:
			return usageErrorf("both shared and local config exist; run 'awa config show shared' or 'awa config show local'")
		case sharedExists:
			layer, path = "shared", files.Shared
		case localExists:
			layer, path = "local", files.Local
		default:
			// No config anywhere: the schema is still discoverable, so point there
			// rather than implying it is unavailable.
			if opts.JSON {
				return w.JSON("config.show", configShowDoc{})
			}
			w.Line("# no shared awa.toml or local .awa/config.toml; using built-in defaults")
			w.Line("# see 'awa config template', 'awa config init', or 'awa config effective'")
			return nil
		}
	}

	data, err := configcmd.Show(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if opts.JSON {
				return w.JSON("config.show", configShowDoc{Layer: layer, Path: path})
			}
			w.Linef("# no %s config file at %s", layer, path)
			w.Line("# see 'awa config effective' for the composed configuration")
			return nil
		}
		return classifyConfigLoad(err)
	}
	if opts.JSON {
		return w.JSON("config.show", configShowDoc{Layer: layer, Path: path, Exists: true, Content: string(data)})
	}
	// Name the shown layer on stderr so stdout stays the file's exact bytes for
	// scripting; "show" must not rewrite trailing newlines.
	w.Diagnostic(fmt.Sprintf("note: showing %s config: %s", layer, path))
	w.Raw(data)
	return nil
}

func runConfigEffective(w *output.Writer, inv invocation) error {
	opts, args := inv.options, inv.args[1:]
	if len(args) > 0 {
		return rejectExtra("config effective", args[0])
	}
	files, err := configLayerFiles(opts)
	if err != nil {
		return err
	}
	loaded, err := configcmd.LoadLayered(files, trustModeOverride(opts))
	if err != nil {
		return classifyConfigLoad(err)
	}
	view := configcmd.NewView(loaded.Config())
	if opts.JSON {
		// The section views stay at the top level of data (embedded, not nested under
		// a "config" key) so the established config.effective contract is unchanged;
		// origins is added additively alongside them, as stable layer tokens.
		return w.JSON("config.effective", struct {
			configcmd.View
			Origins map[string]string `json:"origins"`
		}{view, originTokens(loaded)})
	}
	renderEffective(w, view, loaded)
	return nil
}

func runConfigValidate(w *output.Writer, inv invocation) error {
	opts, args := inv.options, inv.args[1:]
	if len(args) > 0 {
		return rejectExtra("config validate", args[0])
	}
	files, err := configLayerFiles(opts)
	if err != nil {
		return err
	}
	if _, err := configcmd.LoadLayered(files, trustModeOverride(opts)); err != nil {
		return classifyConfigLoad(err)
	}
	active := configLayerDocs(files, true)
	if opts.JSON {
		return w.JSON("config.validate", struct {
			Valid  bool             `json:"valid"`
			Layers []configLayerDoc `json:"layers"`
		}{true, active})
	}
	if len(active) == 0 {
		w.Line("no config files; built-in defaults are valid")
		return nil
	}
	for _, l := range active {
		w.Linef("valid: %s config %s", l.Layer, l.Path)
	}
	return nil
}

func runConfigTemplate(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	if len(args) > 0 {
		return rejectExtra("config template", args[0])
	}
	w.Raw([]byte(configref.Template()))
	return nil
}

// runConfigInit writes an annotated config scaffold. --shared writes the
// committable root awa.toml; --local writes the private .awa/config.toml. Exactly
// one target must be chosen. An existing file is never overwritten without --force.
func runConfigInit(w *output.Writer, inv invocation) error {
	opts, args := inv.options, inv.args[1:]
	var shared, local, force bool
	for _, a := range args {
		switch a {
		case "--shared":
			shared = true
		case "--local":
			local = true
		case "--force":
			force = true
		default:
			return usageErrorf("config init: unexpected argument %q; use --shared or --local (with optional --force)", a)
		}
	}
	if shared == local {
		return usageErrorf("config init: choose exactly one of --shared or --local")
	}
	proj, err := resolveProject(opts)
	if err != nil {
		return err
	}
	layout, err := proj.Paths()
	if err != nil {
		return err
	}

	layer := "local"
	path := layout.ConfigFile()
	if shared {
		layer, path = "shared", layout.SharedConfigFile()
	}
	// Publish atomically. Without --force, WriteNewFileAtomic makes "does not exist"
	// and "create" one exclusive step, so an existing config is never clobbered by a
	// check-then-write race; --force replaces in place.
	data := []byte(configref.Template())
	write := projfs.WriteNewFileAtomic
	if force {
		write = projfs.WriteFileAtomic
	}
	if err := write(path, data, paths.FilePerm); err != nil {
		if !force && errors.Is(err, os.ErrExist) {
			return genericErrorf("%s config already exists: %s\nedit it, or pass --force to overwrite", layer, path)
		}
		return genericErrorf("writing %s config: %v", layer, err)
	}

	if opts.JSON {
		return w.JSON("config.init", struct {
			Layer       string `json:"layer"`
			Path        string `json:"path"`
			Committable bool   `json:"committable"`
		}{layer, path, shared})
	}
	w.Linef("wrote %s config: %s", layer, path)
	if shared {
		w.Line("commit awa.toml to share this policy; verify with 'awa config effective'")
	} else {
		w.Line("this local override stays private (ignored with .awa/); verify with 'awa config effective'")
	}
	return nil
}

// trustModeOverride maps an explicit --trust-mode/--strict into a config
// override. When neither was given, the file's own value stands.
func trustModeOverride(opts GlobalOptions) configcmd.Overrides {
	if !opts.has(capTrustMode) {
		return configcmd.Overrides{}
	}
	var m domainconfig.TrustMode
	switch opts.TrustMode {
	case TrustStrict:
		m = domainconfig.TrustStrict
	case TrustFast:
		m = domainconfig.TrustFast
	default:
		m = domainconfig.TrustNormal
	}
	return configcmd.Overrides{TrustMode: &m}
}

// classifyConfigLoad maps a config load failure to the right exit code: a
// missing file is "not found" (3); an I/O failure reading it is a generic
// error rather than a content problem; anything else is a parse/validation
// failure, reported as a config error (4). An absent default config is not a
// failure (it resolves to the built-in defaults), so a not-found here means an
// explicitly named config file is missing.
func classifyConfigLoad(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return notFoundErrorf("config file not found: %v", err)
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return genericErrorf("reading config: %v", err)
	}
	return configErrorf("%v", err)
}

// rejectExtra builds a usage error for an unexpected trailing token, telling
// flags apart from positional arguments so the message points at the mistake.
func rejectExtra(cmd, tok string) error {
	if strings.HasPrefix(tok, "-") {
		return usageErrorf("unknown flag %q for %s", tok, cmd)
	}
	return usageErrorf("unexpected argument %q for %s", tok, cmd)
}

// renderEffective prints the effective config as section.key = value lines.
// renderEffective prints the composed configuration as TOML-like output, tagging
// each key a config layer (shared/local/--config/flag) overrode with a trailing
// "# <layer>" origin comment. A value carried by the product defaults is left
// unmarked to avoid a wall of "# default" noise; a legend states that. Derived
// effective_* lists are composed across layers and carry no single origin, so they
// stay unannotated.
func renderEffective(w *output.Writer, v configcmd.View, resolved domainconfig.ResolvedConfig) {
	kv := func(dotted, format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		if layer := resolved.OriginOf(dotted); layer != domainconfig.LayerDefault {
			s += "  # " + layer.String()
		}
		w.Line(s)
	}
	w.Line("# unmarked values are product defaults; # <layer> marks an override (shared/local/config/flag)")
	w.Line("[scope]")
	kv("scope.include", "include = %s", quoteList(v.Scope.Include))
	kv("scope.extra_excludes", "extra_excludes = %s", quoteList(v.Scope.ExtraExcludes))
	w.Linef("effective_excludes = %s", quoteList(v.Scope.EffectiveExcludes))
	kv("scope.use_gitignore", "use_gitignore = %t", v.Scope.UseGitignore)
	kv("scope.use_awaignore", "use_awaignore = %t", v.Scope.UseAwaignore)
	kv("scope.follow_symlinks", "follow_symlinks = %t", v.Scope.FollowSymlinks)
	kv("scope.symlink_max_depth", "symlink_max_depth = %d", v.Scope.SymlinkMaxDepth)
	kv("scope.allow_symlink_root_escape", "allow_symlink_root_escape = %t", v.Scope.AllowSymlinkRootEscape)
	w.Line("")
	w.Line("[history]")
	kv("history.extra_excludes", "extra_excludes = %s", quoteList(v.History.ExtraExcludes))
	w.Line("")
	w.Line("[hashing]")
	kv("hashing.trust_mode", "trust_mode = %q", v.Hashing.TrustMode)
	kv("hashing.max_file_size", "max_file_size = %q", v.Hashing.MaxFileSize)
	kv("hashing.large_file_policy", "large_file_policy = %q", v.Hashing.LargeFilePolicy)
	w.Line("")
	w.Line("[checkpoint]")
	kv("checkpoint.store_file_contents", "store_file_contents = %t", v.Checkpoint.StoreFileContents)
	kv("checkpoint.diff_context", "diff_context = %d", v.Checkpoint.DiffContext)
	kv("checkpoint.rename_detection", "rename_detection = %t", v.Checkpoint.RenameDetection)
	w.Line("")
	w.Line("[run]")
	kv("run.default_scope", "default_scope = %s", quoteList(v.Run.DefaultScope))
	kv("run.extra_excludes", "extra_excludes = %s", quoteList(v.Run.ExtraExcludes))
	w.Linef("effective_excludes = %s", quoteList(v.Run.EffectiveExcludes))
	kv("run.use_gitignore", "use_gitignore = %t", v.Run.UseGitignore)
	kv("run.use_awaignore", "use_awaignore = %t", v.Run.UseAwaignore)
	kv("run.env_allowlist", "env_allowlist = %s", quoteList(v.Run.EnvAllowlist))
	w.Linef("effective_env_allowlist = %s", quoteList(v.Run.EffectiveEnvAllowlist))
	// Injected facts are awa's own, not a config layer, so they carry no origin
	// annotation and are shown with their values — unlike the inherited names above,
	// whose ambient values are deliberately never projected.
	w.Linef("injected_env = %s", quoteList(v.Run.InjectedEnv))
	kv("run.ttl", "ttl = %q", v.Run.TTL)
	kv("run.max_stdout_size", "max_stdout_size = %q", v.Run.MaxStdoutSize)
	kv("run.max_stderr_size", "max_stderr_size = %q", v.Run.MaxStderrSize)
	kv("run.capture_output", "capture_output = %t", v.Run.CaptureOutput)
	kv("run.cache_failed_runs", "cache_failed_runs = %t", v.Run.CacheFailedRuns)
	kv("run.extra_effect_roots", "extra_effect_roots = %s", quoteList(v.Run.ExtraEffectRoots))
	w.Linef("effective_effect_roots = %s", quoteList(v.Run.EffectiveEffectRoots))
	w.Line("")
	w.Line("[gc]")
	kv("gc.keep_last_checkpoints", "keep_last_checkpoints = %d", v.GC.KeepLastCheckpoints)
	kv("gc.keep_runs_for", "keep_runs_for = %q", v.GC.KeepRunsFor)
	kv("gc.keep_restores_for", "keep_restores_for = %q", v.GC.KeepRestoresFor)
	w.Line("")
	w.Line("[diff]")
	kv("diff.algorithm", "algorithm = %q", v.Diff.Algorithm)
	w.Line("")
	w.Line("[locks]")
	kv("locks.timeout", "timeout = %q", v.Locks.Timeout)
	w.Line("")
	w.Line("[ui]")
	kv("ui.time", "time = %q", v.UI.Time)
}

func quoteList(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
