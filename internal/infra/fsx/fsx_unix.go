//go:build darwin || linux || freebsd

package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// tempSeq makes within-process temp names unique; O_EXCL catches the rare
// cross-process collision and the caller retries.
var tempSeq atomic.Uint64

// OpenNoFollow opens path for reading without following a final-component symlink.
// If path is a symlink, the open fails rather than resolving it, so reading a regular
// file is atomic with respect to a concurrent swap of the path to a symlink — the gap
// a separate Lstat shape check cannot close. A path that became a symlink (even to a
// hard link with the same inode and content) is rejected at the open itself, not read
// through the new shape.
func OpenNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, noFollowErr(err)
	}
	return f, nil
}

// noFollowErr normalizes the error an O_NOFOLLOW open returns when its target is a
// symlink so it carries the ELOOP this package classifies on, and returns every other
// error untouched. It exists because the errno is not the same everywhere: darwin and
// linux report ELOOP, while FreeBSD's open(2) documents "[EMLINK] O_NOFOLLOW was
// specified and the target is a symbolic link".
//
// It takes either shape the callers below hold — a bare errno, or one already wrapped in
// an os.PathError naming the operation and component — and preserves that naming, since
// which component was refused is what makes the diagnostic actionable.
//
// It is applied at every O_NOFOLLOW open in this file and nowhere else, which is the
// whole point: the errno means "symlink" only in that operation. EMLINK from linkat means
// what it always meant — too many links — and IsSymlinkOpenRejection must not start
// reading it as a symlink refusal wherever it appears.
func noFollowErr(err error) error {
	if !isNoFollowSymlinkErrno(err) {
		return err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return &os.PathError{Op: pe.Op, Path: pe.Path, Err: unix.ELOOP}
	}
	return unix.ELOOP
}

// OpenNoFollowAt opens the file at the slash-separated rel under root, descending
// one component at a time and refusing a symlink at every component. This closes
// the gap OpenNoFollow leaves: an ancestor directory swapped to a symlink between a
// walk and the read (e.g. root/dir replaced by a link to an outside directory
// holding a hard link of the same file) cannot redirect the open, because each
// component is opened with O_NOFOLLOW as the trailing element of its own openat, so
// a symlink there fails the open. O_DIRECTORY is deliberately not set on
// intermediates: combined with O_NOFOLLOW a symlink component reports ENOTDIR rather
// than the symlink errno on some kernels (darwin), masking the symlink; without it a
// symlink reliably yields it, while a non-directory intermediate still fails — the next
// openat into it returns ENOTDIR. Symlinks in root's own path are followed: root is
// the trusted anchor, and only the tree below it must be symlink-free.
func OpenNoFollowAt(root, rel string) (*os.File, error) {
	if err := checkRel(rel); err != nil {
		return nil, err
	}
	cur, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	for _, c := range strings.Split(rel, "/") {
		next, oerr := unix.Openat(cur, c, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(cur)
		if oerr != nil {
			// A non-directory ancestor (a regular file where a directory belongs) is
			// store corruption; report it as ErrNotDirectory so callers map it to their
			// corruption sentinel, consistent with MkdirAllNoFollow and ReadDirNoFollow.
			if errors.Is(oerr, unix.ENOTDIR) {
				return nil, fmt.Errorf("%w: %s", ErrNotDirectory, c)
			}
			return nil, &os.PathError{Op: "openat", Path: c, Err: noFollowErr(oerr)}
		}
		cur = next
	}
	return os.NewFile(uintptr(cur), filepath.Join(root, filepath.FromSlash(rel))), nil
}

// StatNoFollowAt reports whether rel under root exists and is stat-able without opening
// it for reading and without following a symlink in any component. It is a metadata-only
// capability: it never returns (or internally holds) a readable descriptor to the target,
// so a caller structurally cannot read payload bytes through it — a payload whose bytes
// are unreadable still stats present. It opens the parent directory chain no-follow, then
// fstatat's the final component with AT_SYMLINK_NOFOLLOW. It returns nil when the target
// stats as a non-symlink, os.ErrNotExist when it (or an ancestor) is absent, and another
// error when a component is a symlink or otherwise cannot be traversed.
func StatNoFollowAt(root, rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	parent, err := OpenParentDirNoFollow(root, rel)
	if err != nil {
		// A missing ancestor (e.g. a concurrently GC-removed entry directory) surfaces as
		// os.ErrNotExist so the caller maps it to a calm "missing", not "unreadable".
		return err
	}
	defer func() { _ = parent.Close() }()
	_, base := splitRel(rel)
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return os.ErrNotExist
		}
		return &os.PathError{Op: "fstatat", Path: base, Err: err}
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return &os.PathError{Op: "fstatat", Path: base, Err: unix.ELOOP}
	}
	return nil
}

