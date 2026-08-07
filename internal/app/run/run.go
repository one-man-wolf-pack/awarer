package run

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"runtime"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/projfs"
)

// maxSkippedSamples bounds how many skipped inputs are recorded on an entry for
// diagnostics, so a scan that skips thousands of files does not bloat metadata.
const maxSkippedSamples = 16

// Deps are the ports the run service depends on.
type Deps struct {
	Scanner  Scanner
	Store    runcache.Store
	Runner   runcache.Runner
	Resolver ExecutableResolver
	// EffectObserver computes the bounded effect signature over the watched
	// generated-output roots. It is a correctness dependency: a run that does not
	// observe effect state can publish a false hit after excluded generated output
	// changes. Required — New rejects a nil observer, so the dependency is never a
	// silent fallback.
	EffectObserver EffectObserver
	Env            EnvReader
	Clock          Clock
	Hasher         hashing.Hasher
	AwaVersion     string
	// Rand sources the random suffix of a run id; nil falls back to crypto/rand.
	Rand io.Reader
	// Locks acquires the writer presence lock Run holds across the whole call: the
	// pre-run input scan (which persists the worktree index), the child's execution,
	// the post-run scan (which refreshes that index to the state the child left), and
	// the cache write, so a concurrent destructive collector retains the in-flight
	// entry and does not rebuild the index mid-write. A cache hit is serialized too,
	// because it still scans and so still writes the index. It is optional: a nil
	// acquirer skips locking (tests, or a read-only path such as explain, which scans
	// without persisting).
	Locks LockAcquirer
	// RmLocks acquires the presence lock around "run rm" deletions, so a concurrent
	// gc does not race the explicit removal. Optional; a dry run never locks.
	RmLocks LockAcquirer
}

// LockAcquirer takes a presence lock for the duration of a run's scan and (on a
// miss) its execution and cache write. Acquire returns a release function the caller
// defers; it never blocks on other presence locks, only standing down for an active
// collector. The lockfile-backed adapter wired at the composition root satisfies it.
type LockAcquirer interface {
	Acquire() (release func() error, err error)
}

// Service runs commands through the run cache. Construct it with New.
type Service struct {
	deps Deps
}

// New builds a run service from its ports. The scanner, store, runner, resolver,
// env reader, clock, and hasher are required; a nil one is a wiring error, so New
// fails loudly rather than deferring a nil dereference to the first run.
func New(deps Deps) *Service {
	switch {
	case deps.Scanner == nil:
		panic("run.New: scanner must not be nil")
	case deps.Store == nil:
		panic("run.New: store must not be nil")
	case deps.Runner == nil:
		panic("run.New: runner must not be nil")
	case deps.Resolver == nil:
		panic("run.New: resolver must not be nil")
	case deps.EffectObserver == nil:
		panic("run.New: effect observer must not be nil")
	case deps.Env == nil:
		panic("run.New: env reader must not be nil")
	case deps.Clock == nil:
		panic("run.New: clock must not be nil")
	case deps.Hasher == nil:
		panic("run.New: hasher must not be nil")
	case deps.AwaVersion == "":
		panic("run.New: awa version must not be empty")
	}
	if deps.Rand == nil {
		deps.Rand = rand.Reader
	}
	return &Service{deps: deps}
}

// Policy carries the run-local flags that change cache behavior.
type Policy struct {
	Refresh            bool
	NoCache            bool
	NoCacheFailures    bool
	AllowSkippedInputs bool
	// Record runs the command as supervised history: it never reads an existing hit
	// and never publishes a reusable pointer, but still captures full output and
	// pre/post observations and records the run as record-only history.
	Record bool
}

// Request is one run invocation.
type Request struct {
	Project projfs.Project
	Config  config.Config
	Argv    []string
	CWD     runcache.ExecutionCWD
	AbsDir  string
	Scope   ScopeOverrides
	// EffectiveScope, when set, is the already-resolved input scope to scan and key
	// under, bypassing the config-plus-overrides derivation. Explain's stored modes
	// use it to reproduce a stored run's effective scope exactly, so a run recorded
	// with --scope is not re-explained as a scope change against the current default.
	EffectiveScope *config.ScanScope
	StdinMode      runcache.StdinMode
	TTYAllowed     bool
	Policy         Policy
	// Stdout and Stderr receive the command's output: live on a miss, replayed on
	// a hit. The CLI passes the real terminal streams in human mode and io.Discard
	// in JSON mode, so command bytes never pollute awa's JSON stdout.
	Stdout io.Writer
	Stderr io.Writer
}

