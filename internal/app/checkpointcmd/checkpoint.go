// Package checkpointcmd is the application service behind "awa checkpoint".
//
// It orchestrates the scanner, the blob store, the checkpoint repository, and the
// git metadata provider into one use case: scan the project, materialize the blobs
// the checkpoint policy requires, capture git context, and persist an immutable
// checkpoint atomically. It owns the policy decisions — notably applying
// store_file_contents — but no filesystem, hashing, or JSON details, which live
// behind the injected ports.
package checkpointcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/projfs"
)

// idAttempts bounds checkpoint-id regeneration on the (astronomically unlikely)
// event of a collision. A real collision would point at a broken randomness
// source, so a small bound that then fails loudly is correct.
const idAttempts = 3

// GitProvider is the slice of git metadata capture the service needs. It is a
// local port so the service depends only on this capability; gitmeta.Provider
// satisfies it.
type GitProvider interface {
	Capture(ctx context.Context) (*checkpoint.GitMetadata, bool, error)
}

// IndexInvalidator drops a reused-but-wrong index entry. It is the only slice of
// the worktree index the checkpoint recovery path needs, defined here rather
// than on worktree.Index so the scanner's port stays minimal. The sqliteindex
// adapter satisfies it.
type IndexInvalidator interface {
	Invalidate(ctx context.Context, path worktree.RelPath) error
}

// DirectContent opens the current bytes of one ordinary — directly reached, not
// symlink-followed — worktree file from the identity the scan recorded for it. It is
// defined here, by the one use case that needs the capability, rather than in the
// domain or a shared catalogue: the scanner does not need it, and nothing else does.
//
// Its contract is that reopening from a recorded RelPath and StatSignature is not a
// weaker read: a path whose shape or node changed since the observation is refused
// rather than read. A followed entry is never its business; contentSource owns why.
type DirectContent interface {
	Open(path worktree.RelPath, observed worktree.StatSignature) (io.ReadCloser, error)
}

// LockAcquirer takes a presence lock for the duration of the checkpoint write so
// a concurrent destructive gc treats the in-flight blobs and checkpoint as protected
// state. Acquire returns a release function the caller defers; it never blocks on
// other presence locks (concurrent checkpoints publish distinct checkpoints safely).
// The lockfile-backed adapter wired at the composition root satisfies it.
type LockAcquirer interface {
	Acquire() (release func() error, err error)
}

// Deps are the checkpoint service's dependencies.
type Deps struct {
	Scanner     *scanner.Service
	Hasher      hashing.Hasher
	Blobs       blob.Store
	Checkpoints checkpoint.Repository
	Git         GitProvider
	// Content opens an ordinary blob-intent file's bytes when materialization needs
	// them. It is required rather than optional: the policy that decides whether
	// contents are stored arrives with each Request, not with these deps, so a service
	// built without this reader is mis-wired, not configured for a narrower mode.
	Content DirectContent
	// Index is the same worktree index the scanner reuses hashes from, held here so
	// a reused hash that materialization proves wrong can be invalidated. It is
	// optional: a nil index simply means no invalidation (and, in the scanner, no
	// reuse).
	Index IndexInvalidator
	// Locks acquires the writer presence lock around a checkpoint. It is optional:
	// a nil acquirer skips locking (used in tests and any caller that manages
	// exclusion itself).
	Locks LockAcquirer
	// Now and Rand are injected: the composition root supplies time.Now and
	// crypto/rand, tests supply deterministic sources. Both are required — New
	// panics on a nil dependency rather than silently substituting one.
	Now     func() time.Time
	Rand    io.Reader
	Version string
}

// Service records checkpoints. Construct it with New.
type Service struct {
	deps Deps
}

// New builds the service, failing loudly on a missing required port rather than
// deferring a nil dereference to the first checkpoint.
func New(d Deps) *Service {
	switch {
	case d.Scanner == nil:
		panic("checkpointcmd.New: scanner must not be nil")
	case d.Hasher == nil:
		panic("checkpointcmd.New: hasher must not be nil")
	case d.Blobs == nil:
		panic("checkpointcmd.New: blob store must not be nil")
	case d.Checkpoints == nil:
		panic("checkpointcmd.New: checkpoint repository must not be nil")
	case d.Git == nil:
		panic("checkpointcmd.New: git provider must not be nil")
	case d.Content == nil:
		panic("checkpointcmd.New: direct content reader must not be nil")
	case d.Now == nil:
		panic("checkpointcmd.New: clock must not be nil")
	case d.Rand == nil:
		panic("checkpointcmd.New: randomness source must not be nil")
	}
	return &Service{deps: d}
}

