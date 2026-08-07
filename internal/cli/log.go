package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"awarer/internal/app/configcmd"
	"awarer/internal/app/logcmd"
	"awarer/internal/app/timeline"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/compare"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/restorestore"
	"awarer/internal/infra/runstore"
	"awarer/internal/output"
)

// runLog handles "awa log". By default it lists explicit checkpoints newest-first
// (--oneline, -1 detail, or --json). With --all it renders the full project
// timeline — checkpoints plus automatic recorded-run events and observations, each
// labeled so an automatic entry is never mistaken for a checkpoint. Timestamps
// follow the --time policy (or [ui].time); JSON always uses machine timestamps.
func runLog(w *output.Writer, inv invocation) error {
	var (
		limit    int
		limitSet bool
		all      bool
		oneline  bool
		latest   bool
		timeFlag string
	)
	args := inv.args
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
			limit, limitSet = n, true
		case "--all":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			all = true
		case "--oneline":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			oneline = true
		case "-1":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			latest = true
		case "--time":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			timeFlag = v
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for log", tok)
			}
			return usageErrorf("unexpected argument %q for log", tok)
		}
	}

	// The selection flags are mutually exclusive: each names a different "how many",
	// so combining them is ambiguous rather than additive. --all is the full
	// timeline (every event), distinct from the limited checkpoint list.
	if all && limitSet {
		return usageErrorf("--all cannot be combined with --limit/-n")
	}
	if latest && (all || limitSet) {
		return usageErrorf("-1 cannot be combined with --all or --limit/-n")
	}
	if latest && oneline {
		return usageErrorf("-1 cannot be combined with --oneline")
	}
	if all && oneline {
		return usageErrorf("--all cannot be combined with --oneline")
	}
	// --oneline shapes the human listing only; --json always returns the structured
	// log entries, so combining them would silently ignore --oneline. Reject it,
	// matching the changes/diff human-mode-plus-json contract.
	if inv.options.JSON && oneline {
		return jsonHumanModeError("--oneline", "returns the structured log entries")
	}
	// --time is a human display preference; JSON timestamps are always machine UTC.
	if inv.options.JSON && timeFlag != "" {
		return jsonTimeFlagError()
	}

	proj, err := resolveProject(inv.options)
	if err != nil {
		return err
	}
	layout, err := proj.Paths()
	if err != nil {
		return err
	}
	// log reads the project's config only for the human time-display default
	// ([ui].time); it layers shared awa.toml under local .awa/config.toml but does
	// not accept a --config override (see its capabilities).
	loaded, err := configcmd.LoadLayered(configcmd.LayerFiles{Shared: layout.SharedConfigFile(), Local: layout.ConfigFile()}, configcmd.Overrides{})
	if err != nil {
		return configErrorf("%v", err)
	}
	cfg := loaded.Config()
	td, err := timeDisplayFromFlag(timeFlag, loaded.Config())
	if err != nil {
		return usageErrorf("%v", err)
	}
	now := time.Now()

	if all {
		return runLogTimeline(w, inv, layout, cfg, td, now)
	}

	repo := checkpointjson.NewRepo(layout)
	svc := logcmd.New(repo)
	res, err := svc.Run(inv.invCtx(), logcmd.Request{Limit: limit, Latest: latest})
	if err != nil {
		// A cancelled header walk is an operator interrupt, not storage trouble: classify
		// it first so it reports the interrupt code rather than a generic read failure.
		if c := interruptError(err); c != nil {
			return c
		}
		if errors.Is(err, checkpoint.ErrCorruptStore) {
			return stateActionErrorf("reading checkpoints: %v", err)
		}
		return genericErrorf("reading checkpoints: %v", err)
	}

	if inv.options.JSON {
		return w.JSON("log", logView(res))
	}
	// Incompatible records are skipped, not fatal (a corrupt record would have failed
	// the command upstream). Warn so a short log is never mistaken for a complete one,
	// and point at recovery.
	if res.Skipped > 0 {
		w.Diagnostic(fmt.Sprintf("warning: %d checkpoint(s) skipped (incompatible schema); %s", res.Skipped, paths.ResetEvidenceHint()))
	}
	switch {
	case latest:
		renderLogDetail(w, res, td, now)
	case oneline:
		renderLogOneline(w, res, td, now)
	default:
		// Change counts since the previous explicit checkpoint, computed only for the
		// human view so the default JSON stays header-only and cheap. Consecutive shown
		// entries are consecutive checkpoints (the view is the newest contiguous run),
		// so a pair's diff is "since the previous checkpoint".
		counts := checkpointCounts(inv.invCtx(), repo, cfg, res.Entries)
		renderLogHuman(w, res, td, now, counts)
	}
	return nil
}