// Result is the outcome of a run. It is a result DTO, not a persisted record: a
// hit or a stored miss also exists in the store as a runcache.RunEntry addressed
// by RunID, but an uncached or disabled run exists only as this value. Keeping the
// two apart means an invalid-to-persist execution (e.g. one with output capture
// disabled) is never mistaken for a replayable RunEntry.
type Result struct {
	Status   runcache.CacheStatus
	Key      runcache.RunKey
	ExitCode int
	// Written reports whether this invocation published a reusable cache entry (a new
	// key pointer). It is true only for a freshly stored reusable miss; a hit replays
	// without writing, and a recorded non-reusable miss writes history but no pointer,
	// so both are false. "Is there a resolvable entry" is answered by RunID, "is there
	// a reusable cache entry" by this field.
	Written bool
	// Recorded reports whether this invocation has a durable run record — true on a
	// hit (the existing record) and on any miss persisted as history (reusable,
	// record-only, mutating, or unknown-post-state), false only when capture was
	// disabled so nothing was recorded.
	Recorded bool
	// Reuse is the typed reuse classification of the run: the existing record's on a
	// hit, the freshly recorded run's on a miss. It is meaningful only when Recorded;
	// a not-recorded run (capture disabled) still computes a classification for the
	// current-result reason, but it is not durable history, so consumers gate on
	// Recorded before treating Reuse as a stored fact.
	Reuse runcache.ReuseState
	// RunID identifies a resolvable stored entry: set on a hit (the existing entry)
	// and on any recorded miss (the just-written one, reusable or not); zero only when
	// nothing was recorded.
	RunID     runcache.RunID
	Execution Execution
	// Performance carries bounded, actionable latency diagnostics for this invocation
	// (currently a large watched effect root that dominated state observation). It is
	// advisory presentation, never a correctness signal, and is empty when no known
	// cause crossed the interactive latency threshold.
	Performance []perfdiag.Diagnostic
	// EffectDiagnosis, when set, carries best-effort context for a run left
	// non-reusable because its watched generated-output (effect) state changed or
	// could not be observed. awa records effect state as a folded stat signature, not a
	// per-path manifest, so there is deliberately no changed-path sample here — this
	// names the dominant watched root when the observation identified one and otherwise
	// leaves it empty. It is diagnostic context that lets the footer teach the
	// exclude/effect-root/record decision, never per-path evidence.
	EffectDiagnosis *EffectDiagnosis
	// effectReport is the diagnostic side channel of the PRE-run effect observation —
	// the one folded into Execution.KeyInput.Effect. It is unexported because it is raw
	// diagnostic evidence, not a review fact: the only consumer is DiagnoseRun, which
	// compares stored entries against exactly that key input and so must name a root from
	// exactly that observation. A caller outside this package reads the projected
	// EffectDiagnosis instead. It is never persisted and never folded into the key.
	effectReport runcache.EffectReport
}

// EffectDiagnosis is the review-facing context for an effect-state non-reusable run
// or near-miss candidate. Reason is the effect-state mismatch (effect-state-differs or
// effect-state-unavailable); Root names the dominant watched generated-output root the
// paired observation identified. Root is empty both when that observation named none and
// when there is no paired observation at all — a candidate short-circuited to a stored
// historical verdict — so a present Root always means "observed, and observed about
// this". It never carries a changed-path sample: effect state is a bounded stat
// signature, not a manifest.
type EffectDiagnosis struct {
	Reason runcache.MismatchReason
	Root   string
}

// effectDiagnosisFor is the single rule turning a canonical mismatch reason plus one
// already-produced effect report into the review-facing diagnosis. Only the two
// effect-state reasons get one; every other reason returns nil, so a surface teaches
// the exclude/effect-root/record decision exactly where it is relevant. The root is
// read out of the report the caller already holds — the largest fully-observed root,
// else the root that failed the observation closed by exceeding its budget — and is
// left empty when that observation named none. It never scans, stats, parses an effect
// digest, or guesses a conventional path: an unnamed root is reported as unknown.
//
// Callers must pass the report of the SAME observation pass whose identity the reason
// was derived against, so the named root and the reason describe one observation. For a
// just-executed run that is the post-run report (its reason comes from the effect
// guard's pre/post compare); for a near-miss candidate it is the report of the
// observation folded into the current key input it was compared with. A caller holding a
// reason no observation of its own produced — a stored historical verdict — has no such
// report to pass and hands over the empty one, which lands on the same "named no root"
// branch rather than needing a second rule here (see RunReuseCandidate.attachEffect).
func effectDiagnosisFor(reason runcache.MismatchReason, report runcache.EffectReport) *EffectDiagnosis {
	if reason != runcache.ReasonEffectStateDiffers && reason != runcache.ReasonEffectStateUnavailable {
		return nil
	}
	root := ""
	if lr, ok := report.LargestRoot(); ok {
		root = lr.Path
	} else if ob, ok := report.OverBudget(); ok {
		root = ob.Path
	}
	return &EffectDiagnosis{Reason: reason, Root: root}
}

// effectDiagnosisFrom builds the effect-state diagnosis for a just-executed run from
// its reuse classification and the post-run effect report — the observation the effect
// guard compared to reach that classification.
func effectDiagnosisFrom(reuse runcache.ReuseState, post runcache.EffectReport) *EffectDiagnosis {
	return effectDiagnosisFor(reuse.Reason(), post)
}

// Execution is the metadata of one run, recorded whether or not it was stored, so
// the CLI can report an uncached run without inventing a persisted entry for it.
type Execution struct {
	KeyInput   runcache.KeyInput
	Exit       runcache.ExitStatus
	StartedAt  time.Time
	FinishedAt time.Time
	Decision   runcache.CacheDecision
	Skipped    runcache.SkippedSummary
	// Mutation is the outcome of the run mutation guard: whether the command changed
	// observed state between the pre-run and post-run scans, or the post-run scan
	// failed. It is the zero value (MutationUnobserved) for a replayed hit, which
	// executes nothing and so observes nothing.
	Mutation runcache.MutationStatus
	// EffectGuard is the outcome of the effect guard over the watched generated-output
	// roots. Like Mutation it is the zero value (EffectGuardUnset) for a replayed hit.
	EffectGuard runcache.EffectGuardStatus
	// Output is the captured stdout/stderr summary. It is nil when output capture
	// was disabled (run.capture_output=false) — output was not captured at all,
	// which is honest where a zero summary would falsely read as "empty".
	Output *Output
}

// Output is the execution-level summary of a run's captured streams.
type Output struct {
	Stdout StreamSummary
	Stderr StreamSummary
}

