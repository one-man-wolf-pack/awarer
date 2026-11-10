package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

const footerRunID = "18e32e3d74d80be05b5a6ee1e5a03d80"

// footerResult builds a recorded, executed run result for the human footer renderer.
// Each case adjusts only the facts it is about.
func footerResult(t *testing.T, reuse runcache.ReuseState) apprun.Result {
	t.Helper()
	id, err := runcache.ParseRunID(footerRunID)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := runcache.NewExecutionCWD(".")
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := runcache.NewMutationStatus(runcache.MutationUnchanged)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_800_000_000, 0)
	res := apprun.Result{
		Status:   runcache.CacheMiss,
		Recorded: true,
		Reuse:    reuse,
		RunID:    id,
		Execution: apprun.Execution{
			KeyInput:   runcache.KeyInput{CWD: cwd, Command: runcache.Command{Argv: []string{"go", "test", "./..."}}},
			Exit:       runcache.ExitStatus{Kind: runcache.ExitNormal},
			StartedAt:  start,
			FinishedAt: start.Add(1200 * time.Millisecond),
			Mutation:   unchanged,
			Output:     &apprun.Output{},
		},
	}
	res.Written = reuse.IsReusable()
	return res
}

func renderFooter(res apprun.Result, diag apprun.RunInlineDiagnosis) (string, string) {
	var out, errb bytes.Buffer
	emitRunFooter(output.New(&out, &errb), res, diag)
	return out.String(), errb.String()
}

func mustReuse(t *testing.T, kind runcache.ReuseKind, reason runcache.MismatchReason) runcache.ReuseState {
	t.Helper()
	r, err := runcache.NewReuseState(kind, reason)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestRunFooterBaseDensity pins the compact grammar for clean retained executions and
// replays: one summary row and the exact stored-output inspection command, nothing
// else, and never on stdout.
func TestRunFooterBaseDensity(t *testing.T) {
	inspect := "inspect: awa run show " + footerRunID + " --tail 200\n"
	for _, tc := range []struct {
		name  string
		reuse runcache.ReuseState
		hit   bool
		want  string
	}{
		{"stored", runcache.ReusableCacheEntry(), false, "awa run: stored 18e32e3d74d8 exit=0 duration=1.2s cwd=.\n" + inspect},
		{"hit", runcache.ReusableCacheEntry(), true, "awa run: hit (replay) 18e32e3d74d8 exit=0 duration=1.2s cwd=.\n" + inspect},
		{"record", mustReuse(t, runcache.ReuseRecordOnly, runcache.ReasonRecordOnly), false, "awa run: record-only 18e32e3d74d8 exit=0 duration=1.2s cwd=.\n" + inspect},
		{"no-cache", mustReuse(t, runcache.ReuseRecordOnly, runcache.ReasonNoCache), false, "awa run: record-only 18e32e3d74d8 exit=0 duration=1.2s cwd=.\n" + inspect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := footerResult(t, tc.reuse)
			if tc.hit {
				res.Status = runcache.CacheHit
				res.Written = false
				res.Execution.Mutation = runcache.MutationStatus{}
			}
			stdout, stderr := renderFooter(res, apprun.RunInlineDiagnosis{})
			if stdout != "" {
				t.Errorf("footer wrote to stdout: %q", stdout)
			}
			if stderr != tc.want {
				t.Errorf("footer =\n%s\nwant\n%s", stderr, tc.want)
			}
		})
	}
}

// TestRunFooterTechnicalReasonsStayVisible confirms a record-only kind with a technical
// reason keeps that reason, unlike deliberate recording, and a failed or signaled child
// keeps its exit.
func TestRunFooterTechnicalReasonsStayVisible(t *testing.T) {
	for _, reason := range []runcache.MismatchReason{runcache.ReasonStdinNotKeyed, runcache.ReasonSkippedInputs, runcache.ReasonExternalFactsUnavailable} {
		_, stderr := renderFooter(footerResult(t, mustReuse(t, runcache.ReuseRecordOnly, reason)), apprun.RunInlineDiagnosis{})
		if !strings.Contains(stderr, "not-reusable("+reason.String()+")") {
			t.Errorf("%s: technical reason hidden:\n%s", reason, stderr)
		}
	}

	failed := footerResult(t, mustReuse(t, runcache.ReuseNonReusable, runcache.ReasonFailedNotCached))
	failed.Execution.Exit = runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 3}
	_, stderr := renderFooter(failed, apprun.RunInlineDiagnosis{})
	if !strings.Contains(stderr, " exit=3 ") || !strings.Contains(stderr, "not-reusable(failed-not-cached)") {
		t.Errorf("failed run footer:\n%s", stderr)
	}

	signaled := footerResult(t, mustReuse(t, runcache.ReuseNonReusable, runcache.ReasonFailedNotCached))
	signaled.Execution.Exit = runcache.ExitStatus{Kind: runcache.ExitSignaled, Signal: "SIGTERM"}
	_, stderr = renderFooter(signaled, apprun.RunInlineDiagnosis{})
	if !strings.Contains(stderr, " exit=signal:SIGTERM ") {
		t.Errorf("signaled run footer:\n%s", stderr)
	}
}

