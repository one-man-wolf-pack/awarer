// Package timeline aggregates a project's explicit checkpoints and automatic
// recorded-run events into one newest-first, typed event stream for "awa log
// --all". It is the only place that merges the two sources; rendering consumes the
// typed entries and never infers an entry's kind from an id-string prefix.
//
// Default "awa log" stays explicit-checkpoint-only and does not use this service;
// the timeline is the opt-in full view, and every non-checkpoint entry carries a
// visible kind so a user never mistakes an automatic observation for a checkpoint
// they created.
package timeline

import (
	"context"
	"fmt"
	"sort"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
)

// EntryKind is the closed classification of a timeline entry. Rendering switches on
// it; it is never derived from an id string.
type EntryKind int

const (
	// EntryCheckpoint is an explicit user checkpoint (awa checkpoint).
	EntryCheckpoint EntryKind = iota
	// EntryRun is a recorded command execution (awa run).
	EntryRun
	// EntryRunBefore is a recorded run's pre-execution observed state.
	EntryRunBefore
	// EntryRunAfter is a recorded run's post-execution observed state.
	EntryRunAfter
	// EntryGitCommit is a git commit that occurred between awa records. It is a context
	// marker, NOT an awa state reference: it is not addressable by a range and carries
	// an empty Ref.
	EntryGitCommit
	// EntryRestoreBefore is an applied restore's pre-restore recovery observation.
	// It is system-owned evidence, not a user checkpoint: it appears here (the full
	// timeline) and nowhere in the default "awa log", it never moves latest, and its
	// Ref is the restore:<id>:before reference rather than a checkpoint position.
	EntryRestoreBefore
)

// Machine returns the stable JSON enum token for the kind.
func (k EntryKind) Machine() string {
	switch k {
	case EntryCheckpoint:
		return "checkpoint"
	case EntryRun:
		return "run"
	case EntryRunBefore:
		return "run_before"
	case EntryRunAfter:
		return "run_after"
	case EntryGitCommit:
		return "git_commit"
	case EntryRestoreBefore:
		return "restore_before"
	default:
		return "unknown"
	}
}

// Label returns the short human tag for the kind.
func (k EntryKind) Label() string {
	switch k {
	case EntryCheckpoint:
		return "checkpoint"
	case EntryRun:
		return "run"
	case EntryRunBefore:
		return "run:before"
	case EntryRunAfter:
		return "run:after"
	case EntryGitCommit:
		return "git commit"
	case EntryRestoreBefore:
		return "restore:before"
	default:
		return "?"
	}
}

// ChangeCounts is the added/modified/deleted/renamed delta of a checkpoint since
// the previous explicit checkpoint.
type ChangeCounts struct {
	Added    int
	Modified int
	Deleted  int
	Renamed  int
}

// RunSummary is the run-specific payload of a run/observation timeline entry. Which
// observations a run has is conveyed by the separate run-before/run-after entries
// the timeline emits, so the summary itself carries no before/after presence flags.
type RunSummary struct {
	ID         runcache.RunID
	Command    []string
	Exit       runcache.ExitStatus
	DurationMs int64
	Reuse      runcache.ReuseState
	Mutation   runcache.MutationStatus
	Corrupt    bool
}

// RestoreSummary is the restore-specific payload of a recovery-observation entry:
// what the restore was proven from, what it covered, and whether the record is
// readable. It deliberately carries no "undo available until" field — retention has
// an explicit override, so a date here would be a promise awa cannot keep.
type RestoreSummary struct {
	ID        restore.OperationID
	SourceRef string
	Selection string
	Corrupt   bool
}

// SourceSummaryLine renders "from <source-ref> covering <selection>" for the human
// timeline, or the empty string when the record could not be read. It lives here
// rather than in the renderer so the full timeline and any future restore
// inspection surface describe a recovery observation the same way.
func (s RestoreSummary) SourceSummaryLine() string {
	if s.Corrupt || s.SourceRef == "" {
		return ""
	}
	if s.Selection == "" {
		return "from " + s.SourceRef
	}
	return "from " + s.SourceRef + " covering " + s.Selection
}