// CheckNoSymlinkPath verifies that no existing component of rel under root is a
// symlink, opening each component no-follow from root. It tolerates a component that
// does not exist — the first missing one ends the walk, since nothing existing can
// redirect past it — so it suits a write or delete that will create or remove the
// leaf: the existing ancestors are proven real, and a symlink among them is reported
// classified by IsSymlinkOpenRejection. It is the write-path counterpart to
// OpenNoFollowAt: a read opens through the chain; a write first confirms the chain it
// is about to write into, link into, rename within, or delete from has no symlinked
// component, so a tampered store fails closed instead of writing or deleting outside
// the project through a symlinked ancestor.
func CheckNoSymlinkPath(root, rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	cur, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: root, Err: err}
	}
	for _, c := range strings.Split(rel, "/") {
		next, oerr := unix.Openat(cur, c, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(cur)
		if oerr != nil {
			if errors.Is(oerr, unix.ENOENT) {
				return nil
			}
			return &os.PathError{Op: "openat", Path: c, Err: noFollowErr(oerr)}
		}
		cur = next
	}
	_ = unix.Close(cur)
	return nil
}

// MkdirAllNoFollow ensures relDir exists under root, creating each missing
// component with mkdirat, and returns the leaf directory's fd. Every existing
// component is opened no-follow, so a symlinked component fails (ELOOP) instead of
// being traversed: the returned fd is pinned to a directory inside the project,
// making subsequent *at operations on it immune to an ancestor symlink swapped in
// afterward. The caller closes the returned file.
func MkdirAllNoFollow(root, relDir string, perm os.FileMode) (*os.File, error) {
	if err := checkRel(relDir); err != nil {
		return nil, err
	}
	cur, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	return mkdirAllFrom(cur, root, relDir, perm)
}

// MkdirAllAt is MkdirAllNoFollow anchored at an already-open directory instead of a
// path. Here — where openat is real — that means the anchor's IDENTITY holds and not
// just its name: descending from a held descriptor creates the whole chain inside the
// inode the caller opened, so a directory swapped in at the anchor's path afterwards
// receives nothing. Re-resolving the anchor by path cannot offer that, however
// carefully each component is checked, which is why fsx_other.go documents the weaker
// contract it can keep. The caller closes the returned file.
func MkdirAllAt(dir *os.File, relDir string, perm os.FileMode) (*os.File, error) {
	if err := checkRel(relDir); err != nil {
		return nil, err
	}
	cur, err := unix.Dup(int(dir.Fd()))
	if err != nil {
		return nil, &os.PathError{Op: "dup", Path: dir.Name(), Err: err}
	}
	unix.CloseOnExec(cur)
	return mkdirAllFrom(cur, dir.Name(), relDir, perm)
}

