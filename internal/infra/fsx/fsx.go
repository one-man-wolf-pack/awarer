package fsx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"awarer/internal/domain/paths"
)

// ErrUnsafeRelPath is returned by OpenNoFollowAt when rel is not a clean,
// root-relative, slash-separated path: empty, absolute, backslash-bearing, or
// containing a "", ".", or ".." component. Rejecting it makes the root boundary the
// helper's own guarantee rather than caller discipline — a component-wise open can
// never traverse outside root.
var ErrUnsafeRelPath = errors.New("fsx: rel must be a clean slash-separated path within root")

// ErrUnsafeName is returned by the directory-relative helpers when a file name is
// not a single path component (empty, ".", "..", or separator-bearing). Rejecting it
// keeps a name argument from escaping the verified directory it is meant to act in.
var ErrUnsafeName = errors.New("fsx: name must be a single path component")

// ErrNotDirectory is returned by ReadDirNoFollow when the opened store path is not a
// directory. A non-directory where a store directory belongs is corruption, which a
// caller can map to its own store-corruption sentinel.
var ErrNotDirectory = errors.New("fsx: not a directory")

// hasNonDirectoryAncestorUnder reports whether some ancestor of rel under root is
// demonstrably a plain non-directory — the condition that makes rel unopenable no matter
// what sits at its own name.
//
// The first such component decides, because everything below it is absent on account of
// it. A walk that finds none means rel is simply missing, which is not this function's
// answer to give.
//
// It never resolves a link, and that is why it walks rel's components outward-in rather
// than simply Lstat-ing the parent. Lstat does not follow the FINAL component, but it
// does follow every one before it: a single Lstat of `link/file/child`'s parent silently
// resolves `link` and then reports on `file`, so the answer would rest on having followed
// exactly what must not be followed. Stopping at the first symlink is what makes the
// claim honest rather than merely usually right.
//
// A symlink therefore ends the walk with no answer. It is not a directory in its own
// right, yet it may well name one, and finding out would mean resolving it. The caller
// then surfaces the platform's original error, which is the honest outcome: the only
// claim this function may make is one it can see without following anything, and an
// unproven claim of corruption is worse than a plain error.
//
// root is the trusted anchor and is not inspected, the same boundary CheckNoSymlinkPath
// draws: only the tree below root must be symlink-free, and a link in the path used to
// reach root is the caller's own business. Without that boundary the walk would climb
// into the machine's layout and refuse to answer for ordinary locations — /var is a
// symlink on darwin, so every path under it would be undecidable.
//
// It lives here rather than beside its one caller in the fallback because nothing about
// it is platform-specific. It is the question the fallback has to ask because Windows
// reports "an ancestor is a file" and "an ancestor is missing" with a single status, and
// asking it in portable code is what lets the answer be tested on any host.
func hasNonDirectoryAncestorUnder(root, rel string) bool {
	parts := strings.Split(rel, "/")
	cur := root
	// The leaf is the caller's target, not an ancestor of it.
	for _, c := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		switch {
		case err != nil:
			// Absent, or unreadable: everything below it is missing on account of that,
			// which is an absence rather than a non-directory.
			return false
		case fi.Mode()&os.ModeSymlink != 0:
			return false
		case !fi.IsDir():
			return true
		}
	}
	return false
}

// checkLeafName enforces that name is a single path component, safe to pass to a
// directory-relative *at operation.
func checkLeafName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return ErrUnsafeName
	}
	return nil
}

// RelUnder converts abs into a slash-separated path relative to root, suitable for
// OpenNoFollowAt. It errors if abs is not within root, so a caller anchoring a
// no-follow open at a trusted root cannot accidentally point it outside. Anchoring
// at the project root (rather than a store subdirectory) means every component down
// to the file — including .awa and the store subdirs — is checked for a symlink,
// while symlinks above the root, which the caller already trusts, are followed.
func RelUnder(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%w: %s is not within %s", ErrUnsafeRelPath, abs, root)
	}
	return rel, nil
}