// StreamSummary is the execution-level view of one captured output stream: what
// the command produced and how much survived truncation. Unlike
// runcache.OutputCapture it carries no payload file name or hash, because a
// captured stream is not necessarily stored — an uncached run discards its temp
// payload — so a durable store reference would be a lie. The conversion to a
// storable OutputCapture happens only at the storage boundary, when an entry is
// actually written.
type StreamSummary struct {
	// CapturedBytes is the total number of bytes the command produced on the stream.
	CapturedBytes int64
	// RetainedBytes is the number of bytes kept after head truncation (including the
	// truncation marker when one was added).
	RetainedBytes int64
	Truncated     bool
	OmittedBytes  int64
}

// summarizeStream projects a storage-level capture into the execution-level
// summary, dropping the payload file name and hash that only mean anything for a
// stored entry.
func summarizeStream(c runcache.OutputCapture) StreamSummary {
	return StreamSummary{
		CapturedBytes: c.OriginalBytes,
		RetainedBytes: c.StoredBytes,
		Truncated:     c.Truncated,
		OmittedBytes:  c.OmittedBytes,
	}
}

// DurationMs returns the wall-clock duration of the execution in milliseconds.
func (e Execution) DurationMs() int64 {
	return e.FinishedAt.Sub(e.StartedAt).Milliseconds()
}

// executionFromEntry projects a stored or replayed entry into an execution result.
// A persisted entry always carries valid captures, so its output is always present.
func executionFromEntry(entry runcache.RunEntry) Execution {
	return Execution{
		KeyInput:   entry.KeyInput,
		Exit:       entry.Exit,
		StartedAt:  entry.StartedAt,
		FinishedAt: entry.FinishedAt,
		Decision:   entry.Decision,
		Skipped:    entry.Skipped,
		Output:     &Output{Stdout: summarizeStream(entry.Stdout), Stderr: summarizeStream(entry.Stderr)},
	}
}

// Run executes the request through the cache: it scans the inputs, builds the
// key, replays a cached result on a hit, or runs the command and atomically
// publishes a complete entry on a miss. It returns the wrapped command's exit
// code; an awa execution or storage failure is returned as an error.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	if len(req.Argv) == 0 {
		return Result{}, fmt.Errorf("run: empty command")
	}
	// The output sinks are written through io.MultiWriter on a miss; normalize a
	// nil sink to io.Discard so a caller that omits one never triggers a panic.
	if req.Stdout == nil {
		req.Stdout = io.Discard
	}
	if req.Stderr == nil {
		req.Stderr = io.Discard
	}

	// Both scans persist the worktree index (a derived accelerator) — the pre-run one
	// as the baseline, the post-run one to refresh it to whatever the child left — and
	// a miss also publishes a cache entry. Hold a presence lock across all of it, the
	// child's execution included, so a destructive collector (gc, doctor --repair)
	// cannot rebuild the index or sweep the in-flight entry mid-write, and so the two
	// index writes bracketing the child cannot be interleaved with another writer's. A
	// cache hit still scans — and so still writes the index — so it is serialized too;
	// only work that touches no shared state ever runs unlocked.
	if s.deps.Locks != nil {
		release, err := s.deps.Locks.Acquire()
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = release() }()
	}

	kc, err := s.buildKeyContext(ctx, req, true)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = kc.Close() }()

	if kc.CanRead {
		ce, ok, err := s.deps.Store.Lookup(kc.Key)
		if err != nil {
			return Result{}, err
		}
		entry := ce.Entry()
		// --no-cache-failures means a failed result must not be served from the cache,
		// not merely never written: a failure stored by an earlier run must not replay
		// under the flag. A stored failure is treated as a miss, re-executed, and (since
		// the flag also blocks the write) not re-stored.
		if ok && req.Policy.NoCacheFailures && entry.Exit.Failed() {
			ok = false
		}
		if ok && !s.expired(entry, kc.EffCfg.Run.TTL) {
			// Verify both stored payloads against their recorded hashes before the hit
			// is reported, so every caller — human replay and JSON alike — gets the
			// same invariant: a missing or tampered payload is storage corruption, not
			// a successful hit. The caller then replays the output it needs.
			if err := s.verifyPayloads(entry); err != nil {
				return Result{}, err
			}
			return Result{
				Status:       runcache.CacheHit,
				Key:          kc.Key,
				RunID:        entry.ID,
				Recorded:     true,
				Reuse:        entry.Reuse,
				ExitCode:     entry.Exit.Code,
				Execution:    executionFromEntry(entry),
				Performance:  runPerformance([]inputReport{kc.InputReport}, kc.EffectReport),
				effectReport: kc.EffectReport,
			}, nil
		}
	}

	return s.execute(ctx, req, kc)
}

