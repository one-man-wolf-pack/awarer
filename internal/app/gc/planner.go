package gc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	domconfig "awarer/internal/domain/config"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	restoredom "awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/lockfile"
)

// planner carries the per-plan state of one mark/sweep pass. It accumulates typed
// candidates and the safety flags that decide whether the blob sweep may run.
type planner struct {
	svc    *Service
	ctx    context.Context
	req    gcdom.GCRequest
	cfg    domconfig.Config
	now    time.Time
	repos  repos
	layout paths.Layout

	cands    []gcdom.GCCandidate
	warnings []string

	// Safety flags. Any of these means the reachability set is incomplete or
	// deletion is unsafe, so the blob sweep must be suppressed rather than risk a
	// false "unreachable".
	checkpointReadFailed bool
	runReadFailed        bool
	// unreadableCheckpoints holds the ids whose header already failed to read and was
	// already reported. The blob-reachability pass walks the same retained ids and
	// would hit the identical failure a second time, so without this one record would
	// appear twice in the plan and be counted twice as blocked.
	unreadableCheckpoints map[string]bool
	// restoreReadFailed marks a recovery observation GC could not read. Unlike a
	// run, a recovery observation references blobs, so an unreadable one leaves the
	// reachability set incomplete and the blob sweep must fail closed.
	restoreReadFailed bool
	locksBlock        bool
	// committedUnavailable means --committed was requested but git could not anchor a
	// cutoff. The policy then deletes nothing at all (not blobs, not temp), so this
	// suppresses the sweep and temp cleanup the same way the safety flags do.
	committedUnavailable bool

	// blobFootprint is the total blob-store footprint measured while planning the blob
	// sweep, from the complete listing rather than a sample. blobFootprintDone records
	// whether it was measured at all, so run() attaches a footprint only when one exists
	// (mapping to BlobFootprint()'s optional result) rather than a bogus zero. Because a
	// listing failure aborts planning, a completed plan always carries a footprint.
	blobCount         int
	blobBytes         int64
	blobFootprintDone bool
}

// run executes the mark/sweep stages in order and returns the immutable plan.
func (p *planner) run() (gcdom.GCPlan, error) {
	if err := p.inspectLocks(); err != nil {
		return gcdom.GCPlan{}, err
	}
	commitCutoff, committedOK, gitErr := p.committedCutoff()
	if p.req.Committed() && !committedOK {
		// --committed but git could not anchor a cutoff (absent git, not a worktree, no
		// commits yet, or a genuine failure). The contract is a no-delete plan
		// with a clear reason: retain everything and suppress the sweep and temp cleanup
		// rather than guessing. It is not a hard error.
		p.committedUnavailable = true
		msg := "--committed: git metadata unavailable; no deletions planned"
		if gitErr != nil {
			msg = "--committed: git unavailable (" + gitErr.Error() + "); no deletions planned"
		}
		p.warnings = append(p.warnings, msg)
	}

	retainedSnaps, err := p.planCheckpoints(commitCutoff, committedOK)
	if err != nil {
		return gcdom.GCPlan{}, err
	}
	if err := p.planRuns(commitCutoff, committedOK); err != nil {
		return gcdom.GCPlan{}, err
	}
	retainedRestores, err := p.planRestores()
	if err != nil {
		return gcdom.GCPlan{}, err
	}
	if err := p.planBlobs(retainedSnaps, retainedRestores); err != nil {
		return gcdom.GCPlan{}, err
	}
	if err := p.planTemp(); err != nil {
		return gcdom.GCPlan{}, err
	}
	plan := gcdom.NewPlan(p.req.DryRun(), p.cands, p.warnings)
	if p.blobFootprintDone {
		plan = plan.WithBlobFootprint(gcdom.BlobFootprint{
			Count:    p.blobCount,
			Bytes:    p.blobBytes,
			Complete: true,
		})
	}
	return plan, nil
}

// add appends a validated candidate or panics: every argument is planner-controlled,
// so a construction failure is a programmer error that must surface loudly rather
// than silently drop a decision from the plan.
func (p *planner) add(kind gcdom.CandidateKind, reason gcdom.RetentionReason, id, path, message string, bytes int64) {
	c, err := gcdom.NewCandidate(kind, reason, id, path, message, bytes)
	if err != nil {
		panic("gc: invalid candidate construction: " + err.Error())
	}
	p.cands = append(p.cands, c)
}

