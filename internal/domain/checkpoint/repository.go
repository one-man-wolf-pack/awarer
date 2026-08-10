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
// The contract is stream-first by design on both sides: reads expose one header read
// (Header), one bounded store-health read (StoreHealthNewest), and a re-openable record
// stream (OpenManifest), and the write path is PutManifest, which streams the records.
// There is no accessor anywhere — on this interface or on the adapter — that
// materializes a whole checkpoint by an ordinary name, so no caller can load an
// unbounded manifest without saying so at the call site. History cardinality obeys the
// same rule from the other direction: this port carries only the bounded health read, so
// a consumer that genuinely needs every header declares its own narrow port for the
// adapter's full operation rather than finding one waiting here.
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
	// StoreHealthNewest reads one header per committed checkpoint and classifies each
	// unreadable one (incompatible schema vs corrupt) into exact counts rather than
	// collapsing the whole store on the first bad record, so a diagnostic surface can
	// tell empty, healthy, partial, incompatible, and corrupt stores apart. Structural
	// corruption that makes the id set itself unreadable (a foreign node on a reserved
	// checkpoint address) still fails loudly as an error, as does a read failure that is
	// neither an incompatible schema nor store corruption. It takes a context and honors
	// cancellation between records.
	//
	// It retains at most the newest readable headers, newest-first. newest must be
	// positive; a non-positive bound is a caller error, never a silent "all".
	//
	// The bound is a retention window, not a health sample: the scan still inspects every
	// committed header exactly once, so an incompatible or corrupt record — or a
	// cancellation — after the window has filled still changes the result. Totals
	// therefore come from the counts, never from the retained window.
	//
	// This port carries no full-history read. The concrete adapter has one, and the only
	// consumer that needs it — the timeline — declares its own one-method port for it, so
	// a holder of this broad interface cannot retain every header by naming a method here.
	StoreHealthNewest(ctx context.Context, newest int) (CheckpointStoreHealth, error)
}
