package checkpointjson

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/manifestjsonl"
)

// checkpointPerm is the mode for a published checkpoint file. Checkpoint headers and
// manifests are immutable and hold project evidence (paths, modes, hashes, skipped
// records, project shape), so they are written owner-only read-only, matching the
// private-by-default policy every other awa-owned file follows.
const checkpointPerm = paths.ReadOnlyFilePerm

const checkpointExt = ".json"

// Repo stores immutable checkpoint records as a directory per id under the
// checkpoints directory. root is the project root, the trusted anchor for the
// no-follow reads so the whole .awa/checkpoints path is checked for symlinks.
type Repo struct {
	dir  string
	root string
	// fail is a test-only fault-injection seam: nil in production (a single branch),
	// set only from same-package _test.go. It interrupts PutManifest at a named stage
	// so a test can prove a partial write never becomes an authoritative checkpoint.
	fail func(stage failStage) error
}

// failStage names a fault-injection point inside PutManifest. The header is the
// commit point, so the meaningful crash window is between the manifest and the
// header — a stage a fresh reader must treat as an invisible, reclaimable orphan.
type failStage string

const failAfterManifestBeforeHeader failStage = "after-manifest-before-header"

// checkFail invokes the fail hook (if any) for stage and returns its error so the
// caller aborts through the normal error path.
func (r *Repo) checkFail(stage failStage) error {
	if r.fail == nil {
		return nil
	}
	return r.fail(stage)
}

// NewRepo builds a checkpoint repository over a project's layout.
func NewRepo(layout paths.Layout) *Repo {
	return &Repo{dir: layout.CheckpointsDir(), root: layout.Root()}
}

