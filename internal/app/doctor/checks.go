package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/doctor"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	restoredom "awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/restorestore"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/sqliteindex"
)

// checkLayout verifies the required .awa directory layout. The config itself was
// already parsed and validated upstream (a bad config exits before doctor's checks
// run), so it counts as one passed check here; each required directory is then
// checked for presence and for being a real directory. Doctor never creates a
// missing directory — it reports layout damage rather than silently normalizing it.
func (s *Service) checkLayout(layout paths.Layout, rep *report) {
	// Config reached this point already decoded and validated, so it is healthy.
	rep.pass()

	root := layout.Root()
	for _, d := range layout.RequiredDirs() {
		ok, err := projfs.DirExists(d)
		switch {
		case errors.Is(err, projfs.ErrNotDirectory):
			rep.add(finding(doctor.CodeRequiredDirNotDir, doctor.SeverityError, doctor.SubsystemLayout,
				d, filepath.Base(d), "required path exists but is not a directory", false))
		case err != nil:
			rep.add(finding(doctor.CodeRequiredDirMissing, doctor.SeverityError, doctor.SubsystemLayout,
				d, filepath.Base(d), "cannot check required directory: "+err.Error(), false))
		case !ok:
			rep.add(finding(doctor.CodeRequiredDirMissing, doctor.SeverityError, doctor.SubsystemLayout,
				d, filepath.Base(d), "required directory is missing", false))
		default:
			// DirExists stats through symlinks, so a required storage dir replaced by a
			// symlink (even one pointing at a real directory, or at a path outside the
			// project) passes the check above. Confirm the directory is reachable without
			// following any symlinked component, the same no-follow policy the store
			// adapters enforce, so a symlinked .awa path is reported rather than trusted.
			if perr := requireRealDir(root, d); perr != nil {
				rep.add(finding(doctor.CodeRequiredDirNotDir, doctor.SeverityError, doctor.SubsystemLayout,
					d, filepath.Base(d), "required directory is not reachable without following a symlink: "+perr.Error(), false))
			} else {
				rep.pass()
			}
		}
	}
}

// requireRealDir confirms dir is a real directory reached from root without
// following a symlinked component. It is the no-follow counterpart to a stat-based
// check: a symlinked ancestor or leaf surfaces as an error rather than being
// silently traversed.
func requireRealDir(root, dir string) error {
	rel, err := fsx.RelUnder(root, dir)
	if err != nil {
		return err
	}
	f, err := fsx.OpenDirNoFollow(root, rel)
	if err != nil {
		return err
	}
	return f.Close()
}

// checkCheckpoints validates every committed checkpoint's header, streams its manifest
// (never materializing it), and verifies that each regular blob-stored entry's blob
// is present — and, in strict mode, that it still hashes to its address. Hash-only
// entries carry no blob and are not flagged; skipped records are valid diagnostics.
// A corrupt header or manifest is reported through the checkpoint repository's own
// validation, not a more forgiving re-parse.
func (s *Service) checkCheckpoints(ctx context.Context, layout paths.Layout, strict bool, rep *report) {
	repo := checkpointjson.NewRepo(layout)
	ids, err := repo.ListIDs(ctx)
	if err != nil {
		rep.add(finding(doctor.CodeCheckpointCorrupt, doctor.SeverityError, doctor.SubsystemCheckpoints,
			layout.CheckpointsDir(), "checkpoints", "checkpoint store is structurally corrupt: "+err.Error(), false))
		return
	}

	for _, id := range ids {
		// Honor cancellation mid-walk: a large local history is interruptible rather than
		// checked to completion. Run re-reads ctx.Err after this check and surfaces it.
		if ctx.Err() != nil {
			return
		}
		s.checkOneCheckpoint(ctx, layout, repo, id, strict, rep)
	}
}

