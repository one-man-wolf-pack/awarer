package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/projfs"
)

// Resolution errors. They are sentinels so the CLI can map each to the right
// exit code (not-found vs usage) without matching strings.
var (
	// ErrNoCheckpoints reports that a reference needs a checkpoint but none exist.
	ErrNoCheckpoints = errors.New("no checkpoints exist")
	// ErrLatestUnreadable reports that a position-based reference (latest, @-N) cannot
	// be resolved because at least one checkpoint is unreadable (an incompatible
	// format or corrupt). Ordering is by recorded creation time, which lives in the
	// header, so an unreadable record cannot be placed relative to the readable ones —
	// resolving "latest" could otherwise silently select an older checkpoint or report
	// none. It is distinct from ErrNoCheckpoints so the CLI gives a recovery-oriented
	// diagnostic rather than "record one first".
	ErrLatestUnreadable = errors.New("latest checkpoint cannot be resolved: the checkpoint store is partially unreadable")
	// ErrLatestIncompatible and ErrLatestCorrupt refine ErrLatestUnreadable with the reason
	// the store cannot be ordered, classified from the same single store-health read pass
	// (never a second racy read). Both wrap ErrLatestUnreadable, so any caller that only
	// distinguishes "the store is partially unreadable" (e.g. the changes/diff exit-code
	// mapping) keeps working unchanged, while the external-state provider can map incompatible
	// vs corrupt to distinct machine reasons. Incompatible is reported only when every
	// unreadable record declares a schema this build cannot read; a single corrupt
	// record wins (fail-closed), because a corrupt store must never read as merely
	// incompatible.
	ErrLatestIncompatible = fmt.Errorf("%w: every unreadable checkpoint declares an incompatible schema", ErrLatestUnreadable)
	ErrLatestCorrupt      = fmt.Errorf("%w: at least one unreadable checkpoint is corrupt", ErrLatestUnreadable)
	// ErrOutOfRange reports that @-N names a position past the oldest checkpoint.
	ErrOutOfRange = errors.New("checkpoint index out of range")
	// ErrContentChanged reports that a current-workspace file changed between the
	// scan and a diff content read, so its bytes can no longer be trusted.
	ErrContentChanged = errors.New("file changed during read")
	// ErrObservationUnstable reports that a strict current-worktree ("now") observation
	// could not be taken as a consistent point-in-time snapshot because an in-scope input
	// changed while it was being observed. It is distinct from ErrContentChanged (a
	// post-scan diff read of one file): this aborts the observation itself. It arises only
	// when NowContext.RequireStableObservation is set; the external state provider maps it
	// to the observation-unstable outcome.
	ErrObservationUnstable = errors.New("current-worktree observation is unstable")
	// ErrRunObservationNoContent reports that a run observation stores only a manifest
	// (paths, hashes, modes), not file content, so a content diff against it is not
	// available. changes works manifest-to-manifest; diff degrades to hash-only.
	ErrRunObservationNoContent = errors.New("run observation stores no content")
)

// Deps are the ports the resolver needs. Checkpoints loads stored state; Scanner
// produces the ephemeral "now" state; Blobs and Hasher back content reads for
// diffs, and Hasher also re-derives a streamed checkpoint manifest's tree hash so
// it can be checked against the header.
type Deps struct {
	Checkpoints checkpoint.Repository
	Scanner     *scanner.Service
	Blobs       blob.Store
	Hasher      hashing.Hasher
	// Runs resolves recorded-run observation references (run:<id>:before/after). It
	// is optional: a resolver wired without it rejects run references rather than
	// silently failing. The run store satisfies it.
	Runs RunObservations
	// Restores resolves applied-restore recovery-observation references
	// (restore:<id>:before). Like Runs it is optional: a resolver wired without it
	// rejects those references rather than silently failing. The recovery-observation
	// store satisfies it.
	Restores RestoreObservations
}

