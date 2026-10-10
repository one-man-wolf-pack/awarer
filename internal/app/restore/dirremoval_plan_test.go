package restore

import (
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"

	domrestore "awarer/internal/domain/restore"
)

func failureReasons(res domrestore.ApplyResult) map[string][]domrestore.Reason {
	out := map[string][]domrestore.Reason{}
	for _, f := range res.Failures() {
		out[f.Path.String()] = f.Reasons
	}
	return out
}

func requireOnlyFailures(t *testing.T, res domrestore.ApplyResult, want map[string][]domrestore.Reason) {
	t.Helper()
	if got := failureReasons(res); !reflect.DeepEqual(got, want) {
		t.Errorf("failures = %v, want %v", got, want)
	}
}

func (h *harness) restoreScratch(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.layout.TmpDir())
	if err != nil {
		t.Fatalf("read store tmp: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "restore-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestAHiddenChildBlocksItsDirectoryBeforeAnyMutation(t *testing.T) {
	h := setup(t)
	h.write(".awaignore", "*.log\n")
	h.write("keep.txt", "keep")
	id := h.checkpointNow(h.cfg)
	h.write("d/a.log", "hidden")
	h.write("new.txt", "unreviewed")
	before := snapshotTree(t, h.root)

	wantCounts := domrestore.Counts{DeleteFile: 1, Equal: 2, Blocked: 1}
	wantFailures := map[string][]domrestore.Reason{"d": {domrestore.ReasonPathConflict}}

	preview := h.run(t, id.String(), h.selection(), false)
	if got := preview.Result.Outcome(); got != domrestore.OutcomePreview {
		t.Fatalf("preview outcome = %s", got)
	}
	if got := preview.Result.Counts(); got != wantCounts {
		t.Errorf("preview counts = %+v, want %+v", got, wantCounts)
	}
	requireOnlyFailures(t, preview.Result, wantFailures)

	applied := h.run(t, id.String(), h.selection(), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("apply outcome = %s (%v), want refused", got, applied.Result.Reasons())
	}
	if got := applied.Result.Counts(); got != wantCounts {
		t.Errorf("apply counts = %+v, want the preview's %+v", got, wantCounts)
	}
	requireOnlyFailures(t, applied.Result, wantFailures)
	if applied.Result.Completed() != 0 {
		t.Errorf("a refusal reported %d completed operation(s)", applied.Result.Completed())
	}
	if after := snapshotTree(t, h.root); after != before {
		t.Errorf("a refused apply changed the worktree:\n%s\nwant:\n%s", after, before)
	}
	if applied.RecoveryRef != "" || h.recoveryRecords(t) != 0 {
		t.Errorf("a refused apply published recovery %q (%d record(s))", applied.RecoveryRef, h.recoveryRecords(t))
	}
	if left := h.restoreScratch(t); len(left) != 0 {
		t.Errorf("a refused apply left scratch behind: %v", left)
	}
}

func TestADirectoryEmptiedByItsOwnSelectionStaysRestorable(t *testing.T) {
	h := setup(t)
	h.write(".awaignore", "*.log\n")
	h.write("keep.txt", "keep")
	h.write("d-/a.log", "hidden beside, not below")
	id := h.checkpointNow(h.cfg)
	h.write("d/a.txt", "x")
	h.write("d/sub/b.txt", "y")
	h.write("d.file", "z")
	if err := os.Mkdir(h.abs("empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	preview := h.run(t, id.String(), h.selection(), false)
	want := domrestore.Counts{DeleteFile: 3, DeleteDirectory: 3, Equal: 3}
	if got := preview.Result.Counts(); got != want {
		t.Fatalf("preview counts = %+v, want %+v", got, want)
	}

	applied := h.run(t, id.String(), h.selection(), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v; failures %+v)", got, applied.Result.Reasons(), applied.Result.Failures())
	}
	for _, p := range []string{"d", "d.file", "empty"} {
		if h.exists(p) {
			t.Errorf("%s survived", p)
		}
	}
	if got := h.read("d-/a.log"); got != "hidden beside, not below" {
		t.Errorf("d-/a.log = %q", got)
	}
}

func TestAHiddenChildBlocksReplacingItsDirectory(t *testing.T) {
	h := setup(t)
	h.write(".awaignore", "*.log\n")
	h.write("thing", "a plain file\n")
	id := h.checkpointNow(h.cfg)
	h.remove("thing")
	h.write("thing/child.txt", "generated\n")
	h.write("thing/a.log", "hidden")
	before := snapshotTree(t, h.root)

	want := domrestore.Counts{DeleteFile: 1, Equal: 1, Blocked: 1}
	for _, apply := range []bool{false, true} {
		res := h.run(t, id.String(), h.selection(), apply)
		if got := res.Result.Counts(); got != want {
			t.Errorf("apply=%v counts = %+v, want %+v", apply, got, want)
		}
		requireOnlyFailures(t, res.Result, map[string][]domrestore.Reason{"thing": {domrestore.ReasonPathConflict}})
		if apply && res.Result.Outcome() != domrestore.OutcomeRefused {
			t.Errorf("apply outcome = %s, want refused", res.Result.Outcome())
		}
	}
	if after := snapshotTree(t, h.root); after != before {
		t.Errorf("the worktree changed:\n%s\nwant:\n%s", after, before)
	}
	if n := h.recoveryRecords(t); n != 0 {
		t.Errorf("published %d recovery observation(s)", n)
	}
}

func TestAChildOutsideARecoveryScopeBlocksItsDirectory(t *testing.T) {
	h := setup(t)
	h.write("keep.txt", "keep")
	if err := os.Mkdir(h.abs("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	id := h.checkpointNow(h.cfg)
	h.remove("d")

	created := h.run(t, id.String(), h.selection("d"), true)
	if got := created.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v)", got, created.Result.Reasons())
	}
	h.write("d/later.txt", "outside the recovery scope")
	before := snapshotTree(t, h.root)

	for _, apply := range []bool{false, true} {
		undo := h.run(t, created.RecoveryRef, h.selection(), apply)
		if got, want := undo.Result.Counts(), (domrestore.Counts{Blocked: 1}); got != want {
			t.Errorf("apply=%v counts = %+v, want %+v", apply, got, want)
		}
		requireOnlyFailures(t, undo.Result, map[string][]domrestore.Reason{"d": {domrestore.ReasonPathConflict}})
	}
	if after := snapshotTree(t, h.root); after != before {
		t.Errorf("the worktree changed:\n%s\nwant:\n%s", after, before)
	}
}

func TestASkippedChildKeepsItsDirectoryAnEvidenceGap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based unreadability is not modeled the same way here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	h := setup(t)
	h.write("keep.txt", "keep")
	id := h.checkpointNow(h.cfg)
	h.write("d/secret.txt", "unreadable")
	if err := os.Chmod(h.abs("d/secret.txt"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.abs("d/secret.txt"), 0o644) })

	res := h.run(t, id.String(), h.selection(), false)
	if got, want := res.Result.Counts(), (domrestore.Counts{Equal: 1, Blocked: 2}); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
	requireOnlyFailures(t, res.Result, map[string][]domrestore.Reason{
		"d":            {domrestore.ReasonSkippedBoundary},
		"d/secret.txt": {domrestore.ReasonSkippedBoundary},
	})
}

func TestAChildAppearingAfterPlanningStillStopsAtTheFinalGuard(t *testing.T) {
	h := setup(t)
	h.write("keep.txt", "keep")
	id := h.checkpointNow(h.cfg)
	h.write("d/a.txt", "x")
	h.write("new.txt", "unreviewed")

	preview := h.run(t, id.String(), h.selection(), false)
	if got, want := preview.Result.Counts(), (domrestore.Counts{DeleteFile: 2, DeleteDirectory: 1, Equal: 1}); got != want {
		t.Fatalf("preview counts = %+v, want %+v: the stable tree has no conflict to find", got, want)
	}

	h.svc.deps.Recovery = &afterPublishHook{
		inner: h.recov,
		after: func() { h.write("d/late.txt", "appeared after planning") },
	}
	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomePartial {
		t.Fatalf("outcome = %s (%v), want partial", got, res.Result.Reasons())
	}
	requireFailure(t, res.Result, "d", domrestore.ReasonPathConflict)
	if res.Result.Counts().Blocked != 0 {
		t.Errorf("counts = %+v: the plan itself was complete", res.Result.Counts())
	}
	if got := h.read("d/late.txt"); got != "appeared after planning" {
		t.Errorf("d/late.txt = %q, want the late child untouched", got)
	}
	if h.exists("new.txt") || h.exists("d/a.txt") {
		t.Error("the ready deletions before the directory did not run")
	}
	if res.RecoveryRef == "" {
		t.Error("a partial apply reported no recovery reference")
	}
}
