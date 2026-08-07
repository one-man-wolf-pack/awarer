// Package worktreemut stages verified restore payloads and installs them into the
// user's worktree.
//
// It is the only place awa writes to a path the user owns, and it is deliberately
// small and fail-closed. Two responsibilities live here:
//
//   - staging: read a source blob through the caller's verified opener, stream it
//     into awa-owned temporary state while hashing, and refuse unless the bytes
//     hash back to the identity the source proved. The result is a
//     restore.StagedPayload — the capability the domain requires before a regular
//     file may be written at all.
//   - installation: for one staged operation, re-check the destination's shape at
//     the descriptor boundary, then create, replace, restore a symlink, remove a
//     proved file, or remove a proved EMPTY directory.
//
// Everything is anchored no-follow at the project root, so a symlinked ancestor
// cannot redirect a write out of the tree, and every regular-file write goes
// through a same-directory temp that is verified and then atomically renamed, so a
// destination is only ever left holding its old bytes or the complete new ones.
// Recursive deletion never happens here: a directory is removed only when it is
// already empty, which is the one deletion awa can prove is safe.
package worktreemut

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
)

const (
	// stageDirName is the staging area inside this invocation's spool directory.
	// Staged payloads live under `.awa/store/tmp`, never beside the worktree paths
	// they will replace: a sibling temp file would perturb the re-observation the
	// apply protocol takes immediately before committing.
	stageDirName = "stage"

	// spoolPrefix must match the restore spool's directory prefix so one
	// invocation's spill and staging share a directory and are reclaimed together
	// by the existing temp classification.
	spoolPrefix = "restore-"

	// worktreeDirPerm is the mode new parent directories are created with. Unlike
	// awa's own state, these are the user's directories, so the process umask
	// applies rather than awa's owner-only default.
	worktreeDirPerm os.FileMode = 0o777
)

// streamHasher is the slice of a hasher this package needs: a streaming digest it
// can tee bytes through while copying, plus a whole-reader digest for verifying
// the installed copy. It is a local interface (not the domain Hasher port, which
// has no streaming writer) so the applier depends only on the capabilities it
// uses; the blake3hash.Hasher satisfies it.
type streamHasher interface {
	HashReader(r io.Reader) (hashing.ContentHash, error)
	NewWriter() (io.Writer, func() hashing.ContentHash)
}

// Applier stages payloads and installs operations for one restore invocation.
// Construct it with New; the zero value is not usable. It is not safe for
// concurrent use: apply is a single ordered commit by design.
type Applier struct {
	root     string
	stageRel string
	hasher   streamHasher
	// fail is a test-only fault-injection seam: nil in production (a single
	// branch), set only from same-package _test.go. Each stage is a window with a
	// distinct guarantee: one where the destination must survive holding its old bytes,
	// and one where it has already been removed, so a failure there is the only shape
	// that changes the worktree without completing an operation.
	fail func(stage failStage, path worktree.RelPath) error
	// staged counts payloads staged, so a caller can report how much was verified
	// before the first mutation.
	staged int
}

// failStage names a fault-injection point. The constants are the only stages the
// seam recognizes, so a test names the window it is exercising rather than relying on
// where the single hook happens to sit.
type failStage string

const (
	// stageBeforeRename fires after a replacement's temp file is written and before it
	// is renamed over the destination: the window the destination must survive holding
	// its old bytes.
	stageBeforeRename failStage = "before-rename"
	// stageAfterRemoval fires once an installer has removed the node that stood in the
	// way and before the replacement is in place: the only window where a failed
	// operation leaves the worktree changed.
	stageAfterRemoval failStage = "after-removal"
)

// checkFail consults the seam. It is a single nil check in production.
func (a *Applier) checkFail(stage failStage, path worktree.RelPath) error {
	if a.fail == nil {
		return nil
	}
	return a.fail(stage, path)
}