func (s *Service) checkOneCheckpoint(ctx context.Context, layout paths.Layout, repo *checkpointjson.Repo, id checkpoint.CheckpointID, strict bool, rep *report) {
	checkpointPath := filepath.Join(layout.CheckpointsDir(), id.String())

	// The header is read for its own integrity, not its content: an unreadable header
	// is the finding, and every per-entry check below works from the manifest.
	if _, err := repo.Header(id); err != nil {
		// An incompatible schema is intact evidence this build has no reader for, not
		// damage: report it distinctly (a stable code the user can act on) rather than as
		// mysterious corruption. It is not repairable — awa does not migrate, decode, or
		// quarantine such a record; the fix is an explicit reset of local evidence.
		if errors.Is(err, checkpoint.ErrIncompatibleFormat) {
			rep.add(finding(doctor.CodeCheckpointIncompatibleFormat, doctor.SeverityError, doctor.SubsystemCheckpoints,
				checkpointPath, id.Short(), err.Error()+"; "+paths.ResetEvidenceHint(), false))
			return
		}
		rep.add(finding(doctor.CodeCheckpointCorrupt, doctor.SeverityError, doctor.SubsystemCheckpoints,
			checkpointPath, id.Short(), "checkpoint header is corrupt: "+err.Error(), false))
		return
	}

	stream, err := repo.OpenManifest(id)
	if err != nil {
		code, msg := classifyManifestErr(err)
		rep.add(finding(code, doctor.SeverityError, doctor.SubsystemCheckpoints, checkpointPath, id.Short(), msg, false))
		return
	}
	cur, err := stream.Open(ctx)
	if err != nil {
		code, msg := classifyManifestErr(err)
		rep.add(finding(code, doctor.SeverityError, doctor.SubsystemCheckpoints, checkpointPath, id.Short(), msg, false))
		return
	}
	defer func() { _ = cur.Close() }()

	// The hasher lets strict mode recompute blob digests; presence checks do not use it.
	hasher := blake3hash.New()
	blobs := blobstore.New(layout, hasher)

	before := len(rep.findings)
	for cur.Next() {
		rec := cur.Record()
		e, ok := rec.Entry()
		if !ok {
			continue // skipped records are valid diagnostics, not corruption
		}
		if e.Kind != worktree.KindRegular || e.Storage != worktree.StorageBlob {
			continue // directories, symlinks, and hash-only entries carry no blob
		}
		s.verifyBlob(blobs, hasher, e, strict, checkpointPath, rep)
	}
	if err := cur.Err(); err != nil {
		code, msg := classifyManifestErr(err)
		rep.add(finding(code, doctor.SeverityError, doctor.SubsystemCheckpoints, checkpointPath, id.Short(), msg, false))
		return
	}

	if len(rep.findings) == before {
		rep.pass()
	}
}

// verifyBlob reports a missing or (in strict mode) corrupt blob referenced by a
// checkpoint entry. Presence is the cheap default check; strict mode additionally
// streams the blob through the checkpoint's hasher and confirms it still matches its
// content address, catching a blob whose bytes were altered in place.
func (s *Service) verifyBlob(blobs *blobstore.FS, hasher hashing.Hasher, e worktree.Entry, strict bool, checkpointPath string, rep *report) {
	present, err := blobs.Has(e.Content)
	if err != nil {
		rep.add(finding(doctor.CodeCheckpointMissingBlob, doctor.SeverityError, doctor.SubsystemCheckpoints,
			checkpointPath, e.Path.String(), "cannot read referenced blob "+e.Content.String()+": "+err.Error(), false))
		return
	}
	if !present {
		rep.add(finding(doctor.CodeCheckpointMissingBlob, doctor.SeverityError, doctor.SubsystemCheckpoints,
			checkpointPath, e.Path.String(), "checkpoint entry references a missing blob "+e.Content.String(), false))
		return
	}
	if !strict {
		return
	}
	rc, err := blobs.Open(e.Content)
	if err != nil {
		rep.add(finding(doctor.CodeCheckpointMissingBlob, doctor.SeverityError, doctor.SubsystemCheckpoints,
			checkpointPath, e.Path.String(), "cannot open referenced blob "+e.Content.String()+": "+err.Error(), false))
		return
	}
	got, herr := hasher.HashReader(rc)
	_ = rc.Close()
	if herr != nil {
		rep.add(finding(doctor.CodeCheckpointMissingBlob, doctor.SeverityError, doctor.SubsystemCheckpoints,
			checkpointPath, e.Path.String(), "cannot rehash referenced blob "+e.Content.String()+": "+herr.Error(), false))
		return
	}
	if got.String() != e.Content.String() {
		rep.add(finding(doctor.CodeCheckpointMissingBlob, doctor.SeverityError, doctor.SubsystemCheckpoints,
			checkpointPath, e.Path.String(), "referenced blob is present but does not match its address "+e.Content.String(), false))
	}
}

