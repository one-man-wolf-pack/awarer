// Package scanner orchestrates a worktree scan.
//
// It is the composition root for scanning: it receives the filesystem walker,
// the content hasher, and (optionally) the worktree index as ports, drives the
// walk, applies trust-mode reuse and large-file policy, builds the deterministic
// tree, and persists the scan transactionally. It owns no filesystem, hashing,
// or SQL details — those live behind the injected ports — so the policy here
// stays testable and the infrastructure stays swappable.
package scanner

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/manifestsort"
	"awarer/internal/infra/projfs"
)

// Service runs scans against a project. Construct it with New for a mutable index
// or NewReadOnly for read-only acceleration; the split between the read and write
// facets is what keeps a read-only scanner free of any write capability.
type Service struct {
	walker worktree.Walker
	hasher hashing.Hasher
	reader worktree.IndexLookup // reuse lookups; nil => always re-hash
	writer worktree.Index       // scan persist; nil => never persist
}

// New builds a scanner over a mutable worktree index. walker and hasher are
// required; index is optional acceleration state that also receives the persisted
// scan. A nil walker or hasher is a wiring error, so New fails loudly at
// construction rather than deferring a nil dereference to the first scan. A nil
// index yields a scanner that always re-hashes and never persists.
func New(walker worktree.Walker, hasher hashing.Hasher, index worktree.Index) *Service {
	s := newService(walker, hasher)
	// Both facets are the same index. A nil index leaves them nil interfaces, so the
	// reuse guard degrades to always-rehash and the persist guard to never-persist.
	s.reader = index
	s.writer = index
	return s
}

// NewReadOnly builds a scanner that consults a read-only index to accelerate
// hashing but never persists a scan. It depends on the narrow lookup facet, so its
// callers (and their test fakes) carry no write or handle-lifecycle method. A nil
// reader yields a scanner that always re-hashes.
func NewReadOnly(walker worktree.Walker, hasher hashing.Hasher, reader worktree.IndexLookup) *Service {
	s := newService(walker, hasher)
	// A nil reader leaves the reuse facet nil, so lookups degrade to always-rehash;
	// the writer facet stays nil, so a read-only scanner can never persist.
	s.reader = reader
	return s
}

// newService validates the required ports shared by both constructors.
func newService(walker worktree.Walker, hasher hashing.Hasher) *Service {
	if walker == nil {
		panic("scanner: walker must not be nil")
	}
	if hasher == nil {
		panic("scanner: hasher must not be nil")
	}
	return &Service{walker: walker, hasher: hasher}
}

// Result is a completed scan. Its manifest, tree hash, stats, taint, and record
// count are backed by a bounded external sort (manifestsort.Sorted): the records are
// held in memory only while they fit under the sorter's buffer and spill to temp
// files past it, so a scan of millions of paths never materializes a full entry
// slice. When the scan spilled, those temp files live until Close, so every caller
// that holds a Result must Close it when done. sources carries the verified content
// openers for blob-intent regular entries, but only for scans that asked for content
// (checkpoint, diff); it is nil for identity-only scans (run, status, changes), so
// those never retain an opener per file.
type Result struct {
	sorted  manifestsort.Sorted
	meta    worktree.ScanMetadata
	samples []worktree.SkippedSample
	sources map[string]func() (io.ReadCloser, error)
}

// Close releases any temporary files backing the scan's manifest. It is a no-op for
// a scan that fit in memory and is safe to call more than once.
func (r Result) Close() error { return r.sorted.Close() }

// Source returns the verified content opener the walk built for a regular entry,
// or false if the path has none (an identity-only scan, or any non-blob-intent or
// non-regular entry).
func (r Result) Source(p worktree.RelPath) (func() (io.ReadCloser, error), bool) {
	if r.sources == nil {
		return nil, false
	}
	src, ok := r.sources[p.String()]
	return src, ok
}

// Manifest returns a re-openable ManifestStream over the scan's records in canonical
// order, so a consumer (the state resolver, comparison) reads the current-workspace
// state stream-first rather than materializing the entry and skipped slices.
func (r Result) Manifest() worktree.ManifestStream { return r.sorted.Stream() }

// TreeHash returns the scanned tree's hash, derived from the same ordered record
// sequence that backs the manifest.
func (r Result) TreeHash() hashing.TreeHash { return r.sorted.Reduction().Hash }

// Stats returns the per-kind counts and total size folded during the sort.
func (r Result) Stats() worktree.ReducedStats { return r.sorted.Reduction().Stats }