// execute runs the command on a cache miss, capturing output and publishing a
// complete entry when policy permits. It takes the key context buildKeyContext
// produced, so the executed run is keyed and decided exactly as the lookup above.
func (s *Service) execute(ctx context.Context, req Request, kc keyContext) (Result, error) {
	effCfg := kc.EffCfg
	capture := effCfg.Run.CaptureOutput

	// The presence lock covering this miss (the index scan and this entry's publish)
	// is held by Run for the whole call, so execute does not acquire its own.
	var pending runcache.PendingRun
	stdoutSink := req.Stdout
	stderrSink := req.Stderr
	if capture {
		p, err := s.deps.Store.Begin(runcache.CaptureLimits{
			MaxStdout: effCfg.Run.MaxStdoutSize.Bytes(),
			MaxStderr: effCfg.Run.MaxStderrSize.Bytes(),
		})
		if err != nil {
			return Result{}, err
		}
		pending = p
		stdoutSink = io.MultiWriter(req.Stdout, p.Stdout())
		stderrSink = io.MultiWriter(req.Stderr, p.Stderr())
	}

	runRes, err := s.deps.Runner.Run(ctx, runcache.RunSpec{
		Path:   kc.ResolvedPath,
		Argv:   req.Argv,
		Dir:    req.AbsDir,
		Env:    kc.ChildEnv,
		Stdin:  req.StdinMode,
		Stdout: stdoutSink,
		Stderr: stderrSink,
	})
	if err != nil {
		if pending != nil {
			_ = pending.Discard()
		}
		return Result{}, err
	}

	var soCap, seCap runcache.OutputCapture
	if pending != nil {
		soCap, seCap, err = pending.Finalize()
		if err != nil {
			_ = pending.Discard()
			return Result{}, err
		}
	}

	// Observe project state after the child exits and compare it to the pre-run
	// baseline (the tree hash already folded into the key). A command that changed
	// observed state cannot have its file effects replayed, so its result must not
	// become a future hit; a failed post-run scan leaves the state unknown and fails
	// closed. The guard runs for every execution — even one policy already forbids
	// caching — so --no-cache and uncached runs still report honest mutation facts.
	// This scan is also what leaves the worktree index describing the state the child
	// left, so the next run's baseline is the worktree and not a memory of it; the
	// presence lock this call holds covers that write as it covers the pre-run one.
	mutation, afterScan, afterInput := s.observeMutation(ctx, req, kc)
	defer func() { _ = afterScan.Close() }()

	// Observe the watched generated-output roots after the child, comparing to the
	// pre-run signature: a command that created or changed excluded generated output
	// cannot have those effects replayed, so its result must not become a hit. The
	// post-run report feeds the latency diagnostic too, so a command that itself created
	// a huge root is explained, not just marked non-reusable.
	effect, effectPost := s.observeEffectMutation(req, kc.EffectPre)

	failed := runRes.Exit.Failed()
	cacheFailedOK := effCfg.Run.CacheFailedRuns && !req.Policy.NoCacheFailures
	// A recorded run is durable history whenever output was captured, even when it is
	// not reusable; a reusable cache pointer is published only when the run was
	// eligible to write, did not fail (unless policy permits), and left both the input
	// and the watched effect state unchanged. record and publishPointer are the two
	// independent decisions the new model splits apart.
	record := capture
	publishPointer := record && kc.CanWrite && (!failed || cacheFailedOK) &&
		mutation.CacheableUnderMutation() && effect.CacheableUnderEffect()

	reuse, decision := reuseFromOutcome(kc, publishPointer, failed, cacheFailedOK, mutation, effect)

	// Output metadata is present whenever the streams were captured — even on a run
	// that is not stored, it describes what the command produced this time. It is nil
	// only when capture was disabled.
	var output *Output
	if capture {
		output = &Output{Stdout: summarizeStream(soCap), Stderr: summarizeStream(seCap)}
	}
	exec := Execution{
		KeyInput:    kc.KeyInput,
		Exit:        runRes.Exit,
		StartedAt:   runRes.StartedAt,
		FinishedAt:  runRes.FinishedAt,
		Decision:    decision,
		Skipped:     kc.Skipped,
		Mutation:    mutation,
		EffectGuard: effect,
		Output:      output,
	}

	// A run id and a durable record are produced whenever output was captured, so a
	// non-reusable, mutating, or record-only run becomes inspectable history; only a
	// capture-disabled run has no record and no id.
	var runID runcache.RunID
	recorded := false
	if record {
		id, err := runcache.NewRunID(runRes.StartedAt.UnixNano(), s.deps.Rand)
		if err != nil {
			_ = pending.Discard()
			return Result{}, err
		}
		entry := runcache.RunEntry{
			ID:          id,
			Key:         kc.Key,
			KeyInput:    kc.KeyInput,
			StartedAt:   runRes.StartedAt,
			FinishedAt:  runRes.FinishedAt,
			Exit:        runRes.Exit,
			Stdout:      soCap,
			Stderr:      seCap,
			Decision:    decision,
			Skipped:     kc.Skipped,
			Reuse:       reuse,
			Mutation:    mutation,
			EffectGuard: effect,
		}
		obs := runcache.RunObservations{
			Before:               kc.BeforeManifest(),
			After:                afterScan.Manifest(),
			BeforeScanConfigHash: kc.BeforeScanConfigHash(),
			AfterScanConfigHash:  afterScan.Meta().ConfigHash,
		}
		if err := pending.Commit(ctx, entry, obs); err != nil {
			return Result{}, err
		}
		runID = id
		recorded = true
	} else if pending != nil {
		_ = pending.Discard()
	}

	return Result{
		Status:          kc.BaseStatus,
		Key:             kc.Key,
		ExitCode:        runRes.Exit.Code,
		Written:         publishPointer,
		Recorded:        recorded,
		Reuse:           reuse,
		RunID:           runID,
		Execution:       exec,
		Performance:     runPerformance([]inputReport{kc.InputReport, afterInput}, kc.EffectReport, effectPost),
		EffectDiagnosis: effectDiagnosisFrom(reuse, effectPost),
		effectReport:    kc.EffectReport,
	}, nil
}

// verifyPayloads confirms both of a hit entry's stored output streams match their
// recorded hashes, by opening each through the store (which verifies before
// returning a reader) and closing it. It surfaces storage corruption on the hit
// path regardless of whether the caller goes on to replay the bytes.
func (s *Service) verifyPayloads(entry runcache.RunEntry) error {
	so, err := s.deps.Store.OpenStdout(entry.ID)
	if err != nil {
		return err
	}
	_ = so.Close()
	se, err := s.deps.Store.OpenStderr(entry.ID)
	if err != nil {
		return err
	}
	_ = se.Close()
	return nil
}