// checkpointCounts computes the change counts between each shown checkpoint and
// the next older one. The oldest shown entry has no in-view predecessor, so its
// slot is nil ("when available"). A diff error degrades that one slot to nil rather
// than failing the whole log.
func checkpointCounts(ctx context.Context, repo *checkpointjson.Repo, cfg domainconfig.Config, entries []checkpoint.CheckpointHeader) []*timeline.ChangeCounts {
	out := make([]*timeline.ChangeCounts, len(entries))
	c := &checkpointCounter{repo: repo, detectRenames: cfg.Checkpoint.RenameDetection}
	for i := 0; i+1 < len(entries); i++ {
		cc, err := c.Counts(ctx, entries[i+1].ID, entries[i].ID)
		if err != nil {
			continue
		}
		v := cc
		out[i] = &v
	}
	return out
}

// defaultTimelineJSONLimit caps how many timeline entries log --all --json emits. The
// cap is on the emitted output only: the timeline service still materializes the full
// interleaved history (an accepted exception — metadata-only, bounded by history count,
// with checkpoints deliberately never truncated), so the reported `total` stays
// the true count and the bounded fact honestly relates the
// shown prefix to it. Source-bounding the runs would drop older ones and make `total`
// lie, so it is deliberately avoided. The cap is generous — a consumer that hits it
// learns the real total from the document. Human log --all is unbounded (a person paging
// the full history); only the JSON dump is capped.
const defaultTimelineJSONLimit = 1000

// runLogTimeline renders "awa log --all": the full project timeline.
func runLogTimeline(w *output.Writer, inv invocation, layout paths.Layout, cfg domainconfig.Config, td domainconfig.TimeDisplay, now time.Time) error {
	hasher := blake3hash.New()
	repo := checkpointjson.NewRepo(layout)
	runs := runstore.New(layout, hasher)
	svc := timeline.New(repo, &runHistoryAdapter{repo: runs},
		restorestore.New(layout, hasher),
		&checkpointCounter{repo: repo, detectRenames: cfg.Checkpoint.RenameDetection},
		&gitHistoryAdapter{provider: gitmeta.New(layout.Root())})
	res, err := svc.Run(inv.invCtx(), timeline.Request{Limit: 0})
	if err != nil {
		if c := interruptError(err); c != nil {
			return c
		}
		if errors.Is(err, checkpoint.ErrCorruptStore) || errors.Is(err, runcache.ErrCorruptStore) || errors.Is(err, restore.ErrCorruptRecoveryStore) {
			return stateActionErrorf("reading timeline: %v", err)
		}
		return genericErrorf("reading timeline: %v", err)
	}
	if inv.options.JSON {
		return w.JSON("log", timelineView(res, defaultTimelineJSONLimit))
	}
	// Incompatible checkpoints are skipped, not fatal (a corrupt one failed the
	// command above). Warn so the full timeline is never mistaken for complete.
	if res.Skipped > 0 {
		w.Diagnostic(fmt.Sprintf("warning: %d checkpoint(s) skipped (incompatible schema); %s", res.Skipped, paths.ResetEvidenceHint()))
	}
	if res.SkippedRuns > 0 {
		w.Diagnostic(fmt.Sprintf("warning: %d run(s) skipped (incompatible schema); %s", res.SkippedRuns, paths.ResetEvidenceHint()))
	}
	// A genuine git failure left the timeline without commit boundaries the user expected
	// from --all. The timeline still renders; warn so the missing separators are explained
	// rather than read as "no commits happened".
	if res.GitBoundaryError != "" {
		w.Diagnostic("warning: git commit boundaries unavailable: " + res.GitBoundaryError)
	}
	renderTimelineHuman(w, res, td, now)
	return nil
}