// RunObservations is the narrow port the resolver uses to open a recorded run's
// pre/post observed-state manifest. The run store satisfies it; keeping it narrow
// (and separate from the cache-lookup Store) means the resolver can reach run
// observations without reaching cache replay.
type RunObservations interface {
	// Resolve turns a run id or id prefix into a run id, failing on an unknown or
	// ambiguous prefix. It enumerates the stored ids, so it takes a context and honors
	// cancellation mid-scan.
	Resolve(ctx context.Context, prefix string) (runcache.RunID, error)
	// Observation opens a verified stream over the run's pre/post observed-state
	// manifest (after selects the post-run one), returned as one typed value carrying
	// the observed tree hash, the scanner's persisted scan-config policy identity, and
	// the run's reuse state. A missing observation yields runcache.ErrObservationUnavailable.
	Observation(id runcache.RunID, after bool) (runcache.RunObservationRead, error)
}

// RestoreObservations is the narrow port the resolver uses to open an applied
// restore's pre-restore recovery observation. The recovery-observation store
// satisfies it directly (its method names are these), so no adapter stands
// between the resolver and the record.
//
// Unlike a run observation, a recovery observation carries content: it was
// published specifically so the state a restore overwrote can be read back and
// restored. That is why a resolved recovery observation reports content available
// while a resolved run observation does not.
type RestoreObservations interface {
	// ResolvePrefix turns a restore operation id or id prefix into an id, failing
	// on an unknown or ambiguous prefix. It enumerates stored ids, so it takes a
	// context and honors cancellation mid-scan.
	ResolvePrefix(ctx context.Context, prefix string) (restore.OperationID, error)
	// Open returns the record plus verified streams over its captured manifest and
	// its covered path set.
	Open(ctx context.Context, id restore.OperationID) (restore.RecoveryRead, error)
}

// Resolver turns parsed references into resolved states.
type Resolver struct {
	deps Deps
}

// NewResolver builds a resolver from its ports.
func NewResolver(deps Deps) *Resolver { return &Resolver{deps: deps} }

// NowContext carries what an ephemeral "now" scan needs: the project and the
// effective config (with any trust-mode override already applied).
type NowContext struct {
	Project projfs.Project
	Config  config.Config
	// NeedContent, when true, makes the ephemeral "now" scan retain verified content
	// openers so a later step can read file content (diff). It is false for content-
	// free comparisons (changes), so a "now" scan for changes never accumulates an
	// opener per file.
	NeedContent bool
	// RequireStableObservation makes an in-scope input that changes during the scan abort
	// the observation as ErrObservationUnstable instead of being tolerated as a skipped
	// input. The external state provider sets it so a "now" resolution is either a
	// consistent point-in-time snapshot or an honest unavailable assessment; changes and
	// diff leave it false and keep tolerating a racing input as a skip.
	RequireStableObservation bool
}

// Kind distinguishes a resolved checkpoint from the current workspace.
type Kind int

const (
	// KindNow is the current workspace state.
	KindNow Kind = iota
	// KindCheckpoint is a stored checkpoint.
	KindCheckpoint
	// KindRunObservation is a recorded run's pre/post observed state. It exposes a
	// manifest but no content (run observations store no blobs).
	KindRunObservation
	// KindRestoreObservation is an applied restore's pre-restore recovery
	// observation. Unlike a run observation it exposes content: it was published
	// precisely so the state a restore overwrote can be read back, which is what
	// makes an applied restore reversible.
	KindRestoreObservation
)

