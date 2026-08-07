package run_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

// lsEntries runs Ls and returns just the reusable-now rows, so tests that predate
// the near-miss view keep asserting over a plain slice.
func lsEntries(svc *apprun.Service, req apprun.LsRequest) ([]apprun.LsEntry, error) {
	res, err := svc.Ls(context.Background(), req)
	return res.Reusable, err
}

// onlyEntry returns the single ls entry, failing if the count is not exactly one.
func onlyEntry(t *testing.T, entries []apprun.LsEntry) apprun.LsEntry {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("got %d ls entries, want 1", len(entries))
	}
	return entries[0]
}

// lsAll runs run ls --all and fails on error.
func (h *harness) lsAll(t *testing.T) []apprun.LsEntry {
	t.Helper()
	req := h.lsRequest()
	req.All = true
	entries, err := lsEntries(h.svc, req)
	if err != nil {
		t.Fatalf("ls --all: %v", err)
	}
	return entries
}

func (h *harness) lsRequest() apprun.LsRequest {
	return apprun.LsRequest{Project: h.proj, Config: h.cfg}
}

func TestLsListsReusableEntry(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")

	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	entries, err := lsEntries(h.svc, h.lsRequest())
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ls returned %d entries, want 1", len(entries))
	}
	if !entries[0].Reusable() {
		t.Errorf("entry is not reusable, reason = %q", entries[0].Reusability.Reason())
	}
}

func TestLsOmitsStaleEntryByDefault(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}

	// Editing a relevant file changes the observed input state, so the stored entry is
	// no longer a hit candidate and must drop out of the default reuse view.
	h.write(t, "a.txt", "v2")

	entries, err := lsEntries(h.svc, h.lsRequest())
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("ls returned %d entries after input change, want 0", len(entries))
	}
}

func TestLsAllShowsStaleWithReason(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	h.write(t, "a.txt", "v2")

	req := h.lsRequest()
	req.All = true
	entries, err := lsEntries(h.svc, req)
	if err != nil {
		t.Fatalf("ls --all: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ls --all returned %d entries, want 1", len(entries))
	}
	if entries[0].Reusable() {
		t.Error("stale entry must not be reusable under --all")
	}
	if got := entries[0].Reusability.Reason(); got != runcache.ReasonInputTreeDiffers {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonInputTreeDiffers)
	}
}

func TestLsExpiredEntryNotReusable(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	// Advance time past the default TTL (7 days): the entry is stale even though its
	// inputs are unchanged.
	h.clock.now = h.clock.now.Add(8 * 24 * time.Hour)

	req := h.lsRequest()
	req.All = true
	entries, err := lsEntries(h.svc, req)
	if err != nil {
		t.Fatalf("ls --all: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ls --all returned %d entries, want 1", len(entries))
	}
	if entries[0].Reusable() {
		t.Error("expired entry must not be reusable")
	}
	if got := entries[0].Reusability.Reason(); got != runcache.ReasonExpired {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonExpired)
	}
}

// TestLsGroupsScansByPolicy proves the worktree is scanned once per distinct scan
// policy, not once per entry: three runs sharing the default scope must produce a
// single read-only scan during ls.
func TestLsGroupsScansByPolicy(t *testing.T) {
	var cs *countingScanner
	h := newHarness(t, withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
		cs = &countingScanner{inner: inner}
		return cs
	}))
	defer h.cleanup()
	h.write(t, "a.txt", "v1")
	for _, out := range []string{"one", "two", "three"} {
		h.runner.stdout = out
		if _, err := h.svc.Run(context.Background(), h.request("echo", out)); err != nil {
			t.Fatal(err)
		}
	}

	cs.readOnly = 0
	entries, err := lsEntries(h.svc, h.lsRequest())
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("ls returned %d entries, want 3", len(entries))
	}
	if cs.readOnly != 1 {
		t.Errorf("ls performed %d read-only scans, want 1 (grouped by policy)", cs.readOnly)
	}
}

func TestLsEnvMismatchReason(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.env.vars["CI"] = "true" // CI is in the default env allowlist, so it is keyed.
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	// Change the keyed environment: the recorded entry no longer matches.
	h.env.vars["CI"] = "false"

	e := onlyEntry(t, h.lsAll(t))
	if e.Reusable() {
		t.Error("entry must not be reusable after a keyed env change")
	}
	if got := e.Reusability.Reason(); got != runcache.ReasonEnvMismatch {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonEnvMismatch)
	}
}

func TestLsConfigMismatchReason(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	if _, err := h.svc.Run(context.Background(), h.request("echo", "x")); err != nil {
		t.Fatal(err)
	}
	// Change a run-config field folded into the run config hash: the key no longer
	// reproduces, so the entry is non-reusable with a config reason.
	h.cfg.Run.MaxStdoutSize /= 2

	e := onlyEntry(t, h.lsAll(t))
	if e.Reusable() {
		t.Error("entry must not be reusable after a run-config change")
	}
	if got := e.Reusability.Reason(); got != runcache.ReasonConfigMismatch {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonConfigMismatch)
	}
}

func TestLsPayloadMissingReason(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	// Remove a stored payload after the run: the key still matches, but the entry can
	// no longer be replayed honestly, so it must be reported as non-reusable.
	if err := os.Remove(filepath.Join(h.store.EntryPath(res.RunID), "stdout.log")); err != nil {
		t.Fatal(err)
	}

	e := onlyEntry(t, h.lsAll(t))
	if e.Reusable() {
		t.Error("entry with a missing payload must not be reusable")
	}
	if got := e.Reusability.Reason(); got != runcache.ReasonPayloadMissing {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonPayloadMissing)
	}
}