// --- adapters ---

// runHistoryAdapter lists recorded runs for the timeline directly from the run
// store: newest-first by time-prefixed id, capped to a limit (0 = all). A corrupt
// entry is surfaced as a labeled record so it can still be seen and removed; an
// incompatible-schema entry is surfaced as a benign incompatible record the timeline
// counts and skips, rather than failing the whole log on a stale artifact.
type runHistoryAdapter struct {
	repo *runstore.Repo
}

func (a *runHistoryAdapter) History(ctx context.Context, limit int) ([]timeline.RunRecord, error) {
	ids, err := a.repo.ListRefs(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() > ids[j].String() })
	out := make([]timeline.RunRecord, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		entry, err := a.repo.Get(id)
		switch {
		case err == nil:
			out = append(out, timeline.RunRecord{ID: id, Entry: entry})
		case errors.Is(err, runcache.ErrNotFound):
			continue
		case errors.Is(err, runcache.ErrIncompatibleEntry):
			// A schema this build cannot read: never reusable. Count it as incompatible
			// (skipped by the timeline) rather than failing the whole log — the same
			// retained-and-skipped discipline incompatible checkpoints get.
			out = append(out, timeline.RunRecord{ID: id, Incompatible: true})
		case errors.Is(err, runcache.ErrCorruptStore):
			out = append(out, timeline.RunRecord{ID: id, Corrupt: true})
		default:
			return nil, err
		}
	}
	return out, nil
}

// checkpointCounter computes change counts between two checkpoints by comparing their
// stored manifests. It is the timeline's CheckpointDiff port.
type checkpointCounter struct {
	repo          *checkpointjson.Repo
	detectRenames bool
}

func (c *checkpointCounter) Counts(ctx context.Context, older, newer checkpoint.CheckpointID) (timeline.ChangeCounts, error) {
	ls, err := c.repo.OpenManifest(older)
	if err != nil {
		return timeline.ChangeCounts{}, err
	}
	lc, err := ls.Open(ctx)
	if err != nil {
		return timeline.ChangeCounts{}, err
	}
	rs, err := c.repo.OpenManifest(newer)
	if err != nil {
		_ = lc.Close()
		return timeline.ChangeCounts{}, err
	}
	rc, err := rs.Open(ctx)
	if err != nil {
		_ = lc.Close()
		return timeline.ChangeCounts{}, err
	}
	cs, err := compare.CompareToChangeSet(lc, rc, compare.Options{DetectRenames: c.detectRenames})
	if err != nil {
		return timeline.ChangeCounts{}, err
	}
	s := cs.Summary
	return timeline.ChangeCounts{Added: s.Added, Modified: s.Modified, Deleted: s.Deleted, Renamed: s.Renamed}, nil
}

// gitHistoryAdapter lists git commit boundaries for the timeline from the git
// metadata provider. It is the timeline's GitHistory port. A non-git project (or git
// unavailable) yields no boundaries and no error, so "awa log --all" degrades to a
// git-free timeline rather than failing.
type gitHistoryAdapter struct {
	provider *gitmeta.Provider
}