// classifyManifestErr maps a checkpoint-store error to the most specific finding
// code. A manifest the header promised but that is gone is missing-manifest;
// anything else the store rejected (a malformed line, a truncated manifest, a bad
// record) is manifest-corrupt.
func classifyManifestErr(err error) (doctor.FindingCode, string) {
	if strings.Contains(err.Error(), "manifest is missing") {
		return doctor.CodeCheckpointMissingManifest, "checkpoint manifest is missing: " + err.Error()
	}
	if errors.Is(err, checkpoint.ErrCorruptStore) {
		return doctor.CodeCheckpointManifestCorrupt, "checkpoint manifest is corrupt: " + err.Error()
	}
	return doctor.CodeCheckpointManifestCorrupt, "cannot read checkpoint manifest: " + err.Error()
}

// checkRuns validates every run entry through the run store, which verifies its
// metadata, keys, and pointer invariants, and verifies each entry's stdout and
// stderr payloads by opening them (the store checks size and hash on open). Corrupt
// runs are reported, never deleted: corruption must stay visible, not be mistaken
// for an empty cache.
func (s *Service) checkRuns(ctx context.Context, layout paths.Layout, rep *report) {
	repo := runstore.New(layout, blake3hash.New())

	refs, err := repo.ListRefs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // cancelled mid-enumeration: Run surfaces ctx.Err, not a corruption finding
		}
		rep.add(finding(doctor.CodeRunCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
			layout.RunsDir(), "runs", "run store is unreadable: "+err.Error(), false))
		return
	}

	for _, id := range refs {
		if ctx.Err() != nil {
			return // honor cancellation mid-walk; Run re-reads ctx.Err and surfaces it
		}
		runPath := repo.EntryPath(id)
		entry, err := repo.Get(id)
		if err != nil {
			switch {
			case errors.Is(err, runcache.ErrIncompatibleEntry):
				// A run entry declaring a metadata schema this build cannot read: intact, but
				// never reusable, so it is reported as incompatible — a distinct state — rather
				// than corruption. It is never served as a hit (lookup treats it as a miss) and
				// gc retains it; clear it with 'awa run rm' or by removing .awa/runs. The
				// store's error names the persisted and expected schema versions, so this
				// detail forwards it instead of restating a boundary that moves.
				rep.add(finding(doctor.CodeRunIncompatibleFormat, doctor.SeverityWarning, doctor.SubsystemRuns,
					runPath, id.Short(), "never reused: "+err.Error(), false))
			case errors.Is(err, runcache.ErrCorruptStore):
				rep.add(finding(doctor.CodeRunCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
					runPath, id.Short(), "run entry is corrupt: "+err.Error(), false))
			case errors.Is(err, runcache.ErrNotFound):
				// Raced with a concurrent delete; not corruption.
			default:
				rep.add(finding(doctor.CodeRunCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
					runPath, id.Short(), "cannot read run entry: "+err.Error(), false))
			}
			continue
		}
		if s.verifyRunPayloads(ctx, repo, entry, runPath, rep) {
			rep.pass()
		}
	}

	s.checkRunPointers(repo, layout, rep)
}

// checkRestores validates the restore recovery-observation store: the one record
// kind whose loss is irreversible, since it is the only evidence an applied restore
// can be undone from.
//
// Two conditions matter and are reported separately. A record that cannot be read
// is corruption — GC also fails its blob sweep closed on it, so naming it here is
// what tells a user which record to remove. A record that reads cleanly but whose
// captured content is no longer in the blob store is worse in a quieter way: the
// record still claims complete content, so it would resolve as a restore source and
// then refuse at apply time. Reporting it as a distinct code means the honest answer
// — "this undo is no longer available" — is visible before someone relies on it.
func (s *Service) checkRestores(ctx context.Context, layout paths.Layout, rep *report) {
	hasher := blake3hash.New()
	repo := restorestore.New(layout, hasher)
	blobs := blobstore.New(layout, hasher)

	findings, err := repo.List(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // cancelled mid-enumeration: Run surfaces ctx.Err, not a corruption finding
		}
		rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
			layout.RestoresDir(), "restores", "restore recovery store is unreadable: "+err.Error(), false))
		return
	}
	for _, f := range findings {
		if ctx.Err() != nil {
			return
		}
		subject := f.ID.Short()
		path := filepath.Join(layout.RestoresDir(), f.ID.String())
		if f.ID.IsZero() {
			subject, path = f.Name, filepath.Join(layout.RestoresDir(), f.Name)
		}
		if !f.Readable() {
			rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
				path, subject, "restore recovery observation is corrupt or unreadable: "+f.Err.Error(), false))
			continue
		}
		if s.verifyRecoveryPayloads(ctx, repo, blobs, f.ID, path, subject, rep) {
			rep.pass()
		}
	}
}

