package runcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// ErrStartFailed reports that the wrapped command could not be started (for
// example the executable was not found, or was not executable). It is an awa
// execution failure, not a cached command result, so a Runner returns it instead
// of a RunResult and the run is never cached.
var ErrStartFailed = errors.New("command failed to start")

// RunSpec describes one process execution. Path is the resolved executable when
// known (empty lets the runner resolve Argv[0]); Argv is the exact argument
// vector. Stdout and Stderr are the sinks the runner streams to concurrently, so
// a large output on one stream never blocks the other.
type RunSpec struct {
	Path   string
	Argv   []string
	Dir    string
	Env    []string
	Stdin  StdinMode
	Stdout io.Writer
	Stderr io.Writer
}

// RunResult is the outcome of executing a process: its exit status and the wall
// clock window it ran in.
type RunResult struct {
	Exit       ExitStatus
	StartedAt  time.Time
	FinishedAt time.Time
}

// Runner executes a command and streams its output. A failure to start the
// process is returned as an error satisfying errors.Is(err, ErrStartFailed),
// never a RunResult, because a start failure is an awa execution failure.
type Runner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}

// CaptureLimits bound the bytes stored per output stream.
type CaptureLimits struct {
	MaxStdout int64
	MaxStderr int64
}

// Validate checks the limits are positive. A zero or negative limit would store no
// head before truncating, producing a marker-only payload — an impossible capture
// shape — so the store rejects such limits rather than create one.
func (l CaptureLimits) Validate() error {
	if l.MaxStdout <= 0 {
		return fmt.Errorf("stdout capture limit must be positive, got %d", l.MaxStdout)
	}
	if l.MaxStderr <= 0 {
		return fmt.Errorf("stderr capture limit must be positive, got %d", l.MaxStderr)
	}
	return nil
}

// RunObservations carries the pre/post observed-state manifests for one execution,
// as re-openable streams the store tees into the entry's observation manifest
// files and reduces to fill the entry's Before/After refs. Before is required for a
// real execution; After is nil when the post-run scan failed (its absence is what
// makes the run unknown-post-state).
type RunObservations struct {
	Before worktree.ManifestStream
	After  worktree.ManifestStream
	// BeforeScanConfigHash and AfterScanConfigHash are the scanner-produced scan
	// policy identities for the two observations. They are not derivable from the
	// manifest bytes the streams carry, so the caller supplies them from each scan's
	// Result.Meta().ConfigHash; the store folds them into the entry's Before/After
	// observations. Both come from one immutable effective config, so they are equal
	// whenever an after observation exists.
	BeforeScanConfigHash hashing.ConfigHash
	AfterScanConfigHash  hashing.ConfigHash
}

// RunObservationRead is the constructor-owned result of opening a recorded run's
// pre/post observation for the state-reference resolver. It bundles the verified
// manifest stream with the identity facts the resolver needs — the observed tree hash,
// the scanner's persisted scan_config_hash, and the run's reuse state — so the scan
// policy identity always travels with the tree hash it interprets. NewRunObservationRead
// is the only way in: it rejects a nil stream and a zero tree/scan-config identity, so
// the resolver reads accessors instead of realigning a raw field tuple by hand. Reuse
// is carried verbatim from the already-validated entry.
type RunObservationRead struct {
	manifest       worktree.ManifestStream
	treeHash       hashing.TreeHash
	scanConfigHash hashing.ConfigHash
	reuse          ReuseState
}

// NewRunObservationRead builds a validated observation read. It requires a non-nil
// manifest stream and a complete observation identity: a non-zero tree hash and a
// non-zero scan-config hash. Reuse comes from the run entry the store already
// validated, so it is trusted.
func NewRunObservationRead(manifest worktree.ManifestStream, treeHash hashing.TreeHash, scanConfigHash hashing.ConfigHash, reuse ReuseState) (RunObservationRead, error) {
	if manifest == nil {
		return RunObservationRead{}, fmt.Errorf("run observation read requires a manifest stream")
	}
	if treeHash.IsZero() {
		return RunObservationRead{}, fmt.Errorf("run observation read requires a tree hash")
	}
	if scanConfigHash.IsZero() {
		return RunObservationRead{}, fmt.Errorf("run observation read requires a scan config hash")
	}
	return RunObservationRead{manifest: manifest, treeHash: treeHash, scanConfigHash: scanConfigHash, reuse: reuse}, nil
}

// Manifest returns the verified observation manifest stream.
func (r RunObservationRead) Manifest() worktree.ManifestStream { return r.manifest }

// TreeHash returns the observed tree hash.
func (r RunObservationRead) TreeHash() hashing.TreeHash { return r.treeHash }

// ScanConfigHash returns the observation's persisted scan-config policy identity.
func (r RunObservationRead) ScanConfigHash() hashing.ConfigHash { return r.scanConfigHash }

// Reuse returns the run's reuse state.
func (r RunObservationRead) Reuse() ReuseState { return r.reuse }

