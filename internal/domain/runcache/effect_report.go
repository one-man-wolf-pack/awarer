package runcache

// This file holds the diagnostic side channel of an effect observation, the
// non-keyed companion to the keyed EffectObservation in effect.go/observation.go.
// Both halves describe the same domain fact — a run's project-local effect state —
// so they share one ownership boundary: the effect observer produces these values,
// and the run use case consumes them without depending on that sibling app
// subsystem. Nothing here is ever persisted or folded into the run key; these types
// exist only to explain latency.

// OverBudgetFact records that a watched root failed the observation closed because
// it exceeded its effective entry budget. The true entry count is deliberately
// absent: the walk stopped at the budget, so any count would be a fabrication —
// only the crossed threshold is honest.
type OverBudgetFact struct {
	Path  string
	Limit int
}

// LargestRootFact records the largest fully-observed watched root: its
// project-relative path and true entry count. It is set only on the success path,
// where the count is real (folded from the completed traversal).
type LargestRootFact struct {
	Path    string
	Entries int64
}

// EffectReport is the diagnostic side channel of one effect observation. It carries
// how long the observation took (the duration is stamped via WithDuration by the
// caller that owns the clock — the effect observer intentionally has none) and, when a
// single root dominated, bounded evidence about it. Every field is unexported and read
// through methods, so the value cannot be created or mutated into an invalid state: at
// most one root fact is carried ("both facts at once" is unrepresentable — a producer
// picks one via OverBudgetReport or LargestRootReport), and the duration is never
// negative (WithDuration clamps). The zero value carries no fact and no duration — the
// honest report when nothing was watched or every root was small. It is never persisted
// and never folded into the run key — it exists only to explain latency, so it can
// carry no fact that affects reuse.
type EffectReport struct {
	durationMs  int64
	overBudget  *OverBudgetFact
	largestRoot *LargestRootFact
}

// OverBudgetReport builds a report naming a watched root that failed observation
// closed by exceeding its entry budget. A degenerate fact — no path, or a
// non-positive limit — names nothing worth a latency hint, so it collapses to the
// empty report rather than being stored: the diagnostic path stays fail-soft (a bad
// hint is dropped, never emitted and never a hard error) and a stored over-budget
// fact is guaranteed meaningful. DurationMs is stamped afterward by the clock owner
// via WithDuration.
func OverBudgetReport(fact OverBudgetFact) EffectReport {
	if fact.Path == "" || fact.Limit <= 0 {
		return EffectReport{}
	}
	return EffectReport{overBudget: &fact}
}

// LargestRootReport builds a report naming the largest fully-observed watched root. A
// degenerate fact — no path, or a non-positive entry count — collapses to the empty
// report, so a stored largest-root fact is guaranteed meaningful and the diagnostic
// stays fail-soft. DurationMs is stamped afterward by the clock owner via WithDuration.
func LargestRootReport(fact LargestRootFact) EffectReport {
	if fact.Path == "" || fact.Entries <= 0 {
		return EffectReport{}
	}
	return EffectReport{largestRoot: &fact}
}

// WithDuration returns the report with its measured wall-clock duration stamped, so
// the fact producer (which has no clock) and the clock owner can build the value in
// two steps. It is the only way to set the duration — the field is unexported — so a
// report cannot be created or mutated with an arbitrary duration. A negative duration
// is meaningless for a latency hint and clamps to zero, keeping the value fail-soft.
func (r EffectReport) WithDuration(ms int64) EffectReport {
	if ms < 0 {
		ms = 0
	}
	r.durationMs = ms
	return r
}

// DurationMs returns the observation's measured wall-clock duration in milliseconds.
func (r EffectReport) DurationMs() int64 {
	return r.durationMs
}

// OverBudget returns the over-budget fact and true when the report names one.
func (r EffectReport) OverBudget() (OverBudgetFact, bool) {
	if r.overBudget == nil {
		return OverBudgetFact{}, false
	}
	return *r.overBudget, true
}

// LargestRoot returns the largest-root fact and true when the report names one.
func (r EffectReport) LargestRoot() (LargestRootFact, bool) {
	if r.largestRoot == nil {
		return LargestRootFact{}, false
	}
	return *r.largestRoot, true
}
