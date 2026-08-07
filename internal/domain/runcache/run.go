package runcache

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"awarer/internal/domain/config"
)

// runIDHexLen is the rendered length of a RunID: 8 bytes of big-endian timestamp
// plus 8 random bytes, hex-encoded. Time-prefixing makes ids sort in run order,
// so "run log" newest-first and a corruption-free latest lookup fall out of the
// id alone, while the random suffix keeps concurrent runs from colliding.
const runIDHexLen = 32

// runIDShortLen is the number of leading characters shown in human output. The
// repository, which knows every id, confirms a short form is unambiguous before
// acting on it; Short itself is display only.
const runIDShortLen = 12

// RunID uniquely identifies a stored run. Like a scan id it is time-prefixed hex
// with a random suffix; unlike a checkpoint id it sorts chronologically, which the
// run log relies on.
type RunID struct {
	hex string
}

// NewRunID builds a RunID from a unix-nanosecond timestamp and a source of
// randomness. The timestamp leads so lexical order matches chronological order.
// rand is injected so tests can supply a deterministic source.
func NewRunID(unixNano int64, rand io.Reader) (RunID, error) {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(unixNano))
	if _, err := io.ReadFull(rand, buf[8:]); err != nil {
		return RunID{}, fmt.Errorf("generating run id: %w", err)
	}
	return RunID{hex: hex.EncodeToString(buf[:])}, nil
}

// ParseRunID validates a persisted run id: exactly runIDHexLen lowercase hex
// characters, so a stray filename in the runs directory cannot pass as an id.
func ParseRunID(s string) (RunID, error) {
	if len(s) != runIDHexLen {
		return RunID{}, fmt.Errorf("invalid run id %q: want %d hex characters", s, runIDHexLen)
	}
	if !isLowerHex(s) {
		return RunID{}, fmt.Errorf("invalid run id %q: non-hex character", s)
	}
	return RunID{hex: s}, nil
}

// ValidateRunIDPrefix checks that s is a syntactically valid short id prefix: a
// non-empty run of lowercase hex no longer than a full id. It lets a repository
// tell malformed input from a well-formed prefix that simply matches nothing, so
// a blank or garbage reference is never treated as "match all".
func ValidateRunIDPrefix(s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty", ErrInvalidPrefix)
	}
	if len(s) > runIDHexLen {
		return fmt.Errorf("%w: %q is longer than a full id (%d)", ErrInvalidPrefix, s, runIDHexLen)
	}
	if !isLowerHex(s) {
		return fmt.Errorf("%w: %q has a non-hex character", ErrInvalidPrefix, s)
	}
	return nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// String returns the full hex id.
func (id RunID) String() string { return id.hex }

// Short returns the leading runIDShortLen characters for human display. It is
// never a lookup key on its own — prefix resolution belongs to the repository.
func (id RunID) Short() string {
	if len(id.hex) <= runIDShortLen {
		return id.hex
	}
	return id.hex[:runIDShortLen]
}

// IsZero reports whether the id is unset.
func (id RunID) IsZero() bool { return id.hex == "" }

// ExitKind distinguishes how a process ended. A process that failed to start is
// not modeled here: a start failure is an awa execution failure, never a cached
// command result, so it never produces an ExitStatus.
type ExitKind int

const (
	// ExitNormal is a process that exited with a status code.
	ExitNormal ExitKind = iota
	// ExitSignaled is a process the platform reported as killed by a signal.
	ExitSignaled
)

func (k ExitKind) String() string {
	switch k {
	case ExitNormal:
		return "normal"
	case ExitSignaled:
		return "signaled"
	default:
		return "unknown"
	}
}

// Valid reports whether k is a known exit kind.
func (k ExitKind) Valid() bool { return k == ExitNormal || k == ExitSignaled }

// ParseExitKind resolves a persisted token.
func ParseExitKind(s string) (ExitKind, error) {
	switch s {
	case "normal":
		return ExitNormal, nil
	case "signaled":
		return ExitSignaled, nil
	default:
		return 0, fmt.Errorf("invalid exit kind %q: want normal or signaled", s)
	}
}

