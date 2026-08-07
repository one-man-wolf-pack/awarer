package lockfile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/fsx"
)

// presencePerm is the mode lock tokens are published with. A lock record is a
// small advisory marker, not durable state a reader rewrites, so it is created
// read-only — the only legitimate mutation is its owner removing it on release.
// Owner-only, matching every other awa-owned file.
const presencePerm = paths.ReadOnlyFilePerm

// ErrLockTimeout reports that a required lock could not be acquired in time: a live
// owner held it (the exclusive collector lock, or, for a writer, an active
// collector it had to stand down for) longer than the caller was willing to wait.
// The CLI maps it to the lock-timeout exit code (6), distinct from the state-action-required code.
var ErrLockTimeout = errors.New("lock acquisition timed out")

// ErrLockUnknown reports that a required lock could not be acquired because a
// conflicting lock file is unrecognized (an unsupported schema, malformed JSON,
// unreadable, or otherwise not one of ours). It is never removed automatically —
// discarding it could drop another tool's protection — so acquisition fails loudly
// instead of waiting out a timeout a retry could never clear.
var ErrLockUnknown = errors.New("conflicting lock is unrecognized")

// Operation is the validated kind of work a lock protects. It is a typed value so
// the operation field of a lock record cannot be an arbitrary string: only the
// constants below are acquirable, and Acquire* rejects anything else.
type Operation string

const (
	// OpCheckpoint marks an in-progress "awa checkpoint" writer.
	OpCheckpoint Operation = "checkpoint"
	// OpRun marks an in-progress "awa run": its input scan (which writes the worktree
	// index, on a hit as well as a miss) and any cache-writing execution.
	OpRun Operation = "run"
	// OpRunRm marks an in-progress "awa run rm" deletion.
	OpRunRm Operation = "run-rm"
	// OpGC marks an in-progress "awa gc" (the exclusive collector lock).
	OpGC Operation = "gc"
	// OpDoctorRepair marks an in-progress "awa doctor --repair" pass.
	OpDoctorRepair Operation = "doctor-repair"
	// OpRestore marks an in-progress "awa restore --apply". It is taken twice with
	// different mechanics: as an exclusive lock on RestoreLockName, which serializes
	// restore against restore, and as a writer presence lock, which stands the
	// operation down for an active collector and then keeps gc from reclaiming the
	// source blobs it is about to read.
	OpRestore Operation = "restore"
)

// CollectorLockName is the fixed filename of the exclusive collector lock awa gc
// takes. It is the shared anchor of the writer/collector protocol: gc acquires it
// exclusively (AcquireExclusive), and every writer (AcquirePresence) stands down
// while it is held by a live collector, so gc's deletion phase never overlaps a
// writer's in-flight work.
const CollectorLockName = "gc.lock"

// RestoreLockName is the fixed filename of the exclusive lock "awa restore --apply"
// takes. Two concurrent restores would each plan against a worktree the other is
// changing, so they serialize here rather than racing; a second restore waits out
// the configured lock timeout and then reports the lock-timeout exit.
const RestoreLockName = "restore.lock"

// knownOperations is the closed set of acquirable operations. A presence or
// exclusive lock for anything outside it is a wiring error, not a runtime input.
var knownOperations = map[Operation]struct{}{
	OpCheckpoint:   {},
	OpRun:          {},
	OpRunRm:        {},
	OpGC:           {},
	OpDoctorRepair: {},
	OpRestore:      {},
}

// Valid reports whether op is one of the known operations.
func (op Operation) Valid() bool {
	_, ok := knownOperations[op]
	return ok
}

// LeaseState is the lifecycle of a lock token this process owns. It is a distinct
// axis from the Status/EntryState classification of an *observed* lock: those
// describe someone else's lock as active/stale/unknown, while LeaseState describes
// our own acquired-then-released token.
type LeaseState int

const (
	// LeaseAcquired: this process holds the lock; its token is present on disk.
	LeaseAcquired LeaseState = iota + 1
	// LeaseReleased: Release succeeded (or was a no-op); the token is gone.
	LeaseReleased
)

// Owner is the injectable identity, clock, and liveness seam an acquisition uses.
// Zero-valued fields fall back to the real environment via withDefaults, so
// production callers can pass a zero Owner and tests can pin every field. It is
// passed by value: an acquisition reads it once and never mutates the caller's.
type Owner struct {
	Hostname   string
	PID        int
	Now        func() time.Time
	Rand       io.Reader
	Alive      func(pid int) bool
	StaleAfter time.Duration
}

