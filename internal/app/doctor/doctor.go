// Package doctor implements the "awa doctor" use case: it inspects a project's
// durable .awa state through the same repositories the rest of awa reads it
// through, records what it finds as typed domain findings, and — only when asked —
// performs a small, closed set of mechanically-safe repairs.
//
// The governing rule is that doctor never turns unknown damage into "healthy"
// state. Every check reuses an existing read/validation boundary (the checkpoint,
// run, blob, and index repositories), so a corrupt record is surfaced exactly as
// those stores already classify it rather than re-parsed by a more forgiving
// reader. Repairs are limited to removing orphan temp artifacts, provably-stale
// recognized locks, and the derived worktree index, plus restoring the awa-owned
// .awa/.gitignore guard; doctor never deletes a checkpoint, run, or blob.
package doctor

import (
	"context"
	"os"
	"time"

	domconfig "awarer/internal/domain/config"
	"awarer/internal/domain/doctor"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/projfs"
)

// GitTracker reports whether .awa is tracked by git. It is the one external seam
// doctor injects (gitmeta.Provider satisfies it) so the git checker is testable
// without a real repository; every other subsystem is exercised with real
// filesystem fixtures.
type GitTracker interface {
	AwaTracking(ctx context.Context) (gitmeta.Tracking, error)
}

// Deps carries the ambient, injectable inputs doctor needs that are not derived
// from the project itself: a clock, the local hostname and a process-liveness
// probe (for stale-lock classification), the stale-lock threshold, and an optional
// GitTracker override. Zero-valued fields fall back to real-environment defaults,
// so production wiring can pass an empty Deps.
type Deps struct {
	Now          func() time.Time
	Hostname     string
	ProcessAlive func(pid int) bool
	StaleAfter   time.Duration
	// Git overrides the git tracker. When nil, Run builds gitmeta.New(root).
	Git GitTracker
	// Locks acquires exclusive access for the --repair pass: the destructive repairs
	// (removing leftovers, rebuilding the index) must not race a concurrent writer or
	// collector. It is optional: a nil acquirer skips locking, and read-only diagnosis
	// never locks.
	Locks LockAcquirer
}

// LockAcquirer takes the exclusive collector lock for doctor's repair pass and waits
// out any in-flight writer, so the repairs run with the store to themselves. Acquire
// returns a release function the caller defers, or a lock-timeout error if exclusive
// access is not reached in time. The lockfile-backed adapter wired at the composition
// root satisfies it.
type LockAcquirer interface {
	Acquire() (release func() error, err error)
}

// Request is one doctor invocation against an already-resolved, config-loaded
// project. Strict raises verification depth; Repair opts into the safe repairs.
type Request struct {
	Project projfs.Project
	// Resolved is the composed config plus provenance, so a config finding can name
	// the exact layer (shared awa.toml, local .awa/config.toml, or --config) a value
	// came from instead of assuming the local path.
	Resolved domconfig.ResolvedConfig
	Strict   bool
	Repair   bool
}

// Service runs the doctor checks. It is constructed with Deps and is safe to reuse
// across requests; it holds no per-request state.
type Service struct {
	now          func() time.Time
	hostname     string
	processAlive func(pid int) bool
	staleAfter   time.Duration
	git          GitTracker
	locks        LockAcquirer
}