// ExitStatus is how a completed command process ended: its exit code and, when
// the platform reported the process was signaled, the signal name. Code is the
// value awa returns to its own caller — the conventional 128+signal synthesis on
// a signaled process — so a hit and a miss surface the same exit code.
type ExitStatus struct {
	Kind   ExitKind
	Code   int
	Signal string
}

// Validate checks that an exit status is self-consistent: a signaled process
// names a signal, a normal exit does not, and the code is a non-negative process
// exit code. The code becomes awa's own exit status on a hit, so a negative
// (impossible) value would be corrupt durable state driving the CLI.
func (s ExitStatus) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("exit status has invalid kind %v", s.Kind)
	}
	if s.Code < 0 {
		return fmt.Errorf("exit status has a negative code %d", s.Code)
	}
	if s.Kind == ExitSignaled && s.Signal == "" {
		return fmt.Errorf("signaled exit status has no signal name")
	}
	// A signaled process is recorded with the conventional 128+signal code, so its
	// code is always at least 128. A lower value (e.g. a hand-edited 0) would replay
	// as success on a hit while the metadata says the process was killed.
	if s.Kind == ExitSignaled && s.Code < 128 {
		return fmt.Errorf("signaled exit status has code %d, below the 128+signal floor", s.Code)
	}
	if s.Kind == ExitNormal && s.Signal != "" {
		return fmt.Errorf("normal exit status %d carries a signal name %q", s.Code, s.Signal)
	}
	return nil
}

// Failed reports whether the process did not exit successfully — a non-zero exit
// code or a signal. A successful run has a zero code and no signal.
func (s ExitStatus) Failed() bool {
	return s.Kind == ExitSignaled || s.Code != 0
}

// ExitOrigin names the provenance of a recorded exit status. A stored run record only
// ever holds the wrapped child command's own result — a wrapper/start failure is an awa
// execution error and never produces an ExitStatus (see ExitKind) — so a recorded exit's
// origin is always the child. It is protocol provenance, not variable domain state: it
// lets a consumer distinguish the wrapped process's exit from an awa-origin error without
// guessing across overlapping numeric codes.
type ExitOrigin int

const (
	// ExitOriginChild is the wrapped command process's own exit — the only origin a
	// stored ExitStatus carries. It is iota+1 so the zero value is an undefined origin.
	ExitOriginChild ExitOrigin = iota + 1
)

// String returns the stable machine token for the origin.
func (o ExitOrigin) String() string {
	switch o {
	case ExitOriginChild:
		return "child"
	default:
		return "unknown"
	}
}

// Origin returns the provenance of this recorded exit: always the child command, since
// awa never persists a wrapper/start failure as an ExitStatus. It is the single source of
// the exit-origin token both the run and run-evidence JSON surfaces project.
func (s ExitStatus) Origin() ExitOrigin { return ExitOriginChild }

// CacheStatus is the outcome of a run with respect to the cache, reported to the
// user and persisted on the entry.
type CacheStatus int

const (
	// CacheHit replayed a stored result without executing the command.
	CacheHit CacheStatus = iota
	// CacheMiss executed the command and (policy permitting) stored the result.
	CacheMiss
	// CacheDisabled executed without reading or writing the cache (--no-cache).
	CacheDisabled
	// CacheUncached executed but could not use the cache: skipped inputs without
	// --allow-skipped-inputs, a piped stdin, or capture disabled.
	CacheUncached
)

func (s CacheStatus) String() string {
	switch s {
	case CacheHit:
		return "hit"
	case CacheMiss:
		return "miss"
	case CacheDisabled:
		return "disabled"
	case CacheUncached:
		return "uncached"
	default:
		return "unknown"
	}
}

// CacheDecision records whether a result was cacheable and, when it was not, a
// short machine-stable reason so explain and JSON output can say why.
type CacheDecision struct {
	Cacheable bool
	Reason    string
}

