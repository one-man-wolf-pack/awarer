package run_test

import (
	"context"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
)

// TestDiagnoseRunHitIsEmpty confirms a cache hit produces no inline diagnosis: a
// replay of a reusable entry has nothing to explain.
func TestDiagnoseRunHitIsEmpty(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "data.txt", "v1")

	if _, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt")); err != nil {
		t.Fatalf("first run: %v", err)
	}
	hit, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if hit.Status != runcache.CacheHit {
		t.Fatalf("second run status = %v, want hit", hit.Status)
	}

	diag, err := h.svc.DiagnoseRun(context.Background(), hit, h.cfg.Run.TTL)
	if err != nil {
		t.Fatalf("DiagnoseRun: %v", err)
	}
	if diag.Available() || diag.Nearest != nil {
		t.Errorf("hit diagnosis = %+v, want unavailable with no nearest", diag)
	}
}

// TestDiagnoseRunStaleMissExcludesSelf confirms that after an input edit the inline
// diagnosis finds the previous run as the nearest candidate — not the current run's
// own freshly stored entry, which shares the exact key — and names the shared
// input-tree-differs reason with a changed-path sample of the edited file.
func TestDiagnoseRunStaleMissExcludesSelf(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "data.txt", "v1")

	first, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	h.write(t, "data.txt", "v2")
	second, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Status == runcache.CacheHit {
		t.Fatalf("second run after edit should miss, got hit")
	}

	diag, err := h.svc.DiagnoseRun(context.Background(), second, h.cfg.Run.TTL)
	if err != nil {
		t.Fatalf("DiagnoseRun: %v", err)
	}
	if !diag.Available() || diag.Nearest == nil {
		t.Fatalf("stale miss diagnosis = %+v, want a nearest candidate", diag)
	}
	if diag.Nearest.Entry.ID == second.RunID {
		t.Errorf("nearest candidate is the current run itself (%s); self-match not excluded", second.RunID.Short())
	}
	if diag.Nearest.Entry.ID != first.RunID {
		t.Errorf("nearest = %s, want the previous run %s", diag.Nearest.Entry.ID.Short(), first.RunID.Short())
	}
	if diag.Nearest.Reason != runcache.ReasonInputTreeDiffers {
		t.Errorf("nearest reason = %q, want input-tree-differs", diag.Nearest.Reason)
	}
	if diag.Nearest.Changed == nil {
		t.Fatal("nearest changed-path sample missing")
	}
	found := false
	for _, p := range diag.Nearest.Changed.Paths() {
		if p.Path == "data.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("changed-path sample does not name data.txt: %+v", diag.Nearest.Changed.Paths())
	}
}

// TestDiagnoseRunRecordOnlyNearestIsHonest confirms that when the nearest previous run
// is non-reusable history with an identical key (a record-only run), the diagnosis
// names its real verdict reason — record-only — rather than fabricating a key-diff
// reason from a comparison the run never underwent, and claims no matched-category
// score it never earned.
func TestDiagnoseRunRecordOnlyNearestIsHonest(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "data.txt", "v1")

	rec := h.request("cmd", "-cat", "data.txt")
	rec.Policy.Record = true
	if _, err := h.svc.Run(context.Background(), rec); err != nil {
		t.Fatalf("record-only run: %v", err)
	}

	// A plain run of the identical command now misses (record-only published no reusable
	// pointer) and stores a fresh reusable entry; the record-only run is its nearest.
	miss, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("plain run: %v", err)
	}
	if miss.Status == runcache.CacheHit {
		t.Fatalf("plain run after a record-only run should miss, got hit")
	}

	diag, err := h.svc.DiagnoseRun(context.Background(), miss, h.cfg.Run.TTL)
	if err != nil {
		t.Fatalf("DiagnoseRun: %v", err)
	}
	if !diag.Available() || diag.Nearest == nil {
		t.Fatalf("diagnosis = %+v, want the record-only run as nearest", diag)
	}
	if diag.Nearest.Reason != runcache.ReasonRecordOnly {
		t.Errorf("nearest reason = %q, want record-only (not a fabricated key-diff reason)", diag.Nearest.Reason)
	}
	if len(diag.Nearest.Score.Matched) != 0 || len(diag.Nearest.Score.Different) != 0 {
		t.Errorf("record-only nearest must claim no comparison score, got %+v", diag.Nearest.Score)
	}
	if diag.Nearest.Changed != nil {
		t.Errorf("record-only nearest must not carry a changed-path sample, got %+v", diag.Nearest.Changed)
	}
}

// TestDiagnoseRunExpiredNearestIsHonest confirms that when the nearest previous run
// is a once-reusable entry whose key still matches but has aged past the TTL, the
// diagnosis names it "expired" — the same reason the hit path and run ls --near use —
// rather than a fabricated key-diff reason or an empty one.
func TestDiagnoseRunExpiredNearestIsHonest(t *testing.T) {
	h := newHarness(t)
	defer h.cleanup()
	h.write(t, "data.txt", "v1")

	if _, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt")); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Age the first entry past the default 7d TTL, then rerun the identical command: the
	// stale entry can no longer hit, so the run misses and re-stores a fresh entry.
	h.clock.now = h.clock.now.Add(8 * 24 * time.Hour)
	miss, err := h.svc.Run(context.Background(), h.request("cmd", "-cat", "data.txt"))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if miss.Status == runcache.CacheHit {
		t.Fatalf("second run past TTL should miss, got hit")
	}

	diag, err := h.svc.DiagnoseRun(context.Background(), miss, h.cfg.Run.TTL)
	if err != nil {
		t.Fatalf("DiagnoseRun: %v", err)
	}
	if !diag.Available() || diag.Nearest == nil {
		t.Fatalf("diagnosis = %+v, want the expired run as nearest", diag)
	}
	if diag.Nearest.Reason != runcache.ReasonExpired {
		t.Errorf("nearest reason = %q, want expired", diag.Nearest.Reason)
	}
}
