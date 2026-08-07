// Package blobstore implements the blob.Store port over the filesystem.
//
// It owns the on-disk content-addressed store under .awa/store: the sharded blob
// layout, atomic publication through .awa/store/tmp, and streaming hash
// verification. It is the only blob-related package that touches os/filepath, so
// the application service depends on the blob.Store interface and never on these
// details.
package blobstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"awarer/internal/domain/blob"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/fsx"
)

// streamHasher is the slice of a hasher the store needs: a streaming digest it
// can tee content through. It is a local interface (not the domain Hasher port)
// so the store depends only on the one capability it uses; the blake3hash.Hasher
// satisfies it.
type streamHasher interface {
	NewWriter() (io.Writer, func() hashing.ContentHash)
}

// blobPerm is the mode for a published blob. Blobs are immutable, so they are
// stored read-only to make accidental in-place modification fail loudly, and
// owner-only so a private source file can never become world-readable through the
// content store.
const blobPerm = paths.ReadOnlyFilePerm

// FS is a filesystem-backed blob store. Construct it with New; the zero value is
// not usable.
type FS struct {
	blobsDir string
	tmpDir   string
	root     string // project root: trusted anchor for no-follow blob reads
	hasher   streamHasher
	// link publishes a blob by hard-linking the temp file into the verified shard
	// directory — fsx.LinkAt in production, whose ErrExist-on-existing-target is what
	// makes publication atomic and whose descriptor-relative linkat cannot be
	// redirected by a symlinked ancestor. It is injected so a test can drive the
	// concurrent-publish race without any package-global mutable state.
	link func(oldDir *os.File, oldName string, newDir *os.File, newName string) error
	// fail is a test-only fault-injection seam: nil in production (a single branch),
	// set only from same-package _test.go. A hook returning a plain error exercises
	// the live rollback path; returning errCrash signals the deferred cleanup to
	// leave the temp file behind, simulating a true crash window a fresh process must
	// recover from.
	fail func(stage failStage) error
}

// failStage names a fault-injection point inside Materialize. The constants are the
// only stages the seam recognizes.
type failStage string

const (
	failBeforeTemp          failStage = "before-temp"
	failAfterTempBeforeLink failStage = "after-temp-before-link"
)

// errCrash, returned by a fail hook, means "simulate a crash here": skip the
// rollback so on-disk partial state survives for a fresh process to observe.
var errCrash = errors.New("blobstore: simulated crash (test seam)")

// checkFail invokes the fail hook (if any) for stage, recording whether it asked
// for a crash-style leftover. It returns the hook's error so the caller aborts
// through the normal error path.
func (s *FS) checkFail(stage failStage, crashed *bool) error {
	if s.fail == nil {
		return nil
	}
	err := s.fail(stage)
	if errors.Is(err, errCrash) {
		*crashed = true
	}
	return err
}

// New builds a blob store over a project's layout. A nil hasher is a wiring error
// and panics at construction.
func New(layout paths.Layout, hasher streamHasher) *FS {
	return newWithLink(layout, hasher, fsx.LinkAt)
}

// newWithLink builds an FS with an explicit publish primitive. Production goes
// through New (fsx.LinkAt); tests use this to fake the link and exercise the
// concurrent-publish race.
func newWithLink(layout paths.Layout, hasher streamHasher, link func(oldDir *os.File, oldName string, newDir *os.File, newName string) error) *FS {
	if hasher == nil {
		panic("blobstore.New: hasher must not be nil")
	}
	return &FS{blobsDir: layout.BlobsDir(), tmpDir: layout.TmpDir(), root: layout.Root(), hasher: hasher, link: link}
}

// finalPath resolves the absolute on-disk path for a content hash.
func (s *FS) finalPath(h hashing.ContentHash) (string, error) {
	bp, err := blob.PathFor(h)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.blobsDir, filepath.FromSlash(bp.Rel())), nil
}

// Has reports whether the blob for h exists on disk. It is a cheap existence check
// (no content read); Materialize performs the integrity check before trusting an
// existing blob. It uses the same root-anchored component-wise no-follow policy as
// Open, so a blob reached only through a symlinked shard directory is reported as
// corruption — not a present blob — keeping Has and Open consistent.
func (s *FS) Has(h hashing.ContentHash) (bool, error) {
	f, err := s.openRegular(h)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_ = f.Close()
	return true, nil
}

