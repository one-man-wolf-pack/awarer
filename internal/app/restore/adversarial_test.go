package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/domain/checkpoint"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	domrestore "awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/restorespool"
)

// These tests cover the shapes and failure modes the happy-path suite does not:
// every operation kind, the evidence boundaries, cancellation at each phase, and
// the honest partial/conflict outcomes.

// --- operation kinds ------------------------------------------------------

func TestApplyHandlesEveryOperationKind(t *testing.T) {
	h := setup(t)
	// A source with a file, an executable, a symlink, a directory that will become a
	// file, and a directory that will disappear.
	h.write("keep/file.txt", "kept")
	h.write("mode/tool.sh", "#!/bin/sh\n")
	if err := os.Chmod(h.abs("mode/tool.sh"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.Symlink("file.txt", h.abs("keep/link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h.write("becomes-file/child.txt", "child")
	id := h.checkpointNow(h.cfg)

	// Now drift every way at once.
	h.remove("keep/file.txt")                       // -> create
	h.write("mode/tool.sh", "#!/bin/sh\nchanged\n") // -> replace (and mode)
	if err := os.Chmod(h.abs("mode/tool.sh"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	h.remove("keep/link")
	if err := os.Symlink("elsewhere", h.abs("keep/link")); err != nil {
		t.Fatalf("symlink: %v", err)
	} // -> restore-symlink
	h.remove("becomes-file")
	h.write("becomes-file", "now a file") // -> type change (file back to directory)
	h.write("added/new.txt", "unwanted")  // -> delete file + delete directory

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v; failures %+v)", got, res.Result.Reasons(), res.Result.Failures())
	}
	c := res.Result.Counts()
	if c.Create == 0 || c.Replace == 0 || c.Symlink == 0 || c.TypeChange == 0 || c.DeleteFile == 0 || c.DeleteDirectory == 0 {
		t.Fatalf("counts did not exercise every kind: %+v", c)
	}

	if got := h.read("keep/file.txt"); got != "kept" {
		t.Errorf("create: %q", got)
	}
	info, err := os.Lstat(h.abs("mode/tool.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("replace did not restore the mode: %v %v", info, err)
	}
	target, err := os.Readlink(h.abs("keep/link"))
	if err != nil || target != "file.txt" {
		t.Errorf("symlink restore: %q %v", target, err)
	}
	if got := h.read("becomes-file/child.txt"); got != "child" {
		t.Errorf("type change back to a directory: %q", got)
	}
	if h.exists("added/new.txt") || h.exists("added") {
		t.Error("the added file and its now-empty directory were not removed")
	}
}

// TestATypeChangeOverADirectoryRunsAfterItsChildrenAreRemoved is the end-to-end
// form of the ordering rule. Every operation here is proved and available, so the
// only thing that can make the apply fail is the executor's own order: replacing
// `thing` requires it to be empty, and the entries that empty it are this same
// plan's child deletions. Ordering by operation kind alone puts the replacement in
// the first phase, where it reproducibly stops with path-conflict and restores
// nothing — while blaming a change that never happened.
func TestATypeChangeOverADirectoryRunsAfterItsChildrenAreRemoved(t *testing.T) {
	h := setup(t)
	h.write("thing", "a plain file\n")
	id := h.checkpointNow(h.cfg)

	// A generator replaced the file with a whole subtree.
	h.remove("thing")
	h.write("thing/child.txt", "generated\n")
	h.write("thing/sub/deep.txt", "generated too\n")

	res := h.run(t, id.String(), h.selection("thing"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v; failures %+v)", got, res.Result.Reasons(), res.Result.Failures())
	}
	if got := h.read("thing"); got != "a plain file\n" {
		t.Errorf("thing = %q, want the file the source proves", got)
	}
	if h.exists("thing/child.txt") || h.exists("thing/sub") {
		t.Error("the replaced directory's proved children survived")
	}
}

// TestATypeChangeToASymlinkApplies covers the other half of the same defect at the
// service level: the desired shape is a link, which the plan counts as a type
// change. If the executor dispatches on the operation kind rather than the desired
// shape it reports unsupported-entry-kind for a shape restore fully supports.
func TestATypeChangeToASymlinkApplies(t *testing.T) {
	h := setup(t)
	h.write("target.txt", "target\n")
	if err := os.Symlink("target.txt", h.abs("link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	id := h.checkpointNow(h.cfg)

	// A generator clobbered the link with a real file.
	h.remove("link")
	h.write("link", "clobbered\n")

	res := h.run(t, id.String(), h.selection("link"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v; failures %+v)", got, res.Result.Reasons(), res.Result.Failures())
	}
	target, err := os.Readlink(h.abs("link"))
	if err != nil || target != "target.txt" {
		t.Errorf("link target = %q (%v), want target.txt", target, err)
	}
	if got := h.read("target.txt"); got != "target\n" {
		t.Errorf("restoring the link wrote through it: %q", got)
	}
}

func TestDirectoriesAreRemovedDeepestFirst(t *testing.T) {
	h := setup(t)
	h.write("keep.txt", "keep")
	id := h.checkpointNow(h.cfg)
	// A nested tree the source proves absent: removing the parent before the child
	// would fail on a non-empty directory, so this only passes if the commit walks
	// directory deletions deepest-first.
	h.write("a/b/c/d/file.txt", "x")

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v; failures %+v)", got, res.Result.Reasons(), res.Result.Failures())
	}
	for _, p := range []string{"a/b/c/d/file.txt", "a/b/c/d", "a/b/c", "a/b", "a"} {
		if h.exists(p) {
			t.Errorf("%s survived a deepest-first directory removal", p)
		}
	}
}

// --- evidence boundaries --------------------------------------------------

func TestPolicyMismatchBlocksDeletionsButNotReplacements(t *testing.T) {
	h := setup(t)
	h.write("src/keep.go", "package src\n")
	id := h.checkpointNow(h.cfg)
	h.write("src/keep.go", "package src // edited\n")
	// Present now, absent from the source: a deletion candidate — but only if the two
	// observations share a scan policy, which the shift below removes.
	h.write("extra/only-now.txt", "added later")

	// Change the scan boundary: the current observation now has a different
	// scan-config identity than the source's, so absence in the source is no longer
	// proof of absence in awa scope.
	shifted := h.cfg
	shifted.History.ExtraExcludes = []string{"nothing-matches-this"}
	h.cfg = shifted

	res := h.run(t, id.String(), h.selection(), false)
	if res.Result.Boundary().PolicyCompatible {
		t.Fatal("the fixture did not actually change the scan policy identity")
	}
	if res.Result.Counts().Replace == 0 {
		t.Errorf("positive evidence stopped being usable under a policy mismatch: %+v", res.Result.Counts())
	}
	if res.Result.Counts().Delete() != 0 {
		t.Errorf("a deletion was planned from a policy-incompatible source: %+v", res.Result.Counts())
	}
	if res.Result.Counts().Blocked == 0 {
		t.Errorf("the unproven deletion was silently dropped instead of blocked: %+v", res.Result.Counts())
	}
}

func TestASkippedInputBlocksOnlyThePathsItIntersects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based unreadability is not modeled the same way here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unreadable file is still readable")
	}
	h := setup(t)
	h.write("a/readable.txt", "one")
	h.write("b/secret.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("a/readable.txt", "one changed")
	h.write("b/secret.txt", "two changed")
	if err := os.Chmod(h.abs("b/secret.txt"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.abs("b/secret.txt"), 0o644) })

	// The unreadable input is a per-path evidence gap, so a selection that avoids it
	// still applies cleanly.
	res := h.run(t, id.String(), h.selection("a"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("an unrelated selection was blocked by a skipped input: %s (%v)", got, res.Result.Reasons())
	}
	if got := h.read("a/readable.txt"); got != "one" {
		t.Errorf("a/readable.txt = %q", got)
	}

	// Selecting the skipped path itself refuses.
	blocked := h.run(t, id.String(), h.selection("b"), true)
	if got := blocked.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("selecting a skipped input produced %s, want refused", got)
	}
	if !hasReason(blocked.Result, domrestore.ReasonSkippedBoundary) {
		t.Errorf("reasons = %v, want skipped-boundary", blocked.Result.Reasons())
	}
}

func TestRunObservationIsAMetadataOnlySource(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	_ = id
	// A run observation records identity but no content, so every regular-file write
	// it would imply is blocked rather than guessed at. The service must say so
	// through the closed vocabulary, not by failing.
	h.write("a.txt", "changed")

	// Without a run store wired, the reference cannot resolve at all — which is also
	// an honest, loud failure rather than a silent empty plan.
	_, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref("run:abcdef:before"), Selection: h.selection("a.txt"),
	})
	if err == nil {
		t.Fatal("an unresolvable run reference produced a plan")
	}
	if !strings.Contains(err.Error(), "run-observation reader") && !strings.Contains(err.Error(), "run") {
		t.Errorf("error = %v, want it to name the unavailable run observation", err)
	}
}

func TestRecoveryObservationProvesOnlyItsCoveredScope(t *testing.T) {
	h := setup(t)
	h.write("covered/a.txt", "one")
	h.write("elsewhere/b.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("covered/a.txt", "one changed")
	h.write("elsewhere/b.txt", "two changed")

	applied := h.run(t, id.String(), h.selection("covered"), true)
	if applied.Result.Outcome() != domrestore.OutcomeApplied {
		t.Fatalf("apply outcome = %s", applied.Result.Outcome())
	}
	ref := applied.RecoveryRef

	// The observation covers only what that restore was going to change. Selecting
	// --all against it must not touch anything outside that set, even though the
	// selection nominally covers the whole project.
	beforeElsewhere := h.read("elsewhere/b.txt")
	undo := h.run(t, ref, h.selection(), true)
	if got := undo.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("undo outcome = %s (%v)", got, undo.Result.Reasons())
	}
	if !undo.Result.Boundary().ScopeBounded {
		t.Error("a recovery-observation source did not report itself scope-bounded")
	}
	if got := h.read("elsewhere/b.txt"); got != beforeElsewhere {
		t.Errorf("--all against a recovery observation touched a path outside its covered scope: %q", got)
	}
	if got := h.read("covered/a.txt"); got != "one changed" {
		t.Errorf("the covered path was not undone: %q", got)
	}

	// A path the observation never covered is explained, not silently empty.
	outside := h.run(t, ref, h.selection("elsewhere"), false)
	if outside.Result.Counts().Mutating() != 0 {
		t.Errorf("a path outside the covered scope produced work: %+v", outside.Result.Counts())
	}
}

// --- conflicts and partial outcomes ---------------------------------------

func TestConcurrentMutationBetweenPlanningAndCommitIsAConflict(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.write("b.txt", "two changed")

	// A writer that edits a selected path after the plan is built but before the
	// commit. The revalidation pass must catch it and refuse the whole commit rather
	// than mutating a state the preview never described.
	h.svc.deps.Recovery = &afterPublishHook{
		inner: h.recov,
		after: func() { h.write("b.txt", "changed again by someone else") },
	}

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeConflict {
		t.Fatalf("outcome = %s, want conflict (%v)", got, res.Result.Reasons())
	}
	if !hasReason(res.Result, domrestore.ReasonPreconditionMismatch) {
		t.Errorf("reasons = %v, want precondition-mismatch", res.Result.Reasons())
	}
	if res.Result.Completed() != 0 {
		t.Errorf("a conflict reported %d completed operation(s); it must stop before the first mutation", res.Result.Completed())
	}
	// Nothing was written, including the path that was NOT touched by the racer.
	if got := h.read("a.txt"); got != "one changed" {
		t.Errorf("a conflicting apply mutated an unrelated selected path: %q", got)
	}
	// And the recovery observation published a moment before the conflict is gone. It
	// described a restore that never ran, and a leftover record would put an undo point
	// in the timeline (and a resolvable restore:<id>:before reference) for an operation
	// that changed nothing — an invitation to "undo" work awa never touched.
	if !res.Result.Recovery().IsZero() || res.RecoveryRef != "" {
		t.Errorf("a conflict reported recovery observation %q", res.RecoveryRef)
	}
	if got := h.recoveryRecords(t); got != 0 {
		t.Errorf("%d recovery observation(s) survived a conflict that wrote nothing", got)
	}
}

// TestCancellationAfterTheRecoveryObservationLeavesNoRecord covers the other exit
// through the same window: the invocation stops between publishing the recovery
// observation and the first write, this time because it was cancelled rather than
// because the plan went stale. The rule is the record's, not the reason's — a
// recovery observation exists for a restore that ran, so an invocation that stops
// before its first write must take its record with it.
func TestCancellationAfterTheRecoveryObservationLeavesNoRecord(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")

	ctx, cancel := context.WithCancel(context.Background())
	h.svc.deps.Recovery = &afterPublishHook{inner: h.recov, after: cancel}

	_, err := h.svc.Run(ctx, Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection("a.txt"), Apply: true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancelled after the recovery publication = %v, want context.Canceled", err)
	}
	if got := h.read("a.txt"); got != "one changed" {
		t.Errorf("a cancelled apply wrote %q", got)
	}
	if got := h.recoveryRecords(t); got != 0 {
		t.Errorf("%d recovery observation(s) survived a cancellation that wrote nothing", got)
	}
}

// afterPublishHook lets a test run a callback in the window between publishing the
// recovery observation and the revalidation pass — precisely the concurrent-writer
// window the apply protocol is designed to detect.
type afterPublishHook struct {
	inner domrestore.RecoveryRepository
	after func()
}

func (h *afterPublishHook) Publish(ctx context.Context, b domrestore.RecoveryBuild, m worktree.ManifestStream, s domrestore.CoveredScopeStream) (domrestore.RecoveryObservation, error) {
	rec, err := h.inner.Publish(ctx, b, m, s)
	if err == nil && h.after != nil {
		h.after()
	}
	return rec, err
}

func (h *afterPublishHook) Get(id domrestore.OperationID) (domrestore.RecoveryObservation, error) {
	return h.inner.Get(id)
}

func (h *afterPublishHook) Open(ctx context.Context, id domrestore.OperationID) (domrestore.RecoveryRead, error) {
	return h.inner.Open(ctx, id)
}

func (h *afterPublishHook) ResolvePrefix(ctx context.Context, prefix string) (domrestore.OperationID, error) {
	return h.inner.ResolvePrefix(ctx, prefix)
}

func (h *afterPublishHook) List(ctx context.Context) ([]domrestore.RecoveryFinding, error) {
	return h.inner.List(ctx)
}

func (h *afterPublishHook) Delete(id domrestore.OperationID) error { return h.inner.Delete(id) }

func TestAFailureAfterTheFirstWriteIsReportedAsPartial(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	h.write("c.txt", "three")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.write("b.txt", "two changed")
	h.write("c.txt", "three changed")

	// Fail on the second path. Multi-path commit is not transactional, so the honest
	// result is partial with exact completed/remaining counts — never success.
	h.svc.deps.Appliers = failingApplierFactory(h, "b.txt")

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomePartial {
		t.Fatalf("outcome = %s, want partial (%v)", got, res.Result.Reasons())
	}
	if res.Result.Completed() != 1 || res.Result.Remaining() != 2 {
		t.Errorf("completed=%d remaining=%d, want 1 and 2", res.Result.Completed(), res.Result.Remaining())
	}
	if !hasReason(res.Result, domrestore.ReasonPartialApply) {
		t.Errorf("reasons = %v, want partial-apply", res.Result.Reasons())
	}
	if res.Result.Recovery().IsZero() {
		t.Error("a partial apply carries no recovery observation")
	}
	// The prefix that landed really did land, and nothing past the failure did.
	if got := h.read("a.txt"); got != "one" {
		t.Errorf("a.txt = %q, want the completed prefix", got)
	}
	if got := h.read("c.txt"); got != "three changed" {
		t.Errorf("c.txt = %q, want untouched work after the stop", got)
	}

	// A rerun re-plans from current reality and converges on the remaining work.
	h.svc.deps.Appliers = h.applierFactory()
	rerun := h.run(t, id.String(), h.selection(), true)
	if got := rerun.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("rerun outcome = %s (%v)", got, rerun.Result.Reasons())
	}
	if rerun.Result.Completed() != 2 {
		t.Errorf("rerun completed %d, want only the 2 remaining operations", rerun.Result.Completed())
	}
	if h.read("b.txt") != "two" || h.read("c.txt") != "three" {
		t.Errorf("convergence failed: b=%q c=%q", h.read("b.txt"), h.read("c.txt"))
	}
}

