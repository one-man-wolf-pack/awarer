//go:build !darwin && !linux && !freebsd

package lockfile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"awarer/internal/infra/fsx"
)

// acquireCollector is the best-effort, file-content fallback used where advisory
// whole-file locks (flock) are unavailable. Unlike the flock backend, it cannot rely
// on the kernel to release a crashed collector's lock, so it detects a stale lock by
// the recorded owner's liveness and reclaims it. That reclaim has an unavoidable
// multi-party race (no POSIX primitive offers an atomic compare-and-delete on file
// content), so this path is explicitly best-effort; the flock platforms use the
// race-free backend instead.
//
// It publishes a single, deterministically-named token (name, e.g. "gc.lock") so only
// one holder of that name proceeds at a time. On a collision it classifies the
// conflicting token through the same Parse/Classify path doctor and gc use: a stale
// recognized lock is reclaimed and acquisition retries; a live owner is waited on with
// backoff until ctx is done (then ErrLockTimeout); an unrecognized lock returns
// ErrLockUnknown immediately and is never removed.
func acquireCollector(ctx context.Context, root, locksDir, name string, op Operation, o Owner) (*Lease, error) {
	relDir, err := fsx.RelUnder(root, locksDir)
	if err != nil {
		return nil, err
	}
	backoff := 10 * time.Millisecond
	const maxBackoff = 200 * time.Millisecond
	for {
		rec := Record{Operation: string(op), PID: o.PID, Hostname: o.Hostname, CreatedAt: o.Now().UTC()}
		data, err := Encode(rec)
		if err != nil {
			return nil, err
		}
		err = fsx.PublishBytesNoFollow(root, relDir, name, data, presencePerm)
		if err == nil {
			return &Lease{root: root, locksDir: locksDir, name: name, rec: rec, state: LeaseAcquired}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lockfile: publishing exclusive lock %q: %w", name, err)
		}
		// A token already exists under this name: classify it before deciding whether
		// to steal, wait, or refuse. Reuse the reader path so parsing is never duplicated.
		raw, rerr := readLockFile(root, locksDir, name)
		if rerr != nil {
			return nil, fmt.Errorf("lockfile: %s held by an unreadable lock %q: %w", op, name, errors.Join(ErrLockUnknown, rerr))
		}
		conflict, perr := Parse(raw)
		if perr != nil {
			return nil, fmt.Errorf("lockfile: %s held by an unrecognized lock %q: %w", op, name, errors.Join(ErrLockUnknown, perr))
		}
		if conflict.Classify(o.Now(), o.StaleAfter, o.Hostname, o.Alive) == StatusStale {
			// Provably stale, so reclaim it. A plain remove-by-name could delete a live
			// lock a concurrent stealer published between our classify and our remove, so
			// claim it atomically: rename the token to a private name (only one racer wins
			// the rename; a loser gets ErrNotExist and re-reads). Winning the rename is not
			// proof we grabbed the same stale token, though — another stealer may have
			// reclaimed it and published its own live lock under name in the window. So
			// re-identify the claimed file: remove it only if it is the very stale record
			// we classified; otherwise we grabbed a live owner's lock and must restore it
			// rather than destroy its protection. (A 3+-way reclaim storm can still slip a
			// fresh publish past the restore; that residual is why this fallback is
			// best-effort and the flock platforms do not need it.)
			suffix, rerr := randSuffix(o.Rand)
			if rerr != nil {
				return nil, rerr
			}
			claim := fmt.Sprintf("%s.reclaim-%d-%s", name, o.PID, suffix)
			if mverr := renameLockFile(root, locksDir, name, claim); mverr != nil {
				if errors.Is(mverr, os.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("lockfile: reclaiming stale lock %q: %w", name, mverr)
			}
			if claimed, cerr := readLockFile(root, locksDir, claim); cerr == nil {
				if rec, perr := Parse(claimed); perr == nil && !sameRecord(rec, conflict) {
					// Not the stale token we classified — a live owner published it after our
					// classify. Restore it under name (O_EXCL, so a concurrent publisher is
					// not clobbered). Only drop our copy if the restore succeeded; if it failed
					// (a third party already took name), fail closed and leave the claimed token
					// on disk rather than deleting a live owner's lock. Better a leaked reclaim
					// file than a destroyed live token; this residual is why the fallback is
					// best-effort and the flock platforms do not need it.
					if rerr := fsx.PublishBytesNoFollow(root, relDir, name, claimed, presencePerm); rerr != nil {
						return nil, fmt.Errorf("lockfile: reclaim race on %q; refusing to destroy a live lock: %w", name, errors.Join(ErrLockUnknown, rerr))
					}
					_ = removeLockFile(root, locksDir, claim)
					continue
				}
			}
			_ = removeLockFile(root, locksDir, claim)
			continue
		}
		// A live owner holds it: wait with bounded backoff until the caller's deadline.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("lockfile: %s is held by %s (pid %d on %s): %w", op, conflict.Operation, conflict.PID, conflict.Hostname, ErrLockTimeout)
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// collectorActive reports whether a live collector holds CollectorLockName, by reading
// and classifying the lock record. An absent lock is not active; a stale one (a
// crashed collector) is not active and is left for the next gc to reclaim. A lock that
// exists but cannot be read or recognized returns ErrLockUnknown rather than blocking:
// a writer refuses an unrecognized lock instead of waiting out a timeout a retry could
// never clear.
func collectorActive(root, locksDir string, o Owner) (bool, error) {
	raw, err := readLockFile(root, locksDir, CollectorLockName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("lockfile: collector lock %q is unreadable: %w", CollectorLockName, errors.Join(ErrLockUnknown, err))
	}
	rec, err := Parse(raw)
	if err != nil {
		return false, fmt.Errorf("lockfile: collector lock %q is unrecognized: %w", CollectorLockName, errors.Join(ErrLockUnknown, err))
	}
	return rec.Classify(o.Now(), o.StaleAfter, o.Hostname, o.Alive) == StatusActive, nil
}

// collectorLockReportsUnknown reports whether doctor should surface the collector lock
// as an unknown-lock finding. On the file-content fallback an unrecognized gc.lock is
// genuinely actionable: it is not self-healing (the acquire path refuses it rather
// than stealing it) and blocks every collector, so doctor surfaces it for the user —
// but, like any unknown lock, never removes it.
func collectorLockReportsUnknown(e ScanEntry) bool { return e.State == EntryUnknown }

// sameRecord reports whether two parsed records describe the same lock instance —
// same operation, owner, and creation time. It identifies a token across a reclaim so
// a stealer removes only the exact stale record it classified, never a live one
// republished under the same name in the meantime.
func sameRecord(a, b Record) bool {
	return a.Operation == b.Operation && a.PID == b.PID && a.Hostname == b.Hostname && a.CreatedAt.Equal(b.CreatedAt)
}

// renameLockFile atomically renames from to within locksDir under root through a
// verified no-follow directory descriptor. Renaming a missing source returns an
// os.ErrNotExist-classified error, which the stale-reclaim path uses to detect that
// another racer already claimed the token.
func renameLockFile(root, locksDir, from, to string) error {
	relDir, err := fsx.RelUnder(root, locksDir)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(root, relDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return fsx.RenameAt(dir, from, dir, to)
}