// Incomplete reports whether the scan tolerated an unreadable input (a tainting
// skip), so a tainted result is not treated as a complete picture of the worktree.
func (r Result) Incomplete() bool { return r.sorted.Reduction().Tainted }

// Meta returns the scan's metadata.
func (r Result) Meta() worktree.ScanMetadata { return r.meta }

// SkippedSummary returns a bounded summary of the scan's skipped inputs — count,
// taint, and up to maxSamples samples — without holding every SkippedInput. Count and
// taint come from the sort's reduction (which saw every record); the samples are a
// bounded, canonically-ordered head collected during the scan.
func (r Result) SkippedSummary(maxSamples int) worktree.SkippedFacts {
	red := r.sorted.Reduction()
	facts := worktree.SkippedFacts{Count: red.Stats.Skipped, Tainted: red.Tainted}
	n := len(r.samples)
	if maxSamples >= 0 && n > maxSamples {
		n = maxSamples
	}
	if n > 0 {
		facts.Samples = append([]worktree.SkippedSample(nil), r.samples[:n]...)
	}
	return facts
}

// Options tunes a single scan.
type Options struct {
	// AllowSkippedInputs, when true, downgrades an unreadable regular file to a
	// recorded skipped input and taints the result (Incomplete) instead of
	// failing the scan. The default is fail-fast: an unreadable input aborts the
	// scan loudly, because a checkpoint that silently omits files is not a
	// trustworthy picture of the worktree.
	AllowSkippedInputs bool
	// ReadOnly, when true, skips persisting the scan to the worktree index: the scan
	// still consults the index to accelerate hashing, but writes nothing back. It is
	// for callers that only need the tree identity and must not mutate shared state —
	// notably "run explain", which reasons about cacheability without holding a
	// presence lock, so it must never race a collector rebuilding the index.
	ReadOnly bool
	// NeedContentSources, when true, retains a verified content opener per blob-intent
	// regular entry so a later step can materialize or diff file content
	// (checkpoint, diff). It defaults false: identity-only scans (run, status,
	// changes) never read content, so they must not accumulate an opener per file.
	NeedContentSources bool
	// FailOnObservationChange makes a regular input that changed between the walk's stat
	// and its read abort the scan with worktree.ErrObservationChanged instead of being
	// tolerated as a skipped input. It is for a strict point-in-time "now" observation
	// (the external state provider): a moved input means the snapshot is not a consistent
	// observation of the scope, so it is reported unavailable rather than silently
	// partial. Off by default, so checkpoint/changes/diff keep tolerating a racing input
	// as a skip.
	FailOnObservationChange bool
	// ForceRehash, when true, makes the scan read and hash every input instead of
	// standing an indexed hash in for one, whatever the configured trust mode says.
	// It exists because no trust mode below strict can discriminate a rewrite that
	// keeps the file's size and lands inside the filesystem's timestamp granularity:
	// the stat signature is then identical to the indexed one, so the stale hash is
	// reused and the change is invisible. Nor do normal mode's extra fields help —
	// ctime shares mtime's granularity, and dev, ino and nlink do not move at all when
	// a file is rewritten in place.
	//
	// Whether that matters is a question about consequences, not about likelihood. A
	// scan whose answer only accelerates later work can absorb it: the next scan
	// corrects the record, and nothing was decided on the strength of the wrong hash.
	// A scan whose answer decides that work can be skipped entirely cannot: there is no
	// later correction, because the skipped run never happens. Every run-cache decision
	// is of the second kind, so all of them set this — the run's own input scan, the
	// post-run mutation observation, and the projections that tell a user which stored
	// runs would replay.
	//
	// It is deliberately separate from Hashing.TrustMode, which is keyed material: a
	// caller that needs this guarantee for one scan must not have to alter the
	// configuration identity that makes two scans comparable.
	ForceRehash bool
	// BufferRecords overrides the sorter's in-memory record cap before it spills to
	// disk. Zero uses manifestsort's default. Tests and benchmarks set a small value
	// to exercise the spill path on a modest fixture.
	BufferRecords int
	// Now and Rand are injected for deterministic tests. Zero values fall back to
	// the wall clock and crypto/rand.
	Now  time.Time
	Rand io.Reader
}