// Request is one checkpoint invocation.
type Request struct {
	Project    projfs.Project
	Config     config.Config // already-effective config (trust override applied)
	CommandCwd string        // absolute cwd of the invocation
	Message    checkpoint.CheckpointMessage
}

// Result reports what a checkpoint produced. It carries the checkpoint's header,
// not the full manifest: the checkpoint streams its records to the repository and
// never materializes them as one aggregate, so a caller that needs the manifest
// reads it back through the repository.
type Result struct {
	Header       checkpoint.CheckpointHeader
	BlobsWritten int
	BlobsReused  int
	// GitWarning is set when git context could not be captured for a genuine
	// reason (not "no repo" / "no git"). The checkpoint still succeeds; the CLI
	// surfaces this as a diagnostic so a real git problem is not silently hidden.
	GitWarning string
}

// Run executes the checkpoint use case.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	layout, err := req.Project.Paths()
	if err != nil {
		return Result{}, err
	}

	// Hold a presence lock for the whole write window (scan, blob materialization,
	// checkpoint publish) so a concurrent destructive gc retains the in-flight state
	// rather than sweeping it. Presence locks never block each other, so two
	// checkpoints still proceed in parallel.
	if s.deps.Locks != nil {
		release, err := s.deps.Locks.Acquire()
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = release() }()
	}

	// The scan retains only the content openers this checkpoint cannot rebuild (see
	// contentSource). Its manifest is bounded (it spills past the sorter buffer), and
	// Close releases those spill files once the checkpoint is persisted; the retained
	// openers are ordinary process-local values that go with the Result itself.
	scanResult, err := s.deps.Scanner.Scan(ctx, req.Project, req.Config, req.Config.HistoryScanScope(), scanner.Options{Now: s.deps.Now(), Sources: sourceRetention(req.Config)})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = scanResult.Close() }()

	git, gitWarning := s.captureGit(ctx)

	meta := scanResult.Meta()
	policyHash := checkpointPolicyHash(req.Config, s.deps.Hasher)
	cwdRel := relCwd(layout.Root(), req.CommandCwd)
	// The tree hash, stats, and record count are derived by the repository from the
	// streamed manifest, not supplied here: the tree hash excludes content-storage
	// class, so the store_file_contents downgrade does not change it (the storage
	// choice is captured separately in the checkpoint policy hash).
	buildMeta := func(id checkpoint.CheckpointID) checkpoint.CheckpointBuild {
		return checkpoint.CheckpointBuild{
			ID:                    id,
			CreatedAt:             s.deps.Now().UTC(),
			Root:                  layout.Root(),
			CommandCwd:            cwdRel,
			AwaVersion:            s.deps.Version,
			Message:               req.Message,
			ScanConfigHash:        meta.ConfigHash,
			CheckpointPolicyHash:  policyHash,
			TrustMode:             meta.TrustMode,
			FastModeWeakSignature: meta.FastModeWeakSignature,
			OmittedStatFields:     meta.OmittedStatFields,
			Git:                   git,
		}
	}

	// The final manifest is produced as a stream: the scan's records with the
	// store_file_contents policy applied and blobs materialized as each blob-intent
	// regular entry passes by. The repository consumes it and derives the durable
	// fields — the use case never holds the whole manifest as a slice.
	source := &manifestSource{svc: s, cfg: req.Config, scan: scanResult}
	header, err := s.persist(ctx, buildMeta, source)
	if err != nil {
		return Result{}, err
	}

	return Result{Header: header, BlobsWritten: source.written, BlobsReused: source.reused, GitWarning: gitWarning}, nil
}

// sourceRetention maps the effective checkpoint policy to what the scan must keep:
// nothing when no content is ever read after the scan, and followed sources only when
// it is — see contentSource for why that is the whole set.
//
// It is application policy read off an already-effective configuration. It does not
// enter the scan's identity and adds no configuration surface: the same worktree under
// the same scan config produces the same tree hash either way.
func sourceRetention(cfg config.Config) scanner.SourceRetention {
	if cfg.Checkpoint.StoreFileContents {
		return scanner.RetainFollowedSources
	}
	return scanner.RetainNoSources
}