// ResolvedState is a fully loaded state ready for comparison and, where content
// is available, for diffing. It is stream-first: instead of owning the manifest
// slices, it exposes a re-openable manifest stream. For a checkpoint, Header is the
// durable metadata and the stream reads the stored manifest (the source of truth —
// never rescanned). For now, the stream reads an ephemeral scan that published no
// checkpoint.
type ResolvedState struct {
	Kind         Kind
	RequestedRef string
	CanonicalRef string
	Header       *checkpoint.CheckpointHeader
	TreeHash     hashing.TreeHash

	manifest worktree.ManifestStream

	// content access (one of these is set by kind)
	blobs blob.Store     // checkpoint
	now   scanner.Result // now
	hash  hashing.Hasher

	// run-observation identity (set for KindRunObservation), surfaced so the CLI can
	// report that a comparison used a recorded run's observation.
	runID       string
	runSel      RunObservationSel
	runReuse    string
	runReuseWhy string
	// runScanConfig is the persisted scan_config_hash of a run observation. A resolvable
	// run observation always carries it, so its scan policy identity is known and can
	// prove comparison compatibility.
	runScanConfig hashing.ConfigHash

	// recovery identity (set for KindRestoreObservation): the validated record and
	// the stream over the exact path set it proves something about. The covered
	// scope is what bounds this source — it proves nothing outside it — so it
	// always travels with the record rather than being looked up separately.
	recovery      restore.RecoveryObservation
	recoveryScope restore.CoveredScopeStream
}

// Close releases resources held by the resolved state. For a "now" state that is the
// ephemeral scan's Result, whose manifest may be backed by temp spill files; for a
// checkpoint or run observation it is a no-op. It is safe to call on a nil state and
// more than once. Callers close a resolved state once its manifest is no longer read
// (after the comparison drains it).
func (s *ResolvedState) Close() error {
	if s == nil {
		return nil
	}
	return s.now.Close()
}

// ContentAvailable reports whether this state can serve file content for a diff. A
// run observation stores only a manifest (no blobs), so a diff against it degrades
// to hash-only rather than aborting. A recovery observation does carry content —
// that is the whole reason it exists — so it reports available.
func (s *ResolvedState) ContentAvailable() bool { return s.Kind != KindRunObservation }

// RecoveryObservation returns this state's pre-restore recovery record and the
// stream over the exact path set it covers, or ok false when it is not one. The
// covered scope bounds what the record proves: a path inside it but absent from the
// manifest was proved absent, and a path outside it is not evidence at all.
func (s *ResolvedState) RecoveryObservation() (rec restore.RecoveryObservation, scope restore.CoveredScopeStream, ok bool) {
	if s.Kind != KindRestoreObservation {
		return restore.RecoveryObservation{}, nil, false
	}
	return s.recovery, s.recoveryScope, true
}

// RunObservation returns this state's recorded-run observation identity, or ok
// false when it is not a run observation. The reuse label/reason describe the run
// the observation belongs to.
func (s *ResolvedState) RunObservation() (id string, sel RunObservationSel, reuse, reason string, ok bool) {
	if s.Kind != KindRunObservation {
		return "", 0, "", "", false
	}
	return s.runID, s.runSel, s.runReuse, s.runReuseWhy, true
}

// ScanIdentity returns the scan/observation policy identity needed to interpret this
// state's tree hash: the scan-config hash. A checkpoint carries its persisted policy
// (its own scan config hash), a "now" observation carries the scan it just took, and a
// run observation carries the scan_config_hash persisted with it — so every resolvable
// state carries a complete policy identity. The zero scan-config of the unreachable
// default is rejected downstream by NewScanIdentity, not modeled as a separate state.
func (s *ResolvedState) ScanIdentity() hashing.ConfigHash {
	switch s.Kind {
	case KindCheckpoint:
		if s.Header != nil {
			return s.Header.ScanConfigHash
		}
	case KindNow:
		return s.now.Meta().ConfigHash
	case KindRunObservation:
		return s.runScanConfig
	case KindRestoreObservation:
		return s.recovery.ScanConfigHash()
	}
	return hashing.ConfigHash{}
}

// Skipped returns the number of inputs this observation skipped (unreadable or
// policy-excluded at scan time), where a stored count is available: a checkpoint carries
// it in its header stats, and a "now" observation folds it during the scan. A run
// observation reports 0 here — its skipped-input accounting belongs to the run-evidence
// projection, not this read surface.
func (s *ResolvedState) Skipped() int {
	switch s.Kind {
	case KindCheckpoint:
		if s.Header != nil {
			return s.Header.Stats.Skipped
		}
	case KindNow:
		return s.now.Stats().Skipped
	}
	return 0
}

