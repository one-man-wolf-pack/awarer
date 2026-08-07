package perfdiag_test

import (
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/perfdiag"
)

func exactCount(t *testing.T, path string, count int64) perfdiag.Evidence {
	t.Helper()
	ev, ok := perfdiag.ExactCountEvidence(path, count)
	if !ok {
		t.Fatalf("ExactCountEvidence(%q, %d) not ok", path, count)
	}
	return ev
}

func thresholdCrossed(t *testing.T, path string, threshold int) perfdiag.Evidence {
	t.Helper()
	ev, ok := perfdiag.ThresholdCrossedEvidence(path, threshold)
	if !ok {
		t.Fatalf("ThresholdCrossedEvidence(%q, %d) not ok", path, threshold)
	}
	return ev
}

func TestExactCountEvidenceRejectsBadInput(t *testing.T) {
	if _, ok := perfdiag.ExactCountEvidence("", 5); ok {
		t.Error("empty path must be rejected")
	}
	if _, ok := perfdiag.ExactCountEvidence("target", -1); ok {
		t.Error("negative count must be rejected")
	}
	ev := exactCount(t, "target", 1_600_000)
	if c, ok := ev.ExactCount(); !ok || c != 1_600_000 {
		t.Errorf("ExactCount = (%d, %v), want (1600000, true)", c, ok)
	}
	if _, ok := ev.Threshold(); ok {
		t.Error("exact-count evidence must not report a threshold")
	}
}

func TestThresholdCrossedEvidenceRejectsBadInput(t *testing.T) {
	if _, ok := perfdiag.ThresholdCrossedEvidence("", 100); ok {
		t.Error("empty path must be rejected")
	}
	if _, ok := perfdiag.ThresholdCrossedEvidence("target", 0); ok {
		t.Error("non-positive threshold must be rejected")
	}
	ev := thresholdCrossed(t, "target", 100_000)
	if th, ok := ev.Threshold(); !ok || th != 100_000 {
		t.Errorf("Threshold = (%d, %v), want (100000, true)", th, ok)
	}
	if _, ok := ev.ExactCount(); ok {
		t.Error("threshold-crossed evidence must not report an exact count")
	}
}

func TestNewDiagnosticValidation(t *testing.T) {
	ev := thresholdCrossed(t, "target", 100_000)
	// Unknown cause.
	if _, ok := perfdiag.NewDiagnostic(perfdiag.Cause("nope"), 1000, perfdiag.StageRunEffectObservation, ev, nil); ok {
		t.Error("unknown cause must be rejected")
	}
	// Unknown stage.
	if _, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, 1000, perfdiag.Stage("nope"), ev, nil); ok {
		t.Error("unknown stage must be rejected")
	}
	// Negative duration.
	if _, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, -1, perfdiag.StageRunEffectObservation, ev, nil); ok {
		t.Error("negative duration must be rejected")
	}
	// Zero-value (unconstructed) evidence.
	if _, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, 1000, perfdiag.StageRunEffectObservation, perfdiag.Evidence{}, nil); ok {
		t.Error("unconstructed evidence must be rejected")
	}
	// Valid, no hint.
	d, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, 6800, perfdiag.StageRunEffectObservation, ev, nil)
	if !ok {
		t.Fatal("valid diagnostic rejected")
	}
	if _, ok := d.Hint(); ok {
		t.Error("no hint was passed; Hint must report absent")
	}
	if d.DurationMs() != 6800 || d.Cause() != perfdiag.CauseLargeEffectRoot {
		t.Errorf("unexpected diagnostic fields: %d %s", d.DurationMs(), d.Cause())
	}
}

func TestNewHintValidation(t *testing.T) {
	if _, ok := perfdiag.NewHint("", []string{"awa"}); ok {
		t.Error("empty kind must be rejected")
	}
	if _, ok := perfdiag.NewHint(perfdiag.LargeEffectRootHintKind, nil); ok {
		t.Error("empty argv must be rejected")
	}
	h, ok := perfdiag.NewHint(perfdiag.LargeEffectRootHintKind, []string{"awa", "run", "--record"})
	if !ok {
		t.Fatal("valid hint rejected")
	}
	// Argv is copied defensively.
	got := h.Argv()
	got[0] = "mutated"
	if h.Argv()[0] != "awa" {
		t.Error("Argv must return a defensive copy")
	}
}