// Open returns a reader for an existing blob through the shared no-follow open.
func (s *FS) Open(h hashing.ContentHash) (io.ReadCloser, error) {
	return s.openRegular(h)
}

// openRegular opens the blob for h through a component-wise no-follow traversal
// rooted at the project root and confirms the opened descriptor is a regular file.
// It is the shared core of Has and Open: neither a symlink planted at the blob path
// nor a symlinked shard directory above it can redirect a content address to bytes
// that no longer match its hash, and none can do so by being swapped in after a
// separate shape check, since the regular-node check is on the very descriptor the
// bytes are read from. A missing blob surfaces as os.ErrNotExist for the caller to
// interpret.
func (s *FS) openRegular(h hashing.ContentHash) (*os.File, error) {
	abs, err := s.finalPath(h)
	if err != nil {
		return nil, err
	}
	rel, err := fsx.RelUnder(s.root, abs)
	if err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollowAt(s.root, rel)
	if err != nil {
		switch {
		case fsx.IsSymlinkOpenRejection(err):
			return nil, fmt.Errorf("%w: %s is reached through a symlink", blob.ErrCorruptBlob, abs)
		case errors.Is(err, fsx.ErrNotDirectory):
			return nil, fmt.Errorf("%w: %s is reached through a non-directory", blob.ErrCorruptBlob, abs)
		}
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file (%s)", blob.ErrCorruptBlob, abs, info.Mode().Type())
	}
	return f, nil
}

// mkStoreDir creates absDir (if needed) and returns its directory descriptor,
// descending component by component from the project root and refusing to traverse a
// symlink, so the returned fd is pinned to a real directory inside the project. A
// symlinked store ancestor is reported as ErrCorruptBlob. The caller closes the fd;
// any *at write through it (temp create, hard link, unlink) cannot be redirected
// outside the project even if an ancestor is swapped afterward.
func (s *FS) mkStoreDir(absDir string) (*os.File, error) {
	rel, err := fsx.RelUnder(s.root, absDir)
	if err != nil {
		return nil, err
	}
	dir, err := fsx.MkdirAllNoFollow(s.root, rel, paths.DirPerm)
	if err != nil {
		switch {
		case fsx.IsSymlinkOpenRejection(err):
			return nil, fmt.Errorf("%w: %s is reached through a symlink", blob.ErrCorruptBlob, absDir)
		case errors.Is(err, fsx.ErrNotDirectory):
			return nil, fmt.Errorf("%w: %s is not a directory", blob.ErrCorruptBlob, absDir)
		}
		return nil, err
	}
	return dir, nil
}