// GitCommitBoundary is the identity and time of one git commit shown as a timeline
// context marker. It is not an awa record and has no awa reference.
type GitCommitBoundary struct {
	Commit      string // full hash
	ShortCommit string
	Subject     string
	Committed   time.Time
}

// TimelineEntry is one event. Exactly one of Checkpoint, Run, or GitCommit is set,
// per Kind.
type TimelineEntry struct {
	Kind  EntryKind
	At    time.Time
	Ref   string
	Order int // tie-break ordinal within an identical timestamp; not displayed

	// checkpoint entries
	Checkpoint *checkpoint.CheckpointHeader
	Counts     *ChangeCounts

	// run / observation entries
	Run *RunSummary

	// restore recovery-observation entries (Kind == EntryRestoreBefore)
	Restore *RestoreSummary

	// git-commit boundary entries (Kind == EntryGitCommit). A context marker, not an
	// awa state reference: Ref is empty and the entry is not addressable by a range.
	GitCommit *GitCommitBoundary
}

// Result is the merged stream plus the total number of events available, and the
// degradation counts a renderer surfaces so a partial timeline is never mistaken for a
// complete one.
type Result struct {
	Entries []TimelineEntry
	Total   int
	// Skipped counts incompatible checkpoints left out of the timeline; SkippedRuns
	// counts incompatible run entries left out. Both are records this build cannot
	// read, kept as separate counts so a renderer can name which subsystem degraded.
	Skipped     int
	SkippedRuns int
	// GitBoundaryError, when non-empty, is the reason git commit boundaries could not be
	// listed in a project that IS a git repo (a genuine git failure, not a non-git
	// project, which yields no boundaries silently). The timeline still renders; the
	// renderer warns rather than hiding a real git error behind missing separators.
	GitBoundaryError string
}

// Checkpoints reports the checkpoint store's read health so the timeline degrades on
// the same incompatible/corrupt policy as "awa log": incompatible records are skipped and
// counted, a corrupt record fails the whole command.
//
// It names the full-history operation deliberately. The timeline is the explicit
// complete view — every checkpoint carries a "@-N" position and a change count against
// its predecessor — so it is the one checkpoint reader that retains every header.
type Checkpoints interface {
	StoreHealthAll(ctx context.Context) (checkpoint.CheckpointStoreHealth, error)
}

// RunRecord is one recorded run for the timeline: a healthy entry, a corrupt id, or an
// incompatible-schema entry. A corrupt run is durable damage shown as a labeled entry
// so it can be cleaned up; an incompatible run is a record this build cannot read,
// counted and skipped (never shown), the same incompatible-versus-corrupt discipline
// the checkpoint side uses — so an unreadable local artifact degrades the timeline
// explainably rather than failing it.
type RunRecord struct {
	ID           runcache.RunID
	Entry        runcache.RunEntry
	Corrupt      bool
	Incompatible bool
}

// RunHistory lists recorded runs newest-first — the broad record set (reusable,
// record-only, mutating, failed), not the cache-lookup view.
type RunHistory interface {
	History(ctx context.Context, limit int) ([]RunRecord, error)
}

// CheckpointDiff computes the change counts between two checkpoints. It is
// optional: a nil one yields no counts rather than failing the timeline.
type CheckpointDiff interface {
	Counts(ctx context.Context, older, newer checkpoint.CheckpointID) (ChangeCounts, error)
}

// RestoreHistory lists an applied restore's recovery observations. It is optional:
// without it the timeline simply holds no restore entries, the same degradation
// discipline the run and git ports follow. A record that cannot be read is reported
// as a corrupt entry rather than omitted, so damage stays visible.
type RestoreHistory interface {
	List(ctx context.Context) ([]restore.RecoveryFinding, error)
}

// GitHistory lists git commits whose committer time falls within [since, until]. It is
// optional: a nil port yields a timeline with no git boundaries (a non-git project, or
// git unavailable) rather than failing the timeline — the same degradation discipline
// CheckpointDiff follows. A returned error degrades to no boundaries plus the raw
// error for the caller to surface, never a failed timeline.
type GitHistory interface {
	CommitsBetween(ctx context.Context, since, until time.Time) ([]GitCommitBoundary, error)
}

// Service builds timelines from the checkpoint and run-record sources.
type Service struct {
	checkpoints Checkpoints
	runs        RunHistory
	restores    RestoreHistory
	diff        CheckpointDiff
	git         GitHistory
}