func TestTopDeduplicatesAndBounds(t *testing.T) {
	hint, _ := perfdiag.NewHint(perfdiag.LargeEffectRootHintKind, perfdiag.RecordModeHintArgv())
	mk := func(path string, dur int64) perfdiag.Diagnostic {
		d, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, dur, perfdiag.StageRunEffectObservation, thresholdCrossed(t, path, 100_000), &hint)
		if !ok {
			t.Fatalf("NewDiagnostic(%q) not ok", path)
		}
		return d
	}
	// Same (cause, stage, path) twice: the slower one survives, deduped to one.
	diags := []perfdiag.Diagnostic{mk("target", 3000), mk("target", 6800), mk("node_modules", 4000)}
	top := perfdiag.Top(diags, 2)
	if len(top) != 2 {
		t.Fatalf("Top len = %d, want 2", len(top))
	}
	// Slowest first: target(6800) then node_modules(4000).
	if top[0].Evidence().Path() != "target" || top[0].DurationMs() != 6800 {
		t.Errorf("top[0] = %s/%d, want target/6800", top[0].Evidence().Path(), top[0].DurationMs())
	}
	if top[1].Evidence().Path() != "node_modules" {
		t.Errorf("top[1] path = %s, want node_modules", top[1].Evidence().Path())
	}
	// Bound below the deduped count.
	if got := perfdiag.Top(diags, 1); len(got) != 1 || got[0].Evidence().Path() != "target" {
		t.Errorf("Top(_, 1) = %v, want single target", got)
	}
	// Non-positive n and empty input yield nothing.
	if got := perfdiag.Top(diags, 0); got != nil {
		t.Errorf("Top(_, 0) = %v, want nil", got)
	}
	if got := perfdiag.Top(nil, 2); got != nil {
		t.Errorf("Top(nil, 2) = %v, want nil", got)
	}
}

func TestCauseAndStageClosedSets(t *testing.T) {
	if !perfdiag.CauseLargeEffectRoot.Valid() {
		t.Error("large-effect-root must be a valid cause")
	}
	if perfdiag.Cause("duplicate-state-scan").Valid() {
		t.Error("duplicate-state-scan is deferred and must not be a valid cause")
	}
	if !perfdiag.StageRunEffectObservation.Valid() {
		t.Error("run.effect-observation must be a valid stage")
	}
	if perfdiag.Stage("status.dashboard").Valid() {
		t.Error("status.dashboard is not a minted stage")
	}
}

// TestCatalogsEnumerateExactlyTheValidTokens keeps the enumerators honest against the
// closed sets they are derived from. They exist so other packages can iterate the
// published vocabulary instead of restating it; an enumerator that drifted from Valid
// would hand those callers a list to trust that no longer describes the contract.
func TestCatalogsEnumerateExactlyTheValidTokens(t *testing.T) {
	causes := perfdiag.Causes()
	if len(causes) == 0 {
		t.Fatal("Causes() is empty")
	}
	seenCause := map[perfdiag.Cause]bool{}
	for _, c := range causes {
		if !c.Valid() {
			t.Errorf("Causes() lists %q, which Valid rejects", c)
		}
		if seenCause[c] {
			t.Errorf("Causes() lists %q twice", c)
		}
		seenCause[c] = true
	}
	if !seenCause[perfdiag.CauseLargeEffectRoot] || !seenCause[perfdiag.CauseFullInputRehash] {
		t.Errorf("Causes() = %v, want it to contain every declared cause constant", causes)
	}

	stages := perfdiag.Stages()
	if len(stages) == 0 {
		t.Fatal("Stages() is empty")
	}
	seenStage := map[perfdiag.Stage]bool{}
	for _, s := range stages {
		if !s.Valid() {
			t.Errorf("Stages() lists %q, which Valid rejects", s)
		}
		if seenStage[s] {
			t.Errorf("Stages() lists %q twice", s)
		}
		seenStage[s] = true
	}
	if !seenStage[perfdiag.StageRunEffectObservation] || !seenStage[perfdiag.StageRunInputObservation] {
		t.Errorf("Stages() = %v, want it to contain every declared stage constant", stages)
	}
}