// inspectLocks scans .awa/locks and blocks destructive GC on any active or unknown
// lock. Stale recognized locks are left to doctor — GC neither blocks on nor repairs
// them. An unreadable locks directory (a scan error) is itself a block: GC fails
// closed with an unknown-lock candidate rather than deleting state it cannot prove is
// unlocked.
func (p *planner) inspectLocks() error {
	entries, err := lockfile.ScanDir(p.layout.Root(), p.layout.LocksDir(), p.now, p.svc.staleAfter, p.svc.hostname, p.svc.processAlive)
	if err != nil {
		// The locks directory could not be read (symlinked, unreadable, or not a
		// directory). GC cannot prove no writer holds a lock, so it must fail closed:
		// block destructive deletion rather than risk corrupting a concurrent writer's
		// state. An absent directory is not an error (ScanDir maps it to no entries).
		p.locksBlock = true
		p.add(gcdom.KindLock, gcdom.ReasonLockUnknown, "", p.layout.LocksDir(),
			"locks directory is unreadable, blocking destructive gc: "+err.Error(), 0)
		return nil
	}
	for _, e := range entries {
		if lockfile.IsExclusiveLockName(e.Name) {
			// An exclusive lock file is not a writer-presence lock. gc-vs-gc is serialized
			// by AcquireExclusive and its timeout (the lock-timeout exit), not by blocking
			// the plan; treating gc.lock as a writer block would turn a second gc into a
			// clean "blocked" exit instead of the intended timeout. restore.lock is
			// skipped for the same reason plus a stronger one: under the flock backend it
			// deliberately outlives its holder, so reading it as a writer would let a
			// finished restore block collection until the stale threshold. An in-flight
			// restore blocks gc through its writer PRESENCE lock, which is the real
			// interlock and is scanned like every other writer below.
			continue
		}
		switch e.State {
		case lockfile.EntryActive:
			p.locksBlock = true
			p.add(gcdom.KindLock, gcdom.ReasonLockActive, "", e.Path,
				"active lock ("+e.Record.Operation+" on "+e.Record.Hostname+") blocks destructive gc", 0)
		case lockfile.EntryUnknown:
			p.locksBlock = true
			p.add(gcdom.KindLock, gcdom.ReasonLockUnknown, "", e.Path,
				"unrecognized lock blocks destructive gc: "+e.Detail, 0)
		}
	}
	return nil
}

