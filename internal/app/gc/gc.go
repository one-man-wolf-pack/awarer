// Package gc implements the "awa gc" use case: conservative garbage collection of a
// project's durable .awa state. It plans before it mutates and deletes only what it
// can prove disposable — unreachable blobs, checkpoints and runs outside the
// retention policy, restore recovery observations past their own independent
// retention window, and stale temp artifacts — while corrupt, locked, or ambiguous
// state is retained and reported.
//
// A destructive GC decision is authoritative only while the collector owns the
// exclusive collector lease that prevents new writers from publishing state behind
// the decision. Collect enforces that: it takes the lease, plans under the held
// lease, executes that exact plan under the same lease, then releases. Reachability
// and retention are therefore computed against the store as it stands under the
// collector's exclusive boundary, so a writer cannot start and finish in a window
// between planning and deletion and leave a checkpoint referencing an already-swept
// blob.
//
// Plan is the read-only, advisory path: it reasons over the same repositories and
// typed domain candidates without taking the lease, so a path discovered on disk
// never becomes a deletion target until it has been classified and proven
// reachable-or-not through a repository. A dry run is Plan with no deletion — an
// honest snapshot of what GC would do when asked, not a decision that may be
// executed later. Only Collect may delete, and only against facts it established
// under the lease.
package gc

import (
	"context"
	"os"
	"time"

	domconfig "awarer/internal/domain/config"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/gitmeta"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/projfs"
	"awarer/internal/infra/restorestore"
	"awarer/internal/infra/runstore"
)

// gcLockName is the fixed filename of the exclusive collector lock. It is reserved:
// inspectLocks ignores it (gc-vs-gc is handled by the exclusive acquire and its
// timeout, not by the writer-presence block), and the Execute-time recheck skips it
// as the collector's own lock. It is the same anchor writers stand down on, defined
// once in the lockfile package.
const gcLockName = lockfile.CollectorLockName

// GitProvider answers the one git question the conservative --committed policy
// needs: the latest commit on the current branch. It is the single external seam GC
// injects (gitmeta.Provider satisfies it) so the committed policy is testable
// without a real repository.
type GitProvider interface {
	LatestCommit(ctx context.Context) (gitmeta.CommitInfo, bool, error)
}

// Deps carries the ambient, injectable inputs GC needs that are not derived from the
// project: a clock, the local hostname and a process-liveness probe (for lock
// classification), the stale threshold (for locks and temp), and an optional
// GitProvider override. Zero-valued fields fall back to real-environment defaults,
// so production wiring can pass an empty Deps.
type Deps struct {
	Now          func() time.Time
	Hostname     string
	ProcessAlive func(pid int) bool
	StaleAfter   time.Duration
	// Git overrides the git provider. When nil, planning builds gitmeta.New(root).
	Git GitProvider
}

// Request is one GC invocation against an already-resolved, config-loaded project.
// The policy knobs live in the domain gc.GCRequest passed alongside it.
type Request struct {
	Project projfs.Project
	Config  domconfig.Config
}

// Service plans and executes GC. It is constructed with Deps and is safe to reuse
// across requests; it holds no per-request state.
type Service struct {
	now          func() time.Time
	hostname     string
	processAlive func(pid int) bool
	staleAfter   time.Duration
	git          GitProvider
}