func (a *gitHistoryAdapter) CommitsBetween(ctx context.Context, since, until time.Time) ([]timeline.GitCommitBoundary, error) {
	commits, ok, err := a.provider.CommitsBetween(ctx, since, until, 0)
	if err != nil {
		// A genuine git failure in a git repo: surface the reason so the timeline can warn
		// the boundaries are missing, rather than hiding a real error behind no separators.
		return nil, err
	}
	if !ok {
		// A non-git project (or unborn branch): no boundaries, silently — that is normal.
		return nil, nil
	}
	out := make([]timeline.GitCommitBoundary, 0, len(commits))
	for _, c := range commits {
		out = append(out, timeline.GitCommitBoundary{
			Commit:      c.Commit,
			ShortCommit: c.ShortCommit,
			Subject:     c.Subject,
			Committed:   c.Committed,
		})
	}
	return out, nil
}

// --- default log JSON (unchanged shape) ---

type logEntryView struct {
	ID                   string                     `json:"id"`
	ShortID              string                     `json:"short_id"`
	CreatedAt            string                     `json:"created_at"`
	Message              string                     `json:"message,omitempty"`
	TreeHash             string                     `json:"tree_hash"`
	ScanConfigHash       string                     `json:"scan_config_hash"`
	CheckpointPolicyHash string                     `json:"checkpoint_policy_hash"`
	TrustMode            string                     `json:"trust_mode"`
	Stats                checkpoint.CheckpointStats `json:"stats"`
	Git                  *checkpointGit             `json:"git"`
}

type logViewDoc struct {
	Total       int            `json:"total"`
	Skipped     int            `json:"skipped,omitempty"`
	Checkpoints []logEntryView `json:"checkpoints"`
}

func logView(res logcmd.Result) logViewDoc {
	doc := logViewDoc{Total: res.Total, Skipped: res.Skipped, Checkpoints: make([]logEntryView, 0, len(res.Entries))}
	for _, s := range res.Entries {
		ev := logEntryView{
			ID:                   s.ID.String(),
			ShortID:              s.ID.Short(),
			CreatedAt:            s.CreatedAt.UTC().Format(time.RFC3339Nano),
			Message:              s.Message.String(),
			TreeHash:             s.TreeHash.String(),
			ScanConfigHash:       s.ScanConfigHash.String(),
			CheckpointPolicyHash: s.CheckpointPolicyHash.String(),
			TrustMode:            s.TrustMode.String(),
			Stats:                s.Stats,
		}
		if s.Git != nil {
			ev.Git = &checkpointGit{Branch: s.Git.Branch, ShortCommit: s.Git.ShortCommit, Clean: s.Git.Dirty.Clean}
		}
		doc.Checkpoints = append(doc.Checkpoints, ev)
	}
	return doc
}

// renderLogEmpty prints the empty-listing line. When records exist but all were
// skipped as incompatible (a corrupt record fails the command upstream, so it never
// reaches here), it must not claim "none yet" — the skipped warning already explained
// why; say so honestly instead.
func renderLogEmpty(w *output.Writer, res logcmd.Result) {
	if res.Skipped > 0 {
		w.Line("no readable checkpoints")
		return
	}
	w.Line("no checkpoints yet")
}

func renderLogHuman(w *output.Writer, res logcmd.Result, td domainconfig.TimeDisplay, now time.Time, counts []*timeline.ChangeCounts) {
	if len(res.Entries) == 0 {
		renderLogEmpty(w, res)
		return
	}
	for i, s := range res.Entries {
		header := s.ID.Short() + "  " + FormatTime(td, s.CreatedAt, now) + "  checkpoint"
		if i < len(counts) && counts[i] != nil {
			header += "  " + formatCounts(counts[i])
		}
		if s.Git != nil {
			header += "  " + formatGitSummary(s.Git)
		}
		w.Line(header)
		if !s.Message.IsZero() {
			w.Linef("    %s", s.Message.FirstLine())
		}
		w.Linef("    files: %d  blobs: %d  hash-only: %d  skipped: %d", s.Stats.Files, s.Stats.Blobs, s.Stats.HashOnly, s.Stats.Skipped)
	}
	if res.Total > len(res.Entries) {
		w.Linef("(showing %d of %d; use --all for the full timeline)", len(res.Entries), res.Total)
	}
}