// TestBytesSwappedAfterReObservationAreNeverOverwritten drives the narrowest race
// the apply protocol has: an external writer replaces a selected file's bytes
// AFTER the whole-selection re-observation has already approved the plan, with the
// same length and the same permission bits, so nothing about the file's shape
// changed. The commit must refuse that path.
//
// It matters because the recovery observation was published before those bytes
// existed. Overwriting them would destroy work that no awa evidence describes and
// that no undo could bring back — the one irreversible outcome restore exists to
// prevent.
func TestBytesSwappedAfterReObservationAreNeverOverwritten(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.write("b.txt", "two changed")

	// Same byte count, same mode, different content — a size or mtime comparison
	// would not notice, and the plan was built and re-validated against "one changed".
	h.svc.deps.Appliers = swappingApplierFactory(h, "a.txt", "ONE CHANGED")

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeConflict {
		t.Fatalf("outcome = %s, want conflict (%v)", got, res.Result.Reasons())
	}
	if !hasReason(res.Result, domrestore.ReasonPreconditionMismatch) {
		t.Errorf("reasons = %v, want precondition-mismatch", res.Result.Reasons())
	}
	if res.Result.Completed() != 0 {
		t.Errorf("completed = %d, want 0: the commit stopped at its first path", res.Result.Completed())
	}
	if got := h.read("a.txt"); got != "ONE CHANGED" {
		t.Fatalf("a.txt = %q, want the unobserved bytes untouched", got)
	}
	// The commit stops at the first failure rather than continuing past a conflict.
	if got := h.read("b.txt"); got != "two changed" {
		t.Errorf("b.txt = %q, want untouched work after the stop", got)
	}
}