// ObservedAt returns the time this state was observed and whether it is known: a "now"
// observation reports the fresh scan's completion time, and a checkpoint reports its
// recorded creation time. A run observation reports no time here — its observation time
// belongs to the run-evidence projection, not this read surface.
func (s *ResolvedState) ObservedAt() (time.Time, bool) {
	switch s.Kind {
	case KindNow:
		t := s.now.Meta().CompletedAt
		return t, !t.IsZero()
	case KindCheckpoint:
		if s.Header != nil {
			return s.Header.CreatedAt, !s.Header.CreatedAt.IsZero()
		}
	case KindRestoreObservation:
		t := s.recovery.CreatedAt()
		return t, !t.IsZero()
	}
	return time.Time{}, false
}

// CheckpointID returns the resolved checkpoint id, or false for "now".
func (s *ResolvedState) CheckpointID() (checkpoint.CheckpointID, bool) {
	if s.Kind == KindCheckpoint && s.Header != nil {
		return s.Header.ID, true
	}
	return checkpoint.CheckpointID{}, false
}

// Manifest opens a fresh canonical cursor over this state's manifest records. For
// a checkpoint the cursor verifies, on a full drain, that the manifest's tree hash
// matches the header's durable tree hash — so the header's derived field is
// re-checked on read rather than trusted. Comparison consumes one cursor per side;
// content reads (Content) do not re-walk it.
func (s *ResolvedState) Manifest(ctx context.Context) (worktree.ManifestCursor, error) {
	cur, err := s.manifest.Open(ctx)
	if err != nil {
		return nil, err
	}
	// Only a checkpoint carries a durable header tree hash to re-derive and check; the
	// now side is freshly reduced. fromHeader guarantees a checkpoint state has a
	// hasher, so verification is never silently skipped for one.
	if s.Kind != KindCheckpoint || s.Header == nil {
		return cur, nil
	}
	reducer, err := worktree.NewTreeReducer(s.hash)
	if err != nil {
		_ = cur.Close()
		return nil, err
	}
	return &verifyingCursor{
		inner:    cur,
		reducer:  reducer,
		expected: s.Header.TreeHash,
		stats:    s.Header.Stats,
		count:    s.Header.RecordCount,
		id:       s.Header.ID,
	}, nil
}

// Content loads the bytes of a regular entry's content for a diff, verifying
// them against expected. For a checkpoint or a restore recovery observation it
// reads the blob (a missing blob is storage corruption, tagged with that store's
// own sentinel). For now it reads through the scanner's verified opener and
// re-hashes: a mismatch means the file changed since the scan, surfaced as
// ErrContentChanged rather than misleading bytes.
//
// It materializes one file, which is why it backs diff rendering and not restore:
// restore streams a blob into staging instead, so a large file never has to fit in
// memory to be recovered.
func (s *ResolvedState) Content(path worktree.RelPath, expected hashing.ContentHash) ([]byte, error) {
	switch s.Kind {
	case KindCheckpoint, KindRestoreObservation:
		rc, err := s.blobs.Open(expected)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Name the store that promised the bytes, so a diagnostic points at the
				// record whose evidence is incomplete rather than at checkpoints generally.
				missing := checkpoint.ErrCorruptStore
				if s.Kind == KindRestoreObservation {
					missing = restore.ErrCorruptRecoveryStore
				}
				return nil, fmt.Errorf("%w: blob %s for %s is missing", missing, expected, path)
			}
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, err
		}
		if err := s.verify(data, expected); err != nil {
			return nil, fmt.Errorf("%w: blob %s for %s: %v", blob.ErrCorruptBlob, expected, path, err)
		}
		return data, nil
	case KindNow:
		open, ok := s.now.Source(path)
		if !ok {
			return nil, fmt.Errorf("no content source for %s", path)
		}
		// The current workspace is live: a file can move, shrink, or vanish between
		// the scan and this read. The scanner's verified opener surfaces that as a
		// plain "changed during scan" error rather than a sentinel, so normalize any
		// open/read failure here to ErrContentChanged. That lets diff degrade this
		// one file to "unavailable" instead of aborting the whole comparison.
		rc, err := open()
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrContentChanged, path, err)
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrContentChanged, path, err)
		}
		if err := s.verify(data, expected); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrContentChanged, path)
		}
		return data, nil
	case KindRunObservation:
		// A run observation stores only a manifest, never file content, so a content
		// read is not available. diff guards on ContentAvailable and never reaches here
		// in normal use; this is the loud backstop for any caller that does.
		return nil, fmt.Errorf("%w: %s", ErrRunObservationNoContent, path)
	default:
		return nil, fmt.Errorf("unknown state kind %d", s.Kind)
	}
}