// Validate checks that a decision is self-consistent: a cacheable result carries
// no "why not" reason, and a non-cacheable result always names one, so explain
// and JSON output can never show a blank reason or a reason that contradicts the
// flag.
func (d CacheDecision) Validate() error {
	if d.Cacheable && d.Reason != "" {
		return fmt.Errorf("cacheable decision carries a reason %q", d.Reason)
	}
	if !d.Cacheable && d.Reason == "" {
		return fmt.Errorf("non-cacheable decision has no reason")
	}
	return nil
}

// SkippedSummary describes the skipped inputs an input scan produced: how many
// there were, whether they were allowed into the key, and a bounded sample for
// diagnostics. It is persisted so explain can report why a run was uncached.
type SkippedSummary struct {
	Count   int
	Allowed bool
	Samples []SkippedSample
}

// SkippedSample is one skipped input recorded for diagnostics.
type SkippedSample struct {
	Path   string
	Reason string
}

// Validate checks that a skipped summary is self-consistent: a non-negative
// count, a sample set no larger than the count (samples are a bounded subset of
// the skipped inputs), and every sample carrying both a path and a reason so a
// persisted diagnostic can never surface a blank entry.
func (s SkippedSummary) Validate() error {
	if s.Count < 0 {
		return fmt.Errorf("skipped summary has negative count %d", s.Count)
	}
	if len(s.Samples) > s.Count {
		return fmt.Errorf("skipped summary has %d samples but a count of only %d", len(s.Samples), s.Count)
	}
	for i, sm := range s.Samples {
		if sm.Path == "" {
			return fmt.Errorf("skipped sample %d has no path", i)
		}
		if sm.Reason == "" {
			return fmt.Errorf("skipped sample %d (%s) has no reason", i, sm.Path)
		}
	}
	return nil
}

// RunEntry is the persisted record of one executed run: its identity and key,
// the structured key document, the outcome (exit status, timestamps, captured
// output metadata), the cache decision, and — for a real execution — its typed
// reuse classification and pre/post observed-state identities. Completed entries
// are immutable.
//
// A recorded run is history first and a cache entry only when reusable: Reuse
// classifies which. Before/After are the observed-state manifests the mutation
// guard captured: every real execution observed its pre-run state, so Before is
// always present, and After is nil exactly when the post-run scan failed.
// Mutation is a derived, duplicated field — its source of truth is the before/after
// manifests — kept on the entry for cheap inspection and timeline labeling and
// cross-checked against the observations on read.
type RunEntry struct {
	ID         RunID
	Key        RunKey
	KeyInput   KeyInput
	StartedAt  time.Time
	FinishedAt time.Time
	Exit       ExitStatus
	Stdout     OutputCapture
	Stderr     OutputCapture
	Decision   CacheDecision
	Skipped    SkippedSummary
	Reuse      ReuseState
	Mutation   MutationStatus
	// EffectGuard is the post-run comparison of the watched generated-output roots
	// (build, dist, target, node_modules, ...) — the effect scope excluded from the
	// input scan. It is the backing fact for the effect-state disqualification
	// reasons, cross-checked against the reuse state on read the way Mutation is.
	EffectGuard EffectGuardStatus
	Before      *Observation
	After       *Observation
}

// maxExitCode returns the largest exit code a process on the given platform can
// produce. A Unix exit status is an 8-bit value (0-255); a Windows exit code is a
// 32-bit DWORD. Unknown platforms get the conservative Unix bound.
func maxExitCode(goos string) int64 {
	if goos == "windows" {
		return 1<<32 - 1
	}
	return 255
}

// DurationMs returns the wall-clock duration of the run in milliseconds.
func (e RunEntry) DurationMs() int64 {
	return e.FinishedAt.Sub(e.StartedAt).Milliseconds()
}