// swappingApplierFactory wraps the real applier so that the first attempt to
// install the named path is preceded by an external writer replacing that path's
// bytes. It fires strictly between the whole-selection re-observation and the
// mutation, which is the only window the applier's own guard has to cover.
func swappingApplierFactory(h *harness, path, content string) func(domrestore.OperationID) (Applier, error) {
	real := h.applierFactory()
	return func(id domrestore.OperationID) (Applier, error) {
		a, err := real(id)
		if err != nil {
			return nil, err
		}
		return &swappingApplier{Applier: a, h: h, path: path, content: content}, nil
	}
}

type swappingApplier struct {
	Applier
	h       *harness
	path    string
	content string
	done    bool
}

func (a *swappingApplier) Apply(ctx context.Context, op domrestore.StagedOperation) domrestore.MutationResult {
	if !a.done && op.Path().String() == a.path {
		a.done = true
		a.h.write(a.path, a.content)
	}
	return a.Applier.Apply(ctx, op)
}

// failingApplierFactory wraps the real applier so one named path fails to install,
// modelling an I/O failure or an external writer winning the last-moment race.
func failingApplierFactory(h *harness, failPath string) func(domrestore.OperationID) (Applier, error) {
	real := h.applierFactory()
	return func(id domrestore.OperationID) (Applier, error) {
		a, err := real(id)
		if err != nil {
			return nil, err
		}
		return &failingApplier{Applier: a, failPath: failPath}, nil
	}
}

type failingApplier struct {
	Applier
	failPath string
}

func (a *failingApplier) Apply(ctx context.Context, op domrestore.StagedOperation) domrestore.MutationResult {
	if op.Path().String() == a.failPath {
		// A write that refused before touching the destination: the honest report is an
		// untouched one, which is what keeps this an ordinary stopped commit rather than a
		// partial write.
		return domrestore.Untouched(fmt.Errorf("injected write failure for %s", op.Path()))
	}
	return a.Applier.Apply(ctx, op)
}