// TestRunFooterNeverRebuildsArgv confirms no footer line offers a command rebuilt from
// argv, and that a multiline cwd cannot add report rows or terminal controls.
func TestRunFooterNeverRebuildsArgv(t *testing.T) {
	res := footerResult(t, mustReuse(t, runcache.ReuseRecordOnly, runcache.ReasonRecordOnly))
	res.Execution.KeyInput.Command.Argv = []string{"sh", "-c", "printf 'a\nb'\x1b[31m"}
	cwd, err := runcache.NewExecutionCWD("sub/dir\x1b]0;x")
	if err != nil {
		t.Fatal(err)
	}
	res.Execution.KeyInput.CWD = cwd
	_, stderr := renderFooter(res, apprun.RunInlineDiagnosis{})
	if strings.Contains(stderr, "explain") || strings.Contains(stderr, "printf") || strings.ContainsRune(stderr, 0x1b) {
		t.Errorf("footer reconstructs argv or leaks controls:\n%q", stderr)
	}
	if n := strings.Count(stderr, "\n"); n != 2 {
		t.Errorf("footer has %d lines, want summary and inspect:\n%s", n, stderr)
	}
}

// TestRunFooterConditionalRisks confirms each materially degraded fact earns its own
// line: not-recorded output, capture truncation, allowed skipped inputs, and observed
// mutation with its temporal warning and exact full-id compare.
func TestRunFooterConditionalRisks(t *testing.T) {
	notRecorded := footerResult(t, mustReuse(t, runcache.ReuseRecordOnly, runcache.ReasonCaptureDisabled))
	notRecorded.Recorded = false
	notRecorded.RunID = runcache.RunID{}
	notRecorded.Execution.Output = nil
	_, stderr := renderFooter(notRecorded, apprun.RunInlineDiagnosis{})
	if strings.Contains(stderr, "inspect:") || !strings.Contains(stderr, "awa run: uncached exit=0") ||
		!strings.Contains(stderr, "not-reusable(capture-disabled)") || !strings.Contains(stderr, "nothing to inspect later") {
		t.Errorf("not-recorded footer:\n%s", stderr)
	}

	truncated := footerResult(t, runcache.ReusableCacheEntry())
	truncated.Execution.Output.Stderr = apprun.StreamSummary{Truncated: true, OmittedBytes: 42}
	truncated.Execution.Skipped = runcache.SkippedSummary{Count: 2, Allowed: true}
	_, stderr = renderFooter(truncated, apprun.RunInlineDiagnosis{})
	for _, want := range []string{
		"truncated: stored stderr is incomplete evidence — 42 bytes omitted at the capture limit",
		"warning: 2 input(s) skipped (not read); skipping permitted by --allow-skipped-inputs",
		"inspect: awa run show " + footerRunID + " --tail 200",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("footer missing %q:\n%s", want, stderr)
		}
	}

	mutated := footerResult(t, mustReuse(t, runcache.ReuseNonReusable, runcache.ReasonMutatedState))
	changed, err := runcache.NewMutationStatus(runcache.MutationChanged)
	if err != nil {
		t.Fatal(err)
	}
	mutated.Execution.Mutation = changed
	_, stderr = renderFooter(mutated, apprun.RunInlineDiagnosis{})
	for _, want := range []string{
		"not-reusable(mutated-state)",
		"warning: observed state changed during this run's execution window",
		"compare: awa changes run:" + footerRunID + ":before..run:" + footerRunID + ":after",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("mutating footer missing %q:\n%s", want, stderr)
		}
	}

	// Mutation stays visible even when no compare is possible: an unrecorded run has
	// no stored observations to compare.
	unrecordedMutation := mutated
	unrecordedMutation.Recorded = false
	unrecordedMutation.RunID = runcache.RunID{}
	_, stderr = renderFooter(unrecordedMutation, apprun.RunInlineDiagnosis{})
	if !strings.Contains(stderr, "warning: observed state changed during this run's execution window") || strings.Contains(stderr, "compare:") {
		t.Errorf("unrecorded mutating footer:\n%s", stderr)
	}

	// A clean non-reusable recording is history, not a delta to compare.
	_, stderr = renderFooter(footerResult(t, mustReuse(t, runcache.ReuseRecordOnly, runcache.ReasonStdinNotKeyed)), apprun.RunInlineDiagnosis{})
	if strings.Contains(stderr, "compare:") || strings.Contains(stderr, "warning:") {
		t.Errorf("clean non-reusable footer offers a compare or warning:\n%s", stderr)
	}
}