// expired reports whether a cached entry is older than the run TTL and so must be
// treated as a miss. A TTL of zero (or negative) means entries never expire; the
// age is measured from when the cached run finished.
func (s *Service) expired(entry runcache.RunEntry, ttl config.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return s.deps.Clock.Now().Sub(entry.FinishedAt) > ttl.Duration()
}

// keyContext is everything the run cache derives from a request before it either
// replays a hit, executes the command, or — for explain — reports a decision. It
// is produced by buildKeyContext, which performs the scan, environment
// sanitization, executable resolution, key construction, and cacheability decision
// but never executes the command or writes an entry. Run and Explain share it so
// the explained key and decision can never diverge from the executed one.
type keyContext struct {
	EffCfg config.Config
	// EffScope is the resolved input scan scope, retained so the post-run mutation
	// guard re-observes exactly the same boundary the pre-run scan and key used.
	EffScope     config.ScanScope
	KeyInput     runcache.KeyInput
	Key          runcache.RunKey
	ChildEnv     []string
	ResolvedPath string
	Skipped      runcache.SkippedSummary
	// EffectPre is the pre-run effect observation (the watched generated-output state
	// before the command ran), folded into KeyInput.Effect and re-compared after the
	// command by the effect guard. Its status decides whether a post-run re-observation
	// is even attempted.
	EffectPre runcache.EffectObservation
	// InputReport is the diagnostic side channel of the input scan: how long the scan
	// took and how many regular files it hashed. It is never folded into the key; it
	// feeds the run's latency diagnostics on the hit path, the miss path, and the
	// read-only projections, all of which pay for the same scan.
	InputReport inputReport
	// EffectReport is the diagnostic side channel of the pre-run effect observation: its
	// measured duration and, when a watched root dominated, bounded evidence about it. It
	// is never folded into the key; it feeds the run's latency diagnostics on both the
	// hit and miss paths.
	EffectReport runcache.EffectReport
	// beforeScan is the pre-run scan and the single source of the pre-run manifest
	// (exposed via BeforeManifest). Its reduced tree hash equals KeyInput.InputTreeHash
	// (same scan), which the store verifies on commit. Its Close releases any temp spill
	// files behind that manifest; callers Close the keyContext once the manifest is no
	// longer read (after a recorded run commits its before-observation, or after
	// explain's changed-path sample).
	beforeScan scanner.Result
	CanRead    bool
	CanWrite   bool
	BaseStatus runcache.CacheStatus
	// Reason is the typed cacheability reason (the empty reason when cacheable): the
	// policy that keeps the run out of the cache (capture-disabled, record-only,
	// stdin-not-keyed, no-cache, skipped-inputs). It is a record-only run's reuse
	// reason, used directly without re-parsing a stringified token.
	Reason runcache.MismatchReason
}

// BeforeManifest is the re-openable pre-run scan manifest a recorded run persists as
// its before-observation. It is derived from beforeScan rather than stored separately,
// so it can never drift from the scan it belongs to.
func (kc keyContext) BeforeManifest() worktree.ManifestStream { return kc.beforeScan.Manifest() }

// BeforeScanConfigHash is the scanner-produced scan_config_hash of the pre-run scan,
// persisted with the before-observation so the run can be resolved to a complete
// external-state identity. It comes from the same scan as BeforeManifest.
func (kc keyContext) BeforeScanConfigHash() hashing.ConfigHash {
	return kc.beforeScan.Meta().ConfigHash
}

// Close releases the pre-run scan behind the key context, removing any temp spill
// files backing its manifest. Callers defer it once they hold the key context, so it
// runs after the before-observation is committed or inspected. It is safe on a
// zero-value key context (an in-memory scan) and more than once.
func (kc keyContext) Close() error { return kc.beforeScan.Close() }