// Materialize streams the source through the hasher and a temp file, verifies the
// computed hash equals expected, then publishes the temp file into the sharded
// location. It is idempotent (an existing blob is reused after its bytes are
// verified) and atomic (a partial or mismatched write never becomes a published
// blob). It returns blob.ErrHashMismatch when freshly read source bytes do not
// hash to expected, and blob.ErrCorruptBlob when an already-stored blob does not
// hash to its address; in both cases no blob is published, so a manifest never
// references bytes that were not actually stored.
func (s *FS) Materialize(expected hashing.ContentHash, open func() (io.ReadCloser, error)) (ref blob.BlobRef, written bool, err error) {
	if has, herr := s.Has(expected); herr != nil {
		return blob.BlobRef{}, false, herr
	} else if has {
		// A present blob is trusted only after its bytes are confirmed to hash to
		// the address it lives under: a content-addressed store that reused a
		// corrupt or truncated blob would let a checkpoint reference wrong content.
		if verr := s.verifyExisting(expected); verr != nil {
			return blob.BlobRef{}, false, verr
		}
		r, rerr := blob.NewBlobRef(expected)
		return r, false, rerr
	}

	finalPath, err := s.finalPath(expected)
	if err != nil {
		return blob.BlobRef{}, false, err
	}
	finalName := filepath.Base(finalPath)

	crashed := false
	if err := s.checkFail(failBeforeTemp, &crashed); err != nil {
		return blob.BlobRef{}, false, err
	}

	// Open the temp directory through a verified, symlink-free descriptor and create
	// the temp blob inside it: a temp file is never created outside the project.
	tmpDir, err := s.mkStoreDir(s.tmpDir)
	if err != nil {
		return blob.BlobRef{}, false, err
	}
	defer func() { _ = tmpDir.Close() }()
	tmp, tmpName, err := fsx.CreateTempAt(tmpDir, "blob-")
	if err != nil {
		return blob.BlobRef{}, false, fmt.Errorf("creating temp blob: %w", err)
	}
	// Remove the temp file on any failure. On success it is removed explicitly after
	// the link, so the guard is a no-op then. A simulated crash (errCrash) leaves the
	// temp file behind so a fresh process sees the same orphan a real crash would.
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			if !crashed {
				_ = fsx.RemoveAt(tmpDir, tmpName)
			}
		}
	}()

	rc, err := open()
	if err != nil {
		return blob.BlobRef{}, false, fmt.Errorf("opening source for %s: %w", expected, err)
	}
	defer func() { _ = rc.Close() }()

	hw, finalize := s.hasher.NewWriter()
	if _, err := io.Copy(io.MultiWriter(tmp, hw), rc); err != nil {
		return blob.BlobRef{}, false, fmt.Errorf("streaming blob content: %w", err)
	}

	computed := finalize()
	if computed != expected {
		return blob.BlobRef{}, false, fmt.Errorf("%w: source for %s hashed to %s", blob.ErrHashMismatch, expected, computed)
	}

	if err := tmp.Sync(); err != nil {
		return blob.BlobRef{}, false, fmt.Errorf("syncing temp blob: %w", err)
	}
	if err := tmp.Chmod(blobPerm); err != nil {
		return blob.BlobRef{}, false, fmt.Errorf("setting blob permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return blob.BlobRef{}, false, fmt.Errorf("closing temp blob: %w", err)
	}

	// Open the shard directory through a verified, symlink-free descriptor so the
	// hard-link publish lands inside the project even if an ancestor is swapped.
	shardDir, err := s.mkStoreDir(filepath.Dir(finalPath))
	if err != nil {
		return blob.BlobRef{}, false, err
	}
	defer func() { _ = shardDir.Close() }()

	if err := s.checkFail(failAfterTempBeforeLink, &crashed); err != nil {
		return blob.BlobRef{}, false, err
	}

	// Publish by hard-linking: the link is atomic and fails if the target exists.
	// A concurrent writer may have won the race; rather than trust its bytes
	// blindly, verify the existing blob hashes to its address — the same integrity
	// guard the Has path applies — before treating it as a reused blob.
	if err := s.link(tmpDir, tmpName, shardDir, finalName); err != nil {
		if errors.Is(err, os.ErrExist) {
			if verr := s.verifyExisting(expected); verr != nil {
				return blob.BlobRef{}, false, verr
			}
			r, rerr := blob.NewBlobRef(expected)
			return r, false, rerr
		}
		return blob.BlobRef{}, false, fmt.Errorf("publishing blob: %w", err)
	}
	_ = shardDir.Sync()

	_ = fsx.RemoveAt(tmpDir, tmpName)
	committed = true

	r, err := blob.NewBlobRef(expected)
	if err != nil {
		return blob.BlobRef{}, false, err
	}
	return r, true, nil
}

// verifyExisting streams an already-stored blob back through the hasher and
// confirms it hashes to its address. A mismatch is store corruption, reported as
// ErrCorruptBlob so the checkpoint aborts rather than referencing bad bytes.
func (s *FS) verifyExisting(expected hashing.ContentHash) error {
	rc, err := s.Open(expected)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	hw, finalize := s.hasher.NewWriter()
	if _, err := io.Copy(hw, rc); err != nil {
		return fmt.Errorf("reading stored blob: %w", err)
	}
	if got := finalize(); got != expected {
		return fmt.Errorf("%w: blob at %s hashes to %s", blob.ErrCorruptBlob, expected, got)
	}
	return nil
}
