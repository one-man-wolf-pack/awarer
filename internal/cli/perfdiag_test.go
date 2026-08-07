package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/app/status"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
	"awarer/internal/output/inspect"
)

func mustThresholdDiag(t *testing.T, path string, threshold int, durMs int64) perfdiag.Diagnostic {
	t.Helper()
	ev, ok := perfdiag.ThresholdCrossedEvidence(path, threshold)
	if !ok {
		t.Fatalf("ThresholdCrossedEvidence(%q,%d)", path, threshold)
	}
	hint, _ := perfdiag.NewHint(perfdiag.LargeEffectRootHintKind, perfdiag.RecordModeHintArgv())
	d, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, durMs, perfdiag.StageRunEffectObservation, ev, &hint)
	if !ok {
		t.Fatal("NewDiagnostic")
	}
	return d
}

func mustExactDiag(t *testing.T, path string, count int64, durMs int64) perfdiag.Diagnostic {
	t.Helper()
	ev, ok := perfdiag.ExactCountEvidence(path, count)
	if !ok {
		t.Fatalf("ExactCountEvidence(%q,%d)", path, count)
	}
	d, ok := perfdiag.NewDiagnostic(perfdiag.CauseLargeEffectRoot, durMs, perfdiag.StageRunEffectObservation, ev, nil)
	if !ok {
		t.Fatal("NewDiagnostic")
	}
	return d
}