// Scan walks the project, hashes its in-scope content under the effective config
// and trust mode, and returns a deterministic Result. When an index is
// configured, the scan is recorded transactionally: an incomplete scan never
// leaves a completed row with partial entries.
func (s *Service) Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts Options) (Result, error) {
	layout, err := project.Paths()
	if err != nil {
		return Result{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	// The scan boundary is validated separately: a run scope assembled from CLI
	// overrides is not part of cfg and would otherwise reach the walker unchecked.
	if err := scope.Validate(); err != nil {
		return Result{}, err
	}
	startedAt := resolveNow(opts)
	randSrc := opts.Rand
	if randSrc == nil {
		randSrc = rand.Reader
	}
	scanID, err := worktree.NewScanID(startedAt.UnixNano(), randSrc)
	if err != nil {
		return Result{}, err
	}

	proc := &processor{
		ctx:     ctx,
		hasher:  s.hasher,
		index:   s.reader,
		cfg:     cfg,
		opts:    opts,
		maxSize: cfg.Hashing.MaxFileSize.Bytes(),
		sorter:  manifestsort.New(opts.BufferRecords, layout.TmpDir()),
	}
	if opts.NeedContentSources {
		proc.sources = map[string]func() (io.ReadCloser, error){}
	}

	// Pass 1: walk, feeding each record into the bounded external sorter (which
	// spills past its buffer), consulting the index for reuse against prior committed
	// state (outside any write transaction). A walk failure abandons any spilled runs.
	if err := s.walker.Walk(ctx, layout, scope, proc.visit); err != nil {
		_ = proc.sorter.Close()
		return Result{}, err
	}

	// Pass 2: finish the sort. This merges any spilled runs in canonical order and
	// folds the tree hash, stats, taint, and count from the same ordered sequence.
	sorted, err := proc.sorter.Finish(s.hasher)
	if err != nil {
		return Result{}, err
	}
	// A finished scan must carry a real tree hash: the reduction is the only thing
	// that produces one, so a zero here means the fold never ran and the Result would
	// claim an identity it does not have.
	if sorted.Reduction().Hash.IsZero() {
		_ = sorted.Close()
		return Result{}, fmt.Errorf("scan result has no tree hash")
	}

	// The scan completes once the sort is done; capture a single completion timestamp
	// so the in-memory result and the persisted row agree.
	meta := worktree.ScanMetadata{
		ScanID:                scanID,
		Root:                  layout.Root(),
		ConfigHash:            configHash(cfg, scope, s.hasher),
		TrustMode:             cfg.Hashing.TrustMode,
		StartedAt:             startedAt,
		CompletedAt:           resolveNow(opts),
		OmittedStatFields:     proc.omitted,
		FastModeWeakSignature: cfg.Hashing.TrustMode == config.TrustFast,
	}
	if err := meta.ValidateCompleted(); err != nil {
		_ = sorted.Close()
		return Result{}, err
	}

	// Pass 3: persist transactionally, if an index is configured and the caller did
	// not request a read-only scan. A read-only scan reads the index but never writes
	// it, so it needs no presence lock and cannot race a concurrent index rebuild. The
	// index rows are driven from the ordered manifest stream, not a retained slice.
	if s.writer != nil && !opts.ReadOnly {
		if err := s.persist(ctx, meta, sorted); err != nil {
			_ = sorted.Close()
			return Result{}, err
		}
	}

	// proc.samples is already the canonically-smallest skipped inputs in sorted order
	// (kept bounded during the walk), so SkippedSummary presents a deterministic
	// canonical head.
	return Result{sorted: sorted, meta: meta, samples: proc.samples, sources: proc.sources}, nil
}

// persist records the scan's entries and skipped inputs in a single transaction,
// reading them from the ordered manifest stream and marking the scan complete only
// after every record is written.
func (s *Service) persist(ctx context.Context, meta worktree.ScanMetadata, sorted manifestsort.Sorted) (err error) {
	tx, err := s.writer.BeginScan(ctx, meta)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	cur, err := sorted.Stream().Open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		rec := cur.Record()
		if e, ok := rec.Entry(); ok {
			if err = tx.Upsert(ctx, e); err != nil {
				return err
			}
		} else if sk, ok := rec.Skipped(); ok {
			if err = tx.RecordSkipped(ctx, sk); err != nil {
				return err
			}
		}
	}
	if err = cur.Err(); err != nil {
		return err
	}
	return tx.Commit(ctx, sorted.Reduction().Hash, meta.CompletedAt)
}

// resolveNow returns the injected clock when set, falling back to the wall clock.
// Using it for both StartedAt and CompletedAt makes scans deterministic under a
// fixed injected clock while still measuring real elapsed time in production.
func resolveNow(opts Options) time.Time {
	if opts.Now.IsZero() {
		return time.Now()
	}
	return opts.Now
}
