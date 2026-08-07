//go:build !darwin && !linux && !freebsd

package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errSymlinkRejected marks a symlink refused by the non-atomic fallback open, so
// IsSymlinkOpenRejection classifies it like a real O_NOFOLLOW ELOOP.
var errSymlinkRejected = errors.New("path is a symlink")

// OpenNoFollow opens path for reading. Without O_NOFOLLOW the open cannot be made
// symlink-atomic, so it rejects a symlink with a pre-open Lstat (a check-then-open
// race the descriptor-relative platforms avoid) and then opens. The symlink contract is
// thus preserved everywhere; only its atomicity is weaker here.
func OpenNoFollow(path string) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: errSymlinkRejected}
	}
	return os.Open(path)
}

// OpenNoFollowAt opens rel under root, falling back to a single no-follow open of
// the joined path (final-component protection only) where component-wise openat is
// unavailable. The descriptor-relative platforms (darwin, linux, freebsd) reject a
// symlink at every component.
//
// A file standing where a directory belongs is store corruption with its own sentinel,
// so it is separated here from every other reason an open can fail. The condition is what
// the filesystem holds, not which status it reported: on Windows syscall.ENOTDIR is an
// alias for ERROR_PATH_NOT_FOUND — a real status, not a POSIX stand-in — but the same
// status also covers an ancestor that merely does not exist, which unix reports
// separately as ENOENT. Keying on it alone would report every absent path as corruption
// and, by wrapping it, destroy the os.ErrNotExist identity callers rely on to treat
// absence as nothing-to-do. Inspecting the components answers it for any platform this
// file serves, without depending on which of several plausible statuses a given Windows
// build chooses.
//
// The classification lives here and not in OpenNoFollow because it needs root: deciding
// it means walking components, and which components belong to the caller's tree — rather
// than to the machine's own layout, where a link is nobody's corruption — is exactly what
// root says. OpenNoFollow takes a bare path and so returns the platform error unclassified.
func OpenNoFollowAt(root, rel string) (*os.File, error) {
	if err := checkRel(rel); err != nil {
		return nil, err
	}
	f, err := OpenNoFollow(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil && hasNonDirectoryAncestorUnder(root, rel) {
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return f, err
}

// StatNoFollowAt reports whether rel under root exists and is stat-able without opening
// it for reading and without following a symlink final component. It is a metadata-only
// capability: it never returns a readable descriptor, so a caller structurally cannot read
// payload bytes through it. Here it Lstats the joined path (final-component no-follow
// only, matching this platform's weaker OpenNoFollowAt contract). It returns nil when the
// target stats as a non-symlink, os.ErrNotExist when absent, and another error when the
// final component is a symlink or cannot be stat'd.
func StatNoFollowAt(root, rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	// A root-level entry has no parent component below root; the joined path is the
	// same either way here, so the shared helper is only consulted for its error
	// classification on a symlinked or missing ancestor.
	parent, err := OpenParentDirNoFollow(root, rel)
	if err != nil {
		return err
	}
	_ = parent.Close()
	fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return &os.PathError{Op: "lstat", Path: rel, Err: errSymlinkRejected}
	}
	return nil
}

// CheckNoSymlinkPath verifies that no existing component of rel under root is a
// symlink, walking the components left to right with Lstat. Each prefix is checked
// only after the prior was confirmed a non-symlink, so following an intermediate
// component is safe; a missing component ends the walk (nothing existing can
// redirect past it). Without openat this is a check-then-act race the supported
// platforms avoid, but it preserves the fail-closed contract everywhere.
func CheckNoSymlinkPath(root, rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	cur := root
	for _, c := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, c)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return &os.PathError{Op: "lstat", Path: cur, Err: errSymlinkRejected}
		}
	}
	return nil
}

// MkdirAllNoFollow preflights relDir for symlinked components, then creates it with
// the ordinary path API and opens the leaf. Without openat the create is not atomic
// against an ancestor symlink swapped in after the preflight; the descriptor-relative
// platforms create each component with mkdirat from a verified fd.
func MkdirAllNoFollow(root, relDir string, perm os.FileMode) (*os.File, error) {
	if err := CheckNoSymlinkPath(root, relDir); err != nil {
		return nil, err
	}
	abs := filepath.Join(root, filepath.FromSlash(relDir))
	if err := os.MkdirAll(abs, perm); err != nil {
		// MkdirAll fails if the leaf (or a component) exists as a non-directory:
		// report that as ErrNotDirectory so callers map it to store corruption.
		if fi, lerr := os.Lstat(abs); lerr == nil && !fi.IsDir() {
			return nil, fmt.Errorf("%w: %s", ErrNotDirectory, abs)
		}
		return nil, err
	}
	f, err := os.Open(abs)
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
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, abs)
	}
	return f, nil
}

// MkdirAllAt is MkdirAllNoFollow anchored at an already-open directory — in NAME
// only. Without openat the anchor can be re-derived solely from its path, so an
// "At" call here is a convenience that spares the caller the join; it is emphatically
// not the identity anchoring the same call gives on darwin, linux, and freebsd, and a
// caller whose safety depends on writing into one specific inode does not get that
// safety from this function. It must verify identity itself and must describe the
// weaker guarantee it actually has, exactly as the rest of this file does for its own
// check-then-act races.
func MkdirAllAt(dir *os.File, relDir string, perm os.FileMode) (*os.File, error) {
	return MkdirAllNoFollow(dir.Name(), relDir, perm)
}

