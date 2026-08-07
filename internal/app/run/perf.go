package run

import (
	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
)

// maxPerfDiagnostics bounds how many latency notes one run-cache surface surfaces, so a
// command never sprays performance advice. It matches the output-style contract's "at
// most one or two performance notes per invocation".
const maxPerfDiagnostics = 2

// inputScopeEvidencePath is the path a full-input-rehash diagnostic names. The input
// walk always starts at the project root and filters from there — a narrowed --scope
// changes which paths are kept, not where the walk begins — so the project root is the
// honest subject whatever the scope, and the file count beside it says how much of it
// was actually hashed.
const inputScopeEvidencePath = "."

// inputReport is the measured cost of one run-cache input observation: how long the
// scan took and how many regular files it hashed. The count comes from the scan's own
// reduction, which saw every record, so naming it costs no second walk.
//
// It is a value, not a pointer or a pair of loose ints, so a surface either has an
// observation to report or has the zero one — which reports nothing, because a scan
// that never ran took no time.
type inputReport struct {
	durationMs int64
	files      int
}

// newInputReport builds a report from a measured duration and the scan's regular-file
// count. A negative duration (a clock that went backwards) is clamped to zero rather
// than propagated: it is a diagnostic, and an impossible one is worse than none.
func newInputReport(durationMs int64, files int) inputReport {
	if durationMs < 0 {
		durationMs = 0
	}
	return inputReport{durationMs: durationMs, files: files}
}

// runPerformance is the single assembly point for a run-cache surface's latency notes:
// every input observation it paid for plus every effect observation it made,
// deduplicated and bounded together, so the per-invocation bound is applied once over
// the whole union rather than per list.
//
// A miss passes two input observations, because it performs two: the baseline before
// the command and the mutation check after it. They are not summed and not reported
// separately — perfdiag.Top folds them by (cause, stage, path) and keeps the slower,
// so the note names the worse of the two passes. That is the honest shape: they are the
// same stage measured twice, and a user told "the input scan took N" wants N to be a
// duration something actually took, not an arithmetic result. The second pass is the
// one more likely to be named, since a command that generates files leaves it a larger
// tree than the baseline saw.
func runPerformance(inputs []inputReport, reports ...runcache.EffectReport) []perfdiag.Diagnostic {
	// The input note is selected first and keeps its seat. Ranking the whole union by
	// duration would let two slower watched roots evict it, and a scan that crossed the
	// threshold must be exposed — it is the one note that explains a cost the user
	// cannot otherwise attribute to anything, since nothing about a slow content rehash
	// is visible in the command's own output. A big `target/` at least announces itself.
	//
	// Only one seat is reserved, and only when there is something to put in it: the
	// several input reports of one surface are folded to their slowest by Top before the
	// reservation, and a quiet scan reserves nothing.
	var inputDiags []perfdiag.Diagnostic
	for _, in := range inputs {
		if d, ok := inputDiagnostic(in); ok {
			inputDiags = append(inputDiags, d)
		}
	}
	reserved := perfdiag.Top(inputDiags, 1)
	effects := perfdiag.Top(effectDiagnostics(reports...), maxPerfDiagnostics-len(reserved))
	// A final pass over a set already within the bound, so it only restores the
	// slowest-first presentation order the reservation stepped around.
	return perfdiag.Top(append(reserved, effects...), maxPerfDiagnostics)
}

// inputDiagnostic classifies one input observation, or ok false when it is not worth a
// note. The only test is the shared interactive threshold: unlike a watched effect
// root, there is no second corroborating signal to wait for, because the cost is not a
// symptom of anything misconfigured — it is what a safe cache identity costs on a
// worktree of this size. Below the threshold the scan is simply the price of doing
// business and stays silent.
func inputDiagnostic(r inputReport) (perfdiag.Diagnostic, bool) {
	if r.durationMs < perfdiag.InteractiveThresholdMs {
		return perfdiag.Diagnostic{}, false
	}
	ev, ok := perfdiag.ExactCountEvidence(inputScopeEvidencePath, int64(r.files))
	if !ok {
		return perfdiag.Diagnostic{}, false
	}
	hint, _ := perfdiag.NewHint(perfdiag.ReviewRunScopeHintKind, perfdiag.ReviewRunScopeHintArgv())
	return perfdiag.NewDiagnostic(perfdiag.CauseFullInputRehash, r.durationMs,
		perfdiag.StageRunInputObservation, ev, &hint)
}

// effectDiagnostics classifies the effect-observation reports of one run surface into
// latency diagnostics. A run's miss path observes the watched roots twice — once
// before the command (folded into the key) and once after, which is where a command that
// *creates or explodes* a watched root (a build that fills target/) actually pays — so
// both reports are passed and deduplicated: a root that a fast/clean pre-run missed but a
// fail-closed post-run caught still surfaces. A watched root that failed the observation
// closed on its entry budget is always worth naming (the true count is unknowable, so
// only the crossed threshold is reported); a fully-observed root is named only when it is
// both large and slow, so a normal small project stays quiet. Deduplication and the
// per-invocation bound are applied by runPerformance over the whole union, not here.
func effectDiagnostics(reports ...runcache.EffectReport) []perfdiag.Diagnostic {
	var diags []perfdiag.Diagnostic
	for _, report := range reports {
		ev, ok := effectEvidence(report)
		if !ok {
			continue
		}
		// Build the hint only once a diagnostic is actually being formed, so the common
		// quiet path (no root crossed a threshold) allocates nothing.
		hint, _ := perfdiag.NewHint(perfdiag.LargeEffectRootHintKind, perfdiag.RecordModeHintArgv())
		if d, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, report.DurationMs(),
			perfdiag.StageRunEffectObservation, ev, &hint); ok {
			diags = append(diags, d)
		}
	}
	return diags
}

// effectEvidence derives the bounded evidence for a large-effect-root diagnostic from one
// report, or ok false when the report names no root worth a note. A budget overrun is
// always evidence (threshold-crossing, no fabricated count); a fully-observed root is
// evidence only when it is both large and slow.
func effectEvidence(report runcache.EffectReport) (perfdiag.Evidence, bool) {
	if f, ok := report.OverBudget(); ok {
		return perfdiag.ThresholdCrossedEvidence(f.Path, f.Limit)
	}
	if f, ok := report.LargestRoot(); ok &&
		report.DurationMs() >= perfdiag.InteractiveThresholdMs &&
		f.Entries >= perfdiag.LargeRootEntriesThreshold {
		return perfdiag.ExactCountEvidence(f.Path, f.Entries)
	}
	return perfdiag.Evidence{}, false
}