// Validate checks that a completed run entry is well-formed before it is
// persisted, so a builder bug cannot write a record Get would later reject.
func (e RunEntry) Validate() error {
	if e.ID.IsZero() {
		return fmt.Errorf("run entry has no id")
	}
	if e.Key.IsZero() {
		return fmt.Errorf("run entry has no key")
	}
	if e.StartedAt.IsZero() {
		return fmt.Errorf("run entry has no start time")
	}
	if e.FinishedAt.IsZero() {
		return fmt.Errorf("run entry has no finish time")
	}
	if e.FinishedAt.Before(e.StartedAt) {
		return fmt.Errorf("run entry finishes before it starts")
	}
	if err := e.Exit.Validate(); err != nil {
		return err
	}
	if err := e.Stdout.Validate(); err != nil {
		return fmt.Errorf("stdout capture: %w", err)
	}
	if err := e.Stderr.Validate(); err != nil {
		return fmt.Errorf("stderr capture: %w", err)
	}
	if err := e.KeyInput.Validate(); err != nil {
		return fmt.Errorf("key input: %w", err)
	}
	// The exit code's valid range depends on the platform the run executed on, which
	// the key records. A Unix wait status is an 8-bit value (0-255); a code above it
	// could not come from a real Unix process and, returned to os.Exit, would be
	// truncated — breaking miss/hit parity. Windows uses a wider 32-bit code.
	if max := maxExitCode(e.KeyInput.Platform.GOOS); int64(e.Exit.Code) > max {
		return fmt.Errorf("exit code %d exceeds the maximum %d for platform %s", e.Exit.Code, max, e.KeyInput.Platform.GOOS)
	}
	if err := e.Decision.Validate(); err != nil {
		return fmt.Errorf("cache decision: %w", err)
	}
	// A recorded run is history first; whether it is also a reusable cache entry is a
	// property of its typed reuse state, cross-checked here so an impossible state —
	// cacheable but mutated, a reusable record without a clean post-state, a reuse
	// reason that contradicts the cache decision — is rejected at the read boundary
	// rather than trusted.
	if err := e.validateReuse(); err != nil {
		return err
	}
	if err := e.validateObservations(); err != nil {
		return err
	}
	if err := e.Skipped.Validate(); err != nil {
		return fmt.Errorf("skipped summary: %w", err)
	}
	// The skipped-input policy is recorded in two places the write path always fills
	// from the same flag, so they must agree for every run.
	if e.Skipped.Allowed != e.KeyInput.AllowSkippedInputs {
		return fmt.Errorf("skipped-allowed %v disagrees with key input allow-skipped-inputs %v", e.Skipped.Allowed, e.KeyInput.AllowSkippedInputs)
	}
	// A reusable entry with skipped inputs must have allowed them: a cache hit cannot
	// faithfully cover inputs the scan could not read. A record-only run may carry
	// skipped inputs precisely because they were not allowed — that is the reason it
	// is not cacheable — so the rule is scoped to reusable entries.
	if e.Reuse.IsReusable() && e.Skipped.Count > 0 && !e.Skipped.Allowed {
		return fmt.Errorf("reusable run entry has %d skipped inputs but does not allow them", e.Skipped.Count)
	}
	return nil
}