// New builds a Service, filling real-environment defaults for any zero-valued
// dependency. The git provider is resolved per-request from the project root unless
// Deps.Git was supplied.
func New(d Deps) *Service {
	s := &Service{
		now:          d.Now,
		hostname:     d.Hostname,
		processAlive: d.ProcessAlive,
		staleAfter:   d.StaleAfter,
		git:          d.Git,
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

// repos bundles the repositories a plan or execution reads/writes through, built
// from a project's layout so Plan and Execute see identical boundaries.
type repos struct {
	checkpoints *checkpointjson.Repo
	runs        *runstore.Repo
	restores    *restorestore.Repo
	blobs       *blobstore.FS
	git         GitProvider
}

// open builds the repositories for a request. The hasher needs nothing from the
// config, so even a partially-damaged config still permits a read-only plan.
func (s *Service) open(req Request) (repos, paths.Layout, error) {
	layout, err := req.Project.Paths()
	if err != nil {
		return repos{}, paths.Layout{}, err
	}
	hasher := blake3hash.New()
	git := s.git
	if git == nil {
		git = gitmeta.New(layout.Root())
	}
	return repos{
		checkpoints: checkpointjson.NewRepo(layout),
		runs:        runstore.New(layout, hasher),
		restores:    restorestore.New(layout, hasher),
		blobs:       blobstore.New(layout, hasher),
		git:         git,
	}, layout, nil
}

// Plan builds the immutable GC plan for a request without mutating anything. It
// returns a non-nil error only for a failure that prevents safe planning at all (an
// unresolvable layout, an unreadable store root, a corrupt blob layout); ordinary
// per-record corruption and lock obstructions are recorded as blocked candidates so
// the plan still renders and the CLI can map them to the right exit code.
func (s *Service) Plan(ctx context.Context, gcreq gcdom.GCRequest, req Request) (gcdom.GCPlan, error) {
	r, layout, err := s.open(req)
	if err != nil {
		return gcdom.GCPlan{}, err
	}
	return s.planWith(ctx, gcreq, req, r, layout)
}

// planWith builds the plan over an already-opened set of repositories. It is the
// single planner-construction site, so the advisory Plan and the authoritative
// under-lease pass in Collect reason through identical boundaries and cannot drift.
// The clock is sampled here so a plan carries one consistent "now".
func (s *Service) planWith(ctx context.Context, gcreq gcdom.GCRequest, req Request, r repos, layout paths.Layout) (gcdom.GCPlan, error) {
	p := &planner{
		svc:                   s,
		ctx:                   ctx,
		req:                   gcreq,
		cfg:                   req.Config,
		now:                   s.now(),
		repos:                 r,
		layout:                layout,
		unreadableCheckpoints: map[string]bool{},
	}
	return p.run()
}

// Collect is the authoritative destructive GC path used for a real (non-dry-run)
// run. It takes the exclusive collector lease, plans under the held lease, executes
// that exact plan under the same lease, then releases. Because reachability and
// retention are computed while the lease is held, no writer can publish state
// behind the decision: a checkpoint that starts and finishes between an earlier
// advisory plan and this pass is observed here, so its blobs are never swept. An
// externally built plan (for example one produced by Plan for display) is never an
// input to Collect — only the facts established under the lease authorize deletion.
//
// A live lease holder is waited on up to [locks].timeout, then ErrLockTimeout is
// returned (the lock-timeout exit); the collector lease serializes gc against gc. A
// dry-run request never acquires destructive authority — it degrades to Plan with
// no deletion. A blocked plan (corruption or an active/unknown lock) removes
// nothing. Individual deletion failures are recorded, not fatal, so a partial run
// stays visible.
func (s *Service) Collect(ctx context.Context, gcreq gcdom.GCRequest, req Request) (gcdom.GCResult, error) {
	if gcreq.DryRun() {
		// A dry run deletes nothing, so it must not take the collector lease. It is
		// exactly Plan with execution skipped; its output is advisory only.
		plan, err := s.Plan(ctx, gcreq, req)
		if err != nil {
			return gcdom.GCResult{}, err
		}
		return gcdom.NewResult(plan, nil, plan.Warnings()), nil
	}

	r, layout, err := s.open(req)
	if err != nil {
		return gcdom.GCResult{}, err
	}
	release, err := s.acquireCollector(ctx, req, layout)
	if err != nil {
		return gcdom.GCResult{}, err
	}
	defer release()

	// Plan under the held lease (see the method doc) and mint the authoritativePlan
	// here, the only production site that does so, so deletion can consume nothing but
	// facts established under this lease.
	plan, err := s.planWith(ctx, gcreq, req, r, layout)
	if err != nil {
		return gcdom.GCResult{}, err
	}
	return s.deleteAuthoritative(ctx, authoritativePlan{plan: plan}, r, layout)
}

// authoritativePlan is a GCPlan whose reachability and retention facts were
// established under the currently held collector lease. Destructive deletion
// (deleteAuthoritative) consumes only this type, and its field is unexported, so no
// code outside this package can delete from an advisory (pre-lease) plan: the
// stale-plan hazard is not expressible in production. Collect is the only production
// site that mints one, immediately after planning under the lease.
type authoritativePlan struct {
	plan gcdom.GCPlan
}

// acquireCollector takes the exclusive collector lease (gcLockName) for a request,
// honoring [locks].timeout. A zero timeout means fail fast: the already-expired
// context makes a held lease return ErrLockTimeout on the first check, while a free
// or stale lease is still acquired without waiting (a negative timeout cannot reach
// here — config validation rejects it). The returned release closes the lease and
// the timeout context together; the caller defers it. Keeping acquisition in one
// place lets Collect and execute share identical lease semantics without hiding the
// lock behind a broad mutex abstraction.
func (s *Service) acquireCollector(ctx context.Context, req Request, layout paths.Layout) (func(), error) {
	lockCtx, cancel := context.WithTimeout(ctx, req.Config.Locks.Timeout.Duration())
	lease, err := lockfile.AcquireExclusive(lockCtx, layout.Root(), layout.LocksDir(), gcLockName, lockfile.OpGC, s.owner())
	if err != nil {
		cancel()
		return nil, err
	}
	return func() {
		_ = lease.Release()
		cancel()
	}, nil
}

// deleteAuthoritative runs the writer recheck and destructive sweep for a plan whose
// facts hold under the currently held collector lease; the caller owns the lease. A
// dry-run or blocked plan deletes nothing but still carries the plan's warnings.
//
// The writer recheck is defense in depth. Collect plans under the lease so a writer
// cannot publish behind the decision, but the recheck still catches an active or
// unknown writer presence lock (including one this build cannot parse) and retains
// everything rather than convert a race into data loss. Unlike doctor --repair, gc
// does not wait writers out — it retains and reports.
func (s *Service) deleteAuthoritative(ctx context.Context, ap authoritativePlan, r repos, layout paths.Layout) (gcdom.GCResult, error) {
	plan := ap.plan
	if plan.DryRun() || plan.Blocked() {
		return gcdom.NewResult(plan, nil, plan.Warnings()), nil
	}
	blocked, warning, err := s.writerLockAppeared(layout)
	if err != nil {
		return gcdom.GCResult{}, err
	}
	if blocked {
		return gcdom.NewLockSuppressedResult(plan, append(plan.Warnings(), warning)), nil
	}
	ex := &executor{ctx: ctx, repos: r, root: layout.Root()}
	results := ex.run(plan)
	return gcdom.NewResult(plan, results, plan.Warnings()), nil
}

// owner builds the lock owner from the service's injected identity, clock, and
// liveness probe, so the collector lock classifies and stamps consistently with the
// rest of gc and with doctor.
func (s *Service) owner() lockfile.Owner {
	return lockfile.Owner{
		Hostname:   s.hostname,
		Now:        s.now,
		Alive:      s.processAlive,
		StaleAfter: s.staleAfter,
	}
}

// writerLockAppeared reports whether any active or unknown writer presence lock is
// present (excluding the collector's own lock). Under the held collector lease it is
// defense in depth over planning-under-lease: it catches an active or unknown writer
// and retains everything; a true result carries a human warning. Unlike doctor
// --repair, gc does not wait writers out — it retains and reports.
func (s *Service) writerLockAppeared(layout paths.Layout) (bool, string, error) {
	state, detail, err := lockfile.WriterPresent(layout.Root(), layout.LocksDir(), s.now(), s.staleAfter, s.hostname, s.processAlive)
	if err != nil {
		// An unreadable locks directory under the held collector lock: fail closed,
		// the same conservative stance inspectLocks takes at plan time.
		return true, "locks directory became unreadable, retaining state: " + err.Error(), nil
	}
	if state != 0 {
		// Either a recognized active writer or an unrecognized lock: gc never deletes
		// when it cannot rule out a writer (including a newer-awa writer this build
		// cannot parse), so it retains state and reports.
		return true, detail + "; retaining state", nil
	}
	return false, "", nil
}