// verifyRecoveryPayloads drains one recovery observation's manifest and confirms the
// blob store still holds every byte it captured. It reports whether the record is
// intact.
func (s *Service) verifyRecoveryPayloads(ctx context.Context, repo *restorestore.Repo, blobs *blobstore.FS, id restoredom.OperationID, path, subject string, rep *report) bool {
	read, err := repo.Open(ctx, id)
	if err != nil {
		rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
			path, subject, "restore recovery observation payloads are unreadable: "+err.Error(), false))
		return false
	}
	cur, err := read.Manifest().Open(ctx)
	if err != nil {
		rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
			path, subject, "restore recovery manifest is unreadable: "+err.Error(), false))
		return false
	}
	defer func() { _ = cur.Close() }()
	intact := true
	for cur.Next() {
		if ctx.Err() != nil {
			return false
		}
		e, ok := cur.Record().Entry()
		if !ok || e.Kind != worktree.KindRegular || e.Storage != worktree.StorageBlob {
			continue
		}
		has, herr := blobs.Has(e.Content)
		if herr != nil {
			rep.add(finding(doctor.CodeRestoreRecoveryMissingBlob, doctor.SeverityError, doctor.SubsystemRestores,
				path, subject, "cannot verify captured content for "+e.Path.String()+": "+herr.Error(), false))
			intact = false
			continue
		}
		if !has {
			rep.add(finding(doctor.CodeRestoreRecoveryMissingBlob, doctor.SeverityError, doctor.SubsystemRestores,
				path, subject, "captured content for "+e.Path.String()+" is missing, so this restore can no longer be undone", false))
			intact = false
		}
	}
	if err := cur.Err(); err != nil {
		rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
			path, subject, "restore recovery manifest is corrupt: "+err.Error(), false))
		return false
	}
	// The covered scope bounds what the record proves absent, so a record whose scope
	// payload is unreadable cannot drive a correct inverse restore either.
	scope, err := read.Scope().Open(ctx)
	if err != nil {
		rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
			path, subject, "restore recovery covered scope is unreadable: "+err.Error(), false))
		return false
	}
	defer func() { _ = scope.Close() }()
	for scope.Next() { //nolint:revive // draining is the point: the count check runs at clean EOF
	}
	if err := scope.Err(); err != nil {
		rep.add(finding(doctor.CodeRestoreRecoveryCorrupt, doctor.SeverityError, doctor.SubsystemRestores,
			path, subject, "restore recovery covered scope is corrupt: "+err.Error(), false))
		return false
	}
	return intact
}

// checkRunPointers validates the run-cache key pointer store the same way Lookup
// does: a pointer that is undecodable, dangling, points to a run with a different
// key, or points to a non-reusable run would turn a real cache lookup into
// corruption, so it is reported as run-pointer-corrupt rather than left to surface
// only at run time.
func (s *Service) checkRunPointers(repo *runstore.Repo, layout paths.Layout, rep *report) {
	problems, err := repo.InspectKeyPointers()
	if err != nil {
		rep.add(finding(doctor.CodeRunPointerCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
			layout.RunsDir(), "run key pointers", "run key pointer store is unreadable: "+err.Error(), false))
		return
	}
	if len(problems) == 0 {
		rep.pass()
		return
	}
	for _, p := range problems {
		rep.add(finding(doctor.CodeRunPointerCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
			p.Path, filepath.Base(p.Path), p.Detail, false))
	}
}