func renderLogOneline(w *output.Writer, res logcmd.Result, td domainconfig.TimeDisplay, now time.Time) {
	if len(res.Entries) == 0 {
		renderLogEmpty(w, res)
		return
	}
	for _, s := range res.Entries {
		line := s.ID.Short() + " " + FormatTime(td, s.CreatedAt, now)
		if !s.Message.IsZero() {
			line += " " + s.Message.FirstLine()
		}
		w.Line(line)
	}
}

func renderLogDetail(w *output.Writer, res logcmd.Result, td domainconfig.TimeDisplay, now time.Time) {
	if len(res.Entries) == 0 {
		renderLogEmpty(w, res)
		return
	}
	s := res.Entries[0]
	w.Linef("checkpoint: %s", s.ID.String())
	w.Linef("created:    %s", FormatTime(td, s.CreatedAt, now))
	if !s.Message.IsZero() {
		w.Linef("message:    %s", s.Message.String())
	}
	w.Linef("trust mode: %s", s.TrustMode.String())
	w.Linef("tree hash:  %s", s.TreeHash.String())
	w.Linef("files:      %d (%d blobs, %d hash-only)", s.Stats.Files, s.Stats.Blobs, s.Stats.HashOnly)
	w.Linef("dirs:       %d", s.Stats.Dirs)
	w.Linef("symlinks:   %d", s.Stats.Symlinks)
	w.Linef("skipped:    %d", s.Stats.Skipped)
	if s.Git != nil {
		w.Linef("git:        %s", formatGitSummary(s.Git))
	}
}

// --- timeline (--all) JSON + human ---

type timelineEntryView struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	At   string `json:"at,omitempty"`
	// checkpoint
	CheckpointID string         `json:"checkpoint_id,omitempty"`
	Message      string         `json:"message,omitempty"`
	Added        *int           `json:"added,omitempty"`
	Modified     *int           `json:"modified,omitempty"`
	Deleted      *int           `json:"deleted,omitempty"`
	Renamed      *int           `json:"renamed,omitempty"`
	Git          *checkpointGit `json:"git,omitempty"`
	// run / observation
	RunID      string   `json:"run_id,omitempty"`
	Command    []string `json:"command,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	DurationMs *int64   `json:"duration_ms,omitempty"`
	// Reuse, ReuseReason, and Mutation name the run's reuse classification and the
	// mutation guard's outcome with the same field names the run/log/show JSON uses, so
	// the concepts read identically across every run-bearing document.
	Reuse       string `json:"reuse,omitempty"`
	ReuseReason string `json:"reuse_reason,omitempty"`
	Mutation    string `json:"mutation,omitempty"`
	Corrupt     bool   `json:"corrupt,omitempty"`
	// git commit boundary (kind == "git_commit"). A context marker, not an awa state
	// reference: it carries no "ref" field precisely because it is not addressable.
	GitCommit string `json:"git_commit,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Subject   string `json:"subject,omitempty"`
	// restore recovery observation (kind == "restore_before"). It is addressable, so
	// it carries a "ref"; restore_source names the state that restore was proven from
	// and restore_selection what it covered. There is deliberately no "available
	// until" field: retention has an explicit override, so a date would be a promise
	// awa cannot keep — the [gc].keep_restores_for policy is the honest answer.
	RestoreID        string `json:"restore_id,omitempty"`
	RestoreSource    string `json:"restore_source,omitempty"`
	RestoreSelection string `json:"restore_selection,omitempty"`
}