// buildKeyContext scans the inputs, sanitizes the environment, resolves the
// executable, builds the typed key input and run key, and decides cacheability —
// the full pre-execution derivation shared by Run and Explain. It performs no
// command execution and never publishes a cache entry. persistIndex chooses whether
// the input scan writes the worktree index: Run persists it (under a presence lock)
// to keep the accelerator warm, while Explain scans read-only so it never mutates
// shared state without a lock.
func (s *Service) buildKeyContext(ctx context.Context, req Request, persistIndex bool) (keyContext, error) {
	effScope := effectiveScanScope(req.Config, req.Scope)
	// A pre-resolved effective scope (Explain reproducing a stored run) replaces the
	// derived one outright, so both the scan and the key use exactly the requested
	// scope.
	if req.EffectiveScope != nil {
		effScope = *req.EffectiveScope
	}
	if err := effScope.Validate(); err != nil {
		return keyContext{}, err
	}

	// Tolerate unreadable files so the command can still run; they are recorded as
	// skipped inputs and folded into the tree identity. The run flag decides only
	// whether their presence blocks caching, not whether the scan aborts. The scan
	// hashing/trust policy comes from the project config; the boundary is effScope.
	//
	// The rehash is forced, so this key is computed from the bytes and never from an
	// indexed hash a stat signature vouched for. The trust modes below strict cannot
	// vouch for one: a rewrite that keeps a file's size and lands inside the
	// filesystem's timestamp granularity reproduces the recorded signature exactly, and
	// ctime, dev, ino and nlink do not distinguish it either. Reusing a hash there
	// would compute the key of content that is no longer on disk — a hit that replays a
	// stale result and never runs the command, which is the one failure the cache must
	// not have. The index still accelerates every other scanner caller (checkpoint,
	// changes, diff, restore); it is only the run cache's own decisions, whose cost of
	// being wrong is a skipped execution, that decline to trust it. ForceRehash is an
	// operational safety override rather than a trust mode, so it changes no key
	// material: a run recorded before it and a run recorded after it stay comparable.
	//
	// Timed on its own — after the presence lock is already held, before the effect
	// observation and the command — so the diagnostic derived from it names a step the
	// reader can act on rather than an undifferentiated "the run was slow".
	scanStart := s.deps.Clock.Now()
	scanRes, err := s.deps.Scanner.Scan(ctx, req.Project, req.Config, effScope, scanner.Options{
		AllowSkippedInputs: true,
		ReadOnly:           !persistIndex,
		ForceRehash:        true,
		Now:                s.deps.Clock.Now(),
	})
	if err != nil {
		return keyContext{}, err
	}
	inputReport := newInputReport(s.deps.Clock.Now().Sub(scanStart).Milliseconds(), scanRes.Stats().Files)

	// Obtain skipped facts as a bounded summary rather than materializing every
	// skipped input: the run cache needs only the count, the taint, and a few
	// samples, all derived from the same reducer pass as the tree hash.
	facts := scanRes.SkippedSummary(maxSkippedSamples)
	summary := summarizeSkipped(facts, req.Policy.AllowSkippedInputs)

	// The child runs with a sanitized environment: the built-in baseline plus the
	// configured allowlist plus awa's fixed injected facts, and nothing else. The exact
	// environment the child sees is what the key records, so no unkeyed variable can
	// change the command's output behind the cache. The one child env taken here is used
	// for executable resolution (so the keyed binary is found via the PATH the command
	// actually runs with) and for the process itself.
	effEnv := s.effectiveEnv(req.Config.Run.EnvAllowlist)
	childEnv := effEnv.ChildEnv()

	// Resolve the executable for the key (best effort) before any lookup, since the
	// resolved path and stat are part of the key.
	resolvedPath, resolvedStat, _ := s.deps.Resolver.Resolve(req.Argv[0], req.AbsDir, childEnv)

	// Observe the watched generated-output roots before running: the bounded effect
	// signature is folded into the key so a later change to that state (a deleted
	// build/dist/node_modules) computes a different key and misses instead of falsely
	// hitting. It is computed here, before Compute, so the lookup and the publish path
	// key over exactly the same effect identity.
	effectPre, effectReport, err := s.observeEffect(req)
	if err != nil {
		return keyContext{}, err
	}

	keyInput := s.buildKeyInput(req, req.Config, effScope, scanRes.TreeHash(), effectPre, resolvedPath, resolvedStat, effEnv.Keyed())
	key := keyInput.Compute(s.deps.Hasher)

	canRead, canWrite, baseStatus, reason := cacheability(req.Config, req, facts.Count > 0)

	return keyContext{
		EffCfg:       req.Config,
		EffScope:     effScope,
		InputReport:  inputReport,
		KeyInput:     keyInput,
		Key:          key,
		ChildEnv:     childEnv,
		ResolvedPath: resolvedPath,
		Skipped:      summary,
		EffectPre:    effectPre,
		EffectReport: effectReport,
		beforeScan:   scanRes,
		CanRead:      canRead,
		CanWrite:     canWrite,
		BaseStatus:   baseStatus,
		Reason:       reason,
	}, nil
}