// TestAChangedDestinationWithoutACompletedOperationIsPartial closes the gap between
// "no operation completed" and "nothing was written". The applier removes the node
// that stood in the way and then fails, so the worktree has moved while the completed
// count is still zero.
//
// Reporting that as a conflict would be a machine-readable lie — the outcome's whole
// contract is that the restore stopped before its first mutation — and it would make
// the recovery observation look unnecessary, when it is the only record of the node
// that is now gone.
func TestAChangedDestinationWithoutACompletedOperationIsPartial(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")

	h.svc.deps.Appliers = mutatingFailureApplierFactory(h, "a.txt")

	res := h.run(t, id.String(), h.selection("a.txt"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomePartial {
		t.Fatalf("outcome = %s, want partial (%v)", got, res.Result.Reasons())
	}
	if res.Result.Completed() != 0 || res.Result.Remaining() != 1 {
		t.Errorf("completed=%d remaining=%d, want 0 and 1", res.Result.Completed(), res.Result.Remaining())
	}
	if !hasReason(res.Result, domrestore.ReasonPartialApply) {
		t.Errorf("reasons = %v, want partial-apply", res.Result.Reasons())
	}
	// The reason token says which class of stop; only the diagnostic says what actually
	// failed. An operation's own error reaches it exactly like a failure of awa's
	// machinery does — otherwise a partial I/O failure leaves a reader with `io-failure`
	// and a path, which is the report this field exists to prevent.
	if !strings.Contains(res.Diagnostic, "injected failure after removing a.txt") {
		t.Errorf("diagnostic = %q, want the failure that stopped the commit", res.Diagnostic)
	}
	// Partial means the worktree may differ from what the plan proved, so the record
	// that describes what it held must be published, kept, and named.
	if res.Result.Recovery().IsZero() || res.RecoveryRef == "" {
		t.Error("a partial restore reported no recovery observation")
	}
	if got := h.recoveryRecords(t); got != 1 {
		t.Errorf("%d recovery observation(s) after a partial restore, want 1", got)
	}
}

// mutatingFailureApplierFactory models the one failure shape that changes a
// destination without completing its operation: the applier removed what stood in the
// way and then could not install the replacement. It reports that truthfully.
func mutatingFailureApplierFactory(h *harness, path string) func(domrestore.OperationID) (Applier, error) {
	real := h.applierFactory()
	return func(id domrestore.OperationID) (Applier, error) {
		a, err := real(id)
		if err != nil {
			return nil, err
		}
		return &mutatingFailureApplier{Applier: a, path: path, h: h}, nil
	}
}

type mutatingFailureApplier struct {
	Applier
	h    *harness
	path string
}

func (a *mutatingFailureApplier) Apply(ctx context.Context, op domrestore.StagedOperation) domrestore.MutationResult {
	if op.Path().String() == a.path {
		// Actually remove the node, so the reported effect matches the filesystem rather
		// than merely asserting it.
		a.h.remove(a.path)
		return domrestore.Interrupted(fmt.Errorf("injected failure after removing %s", op.Path()))
	}
	return a.Applier.Apply(ctx, op)
}

// TestAWriteFailureThatChangedNothingIsAnErrorNotAConflict separates a fault from an
// outcome. `conflict` means the destination moved out from under the plan and
// `cancelled` means an operator stopped it; a write that failed while leaving the
// worktree exactly as it was is neither, and publishing it as a conflict would send a
// reader looking for a change that never happened — and hide the operating system's
// own explanation, which is the actionable part.
func TestAWriteFailureThatChangedNothingIsAnErrorNotAConflict(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")

	// The only selected path fails, and reports honestly that it touched nothing.
	h.svc.deps.Appliers = failingApplierFactory(h, "a.txt")

	_, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection("a.txt"), Apply: true,
	})
	if err == nil {
		t.Fatal("a failed write that changed nothing was reported as an outcome instead of a failure")
	}
	if !strings.Contains(err.Error(), "injected write failure") {
		t.Errorf("error = %v, want the underlying write failure", err)
	}
	if got := h.read("a.txt"); got != "one changed" {
		t.Errorf("a.txt = %q, want the untouched worktree", got)
	}
	if got := h.recoveryRecords(t); got != 0 {
		t.Errorf("%d recovery observation(s) survived a commit that changed nothing", got)
	}
}

// TestAnApplierThatCannotSayWhatItDidIsTreatedAsAChange covers the report itself. An
// adapter that returns an unreadable result leaves the destination's state unknown —
// and unknown is not "untouched": the applier had already been asked to write there.
//
// Reading it as no-change would discard the recovery observation, which is the only
// description of what that path held, immediately after a mutation that may well have
// landed. So the commit treats it as a change that did not complete: partial, record
// kept and named, and the reason it could not be read carried as a diagnostic.
//
// The double here really does mutate the path before returning its useless report,
// so the assertion below is about a worktree that actually moved.
func TestAnApplierThatCannotSayWhatItDidIsTreatedAsAChange(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")

	h.svc.deps.Appliers = func(oid domrestore.OperationID) (Applier, error) {
		real, err := h.applierFactory()(oid)
		if err != nil {
			return nil, err
		}
		return &silentApplier{Applier: real, h: h, path: "a.txt"}, nil
	}

	res, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection("a.txt"), Apply: true,
	})
	if err != nil {
		t.Fatalf("Run = %v; the worktree may have changed, so the result is the report", err)
	}
	if got := res.Result.Outcome(); got != domrestore.OutcomePartial {
		t.Fatalf("outcome = %s, want partial (%v)", got, res.Result.Reasons())
	}
	if res.RecoveryRef == "" || h.recoveryRecords(t) != 1 {
		t.Errorf("recovery ref %q and %d record(s): an unreadable report must not cost the undo point",
			res.RecoveryRef, h.recoveryRecords(t))
	}
	requireFailure(t, res.Result, "a.txt", domrestore.ReasonIOFailure)
	if !strings.Contains(res.Diagnostic, "a.txt") {
		t.Errorf("diagnostic = %q, want the path whose report could not be read", res.Diagnostic)
	}
	// The double removed the path. Whatever the report said, that is what a reader has
	// to be able to recover — which is the point of keeping the record.
	if h.exists("a.txt") {
		t.Error("the fixture did not actually mutate the path, so it proves nothing about undo evidence")
	}
}

type silentApplier struct {
	Applier
	h    *harness
	path string
}

func (a *silentApplier) Apply(_ context.Context, op domrestore.StagedOperation) domrestore.MutationResult {
	if op.Path().String() == a.path {
		a.h.remove(a.path)
	}
	return domrestore.MutationResult{}
}

// TestAnInfrastructureFailureAfterAWriteStillReportsWhatLanded covers the commit
// phase's other exit: awa's own machinery fails — here a staged payload that vanished
// — after a path has already been written.
//
// Returning the bare error would throw away the only report of what landed and of the
// observation it can be undone from. So the outcome stands, the failure is named as a
// reason, and its text arrives as a human diagnostic instead of as prose inside the
// typed contract.
func TestAnInfrastructureFailureAfterAWriteStillReportsWhatLanded(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.write("b.txt", "two changed")

	h.svc.deps.Appliers = vanishingPayloadApplierFactory(h, 1)

	res := h.run(t, id.String(), h.selection(), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomePartial {
		t.Fatalf("outcome = %s, want partial (%v)", got, res.Result.Reasons())
	}
	if res.Result.Completed() != 1 || res.Result.Remaining() != 1 {
		t.Errorf("completed=%d remaining=%d, want 1 and 1", res.Result.Completed(), res.Result.Remaining())
	}
	if !hasReason(res.Result, domrestore.ReasonIOFailure) {
		t.Errorf("reasons = %v, want io-failure named", res.Result.Reasons())
	}
	if !strings.Contains(res.Diagnostic, "staged payload") {
		t.Errorf("diagnostic = %q, want the failure that ended the commit", res.Diagnostic)
	}
	if res.RecoveryRef == "" || h.recoveryRecords(t) != 1 {
		t.Errorf("recovery ref %q and %d record(s); a commit that wrote must keep and name its undo point",
			res.RecoveryRef, h.recoveryRecords(t))
	}
	if got := h.read("a.txt"); got != "one" {
		t.Errorf("a.txt = %q, want the written prefix", got)
	}
}

// TestAnInfrastructureFailureBeforeAnyWriteIsAnErrorWithNoRecord is the same exit with
// nothing written: no result can be honest about a commit that produced nothing, so
// the failure is returned — and the recovery observation, which describes a restore
// that never ran, goes with it.
func TestAnInfrastructureFailureBeforeAnyWriteIsAnErrorWithNoRecord(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")

	h.svc.deps.Appliers = vanishingPayloadApplierFactory(h, 0)

	_, err := h.svc.Run(context.Background(), Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection("a.txt"), Apply: true,
	})
	if err == nil {
		t.Fatal("Run succeeded although the commit could not read its own staged payload")
	}
	if got := h.read("a.txt"); got != "one changed" {
		t.Errorf("a.txt = %q, want the untouched worktree", got)
	}
	if got := h.recoveryRecords(t); got != 0 {
		t.Errorf("%d recovery observation(s) survived a commit that wrote nothing", got)
	}
}