// verifyRunPayloads opens an entry's durable evidence through the store (which
// verifies size and hash on open): the stdout and stderr payloads and the pre/post
// observation manifests the entry claims to have. It reports any that fail to
// verify and returns true only when all of them are healthy.
func (s *Service) verifyRunPayloads(ctx context.Context, repo *runstore.Repo, entry runcache.RunEntry, runPath string, rep *report) bool {
	healthy := true
	if so, err := repo.OpenStdout(entry.ID); err != nil {
		rep.add(finding(doctor.CodeRunPayloadCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
			runPath, entry.ID.Short(), "run stdout payload is corrupt: "+err.Error(), false))
		healthy = false
	} else {
		_ = so.Close()
	}
	if se, err := repo.OpenStderr(entry.ID); err != nil {
		rep.add(finding(doctor.CodeRunPayloadCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
			runPath, entry.ID.Short(), "run stderr payload is corrupt: "+err.Error(), false))
		healthy = false
	} else {
		_ = se.Close()
	}
	// A recorded run's pre/post observation manifests are durable evidence too. Every
	// decoded entry carries its before-observation, so that manifest is always verified;
	// the after is verified when the entry has one, since a post-scan-failed run is
	// legitimately without it and must not be flagged for the absence.
	if err := drainRunManifest(ctx, repo.OpenBeforeManifest, entry.ID); err != nil {
		rep.add(finding(doctor.CodeRunPayloadCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
			runPath, entry.ID.Short(), "run before-observation manifest is corrupt: "+err.Error(), false))
		healthy = false
	}
	if entry.After != nil {
		if err := drainRunManifest(ctx, repo.OpenAfterManifest, entry.ID); err != nil {
			rep.add(finding(doctor.CodeRunPayloadCorrupt, doctor.SeverityError, doctor.SubsystemRuns,
				runPath, entry.ID.Short(), "run after-observation manifest is corrupt: "+err.Error(), false))
			healthy = false
		}
	}
	return healthy
}

// drainRunManifest opens and fully reads one of a run's observation manifests
// through the verifying stream, so a missing, truncated, or tampered manifest
// surfaces as an error rather than passing silently.
func drainRunManifest(ctx context.Context, open func(runcache.RunID) (worktree.ManifestStream, error), id runcache.RunID) error {
	stream, err := open(id)
	if err != nil {
		return err
	}
	cur, err := stream.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
	}
	return cur.Err()
}

// checkIndex inspects the derived worktree index without modifying it and maps the
// inspector's outcome to a finding: a missing or current index is healthy (built
// lazily); a newer-than-supported schema is a non-repairable error (rebuilding would
// downgrade a newer awa's state); a stale or corrupt schema is a repairable error
// (acceleration-only state the next scan recreates); and an index that could not be
// accessed (busy, locked, or permission) is a non-repairable warning, because doctor
// must not delete an index it merely could not read.
func (s *Service) checkIndex(layout paths.Layout, strict bool, rep *report) {
	err := sqliteindex.Inspect(layout.IndexesDir(), strict)
	indexPath := filepath.Join(layout.IndexesDir(), sqliteindex.FileName)
	switch {
	case err == nil:
		rep.pass()
	case errors.Is(err, sqliteindex.ErrSchemaTooNew):
		// A newer schema cannot be safely rebuilt: that would downgrade state a newer
		// awa owns, so it is reported but not offered for repair.
		rep.add(finding(doctor.CodeIndexSchemaInvalid, doctor.SeverityError, doctor.SubsystemIndex,
			indexPath, "worktree index", err.Error(), false))
	case errors.Is(err, sqliteindex.ErrSchemaStale):
		// An older or structurally-mismatched schema is derived state the next scan
		// recreates, so it is repairable by rebuild.
		rep.add(finding(doctor.CodeIndexStale, doctor.SeverityError, doctor.SubsystemIndex,
			indexPath, "worktree index", "worktree index schema is out of date and can be rebuilt", true))
	case errors.Is(err, sqliteindex.ErrCorrupt):
		// A malformed database is accessible but unusable derived state: safe to discard
		// and rebuild.
		rep.add(finding(doctor.CodeIndexUnreadable, doctor.SeverityError, doctor.SubsystemIndex,
			indexPath, "worktree index", "worktree index is corrupt: "+err.Error(), true))
	case errors.Is(err, sqliteindex.ErrUnavailable):
		// Busy/locked (an active writer), a permission problem, or an I/O failure is not
		// a content judgment: doctor must not delete an index it merely could not read,
		// so this is a non-repairable warning rather than a corruption error.
		rep.add(finding(doctor.CodeIndexUnreadable, doctor.SeverityWarning, doctor.SubsystemIndex,
			indexPath, "worktree index", "worktree index could not be read (busy, locked, or permission); not verified: "+err.Error(), false))
	default:
		// An unexpected internal failure (e.g. computing the expected schema): report it
		// without offering a destructive repair.
		rep.add(finding(doctor.CodeIndexUnreadable, doctor.SeverityError, doctor.SubsystemIndex,
			indexPath, "worktree index", "worktree index could not be verified: "+err.Error(), false))
	}
}