// mkdirAllFrom walks relDir from an already-open anchor descriptor, which it takes
// ownership of and always closes or returns. base names the anchor for diagnostics.
func mkdirAllFrom(anchorFd int, base, relDir string, perm os.FileMode) (*os.File, error) {
	cur := anchorFd
	for _, c := range strings.Split(relDir, "/") {
		next, oerr := unix.Openat(cur, c, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(oerr, unix.ENOENT) {
			if mErr := unix.Mkdirat(cur, c, uint32(perm.Perm())); mErr != nil && !errors.Is(mErr, unix.EEXIST) {
				_ = unix.Close(cur)
				return nil, &os.PathError{Op: "mkdirat", Path: c, Err: mErr}
			}
			next, oerr = unix.Openat(cur, c, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		_ = unix.Close(cur)
		if oerr != nil {
			return nil, &os.PathError{Op: "openat", Path: c, Err: noFollowErr(oerr)}
		}
		// A component that already exists as a non-directory (a regular file where a
		// store directory belongs) is corruption: report it as ErrNotDirectory so the
		// caller maps it to its store-corruption sentinel rather than letting a later
		// *at fail with a raw ENOTDIR.
		var st unix.Stat_t
		if ferr := unix.Fstat(next, &st); ferr != nil {
			_ = unix.Close(next)
			return nil, &os.PathError{Op: "fstat", Path: c, Err: ferr}
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			return nil, fmt.Errorf("%w: %s", ErrNotDirectory, c)
		}
		cur = next
	}
	return os.NewFile(uintptr(cur), filepath.Join(base, filepath.FromSlash(relDir))), nil
}

// CreateTempAt creates a new temp file directly inside the directory dir refers to,
// using openat so it is never redirected by a symlink, and returns the open file
// and its base name (for LinkAt/RenameAt/RemoveAt relative to the same dir).
func CreateTempAt(dir *os.File, prefix string) (*os.File, string, error) {
	for attempt := 0; attempt < 1000; attempt++ {
		name := prefix + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(tempSeq.Add(1), 10)
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return nil, "", &os.PathError{Op: "openat", Path: name, Err: noFollowErr(err)}
		}
		return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), name, nil
	}
	return nil, "", fmt.Errorf("fsx: could not create a unique temp file in %s", dir.Name())
}

// LinkAt hard-links oldName under oldDir to newName under newDir via linkat, so the
// link is published into the verified newDir, never through a symlinked ancestor. It
// preserves os.ErrExist for an existing target (the exclusivity a publish relies on).
func LinkAt(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	if err := checkLeafName(oldName); err != nil {
		return err
	}
	if err := checkLeafName(newName); err != nil {
		return err
	}
	if err := unix.Linkat(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName, 0); err != nil {
		return &os.LinkError{Op: "linkat", Old: oldName, New: newName, Err: err}
	}
	return nil
}

// RenameAt renames oldName under oldDir to newName under newDir via renameat,
// replacing the target atomically within the verified directory.
func RenameAt(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
	if err := checkLeafName(oldName); err != nil {
		return err
	}
	if err := checkLeafName(newName); err != nil {
		return err
	}
	if err := unix.Renameat(int(oldDir.Fd()), oldName, int(newDir.Fd()), newName); err != nil {
		return &os.LinkError{Op: "renameat", Old: oldName, New: newName, Err: err}
	}
	return nil
}

// OpenFileAt opens name under dir for reading via openat with O_NOFOLLOW, so the
// bytes read come from the object that name denotes at open time and from nothing
// a symlink swapped in. It is the read counterpart of LstatAt: a caller that has
// already proved a shape can fstat and read through the ONE descriptor this
// returns, which closes the window a separate lstat-then-open leaves open. An
// absent name surfaces as os.ErrNotExist; a symlink is refused with ELOOP, which
// IsSymlinkOpenRejection classifies.
func OpenFileAt(dir *os.File, name string) (*os.File, error) {
	if err := checkLeafName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, &os.PathError{Op: "openat", Path: name, Err: noFollowErr(err)}
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

// RemoveAt removes name under dir via unlinkat, so a delete cannot be redirected
// through a symlinked ancestor. An absent name surfaces as os.ErrNotExist.
func RemoveAt(dir *os.File, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
		return &os.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}

// RemoveDirAt removes the EMPTY directory name under dir via unlinkat with
// AT_REMOVEDIR, so a delete cannot be redirected through a symlinked ancestor and
// cannot descend. It is the non-destructive counterpart to RemoveTreeAt: a caller
// undoing its own work can only reclaim a directory it actually emptied, and a
// directory holding anything it does not own fails with ENOTEMPTY instead of taking
// that content with it. An absent name surfaces as os.ErrNotExist.
func RemoveDirAt(dir *os.File, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return &os.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}

// SyncDir flushes a directory's entries to stable storage. On Unix a directory
// fsync is meaningful and its error is real (EIO, EACCES, ENOSPC, ...), so it is
// surfaced rather than swallowed: a durable publish must be able to fail loudly
// when the directory entry cannot be flushed before something points at it.
func SyncDir(dir *os.File) error {
	return dir.Sync()
}

// LstatAt reports the mode and size of name under dir without following a final
// symlink, via fstatat with AT_SYMLINK_NOFOLLOW. It returns the FULL mode — type
// bits included — because the caller's question is usually "is this still the
// shape I proved it was?", which the type bits answer and permission bits alone
// do not. An absent name surfaces as os.ErrNotExist.
//
// It is the last-moment identity check a destructive operation makes immediately
// before acting, relative to a directory descriptor it already holds, so nothing
// swapped into an ancestor's path between the plan and the act can redirect it.
func LstatAt(dir *os.File, name string) (os.FileMode, int64, error) {
	if err := checkLeafName(name); err != nil {
		return 0, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return 0, 0, os.ErrNotExist
		}
		return 0, 0, &os.PathError{Op: "fstatat", Path: name, Err: err}
	}
	// st.Mode is uint16 on darwin and freebsd and uint32 on linux, so this conversion is
	// mandatory on two release targets and a no-op on the third. unconvert analyses a
	// single target and reports the no-op; deleting the conversion to satisfy it fails
	// the darwin and freebsd builds. The directive is the narrow answer — the widening
	// is what makes the classification below readable against one type.
	//nolint:unconvert // required on darwin and freebsd, where st.Mode is uint16
	return modeFromStat(uint32(st.Mode)), st.Size, nil
}

// modeFromStat converts a raw stat mode into an os.FileMode carrying the
// permission bits plus the one type bit awa distinguishes. It is deliberately
// narrow: restore acts on regular files, directories, and symlinks, and every
// other shape must read as "not one of those" rather than as an approximation.
func modeFromStat(raw uint32) os.FileMode {
	m := os.FileMode(raw & 0o777)
	switch raw & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= os.ModeDir
	case unix.S_IFLNK:
		m |= os.ModeSymlink
	case unix.S_IFREG:
		// no type bit: os.FileMode represents a regular file as permission bits alone
	default:
		m |= os.ModeIrregular
	}
	return m
}