// validateReuse cross-checks the typed reuse state against the cache decision and
// the mutation outcome, so the two duplicated facts (reuse and decision) and the
// derived mutation status can never contradict each other.
func (e RunEntry) validateReuse() error {
	if e.Reuse.IsReusable() != e.Decision.Cacheable {
		return fmt.Errorf("reuse state %s disagrees with cache decision cacheable=%v", e.Reuse, e.Decision.Cacheable)
	}
	if !e.Reuse.IsReusable() && e.Reuse.Reason().String() != e.Decision.Reason {
		return fmt.Errorf("reuse reason %q disagrees with cache decision reason %q", e.Reuse.Reason(), e.Decision.Reason)
	}
	// Every persisted run is a real execution that the mutation guard observed
	// before and after the command. Unobserved is the guard's "did not run / not
	// observed" zero value: it belongs to a replayed hit's transient result, never to
	// durable history. A record carrying it is corrupt — diagnostic history that
	// silently never measured its own effect — so it is rejected for every kind, not
	// only the reusable one.
	if e.Mutation.Outcome() == MutationUnobserved {
		return fmt.Errorf("recorded run entry has unobserved mutation outcome; a persisted run must have been observed")
	}
	// The effect guard is run for every execution, so its unset zero value never
	// belongs to durable history — a record carrying it never measured its own effect
	// on the watched roots and is corrupt.
	if e.EffectGuard.Outcome() == EffectGuardUnset {
		return fmt.Errorf("recorded run entry has unset effect guard outcome; a persisted run must have been observed")
	}
	switch e.Reuse.Kind() {
	case ReuseReusable:
		// A reusable entry's command must have left observed state explicitly
		// unchanged. Not-changed-and-not-scan-failed is too weak: an unobserved
		// outcome would also pass it, yet an unobserved run never proved its state was
		// unchanged, so it must never replay as a hit. Requiring exactly Unchanged also
		// keeps the mutation status honest against the persisted observations a reusable
		// run carries.
		if e.Mutation.Outcome() != MutationUnchanged {
			return fmt.Errorf("reusable run entry has mutation outcome %v, want unchanged", e.Mutation.Outcome())
		}
		// A reusable entry must also have left the watched effect state explicitly
		// unchanged, carry an effect identity safe for reuse, and not have been
		// observed under the fast trust mode — whose stat-only input comparison can miss a
		// same-size/same-mtime rewrite. These make "reusable ⇒ effect-safe ∧ trust≠fast"
		// unrepresentable on the read path, not merely discouraged.
		if !e.EffectGuard.CacheableUnderEffect() {
			return fmt.Errorf("reusable run entry has effect guard outcome %v, want unchanged", e.EffectGuard.Outcome())
		}
		if !e.KeyInput.Effect.SafeForReuse() {
			return fmt.Errorf("reusable run entry has effect status %v, which is not safe for reuse", e.KeyInput.Effect.Status())
		}
		if e.KeyInput.TrustMode == config.TrustFast {
			return fmt.Errorf("reusable run entry was observed under the fast trust mode")
		}
	case ReuseUnknownPostState:
		// The absent after-observation this kind implies is not restated here:
		// validateObservations owns observation cardinality and derives it from the
		// same scan-failed outcome.
		if !e.Mutation.ScanFailed() {
			return fmt.Errorf("unknown-post-state run entry has mutation outcome %v, want scan-failed", e.Mutation.Outcome())
		}
	case ReuseNonReusable:
		// A non-reusable run was eligible to cache but disqualified after the fact. Each
		// disqualification reason has an entry-sourced fact that must back it, so a
		// hand-edited record cannot claim a disqualification the outcome contradicts: a
		// mutated-state run must have changed observed state, and a failed-not-cached run
		// must actually have failed.
		switch e.Reuse.Reason() {
		case ReasonMutatedState:
			if !e.Mutation.Changed() {
				return fmt.Errorf("non-reusable run entry reason %q but mutation outcome is %v", ReasonMutatedState, e.Mutation.Outcome())
			}
		case ReasonEffectStateDiffers:
			if !e.EffectGuard.Changed() {
				return fmt.Errorf("non-reusable run entry reason %q but effect guard outcome is %v", ReasonEffectStateDiffers, e.EffectGuard.Outcome())
			}
		case ReasonEffectStateUnavailable:
			if !e.EffectGuard.Unavailable() {
				return fmt.Errorf("non-reusable run entry reason %q but effect guard outcome is %v", ReasonEffectStateUnavailable, e.EffectGuard.Outcome())
			}
		case ReasonFailedNotCached:
			if !e.Exit.Failed() {
				return fmt.Errorf("non-reusable run entry reason %q but exit status %d did not fail", ReasonFailedNotCached, e.Exit.Code)
			}
		}
	case ReuseRecordOnly:
		if err := e.validateRecordOnlyReason(); err != nil {
			return err
		}
	}
	return nil
}

