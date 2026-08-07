//go:build darwin || linux || freebsd

package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/fsx"
)

// exclusiveLockPerm is the mode the collector lock file is created with. Unlike a
// presence token (read-only), the collector lock is a long-lived file whose owner
// reopens it to refresh its diagnostic record, so it must be owner-writable.
const exclusiveLockPerm = paths.FilePerm

// acquireCollector takes the exclusive collector lock through an advisory whole-file
// lock (flock LOCK_EX). The kernel owns it: the lock is released automatically when
// the holding process exits or its descriptor closes, so a crashed collector leaves
// nothing to detect or steal — the race a file-content steal protocol cannot avoid
// does not exist here. The lock file persists between runs only as a human-readable
// diagnostic record (operation, pid, host, time); its contents are never the source
// of truth for who holds the lock. ctx bounds the wait for a live holder; if the lock
// is not released before ctx is done, ErrLockTimeout is returned.
func acquireCollector(ctx context.Context, root, locksDir, name string, op Operation, o Owner) (*Lease, error) {
	relDir, err := fsx.RelUnder(root, locksDir)
	if err != nil {
		return nil, err
	}
	dir, err := fsx.MkdirAllNoFollow(root, relDir, paths.DirPerm)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()

	f, err := openExclusiveFile(dir, name)
	if err != nil {
		return nil, fmt.Errorf("lockfile: opening exclusive lock %q: %w", name, err)
	}

	backoff := 10 * time.Millisecond
	const maxBackoff = 200 * time.Millisecond
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lockfile: locking exclusive lock %q: %w", name, err)
		}
		// EWOULDBLOCK: a live collector holds the lock. Wait, bounded by ctx.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, fmt.Errorf("lockfile: %s is held by another collector (%s): %w", op, name, ErrLockTimeout)
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	rec := Record{Operation: string(op), PID: o.PID, Hostname: o.Hostname, CreatedAt: o.Now().UTC()}
	writeDiagnosticRecord(f, rec) // best-effort: the record only explains the lock to humans
	return &Lease{root: root, locksDir: locksDir, name: name, rec: rec, state: LeaseAcquired, file: f}, nil
}

// openExclusiveFile opens (creating if absent) the collector lock file through the
// no-follow directory descriptor. It prefers a writable descriptor so the diagnostic
// record can be refreshed, but falls back to read-only if a leftover file is not
// writable by this user: flock works on a read-only descriptor, and the record is
// only advisory.
func openExclusiveFile(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(exclusiveLockPerm))
	if err == nil {
		return os.NewFile(uintptr(fd), name), nil
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EROFS) {
		return nil, err
	}
	fd, err = unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// writeDiagnosticRecord overwrites the lock file with rec's encoding. It is
// best-effort: a failure (for example a read-only descriptor) leaves the previous
// contents, which only affects the human-facing explanation, never lock ownership.
func writeDiagnosticRecord(f *os.File, rec Record) {
	data, err := Encode(rec)
	if err != nil {
		return
	}
	if err := f.Truncate(int64(len(data))); err != nil {
		return
	}
	_, _ = f.WriteAt(data, 0)
}

// collectorActive reports whether a live collector holds the exclusive lock. It tests
// with a shared (LOCK_SH) request, which conflicts only with the collector's
// exclusive hold and never with another concurrent prober, so two writers can never
// observe each other as a collector. The probe takes no lasting lock and never
// mutates the file. The Owner is unused here: liveness is the kernel's, not a
// record's.
func collectorActive(root, locksDir string, _ Owner) (bool, error) {
	relFile, err := fsx.RelUnder(root, filepath.Join(locksDir, CollectorLockName))
	if err != nil {
		return false, err
	}
	f, err := fsx.OpenNoFollowAt(root, relFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil // no collector lock file: no collector
		}
		return false, fmt.Errorf("lockfile: probing collector lock %q: %w", CollectorLockName, err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil // an exclusive holder — a live collector — is present
		}
		return false, fmt.Errorf("lockfile: probing collector lock %q: %w", CollectorLockName, err)
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil // the file is a harmless leftover; no live holder
}

// collectorLockReportsUnknown reports whether doctor should surface the collector
// lock as an unknown-lock finding. Under the flock backend it never should: the lock
// file's contents are advisory, so an unrecognized or empty file is a harmless
// leftover, not an actionable problem — a live holder is detected by flock, and a
// crashed one leaves a file the next collector simply reacquires.
func collectorLockReportsUnknown(ScanEntry) bool { return false }