// checkTemp inspects only the two known temp locations and reports each artifact
// found there: one older than the stale threshold is a crash leftover and a
// repairable orphan, while a fresh one may belong to an active operation and is a
// non-repairable warning doctor must not delete. A published state directory is
// never a temp directory, so doctor does not touch checkpoints/, runs/, or
// store/blobs/ here.
func (s *Service) checkTemp(layout paths.Layout, rep *report) {
	// store/tmp is a required dir, so its integrity as a real directory is validated
	// no-follow by checkLayout; runs/tmp is created on demand and is not required, so
	// the temp checker owns reporting when it exists but is not a usable directory.
	tempDirs := []struct {
		path     string
		required bool
	}{
		{layout.TmpDir(), true},                         // store/tmp
		{filepath.Join(layout.RunsDir(), "tmp"), false}, // runs/tmp
	}
	for _, td := range tempDirs {
		s.checkOneTempDir(layout.Root(), td.path, td.required, rep)
	}
}

func (s *Service) checkOneTempDir(root, dir string, required bool, rep *report) {
	relDir, err := fsx.RelUnder(root, dir)
	if err != nil {
		return
	}
	entries, err := fsx.ReadDirNoFollow(root, relDir)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			rep.pass() // an absent temp dir is a clean temp dir
		case required:
			// store/tmp's integrity as a real directory is checkLayout's no-follow
			// required-dir check; returning avoids double-reporting the same damage.
		default:
			// A non-required temp dir (runs/tmp) that exists but cannot be read no-follow
			// — a symlink, a file, or an unreadable node — is temp corruption no other
			// check covers, so report it here.
			rep.add(finding(doctor.CodeTempUnreadable, doctor.SeverityError, doctor.SubsystemTemp,
				dir, filepath.Base(dir), "temp directory is not a readable directory: "+err.Error(), false))
		}
		return
	}
	if len(entries) == 0 {
		rep.pass()
		return
	}
	now := s.now()
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		// A temp artifact is only safely removable once it is provably not an active
		// operation: blob materialize and run capture stage their work in these same
		// dirs. Use age as that proof — an artifact older than the stale threshold is a
		// leftover from a crashed operation and is repairable; a fresh one may belong to
		// a writer running right now, so it is reported as a non-repairable warning that
		// --repair must not delete. An unreadable mtime is treated as fresh (the safe,
		// non-destructive default).
		info, err := e.Info()
		stale := err == nil && now.Sub(info.ModTime()) > s.staleAfter
		if stale {
			rep.add(finding(doctor.CodeOrphanTemp, doctor.SeverityWarning, doctor.SubsystemTemp,
				p, e.Name(), "orphan temp artifact (older than the stale threshold)", true))
		} else {
			rep.add(finding(doctor.CodeOrphanTemp, doctor.SeverityWarning, doctor.SubsystemTemp,
				p, e.Name(), "temp artifact present but too recent to remove safely (may be an active operation)", false))
		}
	}
}