// New builds an applier whose staging area lives inside the invocation's spool
// directory under the project's store temp area.
func New(layout paths.Layout, id restore.OperationID, hasher streamHasher) (*Applier, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("worktreemut: an applier requires an operation id")
	}
	if hasher == nil {
		return nil, fmt.Errorf("worktreemut: an applier requires a hasher")
	}
	tmpRel, err := fsx.RelUnder(layout.Root(), layout.TmpDir())
	if err != nil {
		return nil, fmt.Errorf("worktreemut: store temp dir is not under the project root: %w", err)
	}
	stageRel := tmpRel + "/" + spoolPrefix + id.String() + "/" + stageDirName
	dir, err := fsx.MkdirAllNoFollow(layout.Root(), stageRel, paths.DirPerm)
	if err != nil {
		return nil, err
	}
	_ = dir.Close()
	return &Applier{root: layout.Root(), stageRel: stageRel, hasher: hasher, staged: 0}, nil
}

// stageName is the staging file name for a content identity. Staging is
// content-addressed deliberately: apply walks the spooled plan several times and
// must be able to find a payload from the operation alone, so a payload's location
// is derived from what it is rather than remembered in a map that would grow with
// the plan. It also means two paths restoring identical bytes stage once.
func stageName(content hashing.ContentHash) string {
	return hashing.Namespace + "-" + content.Hex()
}

// Stage reads the source bytes for content through open, streams them into
// awa-owned staging while hashing, and returns the capability that lets the domain
// build an executable operation.
//
// The identity check is on the same read the bytes came from: the reader is teed
// into the hasher as it is copied, so what is verified is exactly what was staged.
// A mismatch means the store's bytes are not what the manifest addresses, which is
// corruption; nothing is staged and the caller must refuse before any mutation.
//
// It is idempotent: content already staged in this invocation is not re-read.
func (a *Applier) Stage(ctx context.Context, content hashing.ContentHash, open func() (io.ReadCloser, error)) (restore.StagedPayload, error) {
	if content.IsZero() {
		return restore.StagedPayload{}, fmt.Errorf("worktreemut: staging requires a content hash")
	}
	if err := ctx.Err(); err != nil {
		return restore.StagedPayload{}, err
	}
	name := stageName(content)
	if p, err := a.Payload(content); err == nil {
		return p, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return restore.StagedPayload{}, err
	}

	dir, err := fsx.MkdirAllNoFollow(a.root, a.stageRel, paths.DirPerm)
	if err != nil {
		return restore.StagedPayload{}, err
	}
	defer func() { _ = dir.Close() }()

	rc, err := open()
	if err != nil {
		return restore.StagedPayload{}, err
	}
	defer func() { _ = rc.Close() }()

	tmp, tmpName, err := fsx.CreateTempAt(dir, ".blob-")
	if err != nil {
		return restore.StagedPayload{}, err
	}
	kept := false
	defer func() {
		if !kept {
			_ = tmp.Close()
			_ = fsx.RemoveAt(dir, tmpName)
		}
	}()

	w, sum := a.hasher.NewWriter()
	size, err := io.Copy(io.MultiWriter(tmp, w), rc)
	if err != nil {
		return restore.StagedPayload{}, err
	}
	if got := sum(); got != content {
		return restore.StagedPayload{}, fmt.Errorf("%w: staged bytes hash %s, source addresses %s", restore.ErrStagedContentMismatch, got, content)
	}
	if err := tmp.Sync(); err != nil {
		return restore.StagedPayload{}, err
	}
	if err := tmp.Close(); err != nil {
		return restore.StagedPayload{}, err
	}
	// Publish under the content-addressed name. A concurrent staging of the same
	// content would lose this rename harmlessly — the bytes are identical by
	// construction — but staging is single-threaded within one invocation.
	if err := fsx.RenameAt(dir, tmpName, dir, name); err != nil {
		return restore.StagedPayload{}, err
	}
	kept = true
	a.staged++
	return restore.NewStagedPayload(content, size, name)
}

