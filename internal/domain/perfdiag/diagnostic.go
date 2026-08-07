// Package perfdiag holds the shared, wire-facing vocabulary for actionable latency
// diagnostics: the stable machine tokens and the small value objects that carry a
// slow-but-known-cause fact from an interactive awa command to its human and JSON
// renderers.
//
// It exists so that "this command was slow" is never emitted as prose. A latency
// diagnostic must name a classified cause, carry bounded typed evidence (a path and
// either an exact count or a threshold-crossing fact), a measured duration, and — when
// one is safe and specific — a typed next-action hint. When no cause can be named, no
// diagnostic is emitted at all: silence is preferred over a generic "slow" line.
//
// The discipline mirrors internal/domain/evidence: closed token sets, value objects
// built only through constructors so an under-specified or contradictory diagnostic is
// unrepresentable, and a bounded selection (Top) so a single invocation never sprays
// notes. It is deliberately the narrow shared bottom of the latency-diagnostic funnel,
// not a second store-health model — store-read degradation keeps its own evidence
// tokens and the `awa doctor` next-action.
package perfdiag

import (
	"slices"
	"sort"
)

// closedSet indexes an ordered catalog for constant-time membership tests, so every
// closed vocabulary in this package has exactly one declaration and no hand-maintained
// second copy of it.
func closedSet[T comparable](items []T) map[T]bool {
	m := make(map[T]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// Cause is a stable, lowercase-kebab machine token naming why a command was slow. The
// set is closed so agent-facing JSON never carries a renderer-local phrase where a
// token is possible.
//
// The set is intentionally small. Further candidates (duplicate-state-scan,
// near-miss-detail-skipped, store-health-degraded, maintenance-scan) are deferred
// rather than minted as dead contract surface, and store-health degradation is left
// to the existing evidence tokens so this package does not grow into a parallel
// health model.
type Cause string

const (
	// CauseLargeEffectRoot: a watched generated-output effect root (build, dist, target,
	// node_modules, ...) dominated run state observation — either fully observed with a
	// large entry count, or so large it exceeded the observation budget and failed the
	// effect state closed. It is the diagnostic that makes a pathological `target/`
	// discoverable instead of a silent multi-second stall.
	CauseLargeEffectRoot Cause = "large-effect-root"
	// CauseFullInputRehash: a run-cache decision read and hashed every input rather
	// than standing indexed hashes in for them. Every scan that can publish, serve or
	// present a run as reusable does this, because a matching stat signature does not
	// prove matching content and a false hit skips the command outright. The cost is
	// therefore not a defect to be tuned away, and this diagnostic does not offer to
	// disable it — it exists so that a user whose worktree makes that cost visible
	// learns what they are paying for instead of finding awa mysteriously slow.
	CauseFullInputRehash Cause = "full-input-rehash"
)

// causeCatalog is the closed set of known causes in their published order, and the
// single source of truth for both Valid and Causes.
//
// It is an ordered literal rather than a map that some accessor sorts on the way out.
// Deriving order from a map means re-establishing it on every call, which no test can
// check reliably: Go randomizes map iteration per range, so an accessor that lost its
// sort still returns the right order often enough to pass. Here the order is the
// literal's, so it is a property of the declaration rather than a promise the code has
// to keep — and keeping the literal lexical is a rule a test can state exactly.
var causeCatalog = []Cause{
	CauseFullInputRehash,
	CauseLargeEffectRoot,
}

// causeSet indexes causeCatalog for constant-time validity checks. It is derived, never
// declared, so the two cannot disagree.
var causeSet = closedSet(causeCatalog)

// Valid reports whether c is a known cause.
func (c Cause) Valid() bool { return causeSet[c] }

func (c Cause) String() string { return string(c) }

// Causes returns every known cause in a stable order.
//
// It exists so that "the set of causes" has one enumerable definition rather than a
// definition plus a list of places that separately remember it. The published
// vocabulary reaches users through JSON and through the help corpus, and a cause added
// to the closed set but not to the corpus is a token a consumer meets with no way to
// look it up — the kind of gap a hand-written list of the current tokens cannot notice,
// because it only knows the tokens someone thought to write down.
func Causes() []Cause { return slices.Clone(causeCatalog) }

// Stage names the interactive latency site a diagnostic is attributed to. It is a
// distinct axis from evidence.Component (which names a store subsystem): a Stage is a
// step of an interactive command's work, not a piece of durable state. It is named
// Stage rather than Component precisely so the two `component`-shaped fields that now
// live in adjacent JSON payloads are never conflated; the JSON key is still
// "component" to match the output-style contract.
type Stage string

const (
	// StageRunEffectObservation: the bounded stat-only observation of the watched
	// generated-output effect roots, folded into the run key. It is the step that walks a
	// huge `target/` and dominates latency on generated-output-heavy projects.
	StageRunEffectObservation Stage = "run.effect-observation"
	// StageRunInputObservation: the scan of the run's declared input scope that
	// establishes the tree identity a run key is built from. It is measured alone —
	// not the presence lock it waits behind, the effect observation beside it, or the
	// command it precedes — so a diagnostic that names it is naming the step a reader
	// can act on.
	StageRunInputObservation Stage = "run.input-observation"
)

// stageCatalog is the closed set of known stages in their published order; stageSet
// indexes it. See causeCatalog for why the order lives in the literal.
var stageCatalog = []Stage{
	StageRunEffectObservation,
	StageRunInputObservation,
}

var stageSet = closedSet(stageCatalog)

// Valid reports whether s is a known stage.
func (s Stage) Valid() bool { return stageSet[s] }

func (s Stage) String() string { return string(s) }

// Stages returns every known stage in a stable order, for the same reason Causes does:
// the stage token is published as the JSON "component", so it is vocabulary a consumer
// must be able to look up.
func Stages() []Stage { return slices.Clone(stageCatalog) }

// evidenceKind discriminates the two honest shapes a large-effect-root evidence can
// take. It exists so a consumer switches on a known kind rather than guessing from
// which optional field happens to be set.
type evidenceKind int

const (
	// The zero value (unnamed) is the unconstructed evidence, which valid() rejects, so a
	// zero Evidence can never slip into a diagnostic.
	//
	// evidenceExactCount: the root was fully observed, so its true entry count is known.
	evidenceExactCount evidenceKind = iota + 1
	// evidenceThresholdCrossed: the root exceeded the observation budget and failed
	// closed, so its true count is unknowable — only the threshold it crossed is honest.
	evidenceThresholdCrossed
)

// Evidence is the bounded, typed evidence behind a latency diagnostic: a path and
// exactly one of an exact entry count (the root was fully observed) or a
// threshold-crossing fact (the root exceeded the observation budget, so its true count
// is unknowable). It is a sum type — built only through the two constructors below — so
// the contradictory shape (both a count and a threshold, or a threshold-crossed
// evidence claiming an exact count) is unrepresentable, and the JSON is always exactly
// {path, file_count} or {path, threshold_crossed, threshold}.
type Evidence struct {
	path      string
	kind      evidenceKind
	count     int64
	threshold int
}

// ExactCountEvidence builds evidence for a fully-observed root whose true entry count
// is known (from the observation's per-root entry total). It returns ok false for an
// empty path or a negative count.
func ExactCountEvidence(path string, count int64) (Evidence, bool) {
	if path == "" || count < 0 {
		return Evidence{}, false
	}
	return Evidence{path: path, kind: evidenceExactCount, count: count}, true
}

// ThresholdCrossedEvidence builds evidence for a root that exceeded the observation
// budget and failed closed, so only the crossed threshold — never a fabricated count —
// is honest. It returns ok false for an empty path or a non-positive threshold.
func ThresholdCrossedEvidence(path string, threshold int) (Evidence, bool) {
	if path == "" || threshold <= 0 {
		return Evidence{}, false
	}
	return Evidence{path: path, kind: evidenceThresholdCrossed, threshold: threshold}, true
}

// valid reports whether the evidence was built through a constructor (a real kind and
// a non-empty path), so a zero Evidence cannot slip into a diagnostic.
func (e Evidence) valid() bool {
	return e.path != "" && (e.kind == evidenceExactCount || e.kind == evidenceThresholdCrossed)
}

// Path returns the project-relative root path the evidence concerns.
func (e Evidence) Path() string { return e.path }

// ExactCount returns the observed entry count and whether it is known. The second
// return is false for a threshold-crossed evidence, where the true count is
// unknowable and callers must render the threshold instead of inventing a number.
func (e Evidence) ExactCount() (int64, bool) {
	if e.kind != evidenceExactCount {
		return 0, false
	}
	return e.count, true
}

// Threshold returns the crossed budget threshold and whether the evidence is a
// threshold-crossing fact. The second return is false for an exact-count evidence.
func (e Evidence) Threshold() (int, bool) {
	if e.kind != evidenceThresholdCrossed {
		return 0, false
	}
	return e.threshold, true
}

// HintKind is a stable, lowercase-kebab machine token naming the shape of a
// diagnostic's next-action. It is closed for the same reason Cause and Stage are: a
// consumer switches on it, and the token reaches users through JSON, so it is
// vocabulary that must be enumerable and documented rather than whatever string a
// call site happened to pass. The kinds themselves are declared with their argv in
// threshold.go, beside the policy they belong to.
type HintKind string

// hintKindCatalog is the closed set of known hint kinds in their published order; it
// lives here rather than beside the declarations so Valid, HintKinds and NewHint all
// read the same list. See causeCatalog for why the order lives in the literal.
var hintKindCatalog = []HintKind{
	LargeEffectRootHintKind,
	ReviewRunScopeHintKind,
}

var hintKindSet = closedSet(hintKindCatalog)

// Valid reports whether k is a known hint kind.
func (k HintKind) Valid() bool { return hintKindSet[k] }

func (k HintKind) String() string { return string(k) }

// HintKinds returns every known hint kind in a stable order, so the published
// vocabulary can be enumerated instead of restated.
func HintKinds() []HintKind { return slices.Clone(hintKindCatalog) }

// Hint is a typed, copyable next-action attached to a latency diagnostic: a stable
// kind token and the exact argv a machine consumer runs. It is the same shape the
// review surfaces use for suggestions, kept typed so JSON never carries a prose-only
// command string. The argv may contain a literal placeholder token (e.g. "<command>")
// when the real invocation is not available at diagnosis time.
type Hint struct {
	kind HintKind
	argv []string
}

// NewHint builds a hint from a known kind and a non-empty argv. It returns ok false
// otherwise, so a hint can never carry a bare command string without a kind, an empty
// invocation, or a kind no renderer knows how to phrase and no help topic explains.
func NewHint(kind HintKind, argv []string) (Hint, bool) {
	if !kind.Valid() || len(argv) == 0 {
		return Hint{}, false
	}
	return Hint{kind: kind, argv: append([]string(nil), argv...)}, true
}

// Kind returns the stable hint kind token.
func (h Hint) Kind() HintKind { return h.kind }

// Argv returns a copy of the tokenized invocation.
func (h Hint) Argv() []string { return append([]string(nil), h.argv...) }

// Diagnostic is one actionable latency fact: a classified cause, the measured duration
// in milliseconds, the stage it is attributed to, bounded typed evidence, and an
// optional typed hint. It is built only through NewDiagnostic, which forbids the
// under-specified shapes — no large-effect-root without a path-carrying evidence — so a
// renderer can trust every field.
type Diagnostic struct {
	cause      Cause
	durationMs int64
	stage      Stage
	evidence   Evidence
	hint       *Hint
}

// NewDiagnostic builds a latency diagnostic. It returns ok false when the cause or
// stage is unknown, the duration is negative, or the evidence was not built through a
// constructor. hint is optional; pass nil for a diagnostic that carries no next-action.
func NewDiagnostic(cause Cause, durationMs int64, stage Stage, evidence Evidence, hint *Hint) (Diagnostic, bool) {
	if !cause.Valid() || !stage.Valid() || durationMs < 0 || !evidence.valid() {
		return Diagnostic{}, false
	}
	d := Diagnostic{cause: cause, durationMs: durationMs, stage: stage, evidence: evidence}
	if hint != nil {
		h := *hint
		d.hint = &h
	}
	return d, true
}

// Cause returns the classified latency cause.
func (d Diagnostic) Cause() Cause { return d.cause }

// DurationMs returns the measured duration of the stage in milliseconds.
func (d Diagnostic) DurationMs() int64 { return d.durationMs }

// Stage returns the latency site the diagnostic is attributed to.
func (d Diagnostic) Stage() Stage { return d.stage }

// Evidence returns the bounded typed evidence.
func (d Diagnostic) Evidence() Evidence { return d.evidence }

// Hint returns the typed next-action and whether one is present.
func (d Diagnostic) Hint() (Hint, bool) {
	if d.hint == nil {
		return Hint{}, false
	}
	return *d.hint, true
}

// Top selects at most n diagnostics for one command invocation: it deduplicates by
// (cause, stage, evidence path) keeping the longest-duration instance of each, then
// returns the slowest n. It is the single place the output-style contract's "at most
// one or two performance notes per command" rule is enforced, so a command that
// observes the same huge root twice (a pre/post run pair) or across several candidates
// still surfaces it once. n <= 0 yields an empty result.
func Top(diags []Diagnostic, n int) []Diagnostic {
	if n <= 0 || len(diags) == 0 {
		return nil
	}
	type keyT struct {
		cause Cause
		stage Stage
		path  string
	}
	best := make(map[keyT]Diagnostic, len(diags))
	order := make([]keyT, 0, len(diags))
	for _, d := range diags {
		k := keyT{cause: d.cause, stage: d.stage, path: d.evidence.path}
		if prev, ok := best[k]; ok {
			if d.durationMs > prev.durationMs {
				best[k] = d
			}
			continue
		}
		best[k] = d
		order = append(order, k)
	}
	out := make([]Diagnostic, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	// Slowest first; ties keep first-seen order (stable) so the result is deterministic.
	sort.SliceStable(out, func(i, j int) bool { return out[i].durationMs > out[j].durationMs })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