// checkLocks inspects .awa/locks through the shared lock scanner. A file whose
// schema doctor recognizes is classified as active or stale; a stale recognized
// lock is repairable. A file the scanner does not recognize is an unknown lock — a
// warning, never a repair target, because deleting an unparsed lock could discard a
// live writer's protection.
func (s *Service) checkLocks(layout paths.Layout, rep *report) {
	entries, err := lockfile.ScanDir(layout.Root(), layout.LocksDir(), s.now(), s.staleAfter, s.hostname, s.processAlive)
	if err != nil {
		// As with temp dirs, a symlinked or unreadable locks dir is the required-dir
		// integrity of .awa/locks, which checkLayout validates and reports no-follow.
		return
	}
	if len(entries) == 0 {
		rep.pass()
		return
	}
	for _, e := range entries {
		if lockfile.IsExclusiveLockName(e.Name) {
			// An exclusive lock (gc.lock, restore.lock) is never doctor's to repair. Under
			// the flock backend the file deliberately outlives its holder — releasing means
			// closing the descriptor, because unlinking would break flock for anyone
			// waiting — so a leftover file is normal rather than stale state. Marking it
			// repairable would also let the repair pass delete the very lock it acquires
			// moments later, dropping its own exclusivity mid-run. Whether an unrecognized
			// one is even worth reporting is platform-specific, so defer to the lockfile
			// package, and never repair it either way.
			if lockfile.ExclusiveLockReportsUnknown(e) {
				rep.add(finding(doctor.CodeLockUnknown, doctor.SeverityWarning, doctor.SubsystemLocks,
					e.Path, e.Name, e.Detail, false))
			} else {
				rep.pass()
			}
			continue
		}
		switch e.State {
		case lockfile.EntryUnknown:
			rep.add(finding(doctor.CodeLockUnknown, doctor.SeverityWarning, doctor.SubsystemLocks,
				e.Path, e.Name, e.Detail, false))
		case lockfile.EntryStale:
			rep.add(finding(doctor.CodeLockStale, doctor.SeverityWarning, doctor.SubsystemLocks,
				e.Path, e.Name, "stale lock (owner "+e.Record.Hostname+" pid is gone or record is past the stale threshold)", true))
		default:
			rep.pass() // an active lock is healthy
		}
	}
}

// checkGit warns when .awa is tracked by git. The git tracker is best-effort: an
// absent git, a non-git project, or a transient git failure yields no finding,
// because git's absence is normal and must never read as a project problem.
func (s *Service) checkGit(ctx context.Context, layout paths.Layout, rep *report) {
	tracker := s.git
	if tracker == nil {
		tracker = gitmeta.New(layout.Root())
	}
	tr, err := tracker.AwaTracking(ctx)
	if err != nil {
		// gitmeta already treats an absent git binary and a non-git project as a clean
		// (no error) result, so a non-nil error here is a genuine git failure. It is
		// reported as a warning — doctor could not determine git tracking — not as
		// silence that hides an unrun check behind a clean report.
		rep.add(finding(doctor.CodeGitCheckFailed, doctor.SeverityWarning, doctor.SubsystemGit,
			layout.AwaDir(), ".awa", "could not check whether .awa is tracked by git: "+err.Error(), false))
		return
	}
	if !tr.InRepo {
		return // not a git project: nothing to check
	}
	if tr.AwaTracked {
		rep.add(finding(doctor.CodeAwaTrackedByGit, doctor.SeverityWarning, doctor.SubsystemGit,
			layout.AwaDir(), ".awa", ".awa appears tracked by git; it is local cache, not source", false))
		return
	}
	rep.pass()
}

// checkStateGitignore verifies the awa-owned .awa/.gitignore guard that keeps the
// state directory out of git. It is deliberately git-independent — it inspects only
// the file's presence and content — so the guard is validated (and, with --repair,
// restored) even outside a git worktree. A missing or ineffective guard is a
// repairable warning; an unreadable guard is a non-repairable warning, since doctor
// could not classify it and must not overwrite what it could not read.
func (s *Service) checkStateGitignore(layout paths.Layout, rep *report) {
	state, err := projfs.InspectStateGitignore(layout)
	if err != nil {
		// The guard exists but could not be read (permission, I/O, or it is not a
		// regular file). Like an index doctor merely could not read, this is a
		// non-repairable warning: doctor must not overwrite a guard it could not
		// classify, and it is not the "missing" case.
		rep.add(finding(doctor.CodeStateGitignoreUnreadable, doctor.SeverityWarning, doctor.SubsystemGit,
			layout.StateGitignore(), ".awa/.gitignore", "could not read .awa/.gitignore guard: "+err.Error(), false))
		return
	}
	switch state {
	case projfs.StateGitignoreOK:
		rep.pass()
	case projfs.StateGitignoreMissing:
		rep.add(finding(doctor.CodeStateGitignoreMissing, doctor.SeverityWarning, doctor.SubsystemGit,
			layout.StateGitignore(), ".awa/.gitignore", ".awa/.gitignore guard is missing; .awa is not protected from git", true))
	default: // projfs.StateGitignoreIneffective
		rep.add(finding(doctor.CodeStateGitignoreIneffective, doctor.SeverityWarning, doctor.SubsystemGit,
			layout.StateGitignore(), ".awa/.gitignore", ".awa/.gitignore guard does not ignore .awa contents", true))
	}
}
