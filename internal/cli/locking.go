package cli

import (
	"context"
	"time"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/lockfile"
)

// lockParent returns the parent context for a lock wait, defaulting to a
// background context when a locker was built without a root ctx (only in tests
// that do not model cancellation).
func lockParent(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background() //nolint:forbidigo // documented detached-context fallback for tests that build a locker without a cancellable ctx
	}
	return ctx
}

// presenceLocker adapts lockfile.AcquirePresence to the app-layer LockAcquirer
// ports for the writers (checkpoint, run, run rm): Acquire takes a uniquely-named presence lock
// for op and returns the lease's Release as the deferred cleanup. The operation is
// recorded in the lock so gc and doctor can explain what holds it. Presence locks
// never block one another; they only stand down while a collector (awa gc) holds
// its exclusive lock, waiting up to the configured lock timeout before giving up.
type presenceLocker struct {
	ctx      context.Context
	root     string
	locksDir string
	op       lockfile.Operation
	timeout  time.Duration
}

// newPresenceLocker builds a presence-lock acquirer for op anchored at the
// project's lock directory. timeout bounds how long Acquire waits out an active
// collector before returning a lock-timeout error. ctx is the root cancellable
// context so Ctrl+C aborts a lock wait instead of blocking out the full timeout.
func newPresenceLocker(ctx context.Context, layout paths.Layout, op lockfile.Operation, timeout time.Duration) presenceLocker {
	return presenceLocker{ctx: ctx, root: layout.Root(), locksDir: layout.LocksDir(), op: op, timeout: timeout}
}

// Acquire publishes the presence lock and returns its release function. The Owner
// is left zero so it defaults to this host, pid, the wall clock, and crypto/rand. A
// zero timeout means fail fast if a collector is active.
func (p presenceLocker) Acquire() (func() error, error) {
	ctx, cancel := context.WithTimeout(lockParent(p.ctx), p.timeout)
	defer cancel()
	lease, err := lockfile.AcquirePresence(ctx, p.root, p.locksDir, p.op, lockfile.Owner{})
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

// collectorLocker adapts lockfile.AcquireCollectorExclusive to the LockAcquirer
// port for destructive maintenance that must run alone — doctor --repair. Acquire
// takes the exclusive collector lock (the same one awa gc uses, so the two mutually
// exclude) and then waits out any writer already in flight, so the repair pass has
// exclusive access. New writers stand down for the held lock; the wait is bounded by
// timeout, after which Acquire returns a lock-timeout error.
type collectorLocker struct {
	ctx      context.Context
	root     string
	locksDir string
	op       lockfile.Operation
	timeout  time.Duration
}

// newCollectorLocker builds a collector-lock acquirer for op anchored at the
// project's lock directory. ctx is the root cancellable context so Ctrl+C aborts
// a lock wait instead of blocking out the full timeout.
func newCollectorLocker(ctx context.Context, layout paths.Layout, op lockfile.Operation, timeout time.Duration) collectorLocker {
	return collectorLocker{ctx: ctx, root: layout.Root(), locksDir: layout.LocksDir(), op: op, timeout: timeout}
}

// Acquire takes the exclusive collector lock and returns its release function. The
// Owner is left zero so it defaults to this host, pid, the wall clock, and crypto/rand.
func (c collectorLocker) Acquire() (func() error, error) {
	ctx, cancel := context.WithTimeout(lockParent(c.ctx), c.timeout)
	defer cancel()
	lease, err := lockfile.AcquireCollectorExclusive(ctx, c.root, c.locksDir, c.op, lockfile.Owner{})
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

// restoreExclusiveLocker adapts lockfile.AcquireExclusive to the LockAcquirer port
// for "awa restore --apply". It is a named exclusive lock rather than the collector
// lock: restore must serialize against another restore (two concurrent restores
// would each plan against a worktree the other is changing), but it is a writer, not
// a collector, so it takes the ordinary presence lock separately for the gc
// interlock. A second restore waits out the configured timeout and then reports the
// lock-timeout exit rather than proceeding on a stale plan.
type restoreExclusiveLocker struct {
	ctx      context.Context
	root     string
	locksDir string
	timeout  time.Duration
}

// newRestoreExclusiveLocker builds the restore-vs-restore serialization acquirer.
// ctx is the root cancellable context so Ctrl+C aborts a lock wait instead of
// blocking out the full timeout.
func newRestoreExclusiveLocker(ctx context.Context, layout paths.Layout, timeout time.Duration) restoreExclusiveLocker {
	return restoreExclusiveLocker{ctx: ctx, root: layout.Root(), locksDir: layout.LocksDir(), timeout: timeout}
}

// Acquire takes the exclusive restore lock and returns its release function.
func (r restoreExclusiveLocker) Acquire() (func() error, error) {
	ctx, cancel := context.WithTimeout(lockParent(r.ctx), r.timeout)
	defer cancel()
	lease, err := lockfile.AcquireExclusive(ctx, r.root, r.locksDir, lockfile.RestoreLockName, lockfile.OpRestore, lockfile.Owner{})
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}