// verify re-hashes data and checks it equals expected.
func (s *ResolvedState) verify(data []byte, expected hashing.ContentHash) error {
	got, err := s.hash.HashReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("content hash %s != expected %s", got, expected)
	}
	return nil
}

// verifyingCursor passes a checkpoint's manifest records through unchanged while
// folding them into a tree reducer; at clean EOF it re-checks every derived field
// the header duplicates — the tree hash, the per-kind stats, and the record count —
// against the header. A mismatch (a tampered manifest, or a header that lies about
// stats while keeping a valid tree hash and count) is surfaced as ErrCorruptStore,
// so a header-only command can never trust a derived field a full read would
// contradict. A partial read (early termination) does not run the check; the
// comparison merge always drains both sides, so changes/diff get it for free.
type verifyingCursor struct {
	inner    worktree.ManifestCursor
	reducer  *worktree.TreeReducer
	expected hashing.TreeHash
	stats    checkpoint.CheckpointStats
	count    int
	id       checkpoint.CheckpointID
	err      error
	done     bool
}

func (c *verifyingCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.inner.Next() {
		c.done = true
		if err := c.inner.Err(); err != nil {
			c.err = err
			return false
		}
		red := c.reducer.Finish()
		switch {
		case red.Hash != c.expected:
			c.err = fmt.Errorf("%w: checkpoint %s manifest tree hash %s does not match header %s", checkpoint.ErrCorruptStore, c.id, red.Hash, c.expected)
		case checkpoint.StatsFromReduced(red.Stats) != c.stats:
			c.err = fmt.Errorf("%w: checkpoint %s manifest stats %+v do not match header %+v", checkpoint.ErrCorruptStore, c.id, checkpoint.StatsFromReduced(red.Stats), c.stats)
		case red.Count != c.count:
			c.err = fmt.Errorf("%w: checkpoint %s manifest holds %d records, header declares %d", checkpoint.ErrCorruptStore, c.id, red.Count, c.count)
		}
		return false
	}
	rec := c.inner.Record()
	if err := c.reducer.Add(rec); err != nil {
		c.err = fmt.Errorf("%w: checkpoint %s: %v", checkpoint.ErrCorruptStore, c.id, err)
		c.done = true
		return false
	}
	return true
}

func (c *verifyingCursor) Record() worktree.ManifestRecord { return c.inner.Record() }
func (c *verifyingCursor) Err() error                      { return c.err }
func (c *verifyingCursor) Close() error                    { return c.inner.Close() }

// ResolveRange resolves both sides of a range. Resolving left then right keeps
// errors attributable to the side the user named.
func (r *Resolver) ResolveRange(ctx context.Context, rng Range, now NowContext) (left, right *ResolvedState, err error) {
	left, err = r.Resolve(ctx, rng.Left, now)
	if err != nil {
		return nil, nil, err
	}
	right, err = r.Resolve(ctx, rng.Right, now)
	if err != nil {
		// left was resolved and may own an ephemeral scan (temp spill files); the caller
		// never receives it, so close it here rather than leak it.
		_ = left.Close()
		return nil, nil, err
	}
	return left, right, nil
}