// Payload returns the capability for content already staged in this invocation,
// re-proving that the staged file is still present and still a regular file. It is
// how apply rebuilds an executable operation from the spooled plan without holding
// a payload map whose size would scale with the plan. A never-staged content
// surfaces as os.ErrNotExist.
func (a *Applier) Payload(content hashing.ContentHash) (restore.StagedPayload, error) {
	if content.IsZero() {
		return restore.StagedPayload{}, fmt.Errorf("worktreemut: a staged payload requires a content hash")
	}
	name := stageName(content)
	f, err := fsx.OpenNoFollowAt(a.root, a.stageRel+"/"+name)
	if err != nil {
		return restore.StagedPayload{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return restore.StagedPayload{}, err
	}
	if !info.Mode().IsRegular() {
		return restore.StagedPayload{}, fmt.Errorf("worktreemut: staged payload for %s is not a regular file", content)
	}
	return restore.NewStagedPayload(content, info.Size(), name)
}

// Staged reports how many payloads were verified into staging.
func (a *Applier) Staged() int { return a.staged }

// Discard removes the staging area. It only ever touches awa's own temp
// directory, never a worktree path. It is idempotent.
func (a *Applier) Discard() error {
	parent, base := splitRel(a.stageRel)
	dir, err := fsx.OpenDirNoFollow(a.root, parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := fsx.RemoveTreeAt(dir, base); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Apply installs one staged operation. It opens the destination's parent
// directory no-follow (creating it only for an operation that adds a node),
// re-checks the destination's shape against the planned precondition at that
// descriptor, and then performs exactly the one mutation the operation's kind
// names.
//
// The precondition check here is the last-moment guard: it proves the destination
// is still exactly what the plan proved — present or absent, the same kind, the
// same permission bits, the same symlink target, and for a regular file the same
// content identity re-derived from the descriptor the bytes are read through —
// immediately before acting on it. Shape alone is not enough: an external writer
// can swap different bytes into a file without changing its mode, and the
// whole-selection re-observation the caller takes after staging cannot see a write
// that lands after it. awa cannot lock an external editor, so the honest boundary
// is the gap between this final read and the mutation itself, not a claim of
// exclusivity.
// Every failure path before the first write reports restore.EffectNone, and each
// installer reports EffectPartial when it removed the old node and then could not put
// the new one in place — the one shape where a failed operation still changed the
// worktree.
func (a *Applier) Apply(ctx context.Context, op restore.StagedOperation) restore.MutationResult {
	if op.IsZero() {
		return restore.Untouched(fmt.Errorf("worktreemut: cannot apply an unbuilt operation"))
	}
	if err := ctx.Err(); err != nil {
		return restore.Untouched(err)
	}
	_, base := splitRel(op.Path().String())

	dir, err := a.openParent(op.Path().String(), op.Kind())
	if err != nil {
		return restore.Untouched(err)
	}
	if dir != nil {
		defer func() { _ = dir.Close() }()
	}
	if err := a.checkPrecondition(dir, base, op); err != nil {
		return restore.Untouched(err)
	}

	switch op.Kind() {
	case restore.OpCreate, restore.OpReplace, restore.OpTypeChange, restore.OpRestoreSymlink:
		// The desired shape decides the mechanics, not the operation kind: a
		// type-change can go any of these ways, a create can add a directory just as
		// easily as a file, and a symlink is the desired shape whether the destination
		// was absent, another link, or a file a generator wrote over it.
		switch op.Desired().Kind() {
		case worktree.KindRegular:
			return a.install(dir, base, op)
		case worktree.KindDir:
			return a.installDirectory(dir, base, op)
		case worktree.KindSymlink:
			return a.installSymlink(dir, base, op)
		default:
			return restore.Untouched(fmt.Errorf("%w: %s desires %s", restore.ErrUnsupportedNode, op.Path(), op.Desired().Kind()))
		}
	case restore.OpDeleteFile:
		if err := a.removeFile(dir, base); err != nil {
			return restore.Untouched(err)
		}
		return restore.Completed()
	case restore.OpDeleteEmptyDirectory:
		if err := a.removeEmptyDirectory(dir, base); err != nil {
			return restore.Untouched(err)
		}
		return restore.Completed()
	default:
		return restore.Untouched(fmt.Errorf("worktreemut: cannot apply a %s operation", op.Kind()))
	}
}

// openParent opens the destination's parent directory. For an operation that adds
// or replaces a node the missing components are created; for a deletion nothing is
// created — a parent that is already gone means the work is done or the plan no
// longer describes reality, and creating a directory in order to delete inside it
// would be absurd.
//
// A root-level path has no parent component, so the project root itself is opened.
// Either way the descent is no-follow, so a symlinked ancestor fails closed rather
// than redirecting the write outside the project.
func (a *Applier) openParent(rel string, kind restore.OperationKind) (*os.File, error) {
	creating := kind == restore.OpCreate || kind == restore.OpReplace || kind == restore.OpTypeChange || kind == restore.OpRestoreSymlink
	var (
		f   *os.File
		err error
	)
	if creating {
		f, err = fsx.MkdirParentAllNoFollow(a.root, rel, worktreeDirPerm)
	} else {
		f, err = fsx.OpenParentDirNoFollow(a.root, rel)
	}
	if err != nil {
		return nil, a.classifyPathError(rel, err)
	}
	return f, nil
}

// classifyPathError turns a no-follow traversal refusal into the typed domain
// error the application maps to a closed reason, so a symlinked ancestor is
// reported as exactly that rather than as a raw open failure.
func (a *Applier) classifyPathError(rel string, err error) error {
	switch {
	case fsx.IsSymlinkOpenRejection(err):
		return fmt.Errorf("%w: %s", restore.ErrSymlinkAncestor, rel)
	case errors.Is(err, fsx.ErrNotDirectory):
		return fmt.Errorf("%w: %s is not a directory", restore.ErrPreconditionMismatch, rel)
	default:
		return err
	}
}

// checkPrecondition compares the destination's observed shape against the state
// the plan proved. Every mismatch is ErrPreconditionMismatch: a conflict the
// caller reports, never a plan it silently refreshes.
func (a *Applier) checkPrecondition(dir *os.File, base string, op restore.StagedOperation) error {
	want := op.Current()
	mode, _, err := fsx.LstatAt(dir, base)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if want.Present() {
			return fmt.Errorf("%w: %s was %s and is now absent", restore.ErrPreconditionMismatch, op.Path(), want.Describe())
		}
		return nil
	case err != nil:
		return a.classifyPathError(op.Path().String(), err)
	}
	if !want.Present() {
		return fmt.Errorf("%w: %s was absent and now exists", restore.ErrPreconditionMismatch, op.Path())
	}
	switch want.Kind() {
	case worktree.KindRegular:
		if !mode.IsRegular() {
			return fmt.Errorf("%w: %s was a file and is now %s", restore.ErrPreconditionMismatch, op.Path(), describeMode(mode))
		}
		if got := uint32(mode.Perm()); got != want.Mode() {
			return fmt.Errorf("%w: %s permission bits are %04o, planned from %04o", restore.ErrPreconditionMismatch, op.Path(), got, want.Mode())
		}
		return a.checkContent(dir, base, op, want)
	case worktree.KindDir:
		if !mode.IsDir() {
			return fmt.Errorf("%w: %s was a directory and is now %s", restore.ErrPreconditionMismatch, op.Path(), describeMode(mode))
		}
	case worktree.KindSymlink:
		if mode&os.ModeSymlink == 0 {
			return fmt.Errorf("%w: %s was a symlink and is now %s", restore.ErrPreconditionMismatch, op.Path(), describeMode(mode))
		}
		target, err := fsx.ReadlinkAt(dir, base)
		if err != nil {
			return err
		}
		if target != want.SymlinkTarget().String() {
			return fmt.Errorf("%w: %s points at %q, planned from %q", restore.ErrPreconditionMismatch, op.Path(), target, want.SymlinkTarget())
		}
	default:
		return fmt.Errorf("%w: %s", restore.ErrUnsupportedNode, op.Path())
	}
	return nil
}

// checkContent re-derives a regular destination's content identity immediately
// before the mutation and requires it to be the identity the plan proved.
//
// This is the guard the shape checks cannot be: a build tool or an editor can
// write different bytes into a file without changing its kind or its permission
// bits, and the whole-selection re-observation the caller takes before the commit
// is blind to anything written after it. Overwriting or deleting on a stale
// content proof would destroy work no evidence describes — the recovery
// observation was recorded before those bytes existed and cannot bring them back.
//
// Everything is answered through ONE no-follow descriptor: the shape is re-checked
// with fstat on the open file rather than trusting the lstat above, so a name
// swapped between the two cannot slip a different object past the guard, and the
// bytes hashed are the bytes of the object that was checked.
func (a *Applier) checkContent(dir *os.File, base string, op restore.StagedOperation, want restore.NodeState) error {
	f, err := fsx.OpenFileAt(dir, base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s was %s and is now absent", restore.ErrPreconditionMismatch, op.Path(), want.Describe())
		}
		return a.classifyPathError(op.Path().String(), err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s was a file and is now %s", restore.ErrPreconditionMismatch, op.Path(), describeMode(info.Mode()))
	}
	if got := uint32(info.Mode().Perm()); got != want.Mode() {
		return fmt.Errorf("%w: %s permission bits are %04o, planned from %04o", restore.ErrPreconditionMismatch, op.Path(), got, want.Mode())
	}
	got, err := a.hasher.HashReader(f)
	if err != nil {
		return err
	}
	if got != want.Content() {
		return fmt.Errorf("%w: %s now holds %s, planned from %s", restore.ErrPreconditionMismatch, op.Path(), got, want.Content())
	}
	return nil
}

// describeMode renders an observed shape for a conflict message.
func describeMode(m os.FileMode) string {
	switch {
	case m&os.ModeSymlink != 0:
		return "a symlink"
	case m.IsDir():
		return "a directory"
	case m.IsRegular():
		return "a file"
	default:
		return "a special file"
	}
}

// install writes a regular file from staged bytes. The bytes are copied from the
// awa-owned stage into a temp file in the destination's own directory, re-hashed
// from that same temp descriptor, and only then renamed over the destination — so
// a destination is never left holding a half-written or unverified file, and the
// copy that lands is the copy that was checked.
//
// A type change from a directory is the one case that needs the old node removed
// first: a rename cannot replace a directory. The directory must already be empty,
// which the plan's child operations have made true by the time this runs; if it is
// not, the deletion refuses rather than taking unobserved content with it.
func (a *Applier) install(dir *os.File, base string, op restore.StagedOperation) restore.MutationResult {
	removed := false
	if op.Kind() == restore.OpTypeChange && op.Current().Present() && op.Current().Kind() == worktree.KindDir {
		if err := a.removeEmptyDirectory(dir, base); err != nil {
			return restore.Untouched(err)
		}
		// From here the destination is already gone, so every failure below reports an
		// interrupted mutation: the worktree no longer holds what the plan proved, even
		// though this operation did not complete.
		removed = true
		if err := a.checkFail(stageAfterRemoval, op.Path()); err != nil {
			return restore.Interrupted(err)
		}
	}
	staged, err := a.openStaged(op.Payload().Handle())
	if err != nil {
		return failedAfter(removed, err)
	}
	defer func() { _ = staged.Close() }()

	want := op.Desired().Content()
	perm := os.FileMode(op.Desired().Mode()).Perm()
	err = fsx.ReplaceFileAt(dir, base, perm,
		func(w io.Writer) error {
			if _, cerr := io.Copy(w, staged); cerr != nil {
				return cerr
			}
			if ferr := a.checkFail(stageBeforeRename, op.Path()); ferr != nil {
				return ferr
			}
			return nil
		},
		func(r io.Reader) error {
			got, herr := a.hasher.HashReader(r)
			if herr != nil {
				return herr
			}
			if got != want {
				return fmt.Errorf("worktreemut: replacement copy for %s hashes %s, want %s", op.Path(), got, want)
			}
			return nil
		})
	if err != nil {
		return failedAfter(removed, a.classifyPathError(op.Path().String(), err))
	}
	return restore.Completed()
}

// failedAfter reports a failure that happened after the destination was, or was not,
// already removed. It exists so each installer states that relationship once instead
// of repeating the conditional at every error return.
func failedAfter(removed bool, err error) restore.MutationResult {
	if removed {
		return restore.Interrupted(err)
	}
	return restore.Untouched(err)
}

// installSymlink installs the link the source proves. A same-directory temp link
// renamed over the destination replaces an absent entry, another link, or a regular
// file atomically, so the common case — a generator that wrote a real file over a
// symlink — needs no removal at all. Only a directory in the way has to go first: a
// rename cannot replace one, and that removal is the same proved, empty-only one
// every deletion here uses.
func (a *Applier) installSymlink(dir *os.File, base string, op restore.StagedOperation) restore.MutationResult {
	removed := false
	if cur := op.Current(); cur.Present() && cur.Kind() == worktree.KindDir {
		if err := a.removeEmptyDirectory(dir, base); err != nil {
			return restore.Untouched(err)
		}
		removed = true
		if err := a.checkFail(stageAfterRemoval, op.Path()); err != nil {
			return restore.Interrupted(err)
		}
	}
	if err := fsx.ReplaceSymlinkAt(dir, base, op.Desired().SymlinkTarget().String()); err != nil {
		return failedAfter(removed, a.classifyPathError(op.Path().String(), err))
	}
	return restore.Completed()
}

// installDirectory materializes a directory the source proves. When the
// destination currently holds something else, that node is removed first — a
// directory cannot be created over a file — and the removal is the same proved,
// non-recursive one every deletion uses: a directory in the way must already be
// empty, so unobserved content is never taken along.
func (a *Applier) installDirectory(dir *os.File, base string, op restore.StagedOperation) restore.MutationResult {
	removed := false
	if cur := op.Current(); cur.Present() {
		switch cur.Kind() {
		case worktree.KindRegular, worktree.KindSymlink:
			if err := a.removeFile(dir, base); err != nil {
				return restore.Untouched(err)
			}
			removed = true
			if err := a.checkFail(stageAfterRemoval, op.Path()); err != nil {
				return restore.Interrupted(err)
			}
		case worktree.KindDir:
			// Already a directory; nothing to replace. (An equal pair would not have
			// produced a mutating operation, so this is the defensive branch.)
			return restore.Completed()
		default:
			return restore.Untouched(fmt.Errorf("%w: %s", restore.ErrUnsupportedNode, op.Path()))
		}
	}
	created, err := fsx.MkdirAllAt(dir, base, worktreeDirPerm)
	if err != nil {
		return failedAfter(removed, a.classifyPathError(op.Path().String(), err))
	}
	// The directory exists; the descriptor is only how it was created. Closing it can
	// fail nothing about the worktree, and reporting that as an operation failure would
	// mean describing a completed mutation as a stop — so it is released like every
	// other descriptor this package finishes with.
	_ = created.Close()
	return restore.Completed()
}

// openStaged opens one staged payload for reading through the no-follow boundary.
func (a *Applier) openStaged(handle string) (*os.File, error) {
	if handle == "" {
		return nil, fmt.Errorf("worktreemut: staged payload has no handle")
	}
	f, err := fsx.OpenNoFollowAt(a.root, a.stageRel+"/"+handle)
	if err != nil {
		return nil, fmt.Errorf("worktreemut: opening staged payload: %w", err)
	}
	return f, nil
}

// removeFile unlinks a proved regular file or symlink. The shape was checked at
// this same descriptor immediately above, and unlinkat cannot descend, so this
// can never remove more than the one node the plan proved.
func (a *Applier) removeFile(dir *os.File, base string) error {
	if err := fsx.RemoveAt(dir, base); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Someone else removed it between the guard and the unlink. The desired end
			// state is exactly this, but the plan no longer describes what happened, so
			// report it rather than counting work awa did not do.
			return fmt.Errorf("%w: %s vanished before it could be removed", restore.ErrPreconditionMismatch, base)
		}
		return err
	}
	return nil
}

// removeEmptyDirectory removes a directory only if it is already empty. This is
// the one deletion awa can prove is safe: an entry restore never observed means
// the proof is incomplete, so it refuses instead of taking that content with it.
// awa never calls a recursive removal on a worktree path.
func (a *Applier) removeEmptyDirectory(dir *os.File, base string) error {
	if err := fsx.RemoveDirAt(dir, base); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("%w: %s vanished before it could be removed", restore.ErrPreconditionMismatch, base)
		case isNotEmpty(err):
			return fmt.Errorf("%w: %s still holds an entry restore did not observe", restore.ErrDirectoryNotEmpty, base)
		default:
			return err
		}
	}
	return nil
}

// splitRel splits a slash-separated project-relative path into its parent
// directory (empty for a root-level entry) and its final component.
func splitRel(rel string) (dir, base string) {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}