// CreateTempAt creates a temp file inside the directory dir refers to and returns it
// with its base name.
func CreateTempAt(dir *os.File, prefix string) (*os.File, string, error) {
	f, err := os.CreateTemp(dir.Name(), prefix)
	if err != nil {
		return nil, "", err
	}
	return f, filepath.Base(f.Name()), nil
}

// LinkAt hard-links oldName under oldDir to newName under newDir, preserving
// os.ErrExist for an existing target.
func LinkAt(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	if err := checkLeafName(oldName); err != nil {
		return err
	}
	if err := checkLeafName(newName); err != nil {
		return err
	}
	return os.Link(filepath.Join(oldDir.Name(), oldName), filepath.Join(newDir.Name(), newName))
}

// RenameAt renames oldName under oldDir to newName under newDir.
func RenameAt(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	if err := checkLeafName(oldName); err != nil {
		return err
	}
	if err := checkLeafName(newName); err != nil {
		return err
	}
	return os.Rename(filepath.Join(oldDir.Name(), oldName), filepath.Join(newDir.Name(), newName))
}

// RemoveAt removes name under dir.
func RemoveAt(dir *os.File, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir.Name(), name))
}

// RemoveDirAt removes the EMPTY directory name under dir. It is the non-destructive
// counterpart to RemoveTreeAt: a caller undoing its own work can only reclaim a
// directory it actually emptied, and a directory holding anything it does not own
// fails rather than taking that content with it. Like the rest of this file it can
// only join the name to the parent's path, so it carries the weaker, non-atomic
// ancestor-symlink guarantee; os.Remove refuses to descend either way.
func RemoveDirAt(dir *os.File, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir.Name(), name))
}

// SyncDir is a no-op on platforms outside the supported set, where a directory
// fsync is unsupported or carries different semantics. This matches the rest of
// this file's weaker, best-effort durability posture; the durable-ordering
// guarantee that surfaces real sync errors holds only on the supported platforms.
func SyncDir(*os.File) error { return nil }

// LstatAt reports the mode and size of name under dir without following a final
// symlink. It returns the FULL mode — type bits included — because the caller's
// question is usually "is this still the shape I proved it was?". Like the rest of
// this file it can only join the name to the parent's path, so it carries the
// weaker, non-atomic ancestor guarantee; the supported platforms fstatat relative
// to the held descriptor. An absent name surfaces as os.ErrNotExist.
func LstatAt(dir *os.File, name string) (os.FileMode, int64, error) {
	if err := checkLeafName(name); err != nil {
		return 0, 0, err
	}
	fi, err := os.Lstat(filepath.Join(dir.Name(), name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, os.ErrNotExist
		}
		return 0, 0, err
	}
	return fi.Mode(), fi.Size(), nil
}

// OpenFileAt opens name under dir for reading, rejecting a symlink. Without
// O_NOFOLLOW the refusal is a pre-open Lstat rather than part of the open itself,
// so it carries this file's weaker, non-atomic guarantee: the symlink contract
// holds everywhere, only its atomicity is weaker here. An absent name surfaces as
// os.ErrNotExist.
func OpenFileAt(dir *os.File, name string) (*os.File, error) {
	if err := checkLeafName(name); err != nil {
		return nil, err
	}
	target := filepath.Join(dir.Name(), name)
	fi, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: name, Err: errSymlinkRejected}
	}
	return os.Open(target) //nolint:gosec // the name is a checked single component under a held directory
}

// ReadlinkAt reads the target of the symlink name under dir. It carries this
// file's weaker ancestor guarantee. A name that is not a symlink fails.
func ReadlinkAt(dir *os.File, name string) (string, error) {
	if err := checkLeafName(name); err != nil {
		return "", err
	}
	target, err := os.Readlink(filepath.Join(dir.Name(), name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	if len(target) > maxSymlinkTargetBytes {
		return "", fmt.Errorf("fsx: symlink target for %s exceeds %d bytes", name, maxSymlinkTargetBytes)
	}
	return target, nil
}

// SymlinkAt creates a symlink named name under dir pointing at target. The target
// is stored verbatim and never resolved. An existing name fails with os.ErrExist,
// which is what makes the temp-then-rename replacement atomic. It carries this
// file's weaker ancestor guarantee.
func SymlinkAt(dir *os.File, target, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("fsx: refusing to create symlink %s with an empty target", name)
	}
	return os.Symlink(target, filepath.Join(dir.Name(), name))
}

// RemoveTreeAt recursively removes name beneath dir. The supported platforms pin
// every level to a no-follow directory descriptor; this fallback can only join the
// name to the parent's path and remove that, so it carries the same weaker,
// non-atomic symlink guarantee as the rest of this file. A leaf that is itself a
// symlink is rejected rather than removed through; an absent name is not an error.
func RemoveTreeAt(dir *os.File, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	target := filepath.Join(dir.Name(), name)
	info, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &os.PathError{Op: "removetree", Path: name, Err: errSymlinkRejected}
	}
	return os.RemoveAll(target)
}

// IsSymlinkOpenRejection reports whether err is the fallback open refusing a
// symlink, mirroring the ELOOP classification on the supported platforms.
func IsSymlinkOpenRejection(err error) bool {
	return errors.Is(err, errSymlinkRejected)
}