// ReadlinkAt reads the target of the symlink name under dir via readlinkat, so a
// swapped ancestor cannot redirect the read. A name that is not a symlink fails.
func ReadlinkAt(dir *os.File, name string) (string, error) {
	if err := checkLeafName(name); err != nil {
		return "", err
	}
	buf := make([]byte, 4096)
	for {
		n, err := unix.Readlinkat(int(dir.Fd()), name, buf)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return "", os.ErrNotExist
			}
			return "", &os.PathError{Op: "readlinkat", Path: name, Err: err}
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
		// The target filled the buffer, so it may have been truncated: grow and retry
		// rather than return a silently shortened target, which would restore a link
		// pointing somewhere other than where the source proved.
		if len(buf) >= maxSymlinkTargetBytes {
			return "", fmt.Errorf("fsx: symlink target for %s exceeds %d bytes", name, maxSymlinkTargetBytes)
		}
		buf = make([]byte, len(buf)*2)
	}
}

// SymlinkAt creates a symlink named name under dir pointing at target, via
// symlinkat. The target is stored verbatim and never resolved: a link is what it
// points at, not what lives there. An existing name fails with os.ErrExist, which
// is what makes the temp-then-rename replacement below atomic.
func SymlinkAt(dir *os.File, target, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("fsx: refusing to create symlink %s with an empty target", name)
	}
	if err := unix.Symlinkat(target, int(dir.Fd()), name); err != nil {
		return &os.PathError{Op: "symlinkat", Path: name, Err: err}
	}
	return nil
}

// RemoveTreeAt recursively removes name — a file, a symlink, or a whole directory
// subtree — beneath dir, descending only through directory descriptors opened with
// O_NOFOLLOW. Because every level is reached relative to a pinned descriptor and no
// component is ever resolved through a symlink, the deletion cannot be redirected
// outside the tree by an ancestor swapped to a symlink mid-operation — the race a
// path-based os.RemoveAll after a separate symlink check leaves open. A symlink is
// unlinked, never followed into its target. An absent name is not an error.
func RemoveTreeAt(dir *os.File, name string) error {
	if err := checkLeafName(name); err != nil {
		return err
	}
	return removeTreeAt(int(dir.Fd()), name)
}

func removeTreeAt(parentFd int, name string) error {
	// Open name as a directory without following a symlink. Success means it is a
	// real directory to descend; ENOTDIR or ELOOP means a non-directory or a symlink,
	// which is unlinked in place; ENOENT means it is already gone.
	fd, err := unix.Openat(parentFd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		err = noFollowErr(err)
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil
		case errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP):
			if uerr := unix.Unlinkat(parentFd, name, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
				return &os.PathError{Op: "unlinkat", Path: name, Err: uerr}
			}
			return nil
		default:
			return &os.PathError{Op: "openat", Path: name, Err: err}
		}
	}
	// Wrap the descriptor to read its entries; f.Close releases fd. Children are
	// removed relative to fd (the pinned directory) before the directory itself.
	f := os.NewFile(uintptr(fd), name)
	names, rerr := f.Readdirnames(-1)
	if rerr != nil {
		_ = f.Close()
		return rerr
	}
	for _, child := range names {
		if cerr := removeTreeAt(fd, child); cerr != nil {
			_ = f.Close()
			return cerr
		}
	}
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	if err := unix.Unlinkat(parentFd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "unlinkat", Path: name, Err: err}
	}
	return nil
}

// IsSymlinkOpenRejection reports whether err is a no-follow open refusing a symlink,
// which callers surface as a "changed during scan" or store-corruption rejection rather
// than a raw open error.
//
// It matches ELOOP alone, and that is deliberate on every platform this file serves:
// the opens above normalize their platform's no-follow symlink errno onto ELOOP as they
// produce the error, so the one place that knows the operation is the one place that
// decides. Widening this predicate to a second errno instead would reclassify that errno
// wherever it came from, including operations for which it means something else.
func IsSymlinkOpenRejection(err error) bool {
	return errors.Is(err, unix.ELOOP)
}