// TestCatalogsAreCanonicallyOrdered pins the order the catalogs promise, rather than
// merely observing that two calls happened to agree.
//
// Comparing consecutive calls proves nothing about order: Go randomizes map iteration
// per range, so two calls of a sortless accessor coincide often — for a two-element
// catalog, about half the time — and that oracle goes green on the very change it
// exists to catch. Asserting sortedness on a map-derived slice has the same hole from
// the other side. So the catalogs are ordered literals and the accessors merely copy
// them, which turns the question into one a test can settle outright: is the
// declaration in lexical order? A reordered literal fails here every time, not most of
// the time.
func TestCatalogsAreCanonicallyOrdered(t *testing.T) {
	causes := perfdiag.Causes()
	if !slices.IsSortedFunc(causes, func(a, b perfdiag.Cause) int { return strings.Compare(a.String(), b.String()) }) {
		t.Errorf("Causes() = %v, want lexical order", causes)
	}
	stages := perfdiag.Stages()
	if !slices.IsSortedFunc(stages, func(a, b perfdiag.Stage) int { return strings.Compare(a.String(), b.String()) }) {
		t.Errorf("Stages() = %v, want lexical order", stages)
	}
	kinds := perfdiag.HintKinds()
	if !slices.IsSortedFunc(kinds, func(a, b perfdiag.HintKind) int { return strings.Compare(a.String(), b.String()) }) {
		t.Errorf("HintKinds() = %v, want lexical order", kinds)
	}
	// Anti-vacuity: a single-element (or empty) catalog is sorted by definition, so the
	// assertions above would prove nothing about a catalog that lost an entry.
	for name, n := range map[string]int{"Causes": len(causes), "Stages": len(stages), "HintKinds": len(kinds)} {
		if n < 2 {
			t.Errorf("%s() returned %d entries; ordering cannot be asserted on fewer than 2", name, n)
		}
	}
}

// TestHintKindCatalogEnumeratesExactlyTheValidKinds mirrors the cause/stage guard: the
// kind is a published token a consumer switches on, so the catalog and Valid must not
// drift apart.
func TestHintKindCatalogEnumeratesExactlyTheValidKinds(t *testing.T) {
	kinds := perfdiag.HintKinds()
	seen := map[perfdiag.HintKind]bool{}
	for _, k := range kinds {
		if !k.Valid() {
			t.Errorf("HintKinds() lists %q, which Valid rejects", k)
		}
		if seen[k] {
			t.Errorf("HintKinds() lists %q twice", k)
		}
		seen[k] = true
	}
	if !seen[perfdiag.LargeEffectRootHintKind] || !seen[perfdiag.ReviewRunScopeHintKind] {
		t.Errorf("HintKinds() = %v, want it to contain every declared kind constant", kinds)
	}
}

// TestNewHintRejectsAnUnknownKind proves the constructor is the closed set's boundary.
// An unknown kind would reach JSON as a token no consumer can switch on and no help
// topic explains, and would render with no hint line at all in human output — a
// next-action that silently is not one.
func TestNewHintRejectsAnUnknownKind(t *testing.T) {
	if _, ok := perfdiag.NewHint(perfdiag.HintKind("invent-a-kind"), []string{"awa", "help"}); ok {
		t.Error("an unknown hint kind must be rejected")
	}
	if _, ok := perfdiag.NewHint(perfdiag.LargeEffectRootHintKind, perfdiag.RecordModeHintArgv()); !ok {
		t.Error("a declared hint kind must be accepted")
	}
}