// validateRecordOnlyReason cross-checks a record-only run's policy reason against
// the entry-sourced facts that policy keyed it on, so a persisted record cannot
// claim a reason its own metadata contradicts. capture-disabled is impossible on a
// record at all (a captured run is, by definition, not capture-disabled); skipped-
// inputs and stdin-not-keyed each have a keyed fact that must agree. record-only and
// no-cache are pure invocation flags with no on-entry trace, so they carry no extra
// check here beyond the policy-reason vocabulary the constructor already enforces.
func (e RunEntry) validateRecordOnlyReason() error {
	switch e.Reuse.Reason() {
	case ReasonCaptureDisabled:
		return fmt.Errorf("record-only run entry carries reason %q, but a recorded run captured output", ReasonCaptureDisabled)
	case ReasonSkippedInputs:
		if e.Skipped.Count == 0 || e.Skipped.Allowed {
			return fmt.Errorf("record-only run entry reason %q but it has %d skipped inputs (allowed=%v)", ReasonSkippedInputs, e.Skipped.Count, e.Skipped.Allowed)
		}
	case ReasonStdinNotKeyed:
		if m := e.KeyInput.StdinMode; m != StdinPipe && m != StdinTTY {
			return fmt.Errorf("record-only run entry reason %q but stdin mode is %v", ReasonStdinNotKeyed, m)
		}
	case ReasonFastTrustMode:
		if e.KeyInput.TrustMode != config.TrustFast {
			return fmt.Errorf("record-only run entry reason %q but trust mode is %v", ReasonFastTrustMode, e.KeyInput.TrustMode)
		}
	}
	return nil
}

// validateObservations owns the observation cardinality invariant and cross-checks
// the pre/post manifest identities against the keyed input tree hash and the
// mutation outcome. Every entry is a real execution the mutation guard observed
// before the command, so a before-observation is always present; an after-observation
// is present exactly when the post-run scan succeeded, its absence meaning that scan
// failed and nothing else. The before-manifest must reproduce the tree hash already
// folded into the key, and the after-manifest's equality with the before-manifest must
// match the recorded mutation outcome — so Mutation is verified against its source of
// truth rather than trusted.
func (e RunEntry) validateObservations() error {
	if e.Before == nil {
		return fmt.Errorf("run entry has no before-observation")
	}
	if err := e.Before.Validate(); err != nil {
		return fmt.Errorf("before observation: %w", err)
	}
	if e.Before.Manifest.TreeHash != e.KeyInput.InputTreeHash {
		return fmt.Errorf("before observation tree hash %s does not match keyed input tree hash %s", e.Before.Manifest.TreeHash, e.KeyInput.InputTreeHash)
	}
	if wantAfter := !e.Mutation.ScanFailed(); wantAfter != (e.After != nil) {
		if wantAfter {
			return fmt.Errorf("run entry has no after-observation but its post-run scan did not fail")
		}
		return fmt.Errorf("run entry carries an after-observation but its post-run scan failed")
	}
	if e.After != nil {
		if err := e.After.Validate(); err != nil {
			return fmt.Errorf("after observation: %w", err)
		}
		changed := e.After.Manifest.TreeHash != e.Before.Manifest.TreeHash
		if changed != e.Mutation.Changed() {
			return fmt.Errorf("after observation (changed=%v) disagrees with mutation outcome %v", changed, e.Mutation.Outcome())
		}
		// One run request observes both states under one immutable effective config, so
		// the before and after scan policy identities must be identical. A post-scan
		// failure has no after observation at all (and therefore no fabricated after
		// policy), so this only constrains a genuine before/after pair.
		if e.Before.ScanConfigHash != e.After.ScanConfigHash {
			return fmt.Errorf("after observation scan config hash %s disagrees with before observation scan config hash %s", e.After.ScanConfigHash, e.Before.ScanConfigHash)
		}
	}
	return nil
}