// New builds a timeline service. runs, restores, diff, and git are all optional:
// without runs the timeline holds no run observations; without restores it holds no
// recovery observations; without diff, checkpoints carry no counts; without git, the
// timeline shows no commit boundaries.
func New(checkpoints Checkpoints, runs RunHistory, restores RestoreHistory, diff CheckpointDiff, git GitHistory) *Service {
	return &Service{checkpoints: checkpoints, runs: runs, restores: restores, diff: diff, git: git}
}

// Request bounds the timeline. Limit caps the number of recorded runs pulled (each
// may contribute up to three entries: the run plus its before/after observations);
// checkpoints are always included so the explicit history is never truncated by
// run volume.
type Request struct {
	Limit int
}

// Run builds the merged, newest-first timeline. The checkpoint source degrades on the
// store policy: an incompatible checkpoint is skipped and counted (Result.Skipped)
// rather than collapsing the whole timeline, while a corrupt checkpoint is durable
// damage and fails loud with ErrCorruptStore — the same incompatible/corrupt line "awa
// log" draws, so the full view and the default view stay consistent.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	health, err := s.checkpoints.StoreHealthAll(ctx)
	if err != nil {
		return Result{}, err
	}
	if n := health.Corrupt(); n > 0 {
		return Result{}, fmt.Errorf("%w: %d checkpoint(s) have corrupt metadata", checkpoint.ErrCorruptStore, n)
	}
	headers := health.NewestHeaders()

	var entries []TimelineEntry
	// Checkpoints, newest-first. @-N indexing matches awa log / state references.
	for i, h := range headers {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		header := h
		fe := TimelineEntry{
			Kind:       EntryCheckpoint,
			At:         h.CreatedAt,
			Ref:        fmt.Sprintf("@-%d", i+1),
			Order:      0,
			Checkpoint: &header,
		}
		// Change counts since the previous (older) explicit checkpoint, where one
		// exists and a diff port is wired. The oldest checkpoint has no predecessor.
		if s.diff != nil && i+1 < len(headers) {
			c, derr := s.diff.Counts(ctx, headers[i+1].ID, h.ID)
			if derr != nil {
				return Result{}, derr
			}
			cc := c
			fe.Counts = &cc
		}
		entries = append(entries, fe)
	}

	// Recorded runs and their observations. An incompatible-schema run is counted
	// and skipped rather than shown or failed — the same retained-and-skipped discipline the
	// checkpoint side applies — so a stale run artifact never collapses the whole timeline.
	skippedRuns := 0
	if s.runs != nil {
		records, err := s.runs.History(ctx, req.Limit)
		if err != nil {
			return Result{}, err
		}
		for _, rec := range records {
			if rec.Incompatible {
				skippedRuns++
				continue
			}
			entries = append(entries, runEntries(rec)...)
		}
	}

	// Applied restores' recovery observations. They are system-owned evidence, so
	// they appear only here — never in the default checkpoint log, and never as a
	// checkpoint-relative position.
	if s.restores != nil {
		findings, err := s.restores.List(ctx)
		if err != nil {
			return Result{}, err
		}
		for _, f := range findings {
			entries = append(entries, restoreEntry(f))
		}
	}

	// Git commit boundaries between the awa records, as context markers. Bounded to the
	// span of the awa entries so the git query never pulls the project's whole history,
	// and skipped when there are no awa entries (nothing to interleave). Git history is
	// best-effort context: a query error degrades to no boundaries rather than failing
	// the timeline, mirroring the optional CheckpointDiff port — but a genuine failure is
	// reported (not silently hidden) so the renderer can warn the boundaries are missing.
	var gitBoundaryError string
	if s.git != nil {
		if since, until, ok := timeSpan(entries); ok {
			commits, err := s.git.CommitsBetween(ctx, since, until)
			if err != nil {
				gitBoundaryError = err.Error()
			}
			for _, c := range commits {
				boundary := c
				entries = append(entries, TimelineEntry{
					Kind:      EntryGitCommit,
					At:        c.Committed,
					Ref:       "", // a context marker, never an awa state reference
					Order:     4,  // after a same-timestamp checkpoint (0) and run/observations (1-3)
					GitCommit: &boundary,
				})
			}
		}
	}

	sortTimeline(entries)
	return Result{
		Entries:          entries,
		Total:            len(entries),
		Skipped:          health.Incompatible(),
		SkippedRuns:      skippedRuns,
		GitBoundaryError: gitBoundaryError,
	}, nil
}