// withDefaults returns a copy of o with any zero field filled from the real
// environment: this host's name, this process's pid, the wall clock, crypto/rand,
// the platform liveness probe, and the documented stale threshold.
func (o Owner) withDefaults() Owner {
	if o.Hostname == "" {
		if h, err := os.Hostname(); err == nil {
			o.Hostname = h
		}
	}
	if o.PID == 0 {
		o.PID = os.Getpid()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rand == nil {
		o.Rand = rand.Reader
	}
	if o.Alive == nil {
		o.Alive = ProcessAlive
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	return o
}

// Lease is an owned, scoped handle to a lock token this process created. It is the
// unit of ownership: a caller defers Release rather than manipulating a filename,
// so the token's name never escapes as a mutable string. The zero Lease is not
// usable; obtain one only from AcquirePresence or AcquireExclusive.
type Lease struct {
	root     string
	locksDir string
	name     string // the exact token filename we created; private so Release alone uses it
	rec      Record
	state    LeaseState
	// file is the held descriptor of a flock-backed exclusive lock (darwin, linux,
	// freebsd). It is nil for presence tokens and the file-content exclusive fallback,
	// which Release frees by unlinking name instead. Holding it open is what keeps the
	// kernel lock.
	file *os.File
}

// Operation reports the operation this lease protects.
func (l *Lease) Operation() Operation { return Operation(l.rec.Operation) }

// State reports the lease's lifecycle state.
func (l *Lease) State() LeaseState { return l.state }

// AcquirePresence publishes a uniquely-named presence token for op and returns a
// Lease. Its name embeds the operation, host, pid, and a random suffix, so
// concurrent writers coexist and never block one another. The one party a writer
// must yield to is an active collector: it publishes its token, then checks the
// exclusive CollectorLockName, and if a live gc holds it the writer removes its own
// token and waits (bounded by ctx) before retrying. Combined with gc acquiring the
// collector lock and then scanning for presence tokens, this publish-then-check
// ordering guarantees gc's deletion phase and a writer's work never overlap — the
// safety the one-shot observation alone could not provide. ctx bounds the wait; if a
// collector outlasts it, ErrLockTimeout is returned. The token is published with
// fsx.PublishBytesNoFollow (root-safe, no-follow, creates .awa/locks on demand), and
// is left for doctor to reclaim if the writer dies without releasing it.
func AcquirePresence(ctx context.Context, root, locksDir string, op Operation, owner Owner) (*Lease, error) {
	if !op.Valid() {
		return nil, fmt.Errorf("lockfile: refusing to acquire unknown operation %q", op)
	}
	o := owner.withDefaults()
	rec := Record{Operation: string(op), PID: o.PID, Hostname: o.Hostname, CreatedAt: o.Now().UTC()}
	data, err := Encode(rec)
	if err != nil {
		return nil, err
	}
	suffix, err := randSuffix(o.Rand)
	if err != nil {
		return nil, err
	}
	relDir, err := fsx.RelUnder(root, locksDir)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s-%s-%d-%s.lock", op, sanitizeHost(o.Hostname), o.PID, suffix)

	backoff := 10 * time.Millisecond
	const maxBackoff = 200 * time.Millisecond
	for {
		// Publish the presence token first, then look for a live collector: a gc that
		// scans after this publish will see the token and stand down, so the two cannot
		// both proceed.
		if err := fsx.PublishBytesNoFollow(root, relDir, name, data, presencePerm); err != nil {
			return nil, fmt.Errorf("lockfile: publishing presence lock for %s: %w", op, err)
		}
		active, err := collectorActive(root, locksDir, o)
		if err != nil {
			_ = removeLockFile(root, locksDir, name)
			return nil, err
		}
		if !active {
			return &Lease{root: root, locksDir: locksDir, name: name, rec: rec, state: LeaseAcquired}, nil
		}
		// A collector is running: drop our token so it never blocks gc, then wait and
		// retry once the collector has released its lock.
		_ = removeLockFile(root, locksDir, name)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("lockfile: %s blocked by an active collector (%s): %w", op, CollectorLockName, ErrLockTimeout)
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

// IsExclusiveLockName reports whether name is one of the fixed exclusive-lock
// files. They differ from writer presence locks in a way every observer has to
// respect: the file is a diagnostic record, not the ownership itself, and under the
// flock backend it deliberately outlives its holder — releasing means closing the
// descriptor, never unlinking, because unlinking would break flock for anyone
// waiting on it. A leftover exclusive lock file is therefore normal, not stale
// state to clean up.
func IsExclusiveLockName(name string) bool {
	return name == CollectorLockName || name == RestoreLockName
}

// ExclusiveLockReportsUnknown reports whether doctor should surface the exclusive
// lock entry e as an unknown-lock finding. The answer is platform-specific: under
// the flock backend the lock file's contents are advisory, so an unrecognized file
// is a harmless leftover and never reported; under the file-content fallback an
// unrecognized exclusive lock is actionable (it blocks every acquirer) and is
// surfaced. Only pass a ScanEntry whose Name satisfies IsExclusiveLockName.
func ExclusiveLockReportsUnknown(e ScanEntry) bool { return collectorLockReportsUnknown(e) }

// AcquireExclusive takes the single exclusive lock named name (the collector lock,
// gc.lock) so only one holder proceeds at a time. On darwin, linux, and freebsd it is
// backed by an advisory whole-file lock (flock): the kernel releases it when the holder
// dies, so there is no stale lock to detect or steal. Elsewhere it falls back to a
// best-effort file-content protocol. Either way a live holder is waited on until ctx is
// done (then ErrLockTimeout); the lock file's contents are only ever a diagnostic
// record, never the authority for ownership. The per-platform mechanics live in
// acquireCollector (exclusive_unix.go / exclusive_other.go).
func AcquireExclusive(ctx context.Context, root, locksDir, name string, op Operation, owner Owner) (*Lease, error) {
	if !op.Valid() {
		return nil, fmt.Errorf("lockfile: refusing to acquire unknown operation %q", op)
	}
	return acquireCollector(ctx, root, locksDir, name, op, owner.withDefaults())
}

// AcquireCollectorExclusive takes the exclusive collector lock for op and then waits,
// bounded by ctx, for any writer that was already in flight to finish, so the holder
// has truly exclusive access to mutate the store. It is the acquisition for
// destructive maintenance that must run alone (doctor --repair): unlike gc, which
// retains and reports when a writer is present, a repair waits the in-flight writers
// out. New writers cannot start meanwhile — they stand down for the held collector
// lock — so the wait is bounded; if it is not reached before ctx expires (or the
// scan fails), the lock is released and the error is returned. gc instead pairs a
// plain AcquireExclusive with its own abort-on-writer check.
func AcquireCollectorExclusive(ctx context.Context, root, locksDir string, op Operation, owner Owner) (*Lease, error) {
	o := owner.withDefaults()
	lease, err := AcquireExclusive(ctx, root, locksDir, CollectorLockName, op, o)
	if err != nil {
		return nil, err
	}
	backoff := 10 * time.Millisecond
	const maxBackoff = 200 * time.Millisecond
	for {
		// Wait out recognized live writers, but fail closed on an unrecognized lock. A
		// repair is destructive to temp and the worktree index, and a lock this build
		// cannot parse may be a newer-awa writer mid-operation on exactly that state —
		// removing its orphan temp or rebuilding the index under it could race a writer we
		// cannot reason about. We also cannot know when such a lock will clear, so rather
		// than draining until timeout we give up immediately with a lock-unknown error;
		// doctor's read-only diagnosis surfaces the lock for the user to resolve.
		state, detail, perr := WriterPresent(root, locksDir, o.Now(), o.StaleAfter, o.Hostname, o.Alive)
		if perr != nil {
			_ = lease.Release()
			return nil, fmt.Errorf("lockfile: %s cannot verify the store is idle: %w", op, perr)
		}
		if state == EntryUnknown {
			_ = lease.Release()
			return nil, fmt.Errorf("lockfile: %s cannot proceed, %s: %w", op, detail, ErrLockUnknown)
		}
		if state != EntryActive {
			return lease, nil
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lease.Release()
			return nil, fmt.Errorf("lockfile: %s timed out waiting for an active writer to finish (%s): %w", op, detail, ErrLockTimeout)
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

// WriterPresent reports the state of the most significant lock — other than the
// exclusive collector lock — that shows a writer may be in flight. A destructive
// collector-class operation calls it after taking the collector lock to avoid racing
// a writer that began before the lock was held; detail names the blocking lock for a
// user-facing message. A stale lock never counts (a crashed writer left it) and is
// doctor's to reclaim, so the result is one of: EntryUnknown (an unrecognized lock is
// present — possibly a newer-awa writer this build cannot parse), EntryActive (a
// recognized live writer is present), or the zero state (no writer). Unknown is
// reported in preference to active because it is the condition a caller can least
// reason about: both gc and doctor --repair must treat it as blocking — gc retains and
// reports, doctor --repair fails closed rather than rebuilding the index or removing
// temp state a writer it cannot interpret may still be using.
func WriterPresent(root, locksDir string, now time.Time, staleAfter time.Duration, hostname string, alive func(pid int) bool) (state EntryState, detail string, err error) {
	entries, err := ScanDir(root, locksDir, now, staleAfter, hostname, alive)
	if err != nil {
		return 0, "", err
	}
	var activeDetail string
	var sawActive bool
	for _, e := range entries {
		if IsExclusiveLockName(e.Name) {
			// An exclusive lock is not a writer presence token. Its file is a diagnostic
			// record that deliberately outlives its holder under the flock backend, so
			// reading one as a writer would let a finished gc or restore suppress
			// collection — and fail doctor --repair closed — until the stale threshold,
			// or indefinitely once the recorded pid is reused. An operation that really
			// is in flight is visible through its own presence lock, which is scanned
			// below like every other writer.
			continue
		}
		switch e.State {
		case EntryUnknown:
			return EntryUnknown, "an unrecognized lock is present: " + e.Detail, nil
		case EntryActive:
			if !sawActive {
				sawActive = true
				activeDetail = "a writer lock (" + e.Record.Operation + " on " + e.Record.Hostname + ") is active"
			}
		}
	}
	if sawActive {
		return EntryActive, activeDetail, nil
	}
	return 0, "", nil
}

// Release frees this lease. A flock-backed exclusive lock is released by closing its
// descriptor: the kernel drops the lock, and the lock file is deliberately left on
// disk as a diagnostic record the next collector reacquires and overwrites (unlinking
// a flock'd file would race an acquirer that has already opened it). Every other lease
// — presence tokens and the file-content exclusive fallback — is freed by unlinking
// its own token, by its exact recorded name, through a no-follow directory descriptor;
// that path never reads, scans, or removes any other file, so it cannot delete another
// process's token or an unknown lock. Release is idempotent: a second call, or an
// already-absent token, succeeds.
func (l *Lease) Release() error {
	if l == nil || l.state == LeaseReleased {
		return nil
	}
	if l.file != nil {
		err := l.file.Close()
		l.state = LeaseReleased
		if err != nil {
			return fmt.Errorf("lockfile: releasing exclusive lock %q: %w", l.name, err)
		}
		return nil
	}
	if err := removeLockFile(l.root, l.locksDir, l.name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lockfile: releasing lock %q: %w", l.name, err)
	}
	l.state = LeaseReleased
	return nil
}

// removeLockFile unlinks a single named file from locksDir under root through a
// verified no-follow directory descriptor, so a symlinked locks directory cannot
// redirect the removal and only the named leaf is touched.
func removeLockFile(root, locksDir, name string) error {
	relDir, err := fsx.RelUnder(root, locksDir)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(root, relDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return fsx.RemoveAt(dir, name)
}

// randSuffix returns a short random hex string used to make a presence token's name
// unique, so two writers (even with the same operation and pid across hosts) never
// collide and a crashed process's same-pid successor never accidentally adopts a
// leftover token.
func randSuffix(r io.Reader) (string, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("lockfile: reading randomness for lock name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// sanitizeHost reduces a hostname to a single safe path component so it can appear
// in a lock filename: any separator becomes an underscore, and an empty or
// dot-only result falls back to "host". The hostname is recorded verbatim in the
// record body; this only affects the filename.
func sanitizeHost(h string) string {
	h = strings.ReplaceAll(h, "/", "_")
	h = strings.ReplaceAll(h, `\`, "_")
	if h == "" || h == "." || h == ".." {
		return "host"
	}
	return h
}