// PendingRun is an in-progress capture of a run's output. The caller streams the
// command's stdout and stderr into Stdout() and Stderr(), then either Finalize
// and Commit to publish a complete entry, or Discard to drop the temp payloads.
type PendingRun interface {
	Stdout() io.Writer
	Stderr() io.Writer
	// Finalize stops capture, appends any truncation marker, computes the payload
	// hashes, and returns the resulting capture metadata. Call after the process
	// exits and before Commit. It is idempotent: a repeat call returns the
	// already-computed captures without mutating the payloads or re-hashing.
	Finalize() (stdout, stderr OutputCapture, err error)
	// Commit atomically publishes the recorded run: metadata, both output payloads,
	// the before/after observation manifests, and — only when the entry's reuse state
	// is reusable — the key pointer. The entry's captures must be the ones Finalize
	// returned; the store fills the entry's observation refs from obs and re-validates
	// before publishing. ctx cancels the observation-manifest tee before the atomic
	// publish point; a cancellation there leaves no authoritative state (the staged
	// entry is removed by the cleanup path), exactly like an interrupted write.
	Commit(ctx context.Context, entry RunEntry, obs RunObservations) error
	// Discard removes the temp payloads without publishing anything.
	Discard() error
}

// CacheEntry is a RunEntry the store has proven reusable: it was reached through a
// key pointer and its persisted reuse state is reusable. Only the store constructs
// it (the entry field is unexported), so a caller can never fabricate a cache hit
// from a record-only, mutating, or unknown-post-state run — the lookup path is
// mechanically unable to return one.
type CacheEntry struct {
	entry RunEntry
}

// NewCacheEntryUnchecked wraps an entry the store has already proven reusable. It
// is the store's constructor: the name marks that the caller (the store's Lookup)
// is responsible for the reusable check, so domain code outside the store cannot
// mint a cache hit.
func NewCacheEntryUnchecked(entry RunEntry) CacheEntry { return CacheEntry{entry: entry} }

// Entry returns the underlying reusable run entry.
func (c CacheEntry) Entry() RunEntry { return c.entry }

// Store is the run repository: cache lookup by key, run lookup and listing by id,
// verified output replay, capture sessions for new runs, and deletion. A lookup
// or open that finds a key pointer to a missing or corrupt entry returns
// ErrCorruptStore rather than replaying partial bytes.
type Store interface {
	// Lookup resolves a run key to a reusable cache entry through the key pointer. It
	// can only ever return a reusable entry: a record-only or mutating run has no key
	// pointer, and a pointer found to target a non-reusable run is corruption. A
	// missing pointer is a clean miss (false, nil).
	Lookup(key RunKey) (CacheEntry, bool, error)
	Begin(limits CaptureLimits) (PendingRun, error)
	Get(id RunID) (RunEntry, error)
	// OpenBeforeManifest and OpenAfterManifest return a verified stream over a
	// recorded run's pre/post observed-state manifest. OpenAfterManifest returns
	// ErrObservationUnavailable for a post-scan-failed run, the only run without an
	// after; every recorded run carries a before.
	OpenBeforeManifest(id RunID) (worktree.ManifestStream, error)
	OpenAfterManifest(id RunID) (worktree.ManifestStream, error)
	// Resolve turns a run id prefix into a run id, or ErrNotFound / ErrAmbiguousPrefix. It
	// enumerates the stored ids to match the prefix, so it takes a context and honors
	// cancellation mid-scan on a large store.
	Resolve(ctx context.Context, prefix string) (RunID, error)
	// ListRefs returns the id of every stored run from the directory layout alone,
	// without decoding metadata. It lets a recovery-minded lister (run log) surface a
	// run whose metadata is corrupt — which the validating read path (Get) cannot — so
	// a caller can still see and delete it. Layout corruption (a misplaced entry) is
	// still reported as an error. It enumerates every stored id, so it takes a context
	// and honors cancellation mid-enumeration on a large store.
	ListRefs(ctx context.Context) ([]RunID, error)
	// ListRefsNewest returns at most the limit newest run ids, newest first, without
	// decoding metadata. It is the bounded counterpart to ListRefs for a current-state
	// reuse view (run ls): the implementation keeps only the newest limit ids rather
	// than materializing and sorting every ref, so the candidate window — and the
	// per-entry decode, payload, and scan work it drives — stays bounded on a large
	// store. A non-positive limit returns no ids. The enumeration still scans every id
	// to find the newest, so it takes a context and honors cancellation mid-scan.
	ListRefsNewest(ctx context.Context, limit int) ([]RunID, error)
	// IterRefsNewest yields stored run ids newest first, one at a time, until yield
	// returns stop=true or the ids are exhausted. It is the streaming primitive for
	// latest / latest-matching lookups: unlike ListRefsNewest it is not bounded to a
	// window, so a caller with a "latest" contract still finds an old-but-valid run,
	// yet it never returns the whole id list — the caller stops at the first readable
	// or matching entry and only that entry's metadata is decoded. The working set is
	// bounded (no full id slice is retained); a yield error aborts iteration and is
	// returned. Ids are read from the directory layout alone, no metadata is decoded.
	// Each yielded id costs a full enumeration pass (O(passes*names)), so it takes a
	// context and honors cancellation within and between passes.
	IterRefsNewest(ctx context.Context, yield func(RunID) (stop bool, err error)) error
	// CountRefs returns how many runs are stored, folded from the directory layout
	// without decoding metadata or retaining the id list. It is the bounded counterpart
	// to len(ListRefs()) for callers that need only the total (status): the directory
	// entries are streamed in bounded batches, so a large store — including one where
	// many ids share a single shard — is counted in bounded memory. It walks every id,
	// so it takes a context and honors cancellation mid-count.
	CountRefs(ctx context.Context) (int, error)
	OpenStdout(id RunID) (io.ReadCloser, error)
	OpenStderr(id RunID) (io.ReadCloser, error)
	Delete(id RunID) error
}