// committedCutoff resolves the --committed eligibility cutoff. ok is false when the
// flag is off, or when git has no commit to anchor to (not a worktree, no git, or no
// commits yet) — in which case the committed policy deletes nothing.
func (p *planner) committedCutoff() (time.Time, bool, error) {
	if !p.req.Committed() {
		return time.Time{}, false, nil
	}
	info, ok, err := p.repos.git.LatestCommit(p.ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	if !ok {
		return time.Time{}, false, nil
	}
	return info.Committed, true, nil
}

// checkpointRecord is a discovered checkpoint with the metadata retention needs.
type checkpointRecord struct {
	id        checkpoint.CheckpointID
	createdAt time.Time
	corrupt   bool
}

// planCheckpoints classifies every checkpoint and returns the set retained (those not
// marked for deletion), which the blob sweep marks reachability from. A corrupt
// checkpoint is retained and blocks the blob sweep; a latest, kept, too-recent, or
// not-yet-committed checkpoint is retained with its reason; anything else is a
// deletion candidate, subject to the subsystem filter.
func (p *planner) planCheckpoints(commitCutoff time.Time, committedOK bool) (map[string]bool, error) {
	retained := map[string]bool{}

	ids, err := p.repos.checkpoints.ListIDs(p.ctx)
	if err != nil {
		return nil, fmt.Errorf("listing checkpoints: %w", err)
	}

	var records []checkpointRecord
	for _, id := range ids {
		h, err := p.repos.checkpoints.Header(id)
		if err != nil {
			// A header GC cannot read is retained and blocks the sweep either way: its
			// blobs cannot be proven unreachable while the record will not open. Why it
			// will not open still matters to the person reading the report, so damage and
			// an unreadable schema keep separate reasons.
			p.checkpointReadFailed = true
			p.unreadableCheckpoints[id.String()] = true
			checkpointPath := filepath.Join(p.layout.CheckpointsDir(), id.String())
			reason, msg := gcdom.ReasonCheckpointCorrupt, "checkpoint header is corrupt: "
			if errors.Is(err, checkpoint.ErrIncompatibleFormat) {
				reason, msg = gcdom.ReasonCheckpointIncompatible, "checkpoint header uses an incompatible schema: "
			}
			p.add(gcdom.KindCheckpoint, reason, id.String(), checkpointPath, msg+err.Error(), 0)
			retained[id.String()] = true
			records = append(records, checkpointRecord{id: id, corrupt: true})
			continue
		}
		records = append(records, checkpointRecord{
			id:        id,
			createdAt: h.CreatedAt,
		})
	}

	// Chronological position is the checkpoint domain's to define, so latest and
	// keep-last resolve the newest record through the same owner the reads use rather
	// than through a retention-local rule that could drift from it. Ids are unique store
	// identities, so no two live records compare equal on both axes and the sort needs no
	// stability to be reproducible across plan and execute.
	live := liveRecords(records)
	slices.SortFunc(live, func(a, b checkpointRecord) int {
		return checkpoint.CompareNewestFirst(a.createdAt, a.id, b.createdAt, b.id)
	})

	keepLast := p.req.KeepLast(p.cfg.GC.KeepLastCheckpoints)
	withinKeep := map[string]bool{}
	for i, rec := range live {
		if i < keepLast {
			withinKeep[rec.id.String()] = true
		}
	}
	latestID := ""
	if len(live) > 0 {
		latestID = live[0].id.String()
	}

	for _, rec := range live {
		checkpointPath := filepath.Join(p.layout.CheckpointsDir(), rec.id.String())
		reason, del := p.checkpointDecision(rec, latestID, withinKeep, commitCutoff, committedOK)
		if !del {
			retained[rec.id.String()] = true
			p.add(gcdom.KindCheckpoint, reason, rec.id.String(), checkpointPath, checkpointRetainMsg(reason), 0)
			continue
		}
		if !p.req.Filter().Allows(gcdom.KindCheckpoint) {
			retained[rec.id.String()] = true
			p.add(gcdom.KindCheckpoint, gcdom.ReasonSubsystemFiltered, rec.id.String(), checkpointPath,
				"checkpoint eligible for deletion but excluded by subsystem filter", 0)
			continue
		}
		p.add(gcdom.KindCheckpoint, gcdom.ReasonCheckpointExpired, rec.id.String(), checkpointPath,
			"checkpoint outside retention policy", 0)
	}
	return retained, nil
}

// checkpointDecision returns the retention reason and whether the checkpoint is a
// deletion candidate. Retention rules are checked most-protective first so the
// reported reason is the strongest one that applies.
func (p *planner) checkpointDecision(rec checkpointRecord, latestID string, withinKeep map[string]bool, commitCutoff time.Time, committedOK bool) (gcdom.RetentionReason, bool) {
	if rec.id.String() == latestID {
		return gcdom.ReasonCheckpointLatest, false
	}
	if withinKeep[rec.id.String()] {
		return gcdom.ReasonCheckpointWithinKeepN, false
	}
	if p.req.HasOlderThan() && rec.createdAt.After(p.now.Add(-p.req.OlderThan())) {
		return gcdom.ReasonCheckpointTooRecent, false
	}
	if p.req.Committed() {
		// Under --committed, an otherwise-eligible checkpoint is deleted only if it
		// predates the latest commit and git could anchor that cutoff.
		if !committedOK {
			return gcdom.ReasonGitUnavailable, false
		}
		if !rec.createdAt.Before(commitCutoff) {
			return gcdom.ReasonCheckpointNotCommitted, false
		}
	}
	return gcdom.ReasonCheckpointExpired, true
}

// planRuns classifies every run entry. A corrupt run is retained and blocks the
// sweep; a too-recent or not-yet-committed run is retained; anything else is a
// deletion candidate, subject to the subsystem filter. Deletion reuses the run
// store, which removes the entry, its payloads, and its key pointer together.
func (p *planner) planRuns(commitCutoff time.Time, committedOK bool) error {
	refs, err := p.repos.runs.ListRefs(p.ctx)
	if err != nil {
		return fmt.Errorf("listing runs: %w", err)
	}
	// ListRefs walks shard directories in unspecified order; sort by id so the plan's
	// run candidates are deterministic across platforms.
	sort.Slice(refs, func(i, j int) bool { return refs[i].String() < refs[j].String() })
	window := p.cfg.GC.KeepRunsFor.Duration()
	if p.req.HasOlderThan() {
		window = p.req.OlderThan()
	}
	cutoff := p.now.Add(-window)

	for _, id := range refs {
		runPath := p.repos.runs.EntryPath(id)
		entry, err := p.repos.runs.Get(id)
		if err != nil {
			switch {
			case errors.Is(err, runcache.ErrNotFound):
				// Raced with a concurrent delete; nothing to plan.
				continue
			case errors.Is(err, runcache.ErrIncompatibleEntry):
				// An incompatible schema is distinct from corruption — the record is intact,
				// this build simply has no reader for it — but it is just as undeletable. GC
				// cannot demonstrate what a record it cannot decode references, so it can
				// prove neither that the entry is disposable nor that the blob reachability
				// set is complete without it. Retain it, suppress the sweep, and leave removal
				// to the user's explicit reset (or `run rm` by id), exactly as for a record
				// that will not parse.
				p.runReadFailed = true
				p.add(gcdom.KindRun, gcdom.ReasonRunIncompatible, id.String(), runPath,
					"run entry uses an incompatible metadata schema: "+err.Error(), 0)
				continue
			default:
				// Corrupt or unreadable: retain and block the sweep. GC never deletes a
				// run it cannot read (run rm is the explicit path for that), and an
				// unreadable run also makes the reachability set incomplete — runs carry no
				// blob refs today, but a record we cannot parse cannot prove that, so the
				// blob sweep is suppressed conservatively rather than risk a false unreachable.
				p.runReadFailed = true
				p.add(gcdom.KindRun, gcdom.ReasonRunCorrupt, id.String(), runPath,
					"run entry is corrupt or unreadable: "+err.Error(), 0)
				continue
			}
		}
		bytes := runBytes(entry)
		reason, del := p.runDecision(entry, cutoff, commitCutoff, committedOK)
		if !del {
			p.add(gcdom.KindRun, reason, id.String(), runPath, runRetainMsg(reason), bytes)
			continue
		}
		if !p.req.Filter().Allows(gcdom.KindRun) {
			p.add(gcdom.KindRun, gcdom.ReasonSubsystemFiltered, id.String(), runPath,
				"run eligible for deletion but excluded by subsystem filter", bytes)
			continue
		}
		p.add(gcdom.KindRun, gcdom.ReasonRunExpired, id.String(), runPath,
			"run older than the retention window", bytes)
	}
	return nil
}

func (p *planner) runDecision(entry runcache.RunEntry, cutoff, commitCutoff time.Time, committedOK bool) (gcdom.RetentionReason, bool) {
	if entry.FinishedAt.After(cutoff) {
		return gcdom.ReasonRunTooRecent, false
	}
	if p.req.Committed() {
		if !committedOK {
			return gcdom.ReasonGitUnavailable, false
		}
		if !entry.FinishedAt.Before(commitCutoff) {
			return gcdom.ReasonRunNotCommitted, false
		}
	}
	return gcdom.ReasonRunExpired, true
}

// planBlobs marks blobs reachable from the retained checkpoints, then sweeps the
// unmarked ones. The sweep runs only when reachability is complete and deletion is
// safe (no corrupt checkpoint/run, no active/unknown lock); otherwise it is skipped
// with a warning, because a false "unreachable" is data loss.
func (p *planner) planBlobs(retainedSnaps, retainedRestores map[string]bool) error {
	marked := map[string]bool{}
	// Iterate retained ids in sorted order so any corrupt-manifest candidates emitted
	// here are deterministic, matching the stable ordering of every other stage.
	retainedIDs := make([]string, 0, len(retainedSnaps))
	for idStr := range retainedSnaps {
		retainedIDs = append(retainedIDs, idStr)
	}
	sort.Strings(retainedIDs)
	for _, idStr := range retainedIDs {
		id, err := checkpoint.ParseCheckpointID(idStr)
		if err != nil {
			// A retained id we cannot parse means an incomplete reachability set.
			p.checkpointReadFailed = true
			continue
		}
		if p.unreadableCheckpoints[idStr] {
			// Its header already failed, was reported, and suppressed the sweep. Reaching
			// for the manifest would hit the same failure and restate one record as a
			// second candidate.
			continue
		}
		if err := p.markCheckpointBlobs(id, marked); err != nil {
			// A retained checkpoint whose manifest cannot be read leaves reachability
			// incomplete: block the sweep and report it as corruption.
			p.checkpointReadFailed = true
			checkpointPath := filepath.Join(p.layout.CheckpointsDir(), idStr)
			p.add(gcdom.KindCheckpoint, gcdom.ReasonCheckpointCorrupt, idStr, checkpointPath,
				"checkpoint manifest is unreadable, blocking blob sweep: "+err.Error(), 0)
		}
	}

	// A retained recovery observation is a reachability root exactly like a retained
	// checkpoint: its manifest references the bytes an applied restore can be undone
	// from, and sweeping those would silently turn a reversible restore into an
	// irreversible one.
	restoreIDs := make([]string, 0, len(retainedRestores))
	for idStr := range retainedRestores {
		restoreIDs = append(restoreIDs, idStr)
	}
	sort.Strings(restoreIDs)
	for _, idStr := range restoreIDs {
		id, err := restoredom.ParseOperationID(idStr)
		if err != nil {
			p.restoreReadFailed = true
			continue
		}
		if err := p.markRestoreBlobs(id, marked); err != nil {
			p.restoreReadFailed = true
			p.add(gcdom.KindRestore, gcdom.ReasonRestoreCorrupt, idStr, p.restorePath(idStr),
				"recovery observation manifest is unreadable, blocking blob sweep: "+err.Error(), 0)
		}
	}

	infos, err := p.repos.blobs.List()
	if err != nil {
		return fmt.Errorf("listing blobs: %w", err)
	}
	// Record the full blob-store footprint from the complete listing, regardless of
	// whether the sweep proceeds. This is the exact accounting ordinary status omits.
	p.blobCount = len(infos)
	p.blobBytes = 0
	for _, in := range infos {
		p.blobBytes += in.Bytes
	}
	p.blobFootprintDone = true

	canSweep := !p.checkpointReadFailed && !p.runReadFailed && !p.restoreReadFailed && !p.locksBlock && !p.committedUnavailable
	if !canSweep {
		if !p.committedUnavailable {
			p.warnings = append(p.warnings, "blob sweep skipped: reachability is incomplete or a lock is active")
		}
		// Still report referenced blobs as retained so the plan is complete; never
		// emit a delete based on an incomplete reachability set.
		for _, in := range infos {
			h := in.Ref.Hash()
			if marked[h.String()] {
				p.add(gcdom.KindBlob, gcdom.ReasonBlobReferenced, h.String(), p.blobPath(h), "referenced by retained evidence", in.Bytes)
			}
		}
		return nil
	}

	for _, in := range infos {
		h := in.Ref.Hash()
		path := p.blobPath(h)
		if marked[h.String()] {
			p.add(gcdom.KindBlob, gcdom.ReasonBlobReferenced, h.String(), path, "referenced by retained evidence", in.Bytes)
			continue
		}
		if !p.req.Filter().Allows(gcdom.KindBlob) {
			p.add(gcdom.KindBlob, gcdom.ReasonSubsystemFiltered, h.String(), path,
				"unreachable blob excluded by subsystem filter", in.Bytes)
			continue
		}
		p.add(gcdom.KindBlob, gcdom.ReasonBlobUnreachable, h.String(), path,
			"not referenced by any retained checkpoint or recovery observation", in.Bytes)
	}
	return nil
}

// markCheckpointBlobs streams a checkpoint's manifest and marks every regular,
// blob-stored entry's content hash as reachable, never materializing the manifest.
func (p *planner) markCheckpointBlobs(id checkpoint.CheckpointID, marked map[string]bool) error {
	stream, err := p.repos.checkpoints.OpenManifest(id)
	if err != nil {
		return err
	}
	cur, err := stream.Open(p.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		rec := cur.Record()
		e, ok := rec.Entry()
		if !ok {
			continue // skipped records carry no blob
		}
		if e.Kind != worktree.KindRegular || e.Storage != worktree.StorageBlob {
			continue
		}
		marked[e.Content.String()] = true
	}
	return cur.Err()
}

// blobPath renders the on-disk path of a blob for reporting. The hash came from the
// blob store's own listing, so PathFor cannot fail in practice; an unexpected error
// degrades to the blobs directory rather than panicking a read-only plan.
func (p *planner) blobPath(h hashing.ContentHash) string {
	bp, err := blob.PathFor(h)
	if err != nil {
		return p.layout.BlobsDir()
	}
	return filepath.Join(p.layout.BlobsDir(), filepath.FromSlash(bp.Rel()))
}

// planRestores classifies every restore recovery observation and returns the ids
// of the retained ones, which planBlobs then treats as blob-reachability roots.
//
// Three rules make this stage different from the others, and each is a product
// decision rather than an implementation detail:
//
//   - It has its OWN window, gc.keep_restores_for, not keep_runs_for. A recovery
//     observation is the only evidence an applied restore can be undone from, so
//     how long it survives is a worktree-safety decision; shortening run history
//     must not silently shorten the undo window. An explicit --older-than still
//     overrides it for that invocation.
//   - --committed does NOT classify it. A commit advancing says nothing about
//     whether the pre-restore state of an uncommitted worktree is disposable, so
//     git evidence is never consulted here.
//   - An unreadable record BLOCKS rather than being reclaimed, and unlike an incompatible
//     run entry a recovery observation references blobs, so a record GC cannot read cannot
//     prove which, so the blob sweep stands down instead of guessing.
func (p *planner) planRestores() (map[string]bool, error) {
	retained := map[string]bool{}
	findings, err := p.repos.restores.List(p.ctx)
	if err != nil {
		// The store directory itself is unreadable. Fail closed: reachability is
		// unknown, so nothing may be swept on its account.
		p.restoreReadFailed = true
		p.add(gcdom.KindRestore, gcdom.ReasonRestoreCorrupt, "", p.layout.RestoresDir(),
			"restore recovery store is unreadable, blocking destructive gc: "+err.Error(), 0)
		return retained, nil
	}

	window := p.cfg.GC.KeepRestoresFor.Duration()
	if p.req.HasOlderThan() {
		window = p.req.OlderThan()
	}
	cutoff := p.now.Add(-window)

	for _, f := range findings {
		idStr := f.ID.String()
		if idStr == "" {
			idStr = f.Name
		}
		path := p.restorePath(idStr)
		if !f.Readable() {
			p.restoreReadFailed = true
			p.add(gcdom.KindRestore, gcdom.ReasonRestoreCorrupt, idStr, path,
				"recovery observation is corrupt or unreadable: "+f.Err.Error(), 0)
			continue
		}
		if !f.Record.CreatedAt().Before(cutoff) {
			retained[idStr] = true
			p.add(gcdom.KindRestore, gcdom.ReasonRestoreTooRecent, idStr, path,
				"recovery observation is within the restore retention window", 0)
			continue
		}
		// Eligible by age. A *-only filter restricts deletion to its own subsystem, so
		// it excludes recovery observations; the record stays a reachability root while
		// it is skipped, otherwise the same invocation could sweep the blobs it needs.
		if !p.req.Filter().Allows(gcdom.KindRestore) {
			retained[idStr] = true
			p.add(gcdom.KindRestore, gcdom.ReasonSubsystemFiltered, idStr, path,
				"recovery observation eligible for deletion but excluded by subsystem filter", 0)
			continue
		}
		if p.committedUnavailable {
			// The invocation as a whole plans no deletions; keep the record — and its
			// blobs — rather than reclaiming on a pass that promised to remove nothing.
			// The reason is git-unavailable, the same invocation-wide fact checkpoints,
			// runs, and temp report: the record is past its own window, so calling it
			// too-recent would publish a retention fact that is not true.
			retained[idStr] = true
			p.add(gcdom.KindRestore, gcdom.ReasonGitUnavailable, idStr, path,
				"recovery observation retained: --committed could not confirm git, so this invocation plans no deletions", 0)
			continue
		}
		p.add(gcdom.KindRestore, gcdom.ReasonRestoreExpired, idStr, path,
			"recovery observation older than the restore retention window", 0)
	}
	return retained, nil
}

// markRestoreBlobs folds a retained recovery observation's captured content into
// the reachability set.
func (p *planner) markRestoreBlobs(id restoredom.OperationID, marked map[string]bool) error {
	read, err := p.repos.restores.Open(p.ctx, id)
	if err != nil {
		return err
	}
	cur, err := read.Manifest().Open(p.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		e, ok := cur.Record().Entry()
		if !ok {
			continue
		}
		if e.Kind != worktree.KindRegular || e.Storage != worktree.StorageBlob {
			continue
		}
		marked[e.Content.String()] = true
	}
	return cur.Err()
}

// restorePath renders a recovery observation's on-disk location for a report line.
func (p *planner) restorePath(id string) string {
	if id == "" {
		return p.layout.RestoresDir()
	}
	return filepath.Join(p.layout.RestoresDir(), id)
}

// planTemp classifies temp artifacts under the two known temp locations using the
// same stale-by-age semantics doctor uses: an artifact older than the stale
// threshold is a deletion candidate; a fresh one may belong to an active operation
// and is retained. Symlinks and non-regular nodes are never swept.
func (p *planner) planTemp() error {
	dirs := []string{
		p.layout.TmpDir(),
		filepath.Join(p.layout.RunsDir(), "tmp"),
		filepath.Join(p.layout.RestoresDir(), "tmp"),
	}
	for _, dir := range dirs {
		if err := p.planOneTempDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func (p *planner) planOneTempDir(dir string) error {
	relDir, err := fsx.RelUnder(p.layout.Root(), dir)
	if err != nil {
		return nil // outside root; not ours to touch
	}
	entries, err := fsx.ReadDirNoFollow(p.layout.Root(), relDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // an absent temp dir is a clean temp dir
		}
		// A symlinked or unreadable temp dir is reported by doctor's layout check; GC
		// leaves it alone rather than sweeping through it.
		p.warnings = append(p.warnings, "could not read temp dir "+dir+": "+err.Error())
		return nil
	}
	// Directory order is unspecified; sort by name so temp candidates are deterministic.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		// Only real files and directories are sweepable; a symlink in a temp dir is not
		// our artifact and is never followed or removed.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // cannot judge age; treat as fresh and leave it
		}
		var size int64
		if info.Mode().IsRegular() {
			size = info.Size()
		}
		stale := p.now.Sub(info.ModTime()) > p.svc.staleAfter
		switch {
		case !stale:
			p.add(gcdom.KindTemp, gcdom.ReasonTempFresh, "", path,
				"temp artifact too recent to remove safely (may be an active operation)", size)
		case p.committedUnavailable:
			// --committed could not be confirmed, so the plan deletes nothing — stale temp
			// included — and reports why.
			p.add(gcdom.KindTemp, gcdom.ReasonGitUnavailable, "", path,
				"stale temp retained: --committed could not confirm git", size)
		case !p.req.Filter().Allows(gcdom.KindTemp):
			// A *-only filter restricts deletion to its subsystem; temp is out of scope.
			p.add(gcdom.KindTemp, gcdom.ReasonSubsystemFiltered, "", path,
				"stale temp excluded by subsystem filter", size)
		default:
			p.add(gcdom.KindTemp, gcdom.ReasonTempStale, "", path,
				"orphan temp artifact older than the stale threshold", size)
		}
	}
	return nil
}

