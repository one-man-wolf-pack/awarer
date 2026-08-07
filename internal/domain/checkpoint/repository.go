package checkpoint

import (
	"context"
	"errors"

	"awarer/internal/domain/worktree"
)

// Repository persistence errors. They are sentinel values so the application can
// branch on them (e.g. retry on a collision, report a clear message on an
// ambiguous prefix) rather than matching error strings.
var (
	// ErrIDCollision reports that a checkpoint id already exists. The repository
	// never overwrites an existing checkpoint — an immutable record must not be
	// silently replaced — so a collision is surfaced for the caller to regenerate.
	ErrIDCollision = errors.New("checkpoint id already exists")
	// ErrNotFound reports that no checkpoint matched a lookup.
	ErrNotFound = errors.New("checkpoint not found")
	// ErrAmbiguousPrefix reports that a short id matched more than one checkpoint.
	ErrAmbiguousPrefix = errors.New("checkpoint id prefix is ambiguous")
	// ErrCorruptStore reports that durable checkpoint state on disk is corrupt: a
	// record that cannot be decoded or validated, an id that disagrees with its
	// filename, or a non-regular node where a record belongs. It is distinct from
	// ErrNotFound (absent, normal) so the CLI can signal state-action-required — which
	// needs repair or diagnostics — rather than an ordinary retryable failure.
	ErrCorruptStore = errors.New("corrupt checkpoint store")
	// ErrIncompatibleFormat reports that a durable record declares a checkpoint schema
	// this build does not speak. It is deliberately distinct from ErrCorruptStore: the
	// record is intact and self-describing, awa simply has no reader for that shape, so
	// the CLI can tell the user to reset local evidence rather than report damage.
	//
	// It is an upgrade seam, not compatibility: awa never decodes such a record through
	// another type, and it names no list of prior versions. The record is retained —
	// never a hit, a state reference, a timeline fact, or an automatic reclamation —
	// until the user resets the store explicitly.
	ErrIncompatibleFormat = errors.New("incompatible checkpoint schema")
)

// Repository stores and retrieves immutable checkpoint records. Implemented by the
// checkpointjson filesystem adapter.
//
// The contract is stream-first by design on both sides: reads expose header reads
// (Header/ListHeaders/LatestHeader/StoreHealth) and a re-openable record stream
// (OpenManifest), and the write path is PutManifest, which streams the records. There
// is no accessor anywhere — on this interface or on the adapter — that materializes a
// whole checkpoint by an ordinary name, so no caller can load an unbounded manifest
// without saying so at the call site.
//
// It carries no delete: no holder of this port removes a published checkpoint.
// Reclamation does, and gc calls Delete on the checkpointjson adapter directly.
type Repository interface {
	// PutManifest is the primary streaming write contract: it consumes a manifest
	// record stream alongside the non-derived build metadata, deriving the tree
	// hash, stats, and record count from the stream itself rather than requiring the
	// caller to own a fully materialized checkpoint. It writes the manifest and header
	// atomically (header is the commit point) and returns the resulting header, or
	// ErrIDCollision if the id already exists. Each record is validated as it is
	// consumed.
	PutManifest(ctx context.Context, build CheckpointBuild, manifest worktree.ManifestStream) (CheckpointHeader, error)
	// Header returns the checkpoint's small metadata without materializing its
	// manifest, or ErrNotFound. Durable derived fields are validated on read.
	Header(id CheckpointID) (CheckpointHeader, error)
	// OpenManifest returns a re-openable stream over the checkpoint's manifest
	// records in canonical path order, or ErrNotFound. The stream validates each
	// record as hostile input and fails loudly on a corrupt or out-of-order record.
	OpenManifest(id CheckpointID) (worktree.ManifestStream, error)
	// ResolvePrefix returns the single id a short prefix identifies, or
	// ErrNotFound / ErrAmbiguousPrefix. It enumerates the stored ids to match the prefix,
	// so it takes a context and honors cancellation mid-scan on a large store.
	ResolvePrefix(ctx context.Context, prefix string) (CheckpointID, error)
	// ListHeaders returns every checkpoint's header newest-first with a deterministic
	// tie-breaker, without materializing any manifest. It reads one header per stored
	// checkpoint, so it takes a context and honors cancellation between records: a
	// large local history is interruptible mid-read, returning ctx.Err().
	ListHeaders(ctx context.Context) ([]CheckpointHeader, error)
	// StoreHealth reads every checkpoint's header and classifies the store's read
	// health, collecting the readable headers (newest-first) alongside a per-record
	// finding for each unreadable one (incompatible schema vs corrupt). Unlike
	// ListHeaders it does not collapse the whole store when a single record cannot be
	// read, so a diagnostic surface can tell empty, healthy, partial, incompatible, and
	// corrupt stores apart. Structural corruption that makes the id set itself
	// unreadable (a foreign node on a reserved checkpoint address) still fails
	// loudly as an error. It reads one header per checkpoint, so it takes a context and
	// checks cancellation between records.
	StoreHealth(ctx context.Context) (CheckpointStoreHealth, error)
	// LatestHeader returns the newest checkpoint's header; ok is false when none exist.
	// It walks the headers to find the newest, so it takes a context and is interruptible.
	LatestHeader(ctx context.Context) (h CheckpointHeader, ok bool, err error)
}