type timelineDoc struct {
	Total int `json:"total"`
	// Bounded reports whether entries is the whole timeline or an intentionally capped
	// prefix. log --all can span the entire project history, so its JSON is a bounded
	// report — not an unbounded dump — with total naming the full count and bounded.limit
	// the cap, so a consumer knows an unshown remainder exists and its size. There is no
	// flag to widen it: --all is the whole-timeline mode and rejects --limit (a smaller
	// view is `log -n N` without --all); the cap only bounds the machine dump.
	Bounded boundedOutputView `json:"bounded"`
	// Skipped counts incompatible checkpoints; SkippedRuns counts incompatible run
	// entries — both left out of the timeline as records this build cannot read.
	Skipped     int                 `json:"skipped,omitempty"`
	SkippedRuns int                 `json:"skipped_runs,omitempty"`
	Entries     []timelineEntryView `json:"entries"`
	// GitBoundaryError names a genuine git failure that left the timeline without commit
	// boundaries, so a machine consumer distinguishes "no commits in the window" from
	// "git could not be read" via a stable reason token (with human detail alongside),
	// never a raw err.Error() string. Omitted when boundaries were listed (or the project
	// is non-git).
	GitBoundaryError *gitBoundaryErrorView `json:"git_boundary_error,omitempty"`
}

// gitBoundaryErrorView is the structured form of a timeline git-history failure: a
// stable reason token an agent keys off, plus the human-readable detail.
type gitBoundaryErrorView struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// timelineView projects a timeline result into its JSON document, capping the emitted
// entries at limit (a non-positive limit means no cap) so log --all --json stays a
// bounded report. The bounded fact carries the cap and the full total, so a consumer can
// tell a complete timeline from a capped prefix and knows how much was withheld.
func timelineView(res timeline.Result, limit int) timelineDoc {
	entries := res.Entries
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	doc := timelineDoc{
		Total:       res.Total,
		Bounded:     boundedOutputOf(len(entries), res.Total, limit),
		Skipped:     res.Skipped,
		SkippedRuns: res.SkippedRuns,
		Entries:     make([]timelineEntryView, 0, len(entries)),
	}
	if res.GitBoundaryError != "" {
		doc.GitBoundaryError = &gitBoundaryErrorView{Reason: reasonGitCommandFailed, Detail: res.GitBoundaryError}
	}
	for _, e := range entries {
		ev := timelineEntryView{Kind: e.Kind.Machine(), Ref: e.Ref}
		if !e.At.IsZero() {
			ev.At = e.At.UTC().Format(time.RFC3339Nano)
		}
		if e.Checkpoint != nil {
			s := e.Checkpoint
			ev.CheckpointID = s.ID.String()
			ev.Message = s.Message.String()
			if s.Git != nil {
				ev.Git = &checkpointGit{Branch: s.Git.Branch, ShortCommit: s.Git.ShortCommit, Clean: s.Git.Dirty.Clean}
			}
			if e.Counts != nil {
				ev.Added = intPtr(e.Counts.Added)
				ev.Modified = intPtr(e.Counts.Modified)
				ev.Deleted = intPtr(e.Counts.Deleted)
				ev.Renamed = intPtr(e.Counts.Renamed)
			}
		}
		if e.Run != nil {
			r := e.Run
			ev.RunID = r.ID.String()
			ev.Corrupt = r.Corrupt
			if !r.Corrupt {
				ev.Command = r.Command
				code := r.Exit.Code
				ev.ExitCode = &code
				dur := r.DurationMs
				ev.DurationMs = &dur
				ev.Reuse = r.Reuse.String()
				ev.ReuseReason = r.Reuse.Reason().String()
				ev.Mutation = r.Mutation.Outcome().String()
			}
		}
		if e.GitCommit != nil {
			g := e.GitCommit
			ev.GitCommit = g.ShortCommit
			ev.Commit = g.Commit
			ev.Subject = g.Subject
		}
		if e.Restore != nil {
			r := e.Restore
			ev.RestoreID = r.ID.String()
			ev.Corrupt = r.Corrupt
			if !r.Corrupt {
				ev.RestoreSource = r.SourceRef
				ev.RestoreSelection = r.Selection
			}
		}
		doc.Entries = append(doc.Entries, ev)
	}
	return doc
}

func intPtr(n int) *int { return &n }