// liveRecords filters out corrupt checkpoint records, which are retained separately
// and do not participate in latest/keep-last ordering.
func liveRecords(records []checkpointRecord) []checkpointRecord {
	out := make([]checkpointRecord, 0, len(records))
	for _, r := range records {
		if !r.corrupt {
			out = append(out, r)
		}
	}
	return out
}

// runBytes estimates the reclaimable bytes of a run from its stored payloads. Entry
// metadata and the pre/post observation manifests are not counted (the entry does
// not carry their on-disk byte sizes), so this is a lower bound; the payloads
// dominate. Deletion removes the whole entry directory, so the manifests are always
// reclaimed regardless of this estimate.
func runBytes(e runcache.RunEntry) int64 {
	return e.Stdout.StoredBytes + e.Stderr.StoredBytes
}

func checkpointRetainMsg(reason gcdom.RetentionReason) string {
	switch reason {
	case gcdom.ReasonCheckpointLatest:
		return "latest checkpoint is always retained"
	case gcdom.ReasonCheckpointWithinKeepN:
		return "within the keep-last window"
	case gcdom.ReasonCheckpointTooRecent:
		return "newer than the older-than window"
	case gcdom.ReasonCheckpointNotCommitted:
		return "not older than the latest commit"
	case gcdom.ReasonGitUnavailable:
		return "git unavailable; --committed retains conservatively"
	default:
		return "retained"
	}
}

func runRetainMsg(reason gcdom.RetentionReason) string {
	switch reason {
	case gcdom.ReasonRunTooRecent:
		return "newer than the run-retention window"
	case gcdom.ReasonRunNotCommitted:
		return "not older than the latest commit"
	case gcdom.ReasonGitUnavailable:
		return "git unavailable; --committed retains conservatively"
	default:
		return "retained"
	}
}