// restoreEntry projects one recovery-observation finding into a timeline entry. An
// unreadable record still becomes an entry — labelled corrupt — because a record
// that exists but cannot be read is exactly what a reader needs to see.
func restoreEntry(f restore.RecoveryFinding) TimelineEntry {
	sum := RestoreSummary{ID: f.ID, Corrupt: !f.Readable()}
	if f.Readable() {
		sum.SourceRef = f.Record.Source().CanonicalRef()
		sum.Selection = f.Record.Selection().String()
		return TimelineEntry{
			Kind: EntryRestoreBefore, Order: 5, Restore: &sum,
			At: f.Record.CreatedAt(), Ref: f.Record.BeforeRef(),
		}
	}
	// Unreadable: the record's own reference is unavailable, but the directory name
	// already parsed as an id often enough to name what needs attention.
	e := TimelineEntry{Kind: EntryRestoreBefore, Order: 5, Restore: &sum}
	if !f.ID.IsZero() {
		e.Ref = f.ID.BeforeRef()
	}
	return e
}

// timeSpan returns the earliest and latest non-zero At across the entries, and whether
// any timed entry exists. It bounds the git-history query to the awa records already
// gathered so a git commit outside the review window is never pulled in.
//
// This is an intentional product decision: "awa log --all" shows the awa evidence
// timeline with commit separators BETWEEN awa records, not trailing git history. A
// commit newer than the most recent awa record (or older than the first) is current
// git context, not a separator, and belongs to git tooling / a future status surface —
// not to this view. Consequently a timeline with a single awa record shows no
// boundaries (a zero-width window), which is correct: there is nothing to separate.
func timeSpan(entries []TimelineEntry) (since, until time.Time, ok bool) {
	for _, e := range entries {
		if e.At.IsZero() {
			continue
		}
		if !ok {
			since, until, ok = e.At, e.At, true
			continue
		}
		if e.At.Before(since) {
			since = e.At
		}
		if e.At.After(until) {
			until = e.At
		}
	}
	return since, until, ok
}

// runEntries expands one recorded run into its run entry and the before/after
// observation entries it actually has. A corrupt run contributes only a labeled run
// entry so it can still be seen and cleaned up.
func runEntries(rec RunRecord) []TimelineEntry {
	if rec.Corrupt {
		return []TimelineEntry{{
			Kind:  EntryRun,
			Ref:   "run:" + rec.ID.Short(),
			Order: 1,
			Run:   &RunSummary{ID: rec.ID, Corrupt: true},
		}}
	}
	e := rec.Entry
	sum := &RunSummary{
		ID:         rec.ID,
		Command:    e.KeyInput.Command.Argv,
		Exit:       e.Exit,
		DurationMs: e.DurationMs(),
		Reuse:      e.Reuse,
		Mutation:   e.Mutation,
	}
	out := []TimelineEntry{{
		Kind:  EntryRun,
		At:    e.StartedAt,
		Ref:   "run:" + rec.ID.Short(),
		Order: 1,
		Run:   sum,
	}}
	// Every recorded run observed its pre-run state, so a before-event is not
	// conditional; only the after can be absent, and only when the post-run scan failed.
	out = append(out, TimelineEntry{
		Kind:  EntryRunBefore,
		At:    e.StartedAt,
		Ref:   "run:" + rec.ID.Short() + ":before",
		Order: 2,
		Run:   sum,
	})
	if e.After != nil {
		out = append(out, TimelineEntry{
			Kind:  EntryRunAfter,
			At:    e.FinishedAt,
			Ref:   "run:" + rec.ID.Short() + ":after",
			Order: 3,
			Run:   sum,
		})
	}
	return out
}

// sortTimeline orders entries newest-first, breaking ties deterministically so a
// run and its observations cluster stably: by timestamp descending, then by the
// per-run Order, then by ref.
func sortTimeline(entries []TimelineEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.Ref > b.Ref
	})
}