func renderTimelineHuman(w *output.Writer, res timeline.Result, td domainconfig.TimeDisplay, now time.Time) {
	if len(res.Entries) == 0 {
		w.Line("no timeline events yet")
		return
	}
	for _, e := range res.Entries {
		at := ""
		if !e.At.IsZero() {
			at = FormatTime(td, e.At, now)
		}
		switch e.Kind {
		case timeline.EntryCheckpoint:
			s := e.Checkpoint
			line := e.Ref + "  " + at + "  " + e.Kind.Label()
			if !s.Message.IsZero() {
				line += "  \"" + s.Message.FirstLine() + "\""
			}
			if e.Counts != nil {
				line += "  " + formatCounts(e.Counts)
			}
			if s.Git != nil {
				line += "  " + formatGitSummary(s.Git)
			}
			w.Line(line)
		case timeline.EntryRestoreBefore:
			// A system-owned record, not a checkpoint: it leads with its own addressable
			// restore:<id>:before reference and says what it was proven from, so a reader
			// can tell at a glance that it is undo evidence rather than a review landmark.
			r := e.Restore
			line := e.Ref + "  " + at + "  " + e.Kind.Label()
			if r.Corrupt {
				w.Line(line + "  (corrupt)")
				continue
			}
			if r.SourceSummaryLine() != "" {
				line += "  " + r.SourceSummaryLine()
			}
			w.Line(line)
		case timeline.EntryGitCommit:
			// A context marker, framed differently from an addressable awa row (which leads
			// with @-N / run:…) to reinforce that a git commit is not an awa state reference.
			g := e.GitCommit
			line := "-- git commit " + g.ShortCommit
			if g.Subject != "" {
				line += " \"" + g.Subject + "\""
			}
			if at != "" {
				line += " " + at
			}
			w.Line(line + " --")
		default:
			r := e.Run
			line := e.Ref + "  " + at + "  " + e.Kind.Label()
			if r.Corrupt {
				w.Line(line + "  (corrupt)")
				continue
			}
			if e.Kind == timeline.EntryRun {
				line += "  " + strings.Join(r.Command, " ")
				line += "  " + timelineExit(r.Exit)
				line += "  " + formatDurationMs(r.DurationMs)
				line += "  " + r.Reuse.String()
				if reason := r.Reuse.Reason().String(); reason != "" {
					line += ": " + reason
				}
				switch r.Mutation.Outcome() {
				case runcache.MutationChanged:
					line += "  [mutated]"
				case runcache.MutationScanFailed:
					line += "  [post-scan-failed]"
				}
			}
			w.Line(line)
		}
	}
}

// formatCounts renders the +A ~M -D R change-count cluster for a checkpoint.
func formatCounts(c *timeline.ChangeCounts) string {
	s := strings.Builder{}
	s.WriteString("+")
	s.WriteString(strconv.Itoa(c.Added))
	s.WriteString(" ~")
	s.WriteString(strconv.Itoa(c.Modified))
	s.WriteString(" -")
	s.WriteString(strconv.Itoa(c.Deleted))
	if c.Renamed > 0 {
		s.WriteString(" R")
		s.WriteString(strconv.Itoa(c.Renamed))
	}
	return s.String()
}

// timelineExit renders a run's exit for the compact timeline row's key=value
// grammar ("exit=<code>"). A terminating signal uses the one signal spelling shared
// across every surface, "signal:<NAME>", so the token reads the same in a row and in
// an aligned block.
func timelineExit(e runcache.ExitStatus) string {
	if e.Kind == runcache.ExitSignaled {
		return "exit=signal:" + e.Signal
	}
	return "exit=" + strconv.Itoa(e.Code)
}

// formatDurationMs renders a millisecond duration in the one canonical form
// (durationLabel's "%.1fs"), so the timeline reads like every other run surface.
func formatDurationMs(ms int64) string {
	return durationLabel(time.Duration(ms) * time.Millisecond)
}
