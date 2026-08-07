package worktree

import (
	"context"
	"io"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
)

// Walker is the filesystem port the scanner depends on. It yields scope-filtered
// nodes in arbitrary order — the scanner sorts them — and owns all os/filepath,
// ignore-matching, and stat details. Defined here on the consumer side so the
// concrete walker (and its gitignore dependency) live in infrastructure.
type Walker interface {
	// Walk visits every in-scope node under the layout's root, calling visit for
	// each. The scan boundary is the resolved ScanScope (effective excludes,
	// include, ignore-source toggles, symlink policy), so the walker never derives
	// excludes itself. Returning a non-nil error from visit aborts the walk with
	// that error. Walk itself returns the first error encountered, so a filesystem
	// failure that affects correctness stops the scan loudly rather than being skipped.
	Walk(ctx context.Context, layout paths.Layout, scope config.ScanScope, visit func(Node) error) error
}

// Node is a single filesystem node surfaced by the walker, before hashing. The
// scanner decides — based on trust mode and large-file policy — whether to call
// Open and read content, so the walker never eagerly reads files.
type Node struct {
	Path      RelPath
	Kind      FileKind
	Stat      StatSignature
	Symlink   SymlinkTarget
	Traversal TraversalInfo
	// Skipped marks a node the walker itself determined cannot be scanned —
	// a symlink cycle, a root escape, or exceeded traversal depth. SkipReason
	// explains it. Skips the scanner decides (large-file policy, read errors) are
	// not set here.
	Skipped    bool
	SkipReason SkippedReason
	// Open lazily opens the node's content for streaming. It is non-nil only for
	// regular files. The caller closes the returned reader.
	Open func() (io.ReadCloser, error)
}

// Size is the node's content size, taken from its stat signature so there is a
// single source of truth: the scanner's large-file policy and the persisted stat
// can never disagree about how big a file is.
func (n Node) Size() int64 { return n.Stat.Size }

// IndexLookup is the reuse-lookup capability the scanner needs to accelerate
// hashing. The scan path depends on this facet alone: it consults the index but
// never opens or closes it (the composition root owns that lifecycle), so its
// callers and test fakes carry no handle method the scan never calls — the
// compiler, not caller discipline, keeps the scan read-only.
type IndexLookup interface {
	// Lookup returns the last indexed state for a path, if any.
	Lookup(ctx context.Context, path RelPath) (IndexedEntry, bool, error)
}

// IndexReader is a read-only index handle: the lookup capability plus the handle
// lifecycle. The composition root opens it, lends its lookup capability to the
// scanner, and closes it. Read-only acceleration paths (status run-reuse, run
// explain, run ls, the changes/diff comparison) hold one of these.
type IndexReader interface {
	IndexLookup
	// Close releases the underlying database handle.
	Close() error
}

// Index is the worktree index port: mutable acceleration state that lets normal
// trust mode reuse a content hash when a file's stat signature is unchanged. It
// is the read handle plus the mutable scan-persist capability. It is not the
// source of truth. Implemented by the SQLite adapter.
type Index interface {
	IndexReader
	// BeginScan records an incomplete scan and returns a transaction for its
	// file updates. The scan becomes valid only when the transaction commits.
	BeginScan(ctx context.Context, meta ScanMetadata) (ScanTx, error)
}

// ScanTx is the transactional handle for one scan's index updates. All updates
// and the scan's completion commit atomically, so an interrupted scan leaves no
// completed row and no partial file rows.
type ScanTx interface {
	// Upsert records an entry's current stat and content hash. The transaction
	// owns its scan identity, so no scan id is passed in — rows always belong to
	// this transaction's scan.
	Upsert(ctx context.Context, e Entry) error
	// RecordSkipped records a skipped input under this transaction's scan.
	RecordSkipped(ctx context.Context, s SkippedInput) error
	// Commit marks the scan complete with its final tree hash and completion time
	// and commits. The completion time is passed explicitly — rather than read
	// from the start metadata — because the scan is only complete at this call.
	Commit(ctx context.Context, treeHash hashing.TreeHash, completedAt time.Time) error
	// Rollback abandons the scan, leaving prior index state untouched.
	Rollback() error
}

// IndexedEntry is the persisted state the index returns for a path. Content is
// the zero ContentHash when the path was previously recorded without a hash
// (e.g. a skipped input).
type IndexedEntry struct {
	Stat    StatSignature
	Content hashing.ContentHash
	Storage ContentStorageIntent
}