// Resolve resolves a single reference.
func (r *Resolver) Resolve(ctx context.Context, ref Ref, now NowContext) (*ResolvedState, error) {
	switch ref.Kind {
	case RefNow:
		return r.resolveNow(ctx, now)
	case RefLatest:
		h, err := r.latestReadable(ctx)
		if err != nil {
			return nil, err
		}
		return r.fromHeader(h, ref.Display)
	case RefAtN:
		return r.resolveAtN(ctx, ref)
	case RefCheckpointPrefix:
		return r.resolveCheckpointPrefix(ctx, ref)
	case RefRunObservation:
		return r.resolveRunObservation(ctx, ref)
	case RefRestoreObservation:
		return r.resolveRestoreObservation(ctx, ref)
	default:
		return nil, fmt.Errorf("unknown reference kind %d", ref.Kind)
	}
}

// resolveRunObservation resolves a recorded run's pre/post observed state. It opens
// the run's stored observation manifest (already verified by the run store on
// drain) and exposes it as a manifest-only state — no content, so changes works
// manifest-to-manifest and diff degrades to hash-only. An unknown run, a missing
// observation (a post-scan-failed after), or a corrupt manifest fails loud through
// the run store's errors.
func (r *Resolver) resolveRunObservation(ctx context.Context, ref Ref) (*ResolvedState, error) {
	if r.deps.Runs == nil {
		return nil, fmt.Errorf("state: resolver has no run-observation reader; cannot resolve %s", ref.Display)
	}
	id, err := r.deps.Runs.Resolve(ctx, ref.RunRef)
	if err != nil {
		return nil, err
	}
	read, err := r.deps.Runs.Observation(id, ref.RunSel == RunAfter)
	if err != nil {
		return nil, err
	}
	return &ResolvedState{
		Kind:          KindRunObservation,
		RequestedRef:  ref.Display,
		CanonicalRef:  "run:" + id.Short() + ":" + ref.RunSel.String(),
		TreeHash:      read.TreeHash(),
		manifest:      read.Manifest(),
		hash:          r.deps.Hasher,
		runID:         id.String(),
		runSel:        ref.RunSel,
		runReuse:      read.Reuse().String(),
		runReuseWhy:   read.Reuse().Reason().String(),
		runScanConfig: read.ScanConfigHash(),
	}, nil
}

// resolveRestoreObservation resolves an applied restore's pre-restore recovery
// observation. It opens the record's verified manifest and the stream over the
// exact path set the record covers, and exposes it as a content-bearing state:
// the captured regular entries reference blobs in the shared store, which is what
// lets `awa diff restore:<id>:before..now` show real content and lets a focused
// inverse restore recover the bytes an earlier restore overwrote.
//
// A record that has been reclaimed by retention, or one whose payloads are
// corrupt, fails loud through the store's errors rather than resolving to a
// partially readable state.
func (r *Resolver) resolveRestoreObservation(ctx context.Context, ref Ref) (*ResolvedState, error) {
	if r.deps.Restores == nil {
		return nil, fmt.Errorf("state: resolver has no recovery-observation reader; cannot resolve %s", ref.Display)
	}
	id, err := r.deps.Restores.ResolvePrefix(ctx, ref.RestoreRef)
	if err != nil {
		return nil, err
	}
	read, err := r.deps.Restores.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	record := read.Record()
	return &ResolvedState{
		Kind:          KindRestoreObservation,
		RequestedRef:  ref.Display,
		CanonicalRef:  record.BeforeRef(),
		TreeHash:      record.Manifest().TreeHash,
		manifest:      read.Manifest(),
		blobs:         r.deps.Blobs,
		hash:          r.deps.Hasher,
		recovery:      record,
		recoveryScope: read.Scope(),
	}, nil
}

