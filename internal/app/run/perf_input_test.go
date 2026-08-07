package run_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/perfdiag"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/projfs"
)

// slowScanner makes the harness clock jump while a scan is in flight, so a test can put
// an input observation on either side of the interactive threshold without sleeping and
// without depending on how fast the host hashes a temp directory.
//
// It advances before delegating, which is what the measurement sees: the run reads the
// clock, scans, and reads it again. The jump therefore lands inside the measured window
// and nowhere else — the effect observation and the child are timed separately, so a
// diagnostic this produces can only be about the input scan.
// The plan is consumed one entry per scan, with the last entry repeating, so a test
// can price the pre-run and post-run passes of a miss differently — which is the only
// way to tell which of the two a diagnostic is actually reporting.
type slowScanner struct {
	inner apprun.Scanner
	clock *fakeClock
	plan  []time.Duration
	scans int
}

func (s *slowScanner) Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts scanner.Options) (scanner.Result, error) {
	by := s.plan[min(s.scans, len(s.plan)-1)]
	s.scans++
	s.clock.now = s.clock.now.Add(by)
	return s.inner.Scan(ctx, project, cfg, scope, opts)
}

// newSlowHarness builds a harness whose scans cost the given durations in order, the
// last one repeating for every later scan.
func newSlowHarness(t *testing.T, plan ...time.Duration) (*harness, *slowScanner) {
	t.Helper()
	slow := &slowScanner{plan: plan}
	h := newHarness(t, withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
		slow.inner = inner
		return slow
	}))
	slow.clock = h.clock
	return h, slow
}

// onlyInputDiag returns the single full-input-rehash note, failing if the surface
// produced a different number of them.
func onlyInputDiag(t *testing.T, diags []perfdiag.Diagnostic) perfdiag.Diagnostic {
	t.Helper()
	var found []perfdiag.Diagnostic
	for _, d := range diags {
		if d.Cause() == perfdiag.CauseFullInputRehash {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("got %d full-input-rehash notes among %d diagnostics, want exactly 1", len(found), len(diags))
	}
	return found[0]
}

// TestASlowCacheHitExplainsItsInputScan is the point of the whole diagnostic: the hit
// path is where the cost is least expected and least explicable. Nothing ran, the
// result was replayed, and the user still waited — so that wait is exactly what needs a
// name.
func TestASlowCacheHitExplainsItsInputScan(t *testing.T) {
	h, _ := newSlowHarness(t, time.Duration(perfdiag.InteractiveThresholdMs)*time.Millisecond)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	hit, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if hit.Status != runcache.CacheHit {
		t.Fatalf("status = %v, want a hit", hit.Status)
	}
	d := onlyInputDiag(t, hit.Performance)
	if d.Stage() != perfdiag.StageRunInputObservation {
		t.Errorf("stage = %s, want run.input-observation", d.Stage())
	}
	if d.DurationMs() != perfdiag.InteractiveThresholdMs {
		t.Errorf("duration = %dms, want the %dms the scan actually took", d.DurationMs(), perfdiag.InteractiveThresholdMs)
	}
}

// TestAFastRunSaysNothingAboutItsInputScan pins the quiet path. A diagnostic that
// appears on every run is not a diagnostic, it is noise with a duration in it.
func TestAFastRunSaysNothingAboutItsInputScan(t *testing.T) {
	h, _ := newSlowHarness(t, time.Duration(perfdiag.InteractiveThresholdMs-1)*time.Millisecond)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Performance) != 0 {
		t.Errorf("a run below the threshold emitted %d notes, want silence", len(res.Performance))
	}
	hit, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hit.Performance) != 0 {
		t.Errorf("a hit below the threshold emitted %d notes, want silence", len(hit.Performance))
	}
}

// TestTheInputNoteCountsTheFilesTheScanHashed proves the evidence tracks the scan's own
// reduction rather than a number invented beside it — and that naming it costs no
// second walk, since the run still issues exactly the two scans it always did.
func TestTheInputNoteCountsTheFilesTheScanHashed(t *testing.T) {
	h, slow := newSlowHarness(t, time.Duration(perfdiag.InteractiveThresholdMs)*time.Millisecond)
	defer h.cleanup()
	for i := range 3 {
		h.write(t, fmt.Sprintf("f%d.txt", i), "x")
	}

	first, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := onlyInputDiag(t, first.Performance).Evidence().ExactCount()
	if before < 3 {
		t.Fatalf("counted %d files, want at least the 3 written", before)
	}
	scans := slow.scans

	h.write(t, "f3.txt", "x")
	second, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := onlyInputDiag(t, second.Performance).Evidence().ExactCount()
	if after != before+1 {
		t.Errorf("after adding one file the note counted %d, want %d", after, before+1)
	}
	if got := slow.scans - scans; got != 2 {
		t.Errorf("the run issued %d scans, want the usual 2: the file count must come from the scan's own reduction, not a second walk", got)
	}
}

// TestReadOnlyReuseSurfacesExplainTheirInputScan covers the surfaces that answer "could
// this replay?" without running anything. They perform the same content rehash the run
// path performs, so a user who waits on `run explain` or `run ls` — or on the status
// footer those feed — is owed the same explanation.
func TestReadOnlyReuseSurfacesExplainTheirInputScan(t *testing.T) {
	h, _ := newSlowHarness(t, time.Duration(perfdiag.InteractiveThresholdMs)*time.Millisecond)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	exp, err := h.svc.Explain(context.Background(), h.explainCmd("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	onlyInputDiag(t, exp.Performance)

	ls, err := h.svc.Ls(context.Background(), h.lsRequest())
	if err != nil {
		t.Fatal(err)
	}
	onlyInputDiag(t, ls.Performance)
}

// TestASlowPostRunScanIsExplainedToo covers the half of a miss that is easiest to leave
// unmeasured and worst to leave unexplained. The command has finished, its output is
// already on screen, and awa is still reading the worktree — and after a command that
// generates files, that second pass has more to read than the baseline did.
//
// One note, not two: the two passes are the same stage measured twice, so they fold and
// the slower one is reported. The pricing here is deliberately lopsided — a pre-run scan
// below the threshold and a post-run scan well above it — so a diagnostic can only be
// present if the post-run pass was measured at all.
func TestASlowPostRunScanIsExplainedToo(t *testing.T) {
	fast := time.Duration(perfdiag.InteractiveThresholdMs-1) * time.Millisecond
	slow := time.Duration(perfdiag.InteractiveThresholdMs*3) * time.Millisecond
	h, slowScan := newSlowHarness(t, fast, slow, fast)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")

	miss, err := h.svc.Run(context.Background(), h.request("build", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if slowScan.scans != 2 {
		t.Fatalf("the miss issued %d scans, want the pre-run and post-run pair", slowScan.scans)
	}
	d := onlyInputDiag(t, miss.Performance)
	if d.DurationMs() != perfdiag.InteractiveThresholdMs*3 {
		t.Errorf("duration = %dms, want the post-run pass (%dms); the pre-run pass was below the threshold, so a shorter duration means the post-run scan went unmeasured",
			d.DurationMs(), perfdiag.InteractiveThresholdMs*3)
	}

	// The hit performs only the pre-run scan, priced fast here, so it must be silent —
	// it cannot inherit the previous run's post-scan measurement.
	hit, err := h.svc.Run(context.Background(), h.request("build", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if hit.Status != runcache.CacheHit {
		t.Fatalf("status = %v, want a hit", hit.Status)
	}
	if len(hit.Performance) != 0 {
		t.Errorf("the hit emitted %d notes, want silence: it performed one fast scan and must report only that one", len(hit.Performance))
	}
}