// manifestSource produces the final checkpoint manifest as a re-openable stream:
// the scan's records with the store_file_contents policy applied and blobs
// materialized as each blob-intent regular entry passes by. The repository
// consumes it and derives the durable fields, so the use case never holds the
// whole manifest as a slice. Each Open re-runs materialization (idempotent —
// blobs are content-addressed) and resets the blob counts, so a PutManifest retry
// on an id collision recomputes them correctly.
type manifestSource struct {
	svc  *Service
	cfg  config.Config
	scan scanner.Result

	written int
	reused  int
}

func (m *manifestSource) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	m.written, m.reused = 0, 0
	inner, err := m.scan.Manifest().Open(ctx)
	if err != nil {
		return nil, err
	}
	return &materializingCursor{src: m, ctx: ctx, inner: inner}, nil
}

// materializeEntry applies the store_file_contents policy to one entry and writes
// the blob it requires. A blob-intent regular entry becomes a stored blob when
// contents are stored, or a hash-only entry (no blob) when they are not; every
// other storage intent passes through unchanged.
//
// It owns what happens when a write fails: a post-scan substitution and a hash mismatch
// both abort loudly, and a mismatch also drops the stale index mapping, so no checkpoint
// references bytes that were not actually stored. It does not decide how the bytes are
// read — contentSource does — nor whether they are read at all, which is the store's
// call.
func (m *manifestSource) materializeEntry(ctx context.Context, e worktree.Entry) (worktree.Entry, error) {
	if e.Kind != worktree.KindRegular || e.Storage != worktree.StorageBlob {
		return e, nil
	}
	if !m.cfg.Checkpoint.StoreFileContents {
		// Honor [checkpoint] store_file_contents=false: keep the content hash in the
		// manifest but store no blob. This changes only the persisted storage class,
		// not the tree hash.
		return worktree.NewRegularEntry(e.Path, e.Content, worktree.StorageHashOnly, e.Stat, e.Traversal)
	}
	open, err := m.contentSource(e)
	if err != nil {
		return worktree.Entry{}, err
	}
	_, wrote, merr := m.svc.deps.Blobs.Materialize(e.Content, open)
	if merr != nil {
		// A corrupt stored blob means the source and its index mapping are fine — the
		// store is not. Report it as-is (the CLI maps it to the state-action-required code) and keep
		// the index entry, which a re-scan would reproduce.
		if errors.Is(merr, blob.ErrCorruptBlob) {
			return worktree.Entry{}, fmt.Errorf("storing blob for %q: %w", e.Path, merr)
		}
		// Any other materialization failure — the source changed (hash mismatch),
		// vanished, or could not be read — means the scan's committed stat->hash mapping
		// for this path can no longer be trusted. Drop it from the acceleration index so
		// the next checkpoint re-hashes this path instead of reusing stale evidence
		// (notably in fast mode), rather than leaving a mapping that proved
		// unmaterializable.
		abort := fmt.Errorf("storing blob for %q: %w", e.Path, merr)
		if errors.Is(merr, blob.ErrHashMismatch) {
			abort = fmt.Errorf("file %q changed since the scan; aborting checkpoint: %w", e.Path, merr)
		}
		if m.svc.deps.Index != nil {
			if ivErr := m.svc.deps.Index.Invalidate(ctx, e.Path); ivErr != nil {
				// The recovery action itself failed; surface it alongside the abort so the
				// user knows the stale entry persists, rather than hiding a possible index
				// problem behind the original failure.
				return worktree.Entry{}, errors.Join(abort, fmt.Errorf("could not invalidate stale index entry for %q: %w", e.Path, ivErr))
			}
		}
		return worktree.Entry{}, abort
	}
	if wrote {
		m.written++
	} else {
		m.reused++
	}
	return e, nil
}