// New builds a Service, filling in real-environment defaults for any zero-valued
// dependency. The git tracker is left nil here and resolved per-request from the
// project root unless Deps.Git was supplied.
func New(d Deps) *Service {
	s := &Service{
		now:          d.Now,
		hostname:     d.Hostname,
		processAlive: d.ProcessAlive,
		staleAfter:   d.StaleAfter,
		git:          d.Git,
		locks:        d.Locks,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.hostname == "" {
		if h, err := os.Hostname(); err == nil {
			s.hostname = h
		}
	}
	if s.processAlive == nil {
		s.processAlive = lockfile.ProcessAlive
	}
	if s.staleAfter <= 0 {
		s.staleAfter = lockfile.DefaultStaleAfter
	}
	return s
}

// Run performs every subsystem check and, when Request.Repair is set, executes the
// safe repairs derived from the repairable findings. It returns a DoctorResult
// whose health and summary are derived from the findings. It returns a non-nil
// error only for a failure that prevents diagnosis at all (for example the project
// layout cannot be resolved); ordinary corruption is reported as findings, never as
// an error, so a damaged project still produces a full report.
func (s *Service) Run(ctx context.Context, req Request) (doctor.DoctorResult, error) {
	layout, err := req.Project.Paths()
	if err != nil {
		return doctor.DoctorResult{}, err
	}

	rep := &report{}
	s.checkLayout(layout, rep)
	s.checkCheckpoints(ctx, layout, req.Strict, rep)
	s.checkRuns(ctx, layout, rep)
	s.checkRestores(ctx, layout, rep)
	// The three checks above walk local history and stop early on cancellation; surface
	// a Ctrl+C as an interruption rather than returning a silently partial diagnosis.
	if err := ctx.Err(); err != nil {
		return doctor.DoctorResult{}, err
	}
	s.checkIndex(layout, req.Strict, rep)
	s.checkTemp(layout, rep)
	s.checkLocks(layout, rep)
	s.checkGit(ctx, layout, rep)
	s.checkStateGitignore(layout, rep)
	// Local-privacy diagnostics: config posture, private permissions, and ambiguous
	// project roots.
	s.checkConfig(layout, req.Resolved, rep)
	s.checkPermissions(layout, rep)
	s.checkMarkers(layout, rep)

	if req.Repair {
		// The repairs are destructive (removing orphan temp and stale locks, rebuilding
		// the derived index), so take exclusive access first: the collector lock excludes
		// gc, and the acquirer waits out any writer already in flight while new writers
		// stand down for the lock. A failure to reach exclusive access in time is a
		// lock-timeout error, distinct from corruption. Diagnosis above is read-only and
		// unlocked.
		if s.locks != nil {
			release, err := s.locks.Acquire()
			if err != nil {
				return doctor.DoctorResult{}, err
			}
			defer func() { _ = release() }()
		}
		s.repair(layout, rep)
	}

	return doctor.NewResult(rep.passed, rep.findings), nil
}

// report accumulates the outcome of the subsystem checks: a count of checks that
// passed with nothing to say, and the findings recorded. Checkers append to it;
// repair mutates findings in place to mark them repaired.
type report struct {
	passed   int
	findings []doctor.Finding
}

func (r *report) pass() { r.passed++ }

func (r *report) add(f doctor.Finding) { r.findings = append(r.findings, f) }

// finding builds a validated finding or panics. Every argument is a compile-time
// constant the checkers control, so a construction failure is a programmer error,
// not a runtime condition — failing loud here keeps an invalid finding out of every
// report.
//
// It also enforces the one direction of the repairability contract that has a
// dangerous failure mode: repairable=true requires a repair kind for the code.
// Claiming it without one puts a finding in the report that `--repair` can never
// clear, and since an unrepaired repairable finding is exit 5, `awa doctor --repair`
// would then fail forever on a project nothing is actually wrong with.
//
// The reverse is deliberately NOT enforced. repairKindFor names an available repair
// mechanism; observed state decides whether applying it to this finding is safe, so
// repairable is a property of the individual finding rather than of its code, and both
// values stay legal for the same code.
func finding(code doctor.FindingCode, sev doctor.Severity, sub doctor.Subsystem, path, subject, message string, repairable bool) doctor.Finding {
	if repairable {
		if _, ok := repairKindFor(code); !ok {
			panic("doctor: finding " + code.String() + " is marked repairable but has no repair kind; " +
				"--repair could never clear it and doctor would stay at exit 5")
		}
	}
	f, err := doctor.NewFinding(code, sev, sub, path, subject, message, repairable)
	if err != nil {
		panic("doctor: invalid finding construction: " + err.Error())
	}
	return f
}