// splitRel splits a clean slash-separated rel into its parent directory and final
// component. A rel with no slash has an empty parent (the component sits at the root).
func splitRel(rel string) (dir, base string) {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

// OpenDirNoFollow opens the directory at relDir under root through the component-wise
// no-follow traversal and confirms the opened descriptor is a directory, returning
// it. A symlinked component fails (ELOOP) and a non-directory at the path fails with
// ErrNotDirectory, so the returned fd is a real directory inside the project — safe
// to list or to use as the parent for a directory-relative operation. The caller
// closes it.
func OpenDirNoFollow(root, relDir string) (*os.File, error) {
	f, err := OpenNoFollowAt(root, relDir)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, filepath.Join(root, filepath.FromSlash(relDir)))
	}
	return f, nil
}

// ReadDirNoFollow lists the directory at relDir under root, opening it through the
// component-wise no-follow traversal so a symlinked directory (checkpoints/ replaced
// by a link to an outside copy) is rejected instead of silently listing foreign
// contents, and a non-directory is rejected as ErrNotDirectory. The listing is taken
// from the opened directory descriptor, not re-resolved by path.
func ReadDirNoFollow(root, relDir string) ([]os.DirEntry, error) {
	f, err := OpenDirNoFollow(root, relDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadDir(-1)
}

// dirEntryBatch bounds how many directory entries EachDirEntryNoFollow reads per
// syscall, so streaming a very large directory holds only a batch in memory at once.
const dirEntryBatch = 1024

// EachDirEntryNoFollow streams the entries of the directory at relDir under root to fn,
// reading them in bounded batches from the same no-follow directory descriptor
// ReadDirNoFollow opens (see it for the safety rationale). Unlike ReadDirNoFollow it
// never materializes the whole listing, so a directory holding a very large number of
// entries costs O(batch) memory rather than O(entries) — the property bounded callers
// (run-cache count and newest-first iteration) rely on when many entries share one
// shard. fn returning an error stops the walk and propagates it; a missing directory
// surfaces as the OpenDirNoFollow error.
func EachDirEntryNoFollow(root, relDir string, fn func(os.DirEntry) error) error {
	f, err := OpenDirNoFollow(root, relDir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		batch, rerr := f.ReadDir(dirEntryBatch)
		for _, e := range batch {
			if ferr := fn(e); ferr != nil {
				return ferr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// PublishBytesNoFollow atomically and exclusively writes data as name inside relDir
// under root, all through verified directory descriptors so neither the temp file,
// the hard-link publish, nor the directory creation can be redirected by a symlinked
// store ancestor. It fails with an os.ErrExist-wrapped error if name already exists
// (the exclusivity callers rely on to detect a collision). It is built on the
// platform *at primitives, so it is itself platform-independent.
func PublishBytesNoFollow(root, relDir, name string, data []byte, perm os.FileMode) error {
	dir, err := MkdirAllNoFollow(root, relDir, paths.DirPerm)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return PublishBytesAt(dir, name, data, perm)
}

// PublishBytesAt is PublishBytesNoFollow's publishing half, taking an already-open
// directory rather than a path it resolves itself. It fails with an
// os.ErrExist-wrapped error if name already exists.
//
// How strong "at" is depends on the platform, and a caller must not assume more than
// the platform gives. Where the *at primitives below it are real (darwin, linux,
// freebsd) the write lands in the directory the caller holds, not in whatever occupies
// its path by the time the bytes arrive. Where they fall back to joining dir.Name()
// (fsx_other.go) the path is re-resolved, so this is an anchoring CONVENIENCE there,
// not an identity guarantee: a caller whose correctness depends on writing into one
// specific inode must verify that identity itself, and must state the weaker guarantee
// it has.
func PublishBytesAt(dir *os.File, name string, data []byte, perm os.FileMode) (err error) {
	if err := checkLeafName(name); err != nil {
		return err
	}
	tmp, tmpName, err := CreateTempAt(dir, ".tmp-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = RemoveAt(dir, tmpName)
		}
	}()
	if err := writeTemp(tmp, data, perm); err != nil {
		return err
	}
	if err := LinkAt(dir, tmpName, dir, name); err != nil {
		return err
	}
	committed = true
	_ = RemoveAt(dir, tmpName)
	return SyncDir(dir)
}

// PublishStreamNoFollow is PublishBytesNoFollow for a payload produced by a
// writer callback rather than an in-memory buffer: it creates a temp file in
// relDir, lets write stream the contents into it, then atomically and exclusively
// hard-links it to name (failing with os.ErrExist if name already exists). It lets
// a large payload (a manifest) be persisted without first materializing it whole
// in memory. write must not retain or close the writer.
func PublishStreamNoFollow(root, relDir, name string, perm os.FileMode, write func(io.Writer) error) (err error) {
	if err := checkLeafName(name); err != nil {
		return err
	}
	dir, err := MkdirAllNoFollow(root, relDir, paths.DirPerm)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	tmp, tmpName, err := CreateTempAt(dir, ".tmp-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = RemoveAt(dir, tmpName)
		}
	}()
	if err := writeTempStream(tmp, write, perm); err != nil {
		return err
	}
	if err := LinkAt(dir, tmpName, dir, name); err != nil {
		return err
	}
	committed = true
	_ = RemoveAt(dir, tmpName)
	return SyncDir(dir)
}

// ReplaceBytesNoFollow atomically writes data as name inside relDir under root,
// replacing any existing file, through verified directory descriptors. Like
// PublishBytesNoFollow it cannot be redirected by a symlinked store ancestor; unlike
// it, the rename overwrites rather than failing on an existing target.
func ReplaceBytesNoFollow(root, relDir, name string, data []byte, perm os.FileMode) (err error) {
	if err := checkLeafName(name); err != nil {
		return err
	}
	dir, err := MkdirAllNoFollow(root, relDir, paths.DirPerm)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	tmp, tmpName, err := CreateTempAt(dir, ".tmp-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = RemoveAt(dir, tmpName)
		}
	}()
	if err := writeTemp(tmp, data, perm); err != nil {
		return err
	}
	if err := RenameAt(dir, tmpName, dir, name); err != nil {
		return err
	}
	committed = true
	return SyncDir(dir)
}

// OpenParentDirNoFollow opens the directory holding rel's final component,
// descending no-follow from root. A rel with no slash sits directly at the root,
// whose "parent directory" is the root itself — a case the component-wise helpers
// cannot express, because an empty rel is not a valid path. Centralizing it here
// keeps every caller that acts on a root-level entry from having to special-case
// it, and from quietly getting ErrUnsafeRelPath when it forgets. The caller closes
// the returned file.
func OpenParentDirNoFollow(root, rel string) (*os.File, error) {
	relDir, _ := splitRel(rel)
	if relDir == "" {
		return os.Open(root)
	}
	return OpenDirNoFollow(root, relDir)
}

// MkdirParentAllNoFollow is OpenParentDirNoFollow for a write that may need the
// parent chain created first. It creates missing components with perm and returns
// the leaf; a root-level entry needs nothing created.
func MkdirParentAllNoFollow(root, rel string, perm os.FileMode) (*os.File, error) {
	relDir, _ := splitRel(rel)
	if relDir == "" {
		return os.Open(root)
	}
	return MkdirAllNoFollow(root, relDir, perm)
}

// maxSymlinkTargetBytes bounds a symlink target awa will read or write. Real
// targets are path-sized; anything beyond this is a corrupt or hostile record, not
// a reason to allocate without limit.
const maxSymlinkTargetBytes = 1 << 16

// tempReplacePrefix names the same-directory temp file an atomic replacement goes
// through. It is dotted so an ordinary listing does not surface it, and prefixed so
// a human who does see one knows awa left it.
const tempReplacePrefix = ".awa-tmp-"

// ReplaceFileAt atomically installs a regular file named name inside dir, through
// a temp file created in that same directory and renamed over the target. The
// same-directory temp is what makes the rename atomic: a rename across filesystems
// is not, and a destination is left holding either its old bytes or the complete
// new ones, never a half-written file.
//
// write streams the replacement bytes. verify then reads them back from the very
// descriptor they were written to — not by re-opening the path — so what is
// verified is what will be renamed into place, with no window for a swap between
// the check and the install. A verify failure removes the temp and leaves the
// destination untouched.
//
// perm is applied to the temp before it is installed, so the file is never briefly
// visible at the destination with the wrong mode.
//
// Unlike the publish helpers above it does NOT fsync the destination directory
// after the rename, and that is deliberate: its callers write replaceable output —
// the USER's worktree during a restore, a documentation export directory — rather
// than awa's durable state. A crash that loses the rename loses work that can simply
// be run again (a restore's recovery observation records the state from before it,
// and an export is regenerated from the binary), whereas fsyncing a directory per
// written path would charge every one of them for durability it does not need.
//
// That omission is also what makes this the install of choice for a caller whose
// final step must not fail late: the successful rename is the last operation, so
// every error this can report happens while the destination still holds its old
// node, and no caller needs a protocol for withdrawing a file it was told failed.
func ReplaceFileAt(dir *os.File, name string, perm os.FileMode, write func(io.Writer) error, verify func(io.Reader) error) (err error) {
	if err := checkLeafName(name); err != nil {
		return err
	}
	tmp, tmpName, err := CreateTempAt(dir, tempReplacePrefix)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = tmp.Close()
			_ = RemoveAt(dir, tmpName)
		}
	}()
	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if verify != nil {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := verify(tmp); err != nil {
			return err
		}
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// From here the temp is closed; a rename failure still needs the temp removed,
	// so keep installed false until the rename succeeds.
	if err := RenameAt(dir, tmpName, dir, name); err != nil {
		_ = RemoveAt(dir, tmpName)
		installed = true // already removed; do not remove twice through the deferred path
		return err
	}
	installed = true
	return nil
}

// ReplaceSymlinkAt atomically installs a symlink named name under dir pointing at
// target, through a same-directory temp link renamed over the destination. Like
// ReplaceFileAt it leaves the destination holding either its old node or the
// complete new link. The target is written verbatim and never resolved.
func ReplaceSymlinkAt(dir *os.File, name, target string) (err error) {
	if err := checkLeafName(name); err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("fsx: refusing to install symlink %s with an empty target", name)
	}
	if len(target) > maxSymlinkTargetBytes {
		return fmt.Errorf("fsx: symlink target for %s exceeds %d bytes", name, maxSymlinkTargetBytes)
	}
	// Mint a unique temp name the same way ReplaceFileAt does: create and
	// immediately remove a temp file, then take its name for the link. Doing it
	// through CreateTempAt keeps one source of uniqueness and O_EXCL collision
	// handling rather than a second naming scheme.
	tmp, tmpName, err := CreateTempAt(dir, tempReplacePrefix)
	if err != nil {
		return err
	}
	_ = tmp.Close()
	if err := RemoveAt(dir, tmpName); err != nil {
		return err
	}
	if err := SymlinkAt(dir, target, tmpName); err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = RemoveAt(dir, tmpName)
		}
	}()
	if err := RenameAt(dir, tmpName, dir, name); err != nil {
		return err
	}
	installed = true
	return nil
}

// writeTemp writes data to the open temp file, fsyncs it, sets its final mode, and
// closes it, so the published file is durable with the intended permissions.
func writeTemp(tmp *os.File, data []byte, perm os.FileMode) error {
	return writeTempStream(tmp, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}, perm)
}

// writeTempStream streams a payload into tmp via the write callback, then syncs,
// chmods, and closes it. It is the shared finalize step for the byte and stream
// publish/replace helpers, so durability (fsync) and the read-only mode are applied
// identically however the payload was produced.
func writeTempStream(tmp *os.File, write func(io.Writer) error, perm os.FileMode) error {
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	return tmp.Close()
}

// checkRel enforces the OpenNoFollowAt contract on rel.
func checkRel(rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) {
		return ErrUnsafeRelPath
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return ErrUnsafeRelPath
		}
	}
	return nil
}
