package perfdiag

// Interactive latency thresholds. They are centralized here — not scattered at the
// call sites — so the "when is a command slow enough to explain?" policy is one
// testable place, and tests drive it with a fake clock rather than real sleeps.
const (
	// InteractiveThresholdMs is the duration a single interactive stage must reach
	// before a latency diagnostic is worth emitting. It is deliberately conservative:
	// a dogfood pass found acceptable commands ran 0.5-1.9s while the problem
	// commands ran 2.2-34s, so 2000ms cleanly separates user-visible slowness from
	// normal work and keeps quiet commands quiet.
	InteractiveThresholdMs int64 = 2000

	// LargeRootEntriesThreshold corroborates a large-effect-root diagnostic on the
	// success path: a fully-observed watched root with at least this many entries is
	// "large" and worth naming when it also crosses the time threshold. It sits well
	// below the effect observer's per-root entry budget, so a root that is large but
	// still fully observable is flagged before it can grow past the budget and fail the
	// observation closed. It is a diagnostic policy, independent of that budget by
	// design — perfdiag does not depend on the infra walker.
	LargeRootEntriesThreshold int64 = 50_000
)

// LargeEffectRootHintKind is the stable hint kind for the record-mode next-action a
// large-effect-root diagnostic suggests: recording durable evidence instead of chasing
// a reusable cache hit over a huge generated-output tree.
const LargeEffectRootHintKind HintKind = "record-mode"

// RecordModeHintArgv is the tokenized record-mode invocation a large-effect-root
// diagnostic suggests. The trailing "<command>" is a literal placeholder — the real
// argv is often not available at diagnosis time — so it is documented as a template,
// not a copy-paste-ready command.
func RecordModeHintArgv() []string {
	return []string{"awa", "run", "--record", "--", "<command>"}
}

// ReviewRunScopeHintKind is the stable hint kind for the next-action a
// full-input-rehash diagnostic suggests: read what the run's input scope currently
// includes, and narrow it only where the scope is genuinely wider than the command.
//
// It is deliberately a "read this" action rather than a "run this to make it faster"
// one. The rehash itself is not negotiable, so the only honest lever is the size of
// the scope — and shrinking a scope past the command's real inputs does not make awa
// faster, it makes awa wrong, in the silent direction: an input the key does not see
// is an input whose change cannot miss the cache. Renderers of this hint must carry
// that warning.
const ReviewRunScopeHintKind HintKind = "review-run-scope"

// ReviewRunScopeHintArgv is the tokenized invocation the hint suggests: the help topic
// that explains what the input scope covers and how ignores shape it.
func ReviewRunScopeHintArgv() []string {
	return []string{"awa", "help", "ignores"}
}