// readRegularFile reads the store-owned JSON record at abs, anchored at the project
// root, refusing to follow a symlink at any component below root or to read a
// non-regular node. Checkpoint records are immutable, store-owned regular files;
// following a symlink — at the file or at an ancestor directory such as
// .awa/checkpoints — would let their bytes change outside .awa after the record
// already passed validation. The traversal is component-wise no-follow and the
// regular-node check is on the opened descriptor, so a symlink swapped in after a
// separate shape check cannot be read through: the read is verified on the very
// descriptor it draws bytes from, not a path inspected earlier.
func readRegularFile(root, abs string) ([]byte, error) {
	rel, err := fsx.RelUnder(root, abs)
	if err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollowAt(root, rel)
	if err != nil {
		return nil, storeCorrupt(abs, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file (%s)", checkpoint.ErrCorruptStore, abs, info.Mode().Type())
	}
	return io.ReadAll(f)
}

// storeCorrupt maps a no-follow rejection on a store path to ErrCorruptStore (a
// tampered store: a symlinked component, or a non-directory where a store directory
// belongs), passing any other error through unchanged.
func storeCorrupt(absPath string, err error) error {
	switch {
	case fsx.IsSymlinkOpenRejection(err):
		return fmt.Errorf("%w: %s is reached through a symlink", checkpoint.ErrCorruptStore, absPath)
	case errors.Is(err, fsx.ErrNotDirectory):
		return fmt.Errorf("%w: %s is not a directory", checkpoint.ErrCorruptStore, absPath)
	default:
		return err
	}
}

// statRegular reports whether abs is an existing regular file, reached from the
// project root without following any symlink component. Absent is (false, nil); a
// symlinked ancestor or a non-regular node is ErrCorruptStore. The shape is checked
// on the opened descriptor, not a path inspected separately.
func statRegular(root, abs string) (bool, error) {
	rel, err := fsx.RelUnder(root, abs)
	if err != nil {
		return false, err
	}
	f, err := fsx.OpenNoFollowAt(root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, storeCorrupt(abs, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: %s is not a regular file (%s)", checkpoint.ErrCorruptStore, abs, info.Mode().Type())
	}
	return true, nil
}

func (r *Repo) dirFor(id checkpoint.CheckpointID) string {
	return filepath.Join(r.dir, id.String())
}

func (r *Repo) headerFor(id checkpoint.CheckpointID) string {
	return filepath.Join(r.dirFor(id), headerName)
}

func (r *Repo) manifestFor(id checkpoint.CheckpointID) string {
	return filepath.Join(r.dirFor(id), manifestName)
}

// committed reports whether id names a published checkpoint: its directory holds a
// header, the exclusive commit point. It is the one place "this id exists" is
// decided, so no caller has to reimplement the rule. A symlinked component or a
// non-regular node at the header address fails closed as ErrCorruptStore rather
// than reading as absent.
func committed(root, dir string, id checkpoint.CheckpointID) (bool, error) {
	return statRegular(root, filepath.Join(dir, id.String(), headerName))
}

// PutManifest writes a checkpoint from non-derived build metadata plus a manifest
// stream, deriving the tree hash, stats, and record count from the stream as it
// persists it — the write path never materializes the manifest. The manifest is
// published exclusively (hard-link) first and the header — the commit point — last
// and exclusively, so a crashed write that left only a manifest is invisible to
// readers and two writers racing on one id cannot leave a committed header
// describing a different writer's manifest. A collision is checked up front so it
// never mutates an established checkpoint's records.
func (r *Repo) PutManifest(ctx context.Context, build checkpoint.CheckpointBuild, manifest worktree.ManifestStream) (checkpoint.CheckpointHeader, error) {
	if err := build.Validate(); err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	switch exists, err := committed(r.root, r.dir, build.ID); {
	case err != nil:
		return checkpoint.CheckpointHeader{}, err
	case exists:
		return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: %s", checkpoint.ErrIDCollision, build.ID)
	}

	reducer, err := worktree.NewTreeReducer(blake3hash.New())
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	cur, err := manifest.Open(ctx)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	defer func() { _ = cur.Close() }()

	relIDDir, err := fsx.RelUnder(r.root, r.dirFor(build.ID))
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}

	// Tee the manifest stream into the temp file and the reducer in one pass.
	if err := fsx.PublishStreamNoFollow(r.root, relIDDir, manifestName, checkpointPerm, func(w io.Writer) error {
		return manifestjsonl.Tee(w, cur, reducer)
	}); err != nil {
		if errors.Is(err, os.ErrExist) {
			return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: %s", checkpoint.ErrIDCollision, build.ID)
		}
		return checkpoint.CheckpointHeader{}, storeCorrupt(r.manifestFor(build.ID), err)
	}

	if err := r.checkFail(failAfterManifestBeforeHeader); err != nil {
		// The manifest is published but the header — the commit point — is not. The
		// leftover <id>/manifest.jsonl is invisible to every reader (which key off the
		// header) and is reclaimable by gc/doctor; a fresh checkpoint uses a new id.
		return checkpoint.CheckpointHeader{}, err
	}

	red := reducer.Finish()
	header := build.Header(red.Hash, checkpoint.StatsFromReduced(red.Stats), red.Count)
	if err := header.Validate(); err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	headerData, err := encodeHeader(header)
	if err != nil {
		return checkpoint.CheckpointHeader{}, err
	}
	if err := fsx.PublishBytesNoFollow(r.root, relIDDir, headerName, headerData, checkpointPerm); err != nil {
		if errors.Is(err, os.ErrExist) {
			return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: %s", checkpoint.ErrIDCollision, build.ID)
		}
		return checkpoint.CheckpointHeader{}, storeCorrupt(r.headerFor(build.ID), err)
	}
	return header, nil
}

// readHeader reads and decodes a checkpoint's header, checking the directory id
// matches the body id. An absent header — no directory, or a directory whose
// exclusive commit point was never published — is ErrNotFound; a header written in a
// schema this build does not speak keeps its ErrIncompatibleFormat sentinel so the
// read paths and store health classify it apart from damage.
func (r *Repo) readHeader(id checkpoint.CheckpointID) (checkpoint.CheckpointHeader, error) {
	data, err := readRegularFile(r.root, r.headerFor(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: %s", checkpoint.ErrNotFound, id)
		}
		return checkpoint.CheckpointHeader{}, err
	}
	h, err := decodeHeader(data)
	if err != nil {
		// The decoder already carries the incompatible sentinel and the versions it
		// compared; add the id and pass it through with %w rather than restating the
		// sentinel, which would print the same phrase twice in one message.
		if errors.Is(err, checkpoint.ErrIncompatibleFormat) {
			return checkpoint.CheckpointHeader{}, fmt.Errorf("checkpoint %s: %w", id, err)
		}
		return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: checkpoint %s: %v", checkpoint.ErrCorruptStore, id, err)
	}
	if h.ID != id {
		return checkpoint.CheckpointHeader{}, fmt.Errorf("%w: checkpoint header for %s contains a different id %s", checkpoint.ErrCorruptStore, id, h.ID)
	}
	return h, nil
}

// Header returns a checkpoint's metadata without materializing its manifest.
func (r *Repo) Header(id checkpoint.CheckpointID) (checkpoint.CheckpointHeader, error) {
	return r.readHeader(id)
}

// OpenManifest returns a re-openable stream over a checkpoint's manifest records.
func (r *Repo) OpenManifest(id checkpoint.CheckpointID) (worktree.ManifestStream, error) {
	h, err := r.readHeader(id)
	if err != nil {
		return nil, err
	}
	return r.manifestStream(id, h.RecordCount), nil
}

// ResolvePrefix returns the single id a short prefix identifies. It matches on
// stored filenames so it does not decode every checkpoint, and it is strict: no
// match is ErrNotFound and more than one is ErrAmbiguousPrefix, so a short id is
// only ever acted on when it is unambiguous.
//
// The scan runs to completion rather than stopping at the first match: structural
// corruption anywhere in the id namespace must stay visible, and stopping early would
// resolve a reference out of a store this build has already refused to list. It
// retains one matched id and a count, never the matching set.
func (r *Repo) ResolvePrefix(ctx context.Context, prefix string) (checkpoint.CheckpointID, error) {
	// Reject malformed input (empty, too long, bad characters) distinctly from a
	// well-formed prefix that matches nothing, so a blank or garbage reference is
	// never silently resolved to a checkpoint.
	if err := checkpoint.ValidateIDPrefix(prefix); err != nil {
		return checkpoint.CheckpointID{}, err
	}
	var match checkpoint.CheckpointID
	matches := 0
	if err := r.eachCommittedID(ctx, func(id checkpoint.CheckpointID) error {
		if strings.HasPrefix(id.String(), prefix) {
			matches++
			match = id
		}
		return nil
	}); err != nil {
		return checkpoint.CheckpointID{}, err
	}
	switch matches {
	case 0:
		return checkpoint.CheckpointID{}, fmt.Errorf("%w: %s", checkpoint.ErrNotFound, prefix)
	case 1:
		return match, nil
	default:
		return checkpoint.CheckpointID{}, fmt.Errorf("%w: %s matches %d checkpoints", checkpoint.ErrAmbiguousPrefix, prefix, matches)
	}
}

// StoreHealthAll classifies the store's read health and retains every readable
// header. It is the explicit full-history read: the retained headers grow with the
// store, which is why it is a separate named operation rather than a default.
func (r *Repo) StoreHealthAll(ctx context.Context) (checkpoint.CheckpointStoreHealth, error) {
	var headers []checkpoint.CheckpointHeader
	counts, err := r.scanHealth(ctx, func(h checkpoint.CheckpointHeader) {
		headers = append(headers, h)
	})
	if err != nil {
		return checkpoint.CheckpointStoreHealth{}, err
	}
	return checkpoint.NewCheckpointStoreHealth(counts, headers), nil
}

// StoreHealthNewest classifies the store's read health and retains at most the newest
// readable headers. The scan is identical to StoreHealthAll's — every committed header
// is still read and counted, so a record that fails after the window has filled still
// changes the verdict — only the retained history is bounded, to newest plus one
// directory batch.
//
// A non-positive bound is a caller error rather than a silent "all": the whole point
// of naming this operation is that the retained window is chosen deliberately.
func (r *Repo) StoreHealthNewest(ctx context.Context, newest int) (checkpoint.CheckpointStoreHealth, error) {
	if newest <= 0 {
		return checkpoint.CheckpointStoreHealth{}, fmt.Errorf("checkpoint store health: newest header window must be positive, got %d", newest)
	}
	w := newestWindow{limit: newest}
	counts, err := r.scanHealth(ctx, w.keep)
	if err != nil {
		return checkpoint.CheckpointStoreHealth{}, err
	}
	return checkpoint.NewCheckpointStoreHealth(counts, w.headers), nil
}

// scanHealth walks every committed checkpoint exactly once, reading each header and
// tallying it as readable, incompatible, or corrupt, offering every readable header to
// keep. It is the one health implementation both named operations project from, so a
// bounded caller and a full caller can never disagree about the store's condition.
//
// A per-record read error that is neither an incompatible schema nor store corruption
// — an I/O or permission failure — is surfaced as an error rather than silently
// labeled, since doctor's header read draws that line for the depth checks and status
// must not report an inaccessible record as merely absent.
func (r *Repo) scanHealth(ctx context.Context, keep func(checkpoint.CheckpointHeader)) (checkpoint.StoreReadCounts, error) {
	var counts checkpoint.StoreReadCounts
	err := r.eachCommittedID(ctx, func(id checkpoint.CheckpointID) error {
		h, err := r.Header(id)
		switch {
		case err == nil:
			counts.Readable++
			keep(h)
		case errors.Is(err, checkpoint.ErrIncompatibleFormat):
			counts.Incompatible++
		case errors.Is(err, checkpoint.ErrCorruptStore):
			counts.Corrupt++
		default:
			return err
		}
		return nil
	})
	if err != nil {
		return checkpoint.StoreReadCounts{}, err
	}
	return counts, nil
}

// newestWindow retains the newest limit headers offered to it, in no particular
// order: an oldest-first heap keeps the eviction candidate at the root, so the walk
// neither sorts nor holds more than limit headers. Final ordering belongs to
// CheckpointStoreHealth, so the heap only has to answer "which retained header is the
// oldest" — one use of checkpoint.NewestFirst, not a second tie-break rule.
type newestWindow struct {
	limit   int
	headers oldestFirstHeap
}

func (w *newestWindow) keep(h checkpoint.CheckpointHeader) {
	switch {
	case len(w.headers) < w.limit:
		heap.Push(&w.headers, h)
	case checkpoint.NewestFirst(h, w.headers[0]) < 0:
		// The window is full and this header is newer than the oldest kept: evict it.
		w.headers[0] = h
		heap.Fix(&w.headers, 0)
	}
}

// oldestFirstHeap is a heap of checkpoint headers whose root is the oldest, so a
// bounded newest-window walk can find its eviction candidate in constant time.
type oldestFirstHeap []checkpoint.CheckpointHeader

func (h oldestFirstHeap) Len() int { return len(h) }
func (h oldestFirstHeap) Less(i, j int) bool {
	return checkpoint.NewestFirst(h[i], h[j]) > 0
}
func (h oldestFirstHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *oldestFirstHeap) Push(x any)   { *h = append(*h, x.(checkpoint.CheckpointHeader)) }
func (h *oldestFirstHeap) Pop() any {
	old := *h
	n := len(old)
	last := old[n-1]
	*h = old[:n-1]
	return last
}

// ListIDs returns the ids of all committed checkpoints without decoding their
// headers, mirroring runstore.ListRefs. Unlike the health reads it classifies
// nothing: it reports the durable structure (which checkpoints exist) so a complete
// maintenance pass like "awa doctor" or gc planning can then read each header
// individually and surface a corrupt one as a finding rather than losing every
// checkpoint to one bad file. Structural corruption that makes the set itself
// unreadable — a dual layout, or a non-directory at a checkpoint address — still fails
// loudly.
//
// It is the one place the whole id set is retained, and it is retained because those
// callers must produce a plan covering every stored checkpoint.
func (r *Repo) ListIDs(ctx context.Context) ([]checkpoint.CheckpointID, error) {
	var out []checkpoint.CheckpointID
	if err := r.eachCommittedID(ctx, func(id checkpoint.CheckpointID) error {
		out = append(out, id)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete removes a checkpoint's directory. It is idempotent: an absent id is not an
// error, so a rollback after a failed publish is safe to call unconditionally. It
// removes only the address the current layout owns — a foreign node parked on the id
// namespace is corruption the caller must resolve explicitly, never something an
// ordinary delete quietly reclaims.
func (r *Repo) Delete(id checkpoint.CheckpointID) error {
	relDir, err := fsx.RelUnder(r.root, r.dir)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(r.root, relDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no checkpoints dir: nothing to delete, and delete is idempotent
		}
		return storeCorrupt(r.dir, err)
	}
	defer func() { _ = dir.Close() }()
	if err := fsx.RemoveTreeAt(dir, id.String()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// eachCommittedID calls fn with the id of every committed checkpoint in the
// directory. It reads only directory entries (no JSON decoding): an "<id>/" directory
// holding a header.json is a committed checkpoint. A directory without a header is a
// crashed, uncommitted publish and is skipped rather than reported. An entry that does
// not parse as an id is unrelated to this store and is ignored.
//
// The checkpoints directory reserves the id namespace, and a checkpoint's only
// address is its directory. A node of any other shape on that namespace — a symlink
// or file at "<id>", or a bare "<id>.json", none of which this store writes — is
// structural corruption and fails the listing loudly. It is never stat-probed for a
// layout signature and never opened: this build has one reader, and the alternative —
// an unrecognized artifact silently skipped — would let a store full of them read
// back as empty, which is the one answer that must never be given.
//
// This is the sole owner of checkpoint address grammar: id collection, prefix
// matching, and the health scans all project from it and none of them reimplement the
// rules above.
func (r *Repo) eachCommittedID(ctx context.Context, fn func(checkpoint.CheckpointID) error) error {
	relDir, err := fsx.RelUnder(r.root, r.dir)
	if err != nil {
		return err
	}
	// A callback error is kept apart from the enumeration's own, because the tail below
	// reads a missing directory as an empty store. fn reads real files — a header read
	// that fails with a not-exist cause must surface as that failure, never as "this
	// store holds no checkpoints", the one answer that must never be given.
	var fnErr error
	// Stream the directory in bounded batches and check cancellation per entry, so a large
	// checkpoint history is interruptible mid-scan rather than materialized in full first —
	// the per-entry committed-header stat is real I/O that scales with the store. The
	// enumeration's own errors pass through storeCorrupt unchanged (it transforms only
	// symlink/not-directory open rejections), so a cancelled or structurally corrupt scan
	// keeps its error.
	err = fsx.EachDirEntryNoFollow(r.root, relDir, func(e os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		// A bare id name is the address of a checkpoint directory. It must be a
		// directory: a symlink or other node occupying that address is store tampering,
		// surfaced here as corruption rather than silently skipped.
		if id, err := checkpoint.ParseCheckpointID(name); err == nil {
			if !e.IsDir() {
				return fmt.Errorf("%w: %s occupies a checkpoint directory address but is not a directory (%s)", checkpoint.ErrCorruptStore, name, e.Type())
			}
			ok, err := statRegular(r.root, filepath.Join(r.dir, name, headerName))
			if err != nil {
				return err
			}
			if ok {
				if err := fn(id); err != nil {
					fnErr = err
					return err
				}
			}
			return nil
		}
		// "<id>.json" is not an address this store writes. Refuse it rather than skip it.
		if idStr, ok := strings.CutSuffix(name, checkpointExt); ok {
			if _, err := checkpoint.ParseCheckpointID(idStr); err == nil {
				return fmt.Errorf("%w: %s occupies a reserved checkpoint address; a checkpoint is the directory %s/",
					checkpoint.ErrCorruptStore, name, idStr)
			}
		}
		return nil
	})
	if fnErr != nil {
		return fnErr
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return storeCorrupt(r.dir, err)
	}
	return nil
}