// vanishingPayloadApplierFactory makes the applier lose a staged payload before the
// nth operation, modelling awa's own scratch state failing mid-commit rather than a
// worktree path refusing.
func vanishingPayloadApplierFactory(h *harness, after int) func(domrestore.OperationID) (Applier, error) {
	real := h.applierFactory()
	return func(id domrestore.OperationID) (Applier, error) {
		a, err := real(id)
		if err != nil {
			return nil, err
		}
		return &vanishingPayloadApplier{Applier: a, after: after}, nil
	}
}

type vanishingPayloadApplier struct {
	Applier
	after int
	seen  int
}

func (a *vanishingPayloadApplier) Payload(content hashing.ContentHash) (domrestore.StagedPayload, error) {
	if a.seen == a.after {
		a.seen++
		return domrestore.StagedPayload{}, fmt.Errorf("staged payload for %s is gone", content)
	}
	a.seen++
	return a.Applier.Payload(content)
}

// --- cancellation ---------------------------------------------------------

func TestCancellationAtEachPhaseLeavesNoPartialSuccess(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "changed")

	for _, apply := range []bool{false, true} {
		name := "preview"
		if apply {
			name = "apply"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := h.svc.Run(ctx, Request{
				Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
				Selection: h.selection("a.txt"), Apply: apply,
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run under a cancelled context = %v, want context.Canceled", err)
			}
			if got := h.read("a.txt"); got != "changed" {
				t.Errorf("a cancelled invocation mutated the worktree: %q", got)
			}
		})
	}
}

// TestCancellationDuringStagingRefusesBeforeTheFirstWrite covers the phase between
// planning and the commit. Staging reads and verifies every payload; a cancellation
// there must stop the invocation before anything is written, not leave a half-staged
// apply that proceeds.
func TestCancellationDuringStagingRefusesBeforeTheFirstWrite(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.write("b.txt", "two changed")

	ctx, cancel := context.WithCancel(context.Background())
	h.svc.deps.Appliers = cancellingApplierFactory(h, cancelDuringStage, cancel)

	_, err := h.svc.Run(ctx, Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection(), Apply: true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancelled during staging = %v, want context.Canceled", err)
	}
	if h.read("a.txt") != "one changed" || h.read("b.txt") != "two changed" {
		t.Errorf("a cancellation during staging still wrote: a=%q b=%q", h.read("a.txt"), h.read("b.txt"))
	}
}

// TestCancellationAfterTheFirstWriteIsPartialAndConverges is the mid-apply case,
// and the one with a rule of its own: once a path has been written the worktree has
// changed, and that fact outranks how the stop was requested. So the outcome is
// partial — never cancelled, which would read as "nothing happened" — it carries
// the recovery observation the change can be undone from, and a rerun finishes only
// what remains.
//
// The cancellation is injected exactly after the first successful install, so the
// commit is provably mid-flight rather than racing a timer.
func TestCancellationAfterTheFirstWriteIsPartialAndConverges(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	h.write("b.txt", "two")
	h.write("c.txt", "three")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")
	h.write("b.txt", "two changed")
	h.write("c.txt", "three changed")

	ctx, cancel := context.WithCancel(context.Background())
	h.svc.deps.Appliers = cancellingApplierFactory(h, cancelAfterFirstApply, cancel)

	res, err := h.svc.Run(ctx, Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection(), Apply: true,
	})
	if err != nil {
		t.Fatalf("Run = %v; a cancellation after the first write is a result, not an error", err)
	}
	if got := res.Result.Outcome(); got != domrestore.OutcomePartial {
		t.Fatalf("outcome = %s, want partial (%v)", got, res.Result.Reasons())
	}
	if res.Result.Completed() != 1 || res.Result.Remaining() != 2 {
		t.Errorf("completed=%d remaining=%d, want 1 and 2", res.Result.Completed(), res.Result.Remaining())
	}
	if !hasReason(res.Result, domrestore.ReasonCancelled) {
		t.Errorf("reasons = %v, want cancelled named alongside partial-apply", res.Result.Reasons())
	}
	if !hasReason(res.Result, domrestore.ReasonPartialApply) {
		t.Errorf("reasons = %v, want partial-apply", res.Result.Reasons())
	}
	if res.Result.Recovery().IsZero() {
		t.Error("a partial apply carries no recovery observation")
	}
	if got := h.read("a.txt"); got != "one" {
		t.Errorf("a.txt = %q, want the one completed path", got)
	}
	if h.read("b.txt") != "two changed" || h.read("c.txt") != "three changed" {
		t.Errorf("work past the stop was written: b=%q c=%q", h.read("b.txt"), h.read("c.txt"))
	}

	// A rerun on a live context re-plans from current reality and converges.
	h.svc.deps.Appliers = h.applierFactory()
	rerun := h.run(t, id.String(), h.selection(), true)
	if got := rerun.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("rerun outcome = %s (%v)", got, rerun.Result.Reasons())
	}
	if rerun.Result.Completed() != 2 {
		t.Errorf("rerun completed %d, want only the 2 that remained", rerun.Result.Completed())
	}
	if h.read("b.txt") != "two" || h.read("c.txt") != "three" {
		t.Errorf("convergence failed: b=%q c=%q", h.read("b.txt"), h.read("c.txt"))
	}
}

// cancelPoint names where a cancellingApplier trips the caller's cancel function.
type cancelPoint int

const (
	// cancelDuringStage fires while payloads are being verified, before any write.
	cancelDuringStage cancelPoint = iota + 1
	// cancelAfterFirstApply fires once exactly one path has been installed, which is
	// the only moment that produces a genuinely partial commit.
	cancelAfterFirstApply
)

// cancellingApplierFactory wraps the real applier and cancels the invocation's
// context at a chosen point. Injecting the cancellation at a known step is what
// makes these tests deterministic: signalling a process and hoping the commit is
// still running would test the scheduler, not the protocol.
func cancellingApplierFactory(h *harness, at cancelPoint, cancel context.CancelFunc) func(domrestore.OperationID) (Applier, error) {
	real := h.applierFactory()
	return func(id domrestore.OperationID) (Applier, error) {
		a, err := real(id)
		if err != nil {
			return nil, err
		}
		return &cancellingApplier{Applier: a, at: at, cancel: cancel}, nil
	}
}