// readableHealth reads the store's health retaining the newest headers a positional
// reference needs, but returns it only when the whole store reads cleanly. It is the
// one home of the policy that position-based references (latest, @-N) order by
// recorded creation time — a value only a readable header carries — so a single
// unreadable record makes every position ambiguous and the store must be resolved or
// reinitialized first. That case fails with ErrLatestUnreadable rather than letting a
// caller silently count from a store it cannot fully order or claim it is empty.
//
// The bounded window does not weaken that check: the scan behind it still reads every
// committed header, so an unreadable record older than the requested window is still
// counted and still rejects the reference.
func (r *Resolver) readableHealth(ctx context.Context, newest int) (checkpoint.CheckpointStoreHealth, error) {
	health, err := r.deps.Checkpoints.StoreHealthNewest(ctx, newest)
	if err != nil {
		return checkpoint.CheckpointStoreHealth{}, err
	}
	if health.AnyUnreadable() {
		// Classify the reason from the same read pass: a corrupt record makes the store
		// corrupt (fail-closed), otherwise every unreadable record is merely incompatible.
		if health.Corrupt() > 0 {
			return checkpoint.CheckpointStoreHealth{}, ErrLatestCorrupt
		}
		return checkpoint.CheckpointStoreHealth{}, ErrLatestIncompatible
	}
	return health, nil
}

// latestReadable returns the newest checkpoint header, or ErrNoCheckpoints for an
// empty store. It needs one header, so it asks for one, and inherits the
// fully-readable-store requirement from readableHealth.
func (r *Resolver) latestReadable(ctx context.Context) (checkpoint.CheckpointHeader, error) {
	health, err := r.readableHealth(ctx, 1)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	// Emptiness is the store-wide count's answer, not the retained window's: a window
	// that came back short would otherwise report the far more inviting "no checkpoints
	// exist" for a store that has them, sending a user to record one instead of to the
	// broken read.
	if health.Recorded() == 0 {
		return checkpoint.CheckpointHeader{}, ErrNoCheckpoints
	}
	latest, ok := health.Latest()
	if !ok {
		return checkpoint.CheckpointHeader{}, fmt.Errorf("checkpoint store reported %d readable checkpoint(s) but retained no header for latest", health.Recorded())
	}
	return latest, nil
}

// resolveAtN resolves @-N against the newest-first checkpoint headers, asking for
// exactly the N the position needs and inheriting the fully-readable-store
// requirement from readableHealth. The out-of-range check uses the exact readable
// total, not the retained window, so a position past the oldest checkpoint reports the
// store's real size.
func (r *Resolver) resolveAtN(ctx context.Context, ref Ref) (*ResolvedState, error) {
	health, err := r.readableHealth(ctx, ref.N)
	if err != nil {
		return nil, err
	}
	if health.Recorded() == 0 {
		return nil, ErrNoCheckpoints
	}
	if ref.N > health.Recorded() {
		return nil, fmt.Errorf("%w: %s names position %d but only %d checkpoint(s) exist", ErrOutOfRange, ref.Display, ref.N, health.Recorded())
	}
	// The position exists, so the read was asked to retain a window that reaches it.
	// Index that window by its own length rather than by the store-wide count, which
	// describes more records than the window holds: a store that returned a shorter
	// window than requested is a broken read contract and must say so, not resolve the
	// wrong checkpoint or index past the retained headers.
	window := health.NewestHeaders()
	if ref.N > len(window) {
		return nil, fmt.Errorf("checkpoint store returned %d of the %d newest headers needed to resolve %s", len(window), ref.N, ref.Display)
	}
	return r.fromHeader(window[ref.N-1], ref.Display)
}

