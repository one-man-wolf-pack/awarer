package checkpointjson

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
func (r *Repo) ResolvePrefix(ctx context.Context, prefix string) (checkpoint.CheckpointID, error) {
	// Reject malformed input (empty, too long, bad characters) distinctly from a
	// well-formed prefix that matches nothing, so a blank or garbage reference is
	// never silently resolved to a checkpoint.
	if err := checkpoint.ValidateIDPrefix(prefix); err != nil {
		return checkpoint.CheckpointID{}, err
	}
	// listIDs streams the directory and honors cancellation during the scan; the prefix
	// match below is an in-memory pass over the ids it returned.
	ids, err := r.listIDs(ctx)
	if err != nil {
		return checkpoint.CheckpointID{}, err
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return checkpoint.CheckpointID{}, fmt.Errorf("%w: %s", checkpoint.ErrNotFound, prefix)
	case 1:
		return checkpoint.ParseCheckpointID(matches[0])
	default:
		return checkpoint.CheckpointID{}, fmt.Errorf("%w: %s matches %d checkpoints", checkpoint.ErrAmbiguousPrefix, prefix, len(matches))
	}
}

// ListHeaders returns every checkpoint's header newest-first, without materializing
// any manifest. The order matches List.
func (r *Repo) ListHeaders(ctx context.Context) ([]checkpoint.CheckpointHeader, error) {
	ids, err := r.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]checkpoint.CheckpointHeader, 0, len(ids))
	for _, idStr := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id, err := checkpoint.ParseCheckpointID(idStr)
		if err != nil {
			continue
		}
		h, err := r.Header(id)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

// StoreHealth reads every checkpoint header and classifies the store's read health,
// collecting the readable headers alongside a finding for each unreadable record. It
// is the resilient counterpart to ListHeaders: where ListHeaders collapses the whole
// listing on the first bad header, this reports the readable checkpoints and, per
// unreadable one, whether it is an incompatible schema or corrupt — so a caller can
// tell empty, healthy, partial, incompatible, and corrupt stores apart instead of
// mistaking an unreadable store for an empty one. Structural corruption that makes
// the id set itself unreadable (a foreign node on a reserved address) still fails
// loudly through listIDs. A per-record read error that is neither an incompatible
// schema nor store corruption — an I/O or permission failure — is surfaced as an
// error rather than silently labeled, since doctor's header read draws that line for
// the depth checks and status must not report an inaccessible record as merely
// absent.
func (r *Repo) StoreHealth(ctx context.Context) (checkpoint.CheckpointStoreHealth, error) {
	idStrs, err := r.listIDs(ctx)
	if err != nil {
		return checkpoint.CheckpointStoreHealth{}, err
	}
	var headers []checkpoint.CheckpointHeader
	var findings []checkpoint.ReadFinding
	for _, s := range idStrs {
		if err := ctx.Err(); err != nil {
			return checkpoint.CheckpointStoreHealth{}, err
		}
		id, err := checkpoint.ParseCheckpointID(s)
		if err != nil {
			// listIDs only ever returns parseable ids; skip defensively.
			continue
		}
		h, err := r.Header(id)
		switch {
		case err == nil:
			headers = append(headers, h)
		case errors.Is(err, checkpoint.ErrIncompatibleFormat):
			findings = append(findings, checkpoint.NewIncompatibleReadFinding(id, err.Error()))
		case errors.Is(err, checkpoint.ErrCorruptStore):
			findings = append(findings, checkpoint.NewCorruptReadFinding(id, err.Error()))
		default:
			return checkpoint.CheckpointStoreHealth{}, err
		}
	}
	return checkpoint.NewCheckpointStoreHealth(headers, findings), nil
}

// ListIDs returns the ids of all committed checkpoints without decoding their
// headers, mirroring runstore.ListRefs. Unlike ListHeaders it does not fail when a
// single header is corrupt: it reports the durable structure (which checkpoints
// exist) so a diagnostic like "awa doctor" can then read each header individually
// and surface a corrupt one as a finding rather than losing every checkpoint to one
// bad file. Structural corruption that makes the set itself unreadable — a dual
// layout, or a non-directory at a checkpoint address — still fails loudly.
func (r *Repo) ListIDs(ctx context.Context) ([]checkpoint.CheckpointID, error) {
	idStrs, err := r.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]checkpoint.CheckpointID, 0, len(idStrs))
	for _, s := range idStrs {
		id, err := checkpoint.ParseCheckpointID(s)
		if err != nil {
			// listIDs only ever returns parseable ids, so this is unreachable; skip
			// defensively rather than abort the whole listing.
			continue
		}
		out = append(out, id)
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

// LatestHeader returns the newest checkpoint's header, or ok=false when none exist.
func (r *Repo) LatestHeader(ctx context.Context) (checkpoint.CheckpointHeader, bool, error) {
	all, err := r.ListHeaders(ctx)
	if err != nil {
		return checkpoint.CheckpointHeader{}, false, err
	}
	if len(all) == 0 {
		return checkpoint.CheckpointHeader{}, false, nil
	}
	return all[0], true, nil
}

// listIDs returns the id strings of every committed checkpoint in the directory. It
// reads only directory entries (no JSON decoding): an "<id>/" directory holding a
// header.json is a committed checkpoint. A directory without a header is a crashed,
// uncommitted publish and is skipped rather than reported. An entry that does not
// parse as an id is unrelated to this store and is ignored.
//
// The checkpoints directory reserves the id namespace, and a checkpoint's only
// address is its directory. A node of any other shape on that namespace — a symlink
// or file at "<id>", or a bare "<id>.json", none of which this store writes — is
// structural corruption and fails the listing loudly. It is never stat-probed for a
// layout signature and never opened: this build has one reader, and the alternative —
// an unrecognized artifact silently skipped — would let a store full of them read
// back as empty, which is the one answer that must never be given.
func (r *Repo) listIDs(ctx context.Context) ([]string, error) {
	relDir, err := fsx.RelUnder(r.root, r.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	// Stream the directory in bounded batches and check cancellation per entry, so a large
	// checkpoint history is interruptible mid-scan rather than materialized in full first —
	// the per-entry committed-header stat is real I/O that scales with the store. Read
	// errors and callback corruption pass through storeCorrupt unchanged (it transforms only
	// symlink/not-directory open rejections), so a cancelled or corrupt scan keeps its error.
	err = fsx.EachDirEntryNoFollow(r.root, relDir, func(e os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		// A bare id name is the address of a checkpoint directory. It must be a
		// directory: a symlink or other node occupying that address is store tampering,
		// surfaced here as corruption rather than silently skipped.
		if _, err := checkpoint.ParseCheckpointID(name); err == nil {
			if !e.IsDir() {
				return fmt.Errorf("%w: %s occupies a checkpoint directory address but is not a directory (%s)", checkpoint.ErrCorruptStore, name, e.Type())
			}
			ok, err := statRegular(r.root, filepath.Join(r.dir, name, headerName))
			if err != nil {
				return err
			}
			if ok {
				ids = append(ids, name)
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
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, storeCorrupt(r.dir, err)
	}
	return ids, nil
}