// buildKeyInput assembles the typed key input from the request, the effective
// config, and the scanned input identity.
func (s *Service) buildKeyInput(req Request, cfg config.Config, scope config.ScanScope, treeHash hashing.TreeHash, effect runcache.EffectObservation, resolvedPath, resolvedStat string, env runcache.Environment) runcache.KeyInput {
	return runcache.KeyInput{
		CacheSchemaVersion: runcache.CacheSchemaVersion,
		AwaVersion:         s.deps.AwaVersion,
		InvocationMode:     runcache.InvocationArgv,
		Command: runcache.Command{
			Argv:          append([]string(nil), req.Argv...),
			RawExecutable: req.Argv[0],
			ResolvedPath:  resolvedPath,
			ResolvedStat:  resolvedStat,
		},
		CWD:                req.CWD,
		InputTreeHash:      treeHash,
		Effect:             effect,
		IncludeScope:       scope.CanonicalInclude(),
		ExcludeScope:       append([]string(nil), scope.Exclude...),
		UseGitignore:       scope.UseGitignore,
		UseAwaignore:       scope.UseAwaignore,
		TrustMode:          cfg.Hashing.TrustMode,
		RunConfigHash:      runConfigHash(cfg, s.deps.Hasher),
		Env:                env,
		Platform:           runcache.Platform{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		StdinMode:          req.StdinMode,
		TTYAllowed:         req.TTYAllowed,
		AllowSkippedInputs: req.Policy.AllowSkippedInputs,
	}
}

// effectiveEnv assembles the environment for one run through the single domain
// builder, so the keyed checkpoint and the sanitized child environment are produced
// together and cannot disagree.
//
// The keyed side is redacted: each variable carries its presence class and, for a
// non-empty value, the value's identity fingerprint — never the raw value — so it is
// safe to fold into the key and persist. A raw value is used only to derive the
// fingerprint and to build the child env (NAME=value for present variables), and is not
// retained. The child therefore sees exactly what the key describes: the present
// allowlisted values plus awa's fixed injected facts, which are keyed too. An unset
// variable is recorded in the key but passed to nothing, and a variable set to ""
// keys differently from one unset.
func (s *Service) effectiveEnv(allowlist []string) runcache.EffectiveEnvironment {
	return runcache.BuildEffectiveEnvironment(s.deps.Hasher, runtime.GOOS, allowlist, s.deps.Env.Lookup)
}

// cacheability decides whether a run may read and write the cache, the baseline
// cache status, and the machine-stable reason when it cannot. Precedence, from most
// to least explanatory: capture disabled, --record, unkeyed stdin, --no-cache,
// skipped inputs without --allow-skipped-inputs, then the fast trust mode; otherwise
// the run is cacheable. Fast is reported last of the non-cacheable reasons so an
// explicit flag or a skipped-input correctness gap names itself, but it must stay
// ahead of --refresh: refresh grants canWrite, so a fast run reaching that arm would
// wrongly publish a pointer whose weak signature must never be replayable.
func cacheability(effCfg config.Config, req Request, hasSkipped bool) (canRead, canWrite bool, status runcache.CacheStatus, reason runcache.MismatchReason) {
	switch {
	case !effCfg.Run.CaptureOutput:
		return false, false, runcache.CacheUncached, runcache.ReasonCaptureDisabled
	case req.Policy.Record:
		// --record runs supervised history: never read a hit, never publish a pointer.
		// Capture stays on (record requires it), so the run is recorded as record-only.
		return false, false, runcache.CacheUncached, runcache.ReasonRecordOnly
	case req.StdinMode == runcache.StdinPipe || req.StdinMode == runcache.StdinTTY:
		return false, false, runcache.CacheUncached, runcache.ReasonStdinNotKeyed
	case req.Policy.NoCache:
		return false, false, runcache.CacheDisabled, runcache.ReasonNoCache
	case hasSkipped && !req.Policy.AllowSkippedInputs:
		return false, false, runcache.CacheUncached, runcache.ReasonSkippedInputs
	case effCfg.Hashing.TrustMode == config.TrustFast:
		// The fast trust mode compares inputs by size+mtime only, so a same-size,
		// same-mtime rewrite can slip past the guard. That is too weak to publish a
		// replayable hit, so a fast run executes and records but is never reusable —
		// treated like --record. It also never reads a hit: no fast pointer is ever
		// published, and reading a normal/strict entry is impossible anyway since the
		// trust mode is part of the key.
		return false, false, runcache.CacheUncached, runcache.ReasonFastTrustMode
	case req.Policy.Refresh:
		// Refresh ignores any existing hit but still writes a fresh result.
		return false, true, runcache.CacheMiss, ""
	default:
		return true, true, runcache.CacheMiss, ""
	}
}

// reuseFromOutcome derives the typed reuse state and the matching cache decision
// from one run's outcome, so the two duplicated facts are computed in one place and
// can never diverge (the entry's Validate cross-checks them). Precedence mirrors the
// old decide: a published pointer is reusable; otherwise, for a run that was
// eligible to write, the mutation guard ranks first (changed → non-reusable, scan
// failed → unknown-post-state), then a failed-not-cached command; otherwise policy
// kept the run out of the cache entirely and it is record-only. The mutation reason
// ranks first because it is the strongest safety statement: even cache_failed_runs
// would not make a mutating result reusable.
func reuseFromOutcome(kc keyContext, publishPointer, failed, cacheFailedOK bool, mutation runcache.MutationStatus, effect runcache.EffectGuardStatus) (runcache.ReuseState, runcache.CacheDecision) {
	switch {
	case publishPointer:
		return runcache.ReusableCacheEntry(), runcache.CacheDecision{Cacheable: true}
	case kc.CanWrite && !mutation.CacheableUnderMutation():
		if mutation.ScanFailed() {
			st := runcache.UnknownPostState()
			return st, runcache.CacheDecision{Cacheable: false, Reason: st.Reason().String()}
		}
		st, _ := runcache.NonReusableRun(runcache.ReasonMutatedState)
		return st, runcache.CacheDecision{Cacheable: false, Reason: runcache.ReasonMutatedState.String()}
	case kc.CanWrite && !effect.CacheableUnderEffect():
		// The command changed the watched generated-output state, or that state could
		// not be observed safely. This ranks above failed-not-cached: even
		// cache_failed_runs must not make an effect-changing result reusable.
		reason := effect.Reason()
		st, _ := runcache.NonReusableRun(reason)
		return st, runcache.CacheDecision{Cacheable: false, Reason: reason.String()}
	case kc.CanWrite && failed && !cacheFailedOK:
		st, _ := runcache.NonReusableRun(runcache.ReasonFailedNotCached)
		return st, runcache.CacheDecision{Cacheable: false, Reason: runcache.ReasonFailedNotCached.String()}
	default:
		// Policy kept the run out of the cache entirely (record-only, no-cache, unkeyed
		// stdin, skipped inputs, capture disabled): it is recorded only as history. The
		// reason is the typed one cacheability already produced, so it flows straight
		// into RecordOnly with no re-parse and no silent fallback.
		st, _ := runcache.RecordOnly(kc.Reason)
		return st, runcache.CacheDecision{Cacheable: false, Reason: kc.Reason.String()}
	}
}

// observeMutation re-scans the project after the child process and compares the
// observed input state to the pre-run baseline (the tree hash already keyed in
// kc.KeyInput). It returns the mutation status and the post-run scan, whose Manifest
// a recorded run persists as its after-observation: Unchanged/Changed with a real
// scan, or ScanFailed with a zero scan (whose Manifest is nil) when the scan errors —
// fail-closed, since an unknown post-state must never publish as reusable and has no
// honest after-observation to record. The caller closes the returned scan.
//
// The scan forces a rehash. Reusing an indexed hash here would mean deciding "did the
// child change the tree?" from the very stat signature the child can leave untouched:
// a rewrite that keeps the file's size and lands inside the filesystem's timestamp
// granularity is invisible to it. That is not a "fast" trust-mode tradeoff — the
// normal mode adds ctime, which shares mtime's granularity and so goes blind in the
// same instant, plus dev/ino/nlink, which an in-place rewrite does not touch at all.
// And strict is not an option: the trust mode is keyed material, so raising it here
// would change the scan config hash and leave the before and after scans
// incomparable. ForceRehash is an operational safety override rather than a scan
// policy, so it stays out of the key; the pre-run scan sets it for the same reason,
// which is what makes this comparison one between two equally honest observations.
//
// It also persists, and must. Having read every input, this scan holds the only
// accurate picture of the post-child worktree; the index still describes the pre-run
// one, under a stat signature the mutation may not have disturbed. The run cache no
// longer trusts that index, but everything else does — checkpoint, changes, diff,
// restore and state resolution all read it as an accelerator — and a command that
// rewrites a file in place is exactly how a wrong entry gets there. Refreshing it here
// is the one moment awa knows the truth and holds the lock, so leaving it stale would
// export this command's blind spot to every reader that never had one. Writing is safe
// because Run holds the presence lock across the child and this scan, and the write is
// the scanner's own transaction: a failed scan rolls it back and takes the fail-closed
// path below.
//
// The observation boundary is the effective run input scope, which by default observes
// gitignored files and does not apply history-only excludes, so a mutation under the
// run's own scope cannot hide; protected awa state stays excluded by the walker.
func (s *Service) observeMutation(ctx context.Context, req Request, kc keyContext) (runcache.MutationStatus, scanner.Result, inputReport) {
	// Timed on the same seam as the pre-run scan. This one can be the more expensive of
	// the two — a command that generates files hands this scan a larger tree than the
	// baseline saw — and it is paid after the command, when the user is already waiting
	// on what looks like a finished run.
	scanStart := s.deps.Clock.Now()
	scanRes, err := s.deps.Scanner.Scan(ctx, req.Project, kc.EffCfg, kc.EffScope, scanner.Options{
		AllowSkippedInputs: true,
		ForceRehash:        true,
		Now:                s.deps.Clock.Now(),
	})
	if err != nil {
		// Fail closed: an unknown post-run state must never be published as reusable.
		// The time it spent failing is not reported: a scan that produced no observation
		// has no file count to name, and the run is about to be refused for a reason that
		// matters more than its duration.
		st, _ := runcache.NewMutationStatus(runcache.MutationScanFailed)
		return st, scanner.Result{}, inputReport{}
	}
	report := newInputReport(s.deps.Clock.Now().Sub(scanStart).Milliseconds(), scanRes.Stats().Files)
	outcome := runcache.MutationUnchanged
	if scanRes.TreeHash().String() != kc.KeyInput.InputTreeHash.String() {
		outcome = runcache.MutationChanged
	}
	st, _ := runcache.NewMutationStatus(outcome)
	return st, scanRes, report
}

// observeEffect computes the bounded effect observation over the configured watched
// generated-output roots. That set is additive over a non-empty built-in one, so
// there is always something to observe and the observer rejects an empty set rather
// than inventing an identity. It is read-only, so it runs on both the run and
// explain paths. The observer is a required dependency (run.New rejects a nil one).
func (s *Service) observeEffect(req Request) (runcache.EffectObservation, runcache.EffectReport, error) {
	roots := config.EffectiveEffectRoots(req.Config.Run.WatchedEffectRoots)
	layout, err := req.Project.Paths()
	if err != nil {
		return runcache.EffectObservation{}, runcache.EffectReport{}, err
	}
	// Time the observation here, where the clock port lives (effectobserve has none), so
	// a huge watched root's stat walk carries a measured duration into the latency
	// diagnostic. The duration is diagnostic-only; it is never folded into the key.
	start := s.deps.Clock.Now()
	obs, report, err := s.deps.EffectObserver.Observe(layout.Root(), roots)
	report = report.WithDuration(s.deps.Clock.Now().Sub(start).Milliseconds())
	return obs, report, err
}

// observeEffectMutation re-observes the watched roots after the child exits and
// compares the result to the pre-run effect signature, mirroring observeMutation
// for the effect scope. It fails closed: a pre-run observation that was unavailable,
// or a post-run re-observation that cannot be computed, yields an unavailable guard
// so the run is never published as reusable.
// It also returns the post-run observation's diagnostic report so the run can raise a
// latency note for a root the command itself created or exploded — the pre-run report is
// small/clean in that case, and the cost lands here. The report is empty on the early
// return (a pre-run that was already unavailable and thus already covered by the pre-run
// report).
func (s *Service) observeEffectMutation(req Request, pre runcache.EffectObservation) (runcache.EffectGuardStatus, runcache.EffectReport) {
	if pre.Status() == runcache.EffectUnavailable {
		g, _ := runcache.NewEffectGuardStatus(runcache.EffectGuardUnavailable)
		return g, runcache.EffectReport{}
	}
	post, report, err := s.observeEffect(req)
	if err != nil || post.Status() != runcache.EffectObserved {
		g, _ := runcache.NewEffectGuardStatus(runcache.EffectGuardUnavailable)
		return g, report
	}
	outcome := runcache.EffectGuardUnchanged
	if post.Signature().String() != pre.Signature().String() {
		outcome = runcache.EffectGuardChanged
	}
	g, _ := runcache.NewEffectGuardStatus(outcome)
	return g, report
}

// summarizeSkipped projects the scanner's bounded skipped facts into the run
// cache's skipped summary, attaching whether skipped inputs were allowed. The
// samples are already bounded by the scanner (maxSkippedSamples), so no slice of
// every skipped input is held here.
func summarizeSkipped(facts worktree.SkippedFacts, allowed bool) runcache.SkippedSummary {
	sum := runcache.SkippedSummary{Count: facts.Count, Allowed: allowed}
	for _, s := range facts.Samples {
		sum.Samples = append(sum.Samples, runcache.SkippedSample{Path: s.Path, Reason: s.Reason})
	}
	return sum
}