// TestRunFooterValidationReplayDisclosed confirms a result-only validation hit names
// the replay and its unobserved directories next to the compact summary.
func TestRunFooterValidationReplayDisclosed(t *testing.T) {
	res := footerResult(t, runcache.ReusableCacheEntry())
	res.Status = runcache.CacheHit
	res.Written = false
	res.Execution.Mutation = runcache.MutationStatus{}
	res.Execution.KeyInput.Validation = &runcache.ValidationScope{Label: "check", UnobservedDirs: []string{"target"}}
	_, stderr := renderFooter(res, apprun.RunInlineDiagnosis{})
	if !strings.Contains(stderr, "awa run: hit (replay) ") || !strings.Contains(stderr, "replay: result only (policy check); not observed or recreated: target") {
		t.Errorf("validation hit footer:\n%s", stderr)
	}
}

// TestRunFooterAllowedSkipIsNotAReuseClaim confirms an allowed skip on a run that is
// not reusable for another reason keeps both facts: the skip warning and the
// summary's own non-reusable state, with no suggestion that the flag made it reusable.
func TestRunFooterAllowedSkipIsNotAReuseClaim(t *testing.T) {
	res := footerResult(t, mustReuse(t, runcache.ReuseRecordOnly, runcache.ReasonStdinNotKeyed))
	res.Execution.Skipped = runcache.SkippedSummary{Count: 1, Allowed: true}
	_, stderr := renderFooter(res, apprun.RunInlineDiagnosis{})
	if !strings.Contains(stderr, "not-reusable(stdin-not-keyed)") ||
		!strings.Contains(stderr, "warning: 1 input(s) skipped (not read); skipping permitted by --allow-skipped-inputs") ||
		strings.Contains(stderr, "reuse allowed") {
		t.Errorf("allowed skip on a non-reusable run:\n%s", stderr)
	}
}

// TestRunFooterObservationLimitsSurviveDeliberateRecording confirms incomplete
// observations stay visible from the execution's own facts when the primary reason is
// deliberate recording, without turning that recording into a cache defect, calling
// an unknown observation a change, or repeating a limit the summary already names.
func TestRunFooterObservationLimitsSurviveDeliberateRecording(t *testing.T) {
	scanFailed, err := runcache.NewMutationStatus(runcache.MutationScanFailed)
	if err != nil {
		t.Fatal(err)
	}
	effectUnavailable, err := runcache.NewEffectGuardStatus(runcache.EffectGuardUnavailable)
	if err != nil {
		t.Fatal(err)
	}
	const (
		skippedLine = "warning: 1 input(s) skipped (not read); this run's observation is incomplete"
		scanLine    = "warning: the post-run observation failed; whether observed state changed during this run is unknown"
		effectLine  = "warning: watched generated-output state could not be observed safely; its state after this run is unknown"
	)
	for _, reason := range []runcache.MismatchReason{runcache.ReasonRecordOnly, runcache.ReasonNoCache} {
		t.Run(reason.String(), func(t *testing.T) {
			res := footerResult(t, mustReuse(t, runcache.ReuseRecordOnly, reason))
			res.Execution.Skipped = runcache.SkippedSummary{Count: 1}
			res.Execution.Mutation = scanFailed
			res.Execution.EffectGuard = effectUnavailable
			_, stderr := renderFooter(res, apprun.RunInlineDiagnosis{})
			for _, want := range []string{"awa run: record-only 18e32e3d74d8 exit=0 duration=1.2s cwd=.\n", skippedLine, scanLine, effectLine} {
				if !strings.Contains(stderr, want) {
					t.Errorf("footer missing %q:\n%s", want, stderr)
				}
			}
			if strings.Contains(stderr, "not-reusable") || strings.Contains(stderr, "compare:") || strings.Contains(stderr, "execution window") {
				t.Errorf("deliberate recording shown as a defect or an unknown shown as a change:\n%s", stderr)
			}
		})
	}

	// Each limit the summary already names as its reason is not repeated.
	for _, tc := range []struct {
		kind   runcache.ReuseKind
		reason runcache.MismatchReason
		set    func(*apprun.Result)
		line   string
	}{
		{runcache.ReuseRecordOnly, runcache.ReasonSkippedInputs, func(r *apprun.Result) { r.Execution.Skipped = runcache.SkippedSummary{Count: 1} }, skippedLine},
		{runcache.ReuseUnknownPostState, runcache.ReasonPostScanFailed, func(r *apprun.Result) { r.Execution.Mutation = scanFailed }, scanLine},
		{runcache.ReuseNonReusable, runcache.ReasonEffectStateUnavailable, func(r *apprun.Result) {
			r.Execution.EffectGuard = effectUnavailable
			r.EffectDiagnosis = &apprun.EffectDiagnosis{Reason: runcache.ReasonEffectStateUnavailable}
		}, effectLine},
	} {
		res := footerResult(t, mustReuse(t, tc.kind, tc.reason))
		tc.set(&res)
		_, stderr := renderFooter(res, apprun.RunInlineDiagnosis{})
		if !strings.Contains(stderr, "not-reusable("+tc.reason.String()+")") || strings.Contains(stderr, tc.line) {
			t.Errorf("%s: limit missing from summary or repeated:\n%s", tc.reason, stderr)
		}
	}
}