// resolveCheckpointPrefix resolves a token the parser already proved is a
// syntactically valid id or id prefix. The repository's sentinels are preserved, so
// the two ways a well-formed prefix can fail stay distinguishable to the exit-code
// mapping: no match is not found, and more than one match is ambiguous and fails
// loudly rather than picking one. Only the not-found message is restated, to name
// how the token was read.
func (r *Resolver) resolveCheckpointPrefix(ctx context.Context, ref Ref) (*ResolvedState, error) {
	id, err := r.deps.Checkpoints.ResolvePrefix(ctx, ref.Raw)
	if err != nil {
		// Say how the token was read. changes/diff classify a positional token as a
		// range or a path, so a user who meant a path needs the message to show that
		// this one was taken as a checkpoint id prefix.
		if errors.Is(err, checkpoint.ErrNotFound) {
			return nil, fmt.Errorf("%w: no id starts with %q", checkpoint.ErrNotFound, ref.Raw)
		}
		return nil, err
	}
	h, err := r.deps.Checkpoints.Header(id)
	if err != nil {
		return nil, err
	}
	return r.fromHeader(h, ref.Display)
}

// fromHeader builds a resolved state from a checkpoint header. The manifest is read
// lazily through a stream over the stored records (the source of truth — never
// rescanned), so resolving a checkpoint does not materialize its manifest.
func (r *Resolver) fromHeader(h checkpoint.CheckpointHeader, requested string) (*ResolvedState, error) {
	// A checkpoint's manifest is read stream-first and its tree hash is re-derived and
	// checked against the header on a full read. That check needs a hasher, so a
	// resolver that can reach a checkpoint must be wired with one; missing it is a
	// wiring error surfaced loudly rather than a silently unverified read.
	if r.deps.Hasher == nil {
		return nil, fmt.Errorf("state: resolver has no hasher; cannot verify checkpoint %s manifest", h.ID)
	}
	manifest, err := r.deps.Checkpoints.OpenManifest(h.ID)
	if err != nil {
		return nil, err
	}
	header := h
	return &ResolvedState{
		Kind:         KindCheckpoint,
		RequestedRef: requested,
		CanonicalRef: h.ID.Short(),
		Header:       &header,
		TreeHash:     h.TreeHash,
		manifest:     manifest,
		blobs:        r.deps.Blobs,
		hash:         r.deps.Hasher,
	}, nil
}

// nowSourceRetention maps a "now" context to what its scan must retain. A content
// read against this state is addressed by an arbitrary current path (diff asks for one
// entry at a time, with no publication boundary at which a reopen could be substituted
// for the scan's own opener), so it retains every blob-intent source rather than only
// the followed ones a checkpoint cannot rebuild. A content-free comparison retains
// nothing.
func nowSourceRetention(now NowContext) scanner.SourceRetention {
	if now.NeedContent {
		return scanner.RetainAllSources
	}
	return scanner.RetainNoSources
}

// resolveNow runs an ephemeral, read-only scan of the current workspace. It
// publishes no checkpoint, materializes no blobs, and persists nothing to the
// worktree index: comparison commands compare state, they do not warm durable
// state. Read errors become skipped inputs (so a transient unreadable file does not
// abort the comparison), and the read-only index is acceleration only — an
// unavailable, stale, or corrupt index degrades to re-hashing rather than failing
// the comparison. Only a cancelled context is fail-fast.
func (r *Resolver) resolveNow(ctx context.Context, now NowContext) (*ResolvedState, error) {
	res, err := r.deps.Scanner.Scan(ctx, now.Project, now.Config, now.Config.HistoryScanScope(), scanner.Options{AllowSkippedInputs: true, Sources: nowSourceRetention(now), ReadOnly: true, FailOnObservationChange: now.RequireStableObservation})
	if err != nil {
		// A strict observation that aborted because an in-scope input moved is not a
		// generic scan failure: surface it as the typed unstable-observation error so the
		// provider can report observation-unstable rather than a raw I/O error.
		if errors.Is(err, worktree.ErrObservationChanged) {
			return nil, fmt.Errorf("%w: %v", ErrObservationUnstable, err)
		}
		return nil, err
	}
	return &ResolvedState{
		Kind:         KindNow,
		RequestedRef: "now",
		CanonicalRef: "now",
		TreeHash:     res.TreeHash(),
		manifest:     res.Manifest(),
		now:          res,
		hash:         r.deps.Hasher,
	}, nil
}