// contentSource is where this package's source-ownership rule lives; the surrounding
// comments state their local contracts and defer here.
//
// One fact decides everything: how the walk reached the file. It returns a capability,
// not a read — the content-addressed store calls the opener only when the blob is
// absent, so for an already-stored blob none of this runs.
//
// A followed entry is read only through the opener the walk retained for it, which
// replays every raw symlink target the traversal observed and re-resolves to the
// terminal it validated. Its record carries none of that, so nothing here could
// reconstruct it. There is deliberately no fallback to the direct reader: a repointed
// link whose new target holds the same bytes at the same virtual path would sail through
// one, which is exactly the substitution the retained opener exists to catch. A missing
// opener is an internal wiring fault, caught here before the store is asked to do
// anything, so a mis-wired retention mode fails loudly instead of passing quietly on
// every checkpoint whose blobs already exist.
//
// An ordinary direct entry closes over nothing its record does not already carry, so it
// is reopened through the Content port from exactly that path and stat and reaches the
// same primitive the walk's own closure ended in. It asks the scan for nothing, which is
// why the scan retains nothing for it.
func (m *manifestSource) contentSource(e worktree.Entry) (func() (io.ReadCloser, error), error) {
	if e.Traversal.Followed {
		open, ok := m.scan.Source(e.Path)
		if !ok {
			return nil, fmt.Errorf("no retained traversal-verified content source for followed entry %q; refusing to materialize it through a direct re-open", e.Path)
		}
		return open, nil
	}
	path, observed := e.Path, e.Stat
	return func() (io.ReadCloser, error) { return m.svc.deps.Content.Open(path, observed) }, nil
}

// materializingCursor wraps the scan's manifest cursor, applying the checkpoint
// policy and materializing blobs per record. Skipped inputs pass through unchanged;
// entries are transformed (and their blobs written) as they are pulled. A
// materialization failure becomes the cursor's sticky terminal error, aborting the
// write.
type materializingCursor struct {
	src   *manifestSource
	ctx   context.Context
	inner worktree.ManifestCursor
	cur   worktree.ManifestRecord
	err   error
	done  bool
}

func (c *materializingCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.inner.Next() {
		c.done = true
		c.err = c.inner.Err()
		return false
	}
	rec := c.inner.Record()
	if rec.IsSkipped() {
		c.cur = rec
		return true
	}
	e, _ := rec.Entry()
	out, err := c.src.materializeEntry(c.ctx, e)
	if err != nil {
		c.err = err
		c.done = true
		return false
	}
	c.cur = worktree.EntryRecord(out)
	return true
}

func (c *materializingCursor) Record() worktree.ManifestRecord { return c.cur }
func (c *materializingCursor) Err() error                      { return c.err }
func (c *materializingCursor) Close() error                    { return c.inner.Close() }

// persist generates an id and streams the manifest to the repository, regenerating
// on the unlikely event of an id collision rather than overwriting an existing
// record.
func (s *Service) persist(ctx context.Context, buildMeta func(checkpoint.CheckpointID) checkpoint.CheckpointBuild, manifest worktree.ManifestStream) (checkpoint.CheckpointHeader, error) {
	var lastErr error
	for i := 0; i < idAttempts; i++ {
		id, err := checkpoint.NewCheckpointID(s.deps.Rand)
		if err != nil {
			return checkpoint.CheckpointHeader{}, err
		}
		header, err := s.deps.Checkpoints.PutManifest(ctx, buildMeta(id), manifest)
		if err != nil {
			if errors.Is(err, checkpoint.ErrIDCollision) {
				lastErr = err
				continue
			}
			return checkpoint.CheckpointHeader{}, err
		}
		return header, nil
	}
	return checkpoint.CheckpointHeader{}, fmt.Errorf("could not allocate a unique checkpoint id after %d attempts: %w", idAttempts, lastErr)
}

// captureGit returns git metadata (nil when absent) and a warning string when a
// genuine error prevented capture. A genuine git error never fails the
// checkpoint — git context is best-effort — but it is surfaced rather than
// silently dropped.
func (s *Service) captureGit(ctx context.Context) (*checkpoint.GitMetadata, string) {
	md, ok, err := s.deps.Git.Capture(ctx)
	if err != nil {
		return nil, fmt.Sprintf("git metadata unavailable: %v", err)
	}
	if !ok {
		return nil, ""
	}
	return md, ""
}

// relCwd renders the invocation cwd relative to the project root, "." at the
// root. A cwd that cannot be made relative, or that escapes the root (an unusual
// --root pointing away from the cwd), falls back to "." rather than embedding an
// absolute or escaping path: CommandCwd is documented as project-local, so a
// "../" path would mislead any later replay or state-reference logic.
func relCwd(root, cwd string) string {
	if cwd == "" {
		return "."
	}
	rel, err := filepath.Rel(root, cwd)
	if err != nil {
		return "."
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "."
	}
	return rel
}
