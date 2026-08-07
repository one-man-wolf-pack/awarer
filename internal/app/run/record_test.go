package run_test

import (
	"context"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

// TestRecordModeNeverServesHit proves --record always executes the command even
// when a reusable cache entry for the same inputs exists.
func TestRecordModeNeverServesHit(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "x")

	// First, a normal run stores a reusable cache entry.
	if _, err := h.svc.Run(context.Background(), h.request("echo", "a.txt")); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	before := h.runner.calls

	// A --record run with identical inputs must execute, not replay.
	req := h.request("echo", "a.txt")
	req.Policy.Record = true
	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if h.runner.calls != before+1 {
		t.Errorf("runner calls = %d, want %d (--record must execute, not hit)", h.runner.calls, before+1)
	}
	if res.Status == runcache.CacheHit {
		t.Error("--record must never report a cache hit")
	}
	if res.Reuse.Kind() != runcache.ReuseRecordOnly {
		t.Errorf("reuse = %s, want record-only", res.Reuse)
	}
	if res.Execution.Decision.Reason != string(runcache.ReasonRecordOnly) {
		t.Errorf("reason = %q, want record-only", res.Execution.Decision.Reason)
	}
	if !res.Recorded || res.RunID.IsZero() {
		t.Error("a --record run must be recorded with a resolvable id")
	}
	if res.Written {
		t.Error("a --record run must not publish a reusable pointer")
	}
}

// TestRecordModePersistsObservations proves a record-only run captures full
// evidence: command, exit, cwd, and before/after observation manifests.
func TestRecordModePersistsObservations(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "x")

	req := h.request("echo", "a.txt")
	req.Policy.Record = true
	res, err := h.svc.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	entry, err := h.store.Get(res.RunID)
	if err != nil {
		t.Fatalf("Get recorded run: %v", err)
	}
	if entry.Before == nil {
		t.Error("a record-only run must persist a before-observation")
	}
	if entry.After == nil {
		t.Error("a non-mutating record-only run must persist an after-observation")
	}
	if len(entry.KeyInput.Command.Argv) == 0 || entry.KeyInput.Command.Argv[0] != "echo" {
		t.Errorf("command not persisted: %v", entry.KeyInput.Command.Argv)
	}
	// The before-observation manifest must resolve and verify through the store.
	if _, err := h.store.OpenBeforeManifest(res.RunID); err != nil {
		t.Errorf("OpenBeforeManifest: %v", err)
	}
	if _, err := h.store.OpenAfterManifest(res.RunID); err != nil {
		t.Errorf("OpenAfterManifest: %v", err)
	}
}

// TestCaptureDisabledNotRecorded proves a run with capture disabled executes but is
// not recorded — no entry, no dangling history.
func TestCaptureDisabledNotRecorded(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.cfg.Run.CaptureOutput = false

	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Recorded {
		t.Error("a capture-disabled run must not be recorded")
	}
	if !res.RunID.IsZero() {
		t.Error("a not-recorded run must have no run id")
	}
	refs, err := h.store.ListRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("store has %d entries after a capture-disabled run, want 0", len(refs))
	}
}

// TestPostScanFailedExposesNoAfter proves a post-scan-failed run records its
// before-observation but rejects a request for the after-observation.
func TestPostScanFailedExposesNoAfter(t *testing.T) {
	h := newHarness(t, withScannerDecorator(func(s apprun.Scanner) apprun.Scanner {
		return &failPostScan{inner: s}
	}))
	defer h.cleanup()
	h.write(t, "a.txt", "x")

	res, err := h.svc.Run(context.Background(), h.request("echo", "a.txt"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Reuse.Kind() != runcache.ReuseUnknownPostState {
		t.Errorf("reuse = %s, want unknown-post-state", res.Reuse)
	}
	if !res.Recorded {
		t.Error("a post-scan-failed run must still be recorded (before-only)")
	}
	if _, err := h.store.OpenBeforeManifest(res.RunID); err != nil {
		t.Errorf("OpenBeforeManifest must succeed: %v", err)
	}
	_, err = h.store.OpenAfterManifest(res.RunID)
	if err == nil {
		t.Error("OpenAfterManifest on a post-scan-failed run must fail")
	}
}