type cancellingApplier struct {
	Applier
	at     cancelPoint
	cancel context.CancelFunc
	fired  bool
}

func (a *cancellingApplier) Stage(ctx context.Context, content hashing.ContentHash, open func() (io.ReadCloser, error)) (domrestore.StagedPayload, error) {
	p, err := a.Applier.Stage(ctx, content, open)
	if err == nil && a.at == cancelDuringStage && !a.fired {
		a.fired = true
		a.cancel()
	}
	return p, err
}

func (a *cancellingApplier) Apply(ctx context.Context, op domrestore.StagedOperation) domrestore.MutationResult {
	out := a.Applier.Apply(ctx, op)
	if out.Err() != nil {
		return out
	}
	if a.at == cancelAfterFirstApply && !a.fired {
		a.fired = true
		a.cancel()
	}
	return out
}

// TestAFollowedEntryIsABoundaryNotAReadyOperation covers the one scan policy that
// files a record under a path the mutation boundary cannot act on. With
// follow_symlinks enabled, a file inside a symlinked directory is recorded at its
// virtual path — but every component below the link belongs to the link's target, so
// restore can neither read the current bytes there nor install anything: its
// no-follow descent refuses at the link.
//
// That has to be a planning refusal. Previewing it as ordinary work and only failing
// at apply time would give the user a preview its own apply contradicts, with a reason
// that reads like something changed underfoot when nothing did.
func TestAFollowedEntryIsABoundaryNotAReadyOperation(t *testing.T) {
	h := setup(t)
	h.write("real/data.txt", "reviewed\n")
	if err := os.Symlink("real", h.abs("linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h.cfg.Scope.FollowSymlinks = true

	id := h.checkpointNow(h.cfg)
	// The generator rewrote the file through its real path; the followed record for
	// linked/data.txt now differs from the source too.
	h.write("real/data.txt", "generated\n")

	preview := h.run(t, id.String(), h.selection("linked"), false)
	if preview.Result.Counts().Mutating() != 0 {
		t.Fatalf("a followed record produced %d ready operation(s): %+v",
			preview.Result.Counts().Mutating(), preview.Result.Counts())
	}
	if preview.Result.Counts().Blocked == 0 {
		t.Fatalf("counts = %+v, want the followed path blocked", preview.Result.Counts())
	}
	requireFailure(t, preview.Result, "linked/data.txt", domrestore.ReasonSymlinkAncestor)

	// And apply refuses on the same evidence rather than discovering it at the
	// descriptor: same reason, same path, nothing written.
	applied := h.run(t, id.String(), h.selection("linked"), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused (%v)", got, applied.Result.Reasons())
	}
	if !hasReason(applied.Result, domrestore.ReasonSymlinkAncestor) {
		t.Errorf("reasons = %v, want symlink-ancestor", applied.Result.Reasons())
	}
	if got := h.read("real/data.txt"); got != "generated\n" {
		t.Errorf("a refused apply wrote through the symlink: %q", got)
	}
}

// TestAnUnchangedFollowedEntryIsEqualNotABoundary is the other half of the same rule.
// A path reached through a symlink whose state already equals the source needs no
// mutation, so no mutation boundary applies to it: it is settled, and counting it as
// an evidence gap would be wrong twice over. Under --all it would refuse the entire
// restore over a path nobody asked to change, and the "blocked" fact it produced would
// carry the operation kind `equal` — a failure describing work that does not exist.
func TestAnUnchangedFollowedEntryIsEqualNotABoundary(t *testing.T) {
	h := setup(t)
	h.write("real/data.txt", "reviewed\n")
	h.write("direct.txt", "reviewed\n")
	if err := os.Symlink("real", h.abs("linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h.cfg.Scope.FollowSymlinks = true

	id := h.checkpointNow(h.cfg)
	// Only the direct path drifts. The followed record still matches its source.
	h.write("direct.txt", "generated\n")

	preview := h.run(t, id.String(), h.selection(), false)
	if preview.Result.Counts().Blocked != 0 {
		t.Fatalf("counts = %+v, want nothing blocked: the followed path is unchanged (failures %+v)",
			preview.Result.Counts(), preview.Result.Failures())
	}
	if preview.Result.Counts().Equal == 0 {
		t.Errorf("counts = %+v, want the unchanged followed path counted as proven-equal scope",
			preview.Result.Counts())
	}
	if preview.Result.Counts().Unavailable() != 0 {
		t.Error("an unchanged followed path made the whole plan incomplete")
	}

	// And --all applies the drifted path rather than refusing over an unrelated one.
	applied := h.run(t, id.String(), h.selection(), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v; failures %+v)", got, applied.Result.Reasons(), applied.Result.Failures())
	}
	if got := h.read("direct.txt"); got != "reviewed\n" {
		t.Errorf("direct.txt = %q, want the restored bytes", got)
	}
}

// TestSameBytesReachedThroughASymlinkAreNotEqual pins provenance as part of the
// comparison. The source proves a real file at linked/data.txt; the worktree now
// reaches the same bytes with the same mode only because `linked` became a symlink.
//
// Every field of the two node states agrees, so a comparison built from state alone
// calls this equal — and then restore reports a path that "already matches" a tree
// whose form the source never proved, having restored nothing. The shapes differ, and
// since one side is only reachable through a link, the path is a boundary.
func TestSameBytesReachedThroughASymlinkAreNotEqual(t *testing.T) {
	h := setup(t)
	const shared = "same bytes\n"
	h.write("real/data.txt", shared)
	h.write("linked/data.txt", shared)
	h.cfg.Scope.FollowSymlinks = true
	id := h.checkpointNow(h.cfg)

	// Replace the real directory with a link to one holding identical content.
	h.remove("linked")
	if err := os.Symlink("real", h.abs("linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res := h.run(t, id.String(), h.selection(), false)
	requireFailure(t, res.Result, "linked/data.txt", domrestore.ReasonSymlinkAncestor)
	// And apply refuses rather than reporting a no-op over a tree it did not restore.
	applied := h.run(t, id.String(), h.selection(), true)
	if got := applied.Result.Outcome(); got != domrestore.OutcomeRefused {
		t.Fatalf("outcome = %s, want refused (%v)", got, applied.Result.Reasons())
	}
	if got := h.read("real/data.txt"); got != shared {
		t.Errorf("a refused apply wrote through the symlink: %q", got)
	}
}

// TestTheSameBytesOneHopFurtherAwayAreNotEqual is the narrow form of the same rule.
// Both sides are reached by following symlinks, both hold identical bytes with
// identical modes, and the only difference is how far away they are: one hop at
// checkpoint time, two hops now that the link points at another link.
//
// The project's canonical identity separates those — the tree hash folds the link a
// node was reached through and the number of hops — so a restore comparison that
// collapsed provenance to "followed or not" would call this equal and report a no-op
// for a shape the source never proved.
func TestTheSameBytesOneHopFurtherAwayAreNotEqual(t *testing.T) {
	h := setup(t)
	const shared = "same bytes\n"
	h.write("real/data.txt", shared)
	if err := os.Symlink("real", h.abs("linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	h.cfg.Scope.FollowSymlinks = true
	id := h.checkpointNow(h.cfg)

	// Same destination, same bytes, one more hop to get there.
	h.remove("linked")
	if err := os.Symlink("real", h.abs("mid")); err != nil {
		t.Fatalf("symlink mid: %v", err)
	}
	if err := os.Symlink("mid", h.abs("linked")); err != nil {
		t.Fatalf("symlink linked: %v", err)
	}

	res := h.run(t, id.String(), h.selection("linked"), false)
	requireFailure(t, res.Result, "linked/data.txt", domrestore.ReasonSymlinkAncestor)
	if got := h.read("real/data.txt"); got != shared {
		t.Errorf("the fixture changed the real file: %q", got)
	}
}

// --- preserving the current bytes -----------------------------------------

// countingContent counts how many current files an apply actually reads. It is the
// oracle for the cost rule: the bytes a restore preserves are the bytes it is about
// to destroy, so the work — and the memory behind it — is proportional to the
// selection, not to the observed project.
type countingContent struct {
	inner WorktreeContent
	opens int
	paths []string
}

func (c *countingContent) Open(p worktree.RelPath, observed worktree.StatSignature) (io.ReadCloser, error) {
	c.opens++
	c.paths = append(c.paths, p.String())
	return c.inner.Open(p, observed)
}

// TestCurrentBytesAreReadOnlyForThePathsTheCommitDestroys pins apply's per-file cost
// to the selection. The project holds many observed files and the restore touches
// one, so exactly one current file may be read — the one about to be overwritten.
//
// This is the behavioural half of the boundedness rule the observation used to
// break: a scan asked for content sources retains one closure per blob-intent
// regular entry, whatever the selection, so an apply of a single path in a large
// project paid for the whole project. Reading through the port means there is no
// per-observed-file cost left to pay.
func TestCurrentBytesAreReadOnlyForThePathsTheCommitDestroys(t *testing.T) {
	h := setup(t)
	for i := 0; i < 50; i++ {
		h.write(fmt.Sprintf("bulk/f%02d.txt", i), fmt.Sprintf("bulk %d", i))
	}
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "one changed")

	counting := &countingContent{inner: h.svc.deps.Current}
	h.svc.deps.Current = counting

	res := h.run(t, id.String(), h.selection("a.txt"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v)", got, res.Result.Reasons())
	}
	if counting.opens != 1 || len(counting.paths) != 1 || counting.paths[0] != "a.txt" {
		t.Errorf("apply read %d current file(s) %v, want exactly a.txt: cost must follow the selection, not the observation",
			counting.opens, counting.paths)
	}
}

// TestAHashOnlyCurrentFileIsStillPreservedAndUndoable is the storage-policy half.
// The file about to be overwritten is larger than the project's max_file_size, so the
// observation recorded its identity without intending to store its bytes — and it is
// perfectly readable. Undo evidence may not inherit that gap: the bytes a checkpoint
// would have skipped are exactly the ones nothing else can bring back.
//
// It also proves the published record tells the truth about itself. Restoring from
// restore:<id>:before must return the bytes, which it can only do if the recovery
// manifest advertises the blob storage this record really has rather than echoing the
// scanner's hash-only intent.
func TestAHashOnlyCurrentFileIsStillPreservedAndUndoable(t *testing.T) {
	h := setup(t)
	// Small enough that the current file below is over the limit, large enough that
	// the checkpointed version is under it and therefore has stored bytes to restore.
	h.cfg.Hashing.MaxFileSize = 16
	if h.cfg.Hashing.LargeFilePolicy != domainconfig.LargeFileHashOnly {
		t.Fatalf("this fixture needs the hash-only large-file policy, got %s", h.cfg.Hashing.LargeFilePolicy)
	}
	h.write("big.txt", "reviewed")
	id := h.checkpointNow(h.cfg)

	generated := strings.Repeat("generated ", 20)
	h.write("big.txt", generated)

	res := h.run(t, id.String(), h.selection("big.txt"), true)
	if got := res.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("outcome = %s (%v); a readable file must be preservable whatever the storage policy says",
			got, res.Result.Reasons())
	}
	if got := h.read("big.txt"); got != "reviewed" {
		t.Fatalf("big.txt = %q, want the reviewed bytes", got)
	}

	// The undo has to produce the generated bytes, which only works if the record
	// claims — truthfully — that it holds them.
	undo := h.run(t, res.RecoveryRef, h.selection("big.txt"), true)
	if got := undo.Result.Outcome(); got != domrestore.OutcomeApplied {
		t.Fatalf("undo outcome = %s (%v), failures %+v", got, undo.Result.Reasons(), undo.Result.Failures())
	}
	if got := h.read("big.txt"); got != generated {
		t.Errorf("undo restored %q, want the hash-only bytes the recovery observation captured", got)
	}
}

// --- boundedness ----------------------------------------------------------

// TestPlanningEmitsOperationsWhileTheSourceIsStillStreaming proves the comparison
// and the spool do not hold the operation set — as an ordering fact, which is the
// only form of it a test can observe honestly.
//
// The invariant is backpressure, not retained bytes: a mutating decision crosses the
// spool boundary while the canonical merge is still advancing the source manifest.
// A planner that accumulated its operations and flushed them afterwards would reach
// every Add with the source already exhausted, whatever the heap happened to hold.
//
// So the oracle wraps the two ports the planner already streams through — the
// checkpoint repository's manifest stream and the restore spool — and compares two
// event counts. Everything the wrappers do not count they delegate: the records are
// decoded by the real checkpointjson stream, verified against the header's tree hash,
// stats, and record count by the resolver's cursor, and ordered by worktree.Ordered,
// exactly as in production. The spool is the real one, and it still owns its own
// lifecycle.
//
// Every number here is a cardinality or an algorithmic lookahead. There is no byte
// size, no elapsed time, no allocation count, and nothing the runner or the race
// detector can shift.
func TestPlanningEmitsOperationsWhileTheSourceIsStillStreaming(t *testing.T) {
	// Canonically ordered root-level regular files, all of them changed. Every source
	// record is therefore also an operation, which is what lets the lead below compare
	// like with like; the fixture check further down is what keeps that true rather
	// than assumed.
	fixture := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
	// The merge holds one record per side, so when it decides a path it has already
	// read that path's successor and no more. That one record is the whole accepted
	// lead — a fact about the merge's state, not a budget.
	const acceptedLookahead = 1

	h := setup(t)
	for _, p := range fixture {
		h.write(p, "reviewed")
	}
	id := h.checkpointNow(h.cfg)
	for _, p := range fixture {
		h.write(p, "changed")
	}

	source := &observingCheckpoints{Repository: checkpointjson.NewRepo(h.layout)}
	h.svc.deps.Resolver = h.resolverWith(source)
	var spool *observingSpool
	h.svc.deps.Spools = func(opID domrestore.OperationID) (Spool, error) {
		inner, err := restorespool.Open(h.layout, opID)
		if err != nil {
			return nil, err
		}
		spool = &observingSpool{inner: inner, source: source}
		return spool, nil
	}

	ctx := context.Background()
	// begin and plan are the mutating path's own seam: a preview creates no spool at
	// all, so observing the boundary requires the invocation that owns one. Nothing
	// past planning runs, which keeps the counted window exactly the comparison.
	sess, err := h.svc.begin(ctx, Request{
		Project: h.project, Config: h.cfg, Ref: h.ref(id.String()),
		Selection: h.selection(), Apply: true,
	}, true)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer sess.close()

	planned, err := sess.plan(ctx)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if source.opens != 1 {
		t.Fatalf("planning opened %d source cursors, want 1: the record counts below are only attributable to a single pass", source.opens)
	}
	if source.records != len(fixture) {
		t.Fatalf("the source manifest yielded %d records for a %d-file fixture: a record that carries no operation would inflate the lead this test measures",
			source.records, len(fixture))
	}
	if got, want := planned.Counts(), (domrestore.Counts{Replace: len(fixture)}); got != want {
		t.Fatalf("counts = %+v, want %+v: every fixture path must be one ready replacement", got, want)
	}

	if spool == nil {
		t.Fatal("planning opened no spool, so no operation could have crossed the boundary")
	}
	// Exact counts, not an early-only assertion: a planner could emit the first
	// operation eagerly and still materialize everything after it.
	if len(spool.at) != len(fixture) || spool.inner.Count() != len(fixture) {
		t.Fatalf("the spool observer saw %d operations and the real spool holds %d, want %d each",
			len(spool.at), spool.inner.Count(), len(fixture))
	}

	if spool.at[0] >= source.records {
		t.Fatalf("the first operation reached the spool with all %d source records already consumed: a streaming planner emits it while source work remains",
			source.records)
	}
	for i, consumed := range spool.at {
		emitted := i + 1
		if lead := consumed - emitted; lead > acceptedLookahead {
			t.Errorf("operation %d of %d reached the spool after %d source records: a lead of %d over the operations emitted, above the merge's %d-record lookahead",
				emitted, len(fixture), consumed, lead, acceptedLookahead)
		}
	}
}

// observingCheckpoints counts the source manifest records planning successfully
// reads. It embeds the real repository so every other part of the contract — header
// reads, prefix resolution, store health — is the production one, and only the
// stream it hands out is wrapped.
//
// It deliberately sits BELOW state.ResolvedState.Manifest: the resolver's verifying
// cursor and worktree.Ordered are layered on top of what this returns, so the
// observed pass is a fully verified, canonically ordered read rather than a fixture
// standing in for one.
type observingCheckpoints struct {
	checkpoint.Repository
	// opens counts cursors taken over the manifest, so a second pass cannot be
	// mistaken for a single planner's progress.
	opens int
	// records counts the source records a cursor has successfully yielded.
	records int
}

func (o *observingCheckpoints) OpenManifest(id checkpoint.CheckpointID) (worktree.ManifestStream, error) {
	inner, err := o.Repository.OpenManifest(id)
	if err != nil {
		return nil, err
	}
	return &observingManifest{inner: inner, obs: o}, nil
}

type observingManifest struct {
	inner worktree.ManifestStream
	obs   *observingCheckpoints
}

func (m *observingManifest) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	cur, err := m.inner.Open(ctx)
	if err != nil {
		return nil, err
	}
	m.obs.opens++
	return &observingCursor{inner: cur, obs: m.obs}, nil
}

// observingCursor counts only successful advances, so an end of stream and a failure
// both leave the count at the records the planner actually received.
type observingCursor struct {
	inner worktree.ManifestCursor
	obs   *observingCheckpoints
}

func (c *observingCursor) Next() bool {
	if !c.inner.Next() {
		return false
	}
	c.obs.records++
	return true
}

func (c *observingCursor) Record() worktree.ManifestRecord { return c.inner.Record() }
func (c *observingCursor) Err() error                      { return c.inner.Err() }
func (c *observingCursor) Close() error                    { return c.inner.Close() }

// observingSpool delegates the whole spool contract to the real spool — including
// Discard, so the session's cleanup remains the production lifecycle — and records
// when each operation arrived, measured in source records consumed.
type observingSpool struct {
	inner  *restorespool.Spool
	source *observingCheckpoints
	// at[i] is how many source records planning had consumed when the i-th mutating
	// operation crossed the boundary. Only accepted adds are recorded, so the
	// sequence is the operations the spool actually holds.
	at []int
}

func (s *observingSpool) Add(op domrestore.PlannedOperation) error {
	consumed := s.source.records
	if err := s.inner.Add(op); err != nil {
		return err
	}
	s.at = append(s.at, consumed)
	return nil
}

func (s *observingSpool) Seal() error            { return s.inner.Seal() }
func (s *observingSpool) MaxDirectoryDepth() int { return s.inner.MaxDirectoryDepth() }
func (s *observingSpool) Discard() error         { return s.inner.Discard() }

func (s *observingSpool) Open(ctx context.Context) (domrestore.OperationCursor, error) {
	return s.inner.Open(ctx)
}

// --- spool hygiene --------------------------------------------------------

// TestPreviewCreatesNoRestoreScratch proves the side-effect-free contract the strong
// way: with awa's own temp area read-only, a preview must still produce its full
// report. Checking after the fact that nothing was left behind cannot distinguish
// "created nothing" from "created scratch and cleaned up", and the difference is
// what a read-only project — or a preview interrupted mid-plan — actually feels.
//
// The claim it pins is about RESTORE's scratch, which is what this package owns. The
// observation is the scanner's, and one whose record count exceeds the sorter's
// in-memory buffer spills through this same directory; this fixture is deliberately
// small enough that it does not, so a failure here means restore created state of its
// own rather than that the scan grew.
func TestPreviewCreatesNoRestoreScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permission is not modeled the same way here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only directory is still writable")
	}
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "changed")

	if err := os.Chmod(h.layout.TmpDir(), 0o500); err != nil {
		t.Fatalf("chmod store tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.layout.TmpDir(), 0o700) })

	res := h.run(t, id.String(), h.selection("a.txt"), false)
	if got := res.Result.Outcome(); got != domrestore.OutcomePreview {
		t.Fatalf("outcome = %s, want preview", got)
	}
	if res.Result.Counts().Replace != 1 {
		t.Errorf("counts = %+v, want the one replacement described", res.Result.Counts())
	}
	entries, err := os.ReadDir(h.layout.TmpDir())
	if err != nil {
		t.Fatalf("read store tmp: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("preview created state under %s: %+v", h.layout.TmpDir(), entries)
	}
}

func TestAnInvocationLeavesNoTempBehind(t *testing.T) {
	h := setup(t)
	h.write("a.txt", "one")
	id := h.checkpointNow(h.cfg)
	h.write("a.txt", "changed")

	for _, apply := range []bool{false, true} {
		h.run(t, id.String(), h.selection("a.txt"), apply)
		entries, err := os.ReadDir(h.layout.TmpDir())
		if err != nil {
			t.Fatalf("read store tmp: %v", err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "restore-") {
				t.Errorf("apply=%v left the spool %q behind", apply, filepath.Join(h.layout.TmpDir(), e.Name()))
			}
		}
	}
}