func TestLsCorruptMetadataReason(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	// Persisted metadata is hostile input: a hand-corrupted meta.json must surface as a
	// corrupt row under --all, never crash the listing or read as reusable.
	metaPath := filepath.Join(h.store.EntryPath(res.RunID), "meta.json")
	// The store writes metadata read-only; make it writable before corrupting it.
	if err := os.Chmod(metaPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := onlyEntry(t, h.lsAll(t))
	if !e.Corrupt {
		t.Error("corrupt metadata must be flagged as corrupt")
	}
	if got := e.Reusability.Reason(); got != runcache.ReasonCorrupt {
		t.Errorf("reason = %q, want %q", got, runcache.ReasonCorrupt)
	}
	// The default (reusable-only) view omits a corrupt entry entirely.
	entries, err := lsEntries(h.svc, h.lsRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("default run ls listed %d entries, want 0 (corrupt is not reusable)", len(entries))
	}

	// A command filter cannot be confirmed against unreadable metadata, so even --all
	// must omit a corrupt entry when filtering — the filter stays honest rather than
	// surfacing a row whose command could not be matched.
	req := h.lsRequest()
	req.All = true
	req.Command = "echo"
	filtered, err := lsEntries(h.svc, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 {
		t.Errorf("run ls --all --command listed %d corrupt entries, want 0 (filter must stay honest)", len(filtered))
	}
}

// TestLsSkipsIncompatibleEntry proves an incompatible entry never aborts the
// listing and is silently omitted rather than mistaken for corruption — the failure
// mode a new decode sentinel would otherwise create across every batch caller.
func TestLsSkipsIncompatibleEntry(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	res, err := h.svc.Run(context.Background(), h.request("echo", "x"))
	if err != nil {
		t.Fatal(err)
	}
	// Restamp the record one past whatever the current schema is, so it declares a
	// version this build has no reader for. Reading the number back out of the record
	// keeps the fixture from carrying a version literal that would go stale.
	metaPath := filepath.Join(h.store.EntryPath(res.RunID), "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	current, err := strconv.Atoi(string(m["schema_version"]))
	if err != nil {
		t.Fatalf("record has no numeric schema_version: %v", err)
	}
	m["schema_version"] = json.RawMessage(strconv.Itoa(current + 1))
	incompatible, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(metaPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, incompatible, 0o644); err != nil {
		t.Fatal(err)
	}

	// --all must still succeed and must not surface the incompatible entry as corrupt.
	reqAll := h.lsRequest()
	reqAll.All = true
	all, err := h.svc.Ls(context.Background(), reqAll)
	if err != nil {
		t.Fatalf("run ls --all over an incompatible entry must not error: %v", err)
	}
	if len(all.Reusable) != 0 {
		t.Errorf("run ls --all listed %d entries, want 0 (an incompatible entry is skipped)", len(all.Reusable))
	}
	if all.Corrupt != 0 {
		t.Errorf("incompatible entry counted as corrupt (%d); it is benign, not corruption", all.Corrupt)
	}
}

// TestLsLimitBoundsScanning proves the candidate window bounds the expensive
// worktree scan: with three entries under distinct scan policies, ls -n 1 scans once
// (the single newest candidate), not once per stored entry.
func TestLsLimitBoundsScanning(t *testing.T) {
	var cs *countingScanner
	h := newHarness(t, withScannerDecorator(func(inner apprun.Scanner) apprun.Scanner {
		cs = &countingScanner{inner: inner}
		return cs
	}))
	defer h.cleanup()

	// Three runs under three distinct scopes, so grouping cannot collapse their scans:
	// only the candidate limit can.
	for _, name := range []string{"sub1", "sub2", "sub3"} {
		if err := os.MkdirAll(filepath.Join(h.rootAbs, name), 0o755); err != nil {
			t.Fatal(err)
		}
		h.write(t, filepath.Join(name, "f.txt"), "data")
		req := h.request("echo", name)
		req.Scope = apprun.ScopeOverrides{ScopeReplace: []string{name}}
		if _, err := h.svc.Run(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}

	cs.readOnly = 0
	req := h.lsRequest()
	req.Limit = 1
	entries, err := lsEntries(h.svc, req)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ls -n 1 returned %d entries, want 1", len(entries))
	}
	if cs.readOnly != 1 {
		t.Errorf("ls -n 1 performed %d scans over 3 stored runs, want 1 (limit must bound scanning)", cs.readOnly)
	}
}

// TestLsCommandFilterStaysWithinWindow proves --command filters within the candidate
// window rather than searching all history: a rare command that has been crowded out
// of the newest -n entries is not found, so a rare command never triggers an
// unbounded decode/scan over the whole store.
func TestLsCommandFilterStaysWithinWindow(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "a.txt", "v1")

	// The oldest run carries the rare command; advancing the clock between runs keeps
	// ids time-ordered, so the three newer "common" runs crowd it out of a small window.
	if _, err := h.svc.Run(context.Background(), h.request("rare")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.clock.now = h.clock.now.Add(time.Second)
		h.runner.stdout = "out"
		if _, err := h.svc.Run(context.Background(), h.request("common", string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
	}

	req := h.lsRequest()
	req.Command = "rare"
	req.Limit = 1
	req.All = true
	entries, err := lsEntries(h.svc, req)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("rare command outside the -n 1 window yielded %d rows, want 0 (window must bound the search)", len(entries))
	}

	// Widening the window to cover all runs finds it.
	req.Limit = 10
	entries, err = lsEntries(h.svc, req)
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("rare command within the window yielded %d rows, want 1", len(entries))
	}
}