func TestGroupThousands(t *testing.T) {
	cases := map[int64]string{0: "0", 12: "12", 999: "999", 1000: "1,000", 100000: "100,000", 1628868: "1,628,868", -4200: "-4,200"}
	for in, want := range cases {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPerformanceNoteThresholdCrossedNeverFabricatesCount(t *testing.T) {
	// The fail-closed over-budget case must say "exceeds N", never a fabricated count.
	line := performanceNoteLine(mustThresholdDiag(t, "target", 100_000, 6800))
	if !strings.Contains(line, `effect root "target" exceeds 100,000 files`) {
		t.Errorf("note = %q, want an 'exceeds 100,000 files' phrase", line)
	}
	if !strings.Contains(line, "6.8s") {
		t.Errorf("note = %q, want the measured duration 6.8s", line)
	}
	if strings.Contains(line, "contains") {
		t.Errorf("threshold-crossed note must not claim an exact count: %q", line)
	}
}

func TestPerformanceNoteExactCount(t *testing.T) {
	line := performanceNoteLine(mustExactDiag(t, "node_modules", 1_628_868, 3200))
	if !strings.Contains(line, `effect root "node_modules" contains 1,628,868 files`) {
		t.Errorf("note = %q, want an exact 'contains 1,628,868 files' phrase", line)
	}
}

func TestEmitPerformanceNotesWritesNoteAndHintToStderr(t *testing.T) {
	var out, errb bytes.Buffer
	w := output.New(&out, &errb)
	emitPerformanceNotes(w, []perfdiag.Diagnostic{mustThresholdDiag(t, "target", 100_000, 6800)})
	if out.Len() != 0 {
		t.Errorf("performance notes must not touch stdout, got %q", out.String())
	}
	errStr := errb.String()
	if !strings.Contains(errStr, "note: run state observation took") {
		t.Errorf("stderr missing note line: %q", errStr)
	}
	if !strings.Contains(errStr, "hint: use `awa run --record`") {
		t.Errorf("stderr missing hint line: %q", errStr)
	}
}

func TestEmitPerformanceNotesQuietWhenEmpty(t *testing.T) {
	var out, errb bytes.Buffer
	w := output.New(&out, &errb)
	emitPerformanceNotes(w, nil)
	if out.Len() != 0 || errb.Len() != 0 {
		t.Errorf("empty diagnostics must emit nothing, got out=%q err=%q", out.String(), errb.String())
	}
}

func TestPerfDiagViewThresholdShape(t *testing.T) {
	doc := perfDiagView(mustThresholdDiag(t, "target", 100_000, 6800))
	if doc.Code != "large-effect-root" || doc.DurationMs != 6800 || doc.Component != "run.effect-observation" {
		t.Errorf("unexpected doc header: %+v", doc)
	}
	if doc.Evidence.Path != "target" || !doc.Evidence.ThresholdCrossed || doc.Evidence.Threshold == nil || *doc.Evidence.Threshold != 100_000 {
		t.Errorf("unexpected threshold evidence: %+v", doc.Evidence)
	}
	if doc.Evidence.FileCount != nil {
		t.Error("threshold-crossed doc must not carry file_count")
	}
	if doc.Hint == nil || doc.Hint.Kind != "record-mode" || len(doc.Hint.Argv) == 0 {
		t.Errorf("unexpected hint: %+v", doc.Hint)
	}
}

func TestPerfDiagViewExactShape(t *testing.T) {
	doc := perfDiagView(mustExactDiag(t, "node_modules", 1_628_868, 3200))
	if doc.Evidence.FileCount == nil || *doc.Evidence.FileCount != 1_628_868 {
		t.Errorf("expected exact file_count, got %+v", doc.Evidence)
	}
	if doc.Evidence.ThresholdCrossed || doc.Evidence.Threshold != nil {
		t.Error("exact-count doc must not carry a threshold")
	}
	if doc.Hint != nil {
		t.Error("this diagnostic had no hint; doc.Hint must be nil")
	}
}

func TestPerfDiagnosticsViewNilWhenEmpty(t *testing.T) {
	if perfDiagnosticsView(nil) != nil {
		t.Error("no diagnostics must project to a nil block so JSON omits it")
	}
	block := perfDiagnosticsView([]perfdiag.Diagnostic{mustExactDiag(t, "target", 60_000, 2500)})
	if block == nil || len(block.Performance) != 1 {
		t.Fatalf("expected a one-item block, got %+v", block)
	}
}

// assertLargeEffectRootDoc checks a projected performance doc carries the full
// contract for a threshold-crossed large effect root: stable code, numeric duration,
// component, typed threshold evidence, and a typed record-mode hint argv.
func assertLargeEffectRootDoc(t *testing.T, doc perfDiagDoc) {
	t.Helper()
	if doc.Code != "large-effect-root" {
		t.Errorf("code = %q, want large-effect-root", doc.Code)
	}
	if doc.DurationMs != 6800 {
		t.Errorf("duration_ms = %d, want 6800", doc.DurationMs)
	}
	if doc.Component != "run.effect-observation" {
		t.Errorf("component = %q, want run.effect-observation", doc.Component)
	}
	if doc.Evidence.Path != "target" || !doc.Evidence.ThresholdCrossed || doc.Evidence.Threshold == nil || *doc.Evidence.Threshold != 100_000 {
		t.Errorf("evidence = %+v, want target/threshold-crossed/100000", doc.Evidence)
	}
	if doc.Hint == nil || doc.Hint.Kind != "record-mode" || len(doc.Hint.Argv) == 0 || doc.Hint.Argv[0] != "awa" {
		t.Errorf("hint = %+v, want a record-mode awa argv", doc.Hint)
	}
}

// TestRunViewWiresPerformance protects the run JSON wiring: a Result carrying a latency
// diagnostic must surface it under data.diagnostics.performance. Without this a reviewer
// could drop the projection in runView and most latency tests would stay green.
func TestRunViewWiresPerformance(t *testing.T) {
	res := apprun.Result{
		Status:      runcache.CacheMiss,
		Performance: []perfdiag.Diagnostic{mustThresholdDiag(t, "target", 100_000, 6800)},
	}
	doc := runView(res, inspect.DefaultDisplayMode(), apprun.RunInlineDiagnosis{})
	if doc.Diagnostics == nil || len(doc.Diagnostics.Performance) != 1 {
		t.Fatalf("run JSON did not carry the performance diagnostic: %+v", doc.Diagnostics)
	}
	assertLargeEffectRootDoc(t, doc.Diagnostics.Performance[0])

	// The quiet path omits the diagnostics block entirely (omitempty).
	if quiet := runView(apprun.Result{Status: runcache.CacheMiss}, inspect.DefaultDisplayMode(), apprun.RunInlineDiagnosis{}); quiet.Diagnostics != nil {
		t.Errorf("quiet run must omit diagnostics, got %+v", quiet.Diagnostics)
	}
}

// TestLsViewWiresPerformance protects the run.ls JSON wiring for both plain and --near:
// the latency diagnostic must surface under data.diagnostics.performance, which is always
// present (empty array when quiet).
func TestLsViewWiresPerformance(t *testing.T) {
	for _, near := range []bool{false, true} {
		res := apprun.LsResult{Performance: []perfdiag.Diagnostic{mustThresholdDiag(t, "target", 100_000, 6800)}}
		doc := lsView(res, near)
		if doc.Near != near {
			t.Errorf("near = %v, want %v", doc.Near, near)
		}
		if len(doc.Diagnostics.Performance) != 1 {
			t.Fatalf("near=%v run.ls did not carry the performance diagnostic: %+v", near, doc.Diagnostics)
		}
		assertLargeEffectRootDoc(t, doc.Diagnostics.Performance[0])

		// Quiet listing still carries the block, with an empty (non-nil) array.
		quiet := lsView(apprun.LsResult{}, near)
		if quiet.Diagnostics.Performance == nil {
			t.Errorf("near=%v quiet run.ls must carry an empty performance array, got nil", near)
		}
	}
}

// TestStatusReviewJSONWiresPerformance protects the status JSON wiring: a review whose
// diagnostics are populated must serialize them under data.review.diagnostics.performance,
// and omit the block when quiet.
func TestStatusReviewJSONWiresPerformance(t *testing.T) {
	diags := []perfdiag.Diagnostic{mustThresholdDiag(t, "target", 100_000, 6800)}
	doc := statusDoc{Result: status.Result{}, Review: reviewDoc{Diagnostics: perfDiagnosticsView(diags)}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal statusDoc: %v", err)
	}
	var decoded struct {
		Review struct {
			Diagnostics *perfDiagnosticsDoc `json:"diagnostics"`
		} `json:"review"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal statusDoc: %v\n%s", err, raw)
	}
	if decoded.Review.Diagnostics == nil || len(decoded.Review.Diagnostics.Performance) != 1 {
		t.Fatalf("status review did not carry the performance diagnostic under review.diagnostics:\n%s", raw)
	}
	assertLargeEffectRootDoc(t, decoded.Review.Diagnostics.Performance[0])

	// Quiet review omits the diagnostics block (omitempty).
	quietRaw, err := json.Marshal(statusDoc{Result: status.Result{}, Review: reviewDoc{Diagnostics: perfDiagnosticsView(nil)}})
	if err != nil {
		t.Fatalf("marshal quiet statusDoc: %v", err)
	}
	if strings.Contains(string(quietRaw), "diagnostics") {
		t.Errorf("quiet status review must omit the diagnostics key:\n%s", quietRaw)
	}
}

// mustInputDiag builds a full-input-rehash diagnostic the way the run package does.
func mustInputDiag(t *testing.T, files int64, durMs int64) perfdiag.Diagnostic {
	t.Helper()
	ev, ok := perfdiag.ExactCountEvidence(".", files)
	if !ok {
		t.Fatalf("ExactCountEvidence(%q,%d)", ".", files)
	}
	hint, _ := perfdiag.NewHint(perfdiag.ReviewRunScopeHintKind, perfdiag.ReviewRunScopeHintArgv())
	d, ok := perfdiag.NewDiagnostic(perfdiag.CauseFullInputRehash, durMs, perfdiag.StageRunInputObservation, ev, &hint)
	if !ok {
		t.Fatal("NewDiagnostic")
	}
	return d
}

// TestInputRehashNoteExplainsWhatTheTimeBought asserts the note answers the question a
// slow command actually raises. "run input observation took 3.4s" alone invites the
// reader to look for a misconfiguration; what they need to know is that the time bought
// a cache identity that a same-stat rewrite cannot fool.
func TestInputRehashNoteExplainsWhatTheTimeBought(t *testing.T) {
	line := performanceNoteLine(mustInputDiag(t, 12_400, 3400))
	for _, want := range []string{"run input observation", "3.4s", "12,400 files", `"."`, "file content"} {
		if !strings.Contains(line, want) {
			t.Errorf("note = %q, want it to contain %q", line, want)
		}
	}
	if strings.Contains(line, "effect root") {
		t.Errorf("note = %q, want the input-scan wording, not the effect-root one", line)
	}
}

// TestInputRehashHintRefusesToRecommendShrinkingTheScopeBlindly is the finding this
// hint most needs to survive: the fastest way to act on "your input scan is slow" is to
// exclude files, and the reader who excludes a real input gets a faster awa that is
// also wrong — silently, because an unobserved input cannot invalidate a hit.
func TestInputRehashHintRefusesToRecommendShrinkingTheScopeBlindly(t *testing.T) {
	line, ok := performanceHintLine(mustInputDiag(t, 12_400, 3400))
	if !ok {
		t.Fatal("a full-input-rehash diagnostic must render its hint")
	}
	if !strings.Contains(line, "awa help ignores") {
		t.Errorf("hint = %q, want it to point at the scope help topic", line)
	}
	if !strings.Contains(line, "never exclude") {
		t.Errorf("hint = %q, want an explicit warning against excluding a real input", line)
	}
}

// TestInputRehashJSONCarriesTheTypedTokens pins the machine contract: a consumer reads
// the cause and stage as tokens, the duration as a number, and the evidence as an exact
// count — never by parsing the human sentence.
func TestInputRehashJSONCarriesTheTypedTokens(t *testing.T) {
	doc := perfDiagView(mustInputDiag(t, 12_400, 3400))
	if doc.Code != "full-input-rehash" {
		t.Errorf("code = %q, want full-input-rehash", doc.Code)
	}
	if doc.Component != "run.input-observation" {
		t.Errorf("component = %q, want run.input-observation", doc.Component)
	}
	if doc.DurationMs != 3400 {
		t.Errorf("duration_ms = %d, want 3400", doc.DurationMs)
	}
	if doc.Evidence.Path != "." || doc.Evidence.FileCount == nil || *doc.Evidence.FileCount != 12_400 {
		t.Errorf("evidence = %+v, want path \".\" with file_count 12400", doc.Evidence)
	}
	if doc.Evidence.ThresholdCrossed {
		t.Error("an exactly-counted input scan must not claim a crossed threshold")
	}
	if doc.Hint == nil || doc.Hint.Kind != "review-run-scope" {
		t.Errorf("hint = %+v, want a review-run-scope hint", doc.Hint)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"file_count":12400`)) {
		t.Errorf("json = %s, want an exact file_count", raw)
	}
}
