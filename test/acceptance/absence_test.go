package acceptance

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func restoreJSON(t *testing.T, root string, args ...string) (int, restoreDoc) {
	t.Helper()
	code, stdout, stderr := awa(t, root, append([]string{"restore", "--json"}, args...)...)
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("restore %v exit = %d printed no document; stderr = %q", args, code, stderr)
	}
	return code, decodeRestore(t, stdout)
}

func failureReasons(doc restoreDoc, path string) []string {
	for _, f := range doc.Data.Failures {
		if f.Path == path {
			return f.Reasons
		}
	}
	return nil
}

func recoveryRecords(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, ".awa", "restores"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Name() != "tmp" {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestAbsenceDirectoryOnlyRuleNeverProvesAFileAbsent(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, ".awaignore", "/kind-boundary/\n")
	writeAt(t, root, "kind-boundary/child", "hidden\n")
	writeAt(t, root, "source.txt", "source\n")
	id := checkpointID(t, root)

	if err := os.RemoveAll(filepath.Join(root, "kind-boundary")); err != nil {
		t.Fatal(err)
	}
	writeAt(t, root, "kind-boundary", "admitted file\n")

	_, preview := restoreJSON(t, root, id, "--", "kind-boundary")
	if preview.Data.Complete || preview.Data.Counts.DeleteFile != 0 || preview.Data.Counts.Blocked != 1 {
		t.Fatalf("preview counts = %+v complete = %v, want one blocked operation and no deletion", preview.Data.Counts, preview.Data.Complete)
	}
	if got := failureReasons(preview, "kind-boundary"); !slices.Equal(got, []string{"ignored-boundary"}) {
		t.Errorf("kind-boundary reasons = %v, want ignored-boundary", got)
	}
	if preview.Data.Boundary.SourcePathCoverage != "recorded" || !preview.Data.Boundary.PolicyCompatible {
		t.Errorf("boundary = %+v, want recorded coverage under a compatible policy", preview.Data.Boundary)
	}

	for _, args := range [][]string{{"--apply", id, "--", "kind-boundary"}, {"--apply", "--all", id}} {
		code, doc := restoreJSON(t, root, args...)
		if code == 0 || doc.Data.Outcome != "refused" {
			t.Errorf("restore %v exit = %d outcome = %q, want a refusal", args, code, doc.Data.Outcome)
		}
	}
	if got := readAt(t, root, "kind-boundary"); got != "admitted file\n" {
		t.Errorf("kind-boundary = %q, want the admitted file untouched", got)
	}
	if recs := recoveryRecords(t, root); len(recs) != 0 {
		t.Errorf("a refused restore left recovery records %v", recs)
	}
}

func TestAbsenceExcludedLeavesAndRuleEditsStayUnproved(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, ".awaignore", "*.env\n")
	writeAt(t, root, "secret.env", "token\n")
	writeAt(t, root, "sub/.awaignore", ".awaignore\nscratch/\n")
	writeAt(t, root, "sub/scratch/x", "scratch\n")
	writeAt(t, root, "sub/kept.txt", "kept\n")
	id := checkpointID(t, root)

	writeAt(t, root, ".awaignore", "# no rules\n")
	if err := os.Remove(filepath.Join(root, "sub", ".awaignore")); err != nil {
		t.Fatal(err)
	}

	_, doc := restoreJSON(t, root, "--all", id)
	if doc.Data.Complete || doc.Data.Counts.DeleteFile != 0 {
		t.Fatalf("counts = %+v complete = %v, want no deletion of anything the source excluded", doc.Data.Counts, doc.Data.Complete)
	}
	for _, p := range []string{"secret.env", "sub/scratch/x"} {
		if got := failureReasons(doc, p); !slices.Equal(got, []string{"ignored-boundary"}) {
			t.Errorf("%s reasons = %v, want ignored-boundary", p, got)
		}
	}
	if doc.Data.Counts.Replace != 1 {
		t.Errorf("counts = %+v, want the edited .awaignore planned as one replacement", doc.Data.Counts)
	}
}

func TestAbsenceHiddenCurrentDestinationIsBlockedInPreview(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, ".awaignore", "# none\n")
	writeAt(t, root, "artifacts/generated.txt", "generated\n")
	id := checkpointID(t, root)
	writeAt(t, root, ".awaignore", "/artifacts/\n")
	writeAt(t, root, "artifacts/generated.txt", "current hidden bytes\n")

	_, preview := restoreJSON(t, root, id, "--", "artifacts/generated.txt")
	if preview.Data.Counts.Create != 0 || preview.Data.Counts.Blocked != 1 {
		t.Fatalf("preview counts = %+v, want the create blocked", preview.Data.Counts)
	}
	if got := failureReasons(preview, "artifacts/generated.txt"); !slices.Equal(got, []string{"ignored-boundary"}) {
		t.Errorf("reasons = %v, want ignored-boundary", got)
	}
	code, applied := restoreJSON(t, root, "--apply", id, "--", "artifacts/generated.txt")
	if code == 0 || applied.Data.Outcome != "refused" || applied.Data.Recovery != nil {
		t.Errorf("apply exit = %d outcome = %q recovery = %+v, want a refusal without recovery", code, applied.Data.Outcome, applied.Data.Recovery)
	}
	if got := readAt(t, root, "artifacts/generated.txt"); got != "current hidden bytes\n" {
		t.Errorf("hidden destination = %q, want it untouched", got)
	}
	if recs := recoveryRecords(t, root); len(recs) != 0 {
		t.Errorf("a refused restore left recovery records %v", recs)
	}
}

func TestAbsenceProvedByCoverageStillDeletes(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, "source.txt", "source\n")
	writeAt(t, root, "node", "a file\n")
	id := checkpointID(t, root)

	writeAt(t, root, "extra.txt", "extra\n")
	if err := os.Remove(filepath.Join(root, "node")); err != nil {
		t.Fatal(err)
	}
	writeAt(t, root, "node/inner.txt", "below a former file\n")

	_, explicit := restoreJSON(t, root, id, "--", "extra.txt")
	if !explicit.Data.Complete || explicit.Data.Counts.DeleteFile != 1 {
		t.Fatalf("explicit counts = %+v complete = %v, want one proved deletion", explicit.Data.Counts, explicit.Data.Complete)
	}
	code, applied := restoreJSON(t, root, "--apply", "--all", id)
	if code != 0 || applied.Data.Outcome != "applied" {
		t.Fatalf("apply --all exit = %d outcome = %q failures = %+v", code, applied.Data.Outcome, applied.Data.Failures)
	}
	if existsAt(root, "extra.txt") || readAt(t, root, "node") != "a file\n" {
		t.Fatalf("apply did not converge to the source")
	}
	if applied.Data.Recovery == nil {
		t.Fatal("applied restore published no recovery observation")
	}

	code, undo := restoreJSON(t, root, "--apply", "--all", applied.Data.Recovery.Ref)
	if code != 0 || undo.Data.Outcome != "applied" || undo.Data.Boundary.SourcePathCoverage != "recorded" {
		t.Fatalf("undo exit = %d outcome = %q boundary = %+v failures = %+v", code, undo.Data.Outcome, undo.Data.Boundary, undo.Data.Failures)
	}
	if readAt(t, root, "extra.txt") != "extra\n" || readAt(t, root, "node/inner.txt") != "below a former file\n" {
		t.Error("undo did not restore the pre-restore state")
	}
}

func TestAbsencePartialApplyRecoveryJustifiesUnreachedCreates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permission is not modeled the same way here")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: an unwritable directory is still writable")
	}
	root := initProject(t)
	writeAt(t, root, "gen/early/a.txt", "reviewed early\n")
	writeAt(t, root, "gen/late/new.txt", "reviewed new\n")
	id := checkpointID(t, root)
	writeAt(t, root, "gen/early/a.txt", "generated early\n")
	if err := os.Remove(filepath.Join(root, "gen", "late", "new.txt")); err != nil {
		t.Fatal(err)
	}
	lateDir := filepath.Join(root, "gen", "late")
	if err := os.Chmod(lateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(lateDir, 0o700) }()

	_, partial := restoreJSON(t, root, "--apply", id, "--", "gen")
	if partial.Data.Outcome != "partial" || partial.Data.Recovery == nil {
		t.Fatalf("outcome = %q recovery = %+v, want partial with recovery", partial.Data.Outcome, partial.Data.Recovery)
	}
	if err := os.Chmod(lateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	_, preview := restoreJSON(t, root, partial.Data.Recovery.Ref, "--", "gen")
	if !preview.Data.Complete || preview.Data.Counts.DeleteFile != 0 || preview.Data.Counts.Replace != 1 {
		t.Fatalf("undo preview counts = %+v complete = %v, want one replacement and nothing deleted", preview.Data.Counts, preview.Data.Complete)
	}
	code, undo := restoreJSON(t, root, "--apply", partial.Data.Recovery.Ref, "--", "gen")
	if code != 0 || undo.Data.Outcome != "applied" {
		t.Fatalf("undo exit = %d outcome = %q failures = %+v", code, undo.Data.Outcome, undo.Data.Failures)
	}
	if readAt(t, root, "gen/early/a.txt") != "generated early\n" || existsAt(root, "gen/late/new.txt") {
		t.Error("undo did not return to the pre-restore state")
	}
}

func TestAbsenceProviderNeedsCoverageForOneSidedPaths(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, ".awaignore", "/scratch/\n")
	writeAt(t, root, "artifacts/generated.txt", "generated\n")
	writeAt(t, root, "source.txt", "source\n")
	checkpointID(t, root)

	// scratch/ did not exist at the checkpoint, so the source proves it absent while
	// now holds it only as a boundary name: no manifest record on either side.
	writeAt(t, root, "scratch/x", "excluded\n")
	same := compareJSON(t, root, "latest..now")
	if same.Outcome != "equal" {
		t.Fatalf("latest..now with a frontier-only name = %q (%s), want equal", same.Outcome, same.Reason)
	}
	writeAt(t, root, "new.txt", "new\n")
	if d := compareJSON(t, root, "latest..now"); d.Outcome != "different" || d.Counts == nil || d.Counts.Added != 1 {
		t.Fatalf("latest..now after an added file = %q %+v, want different with one addition", d.Outcome, d.Counts)
	}

	if err := os.Remove(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	newID := checkpointID(t, root)
	if d := compareJSON(t, root, newID+"..now"); d.Outcome != "equal" {
		t.Fatalf("precondition: %s..now = %q (%s), want equal", newID, d.Outcome, d.Reason)
	}
	writeAt(t, root, ".awaignore", "/scratch/\n/artifacts/\n")
	d := compareJSON(t, root, newID+"..now")
	if d.Outcome != "incomparable" || d.Reason != "absence-unproved" || d.Counts != nil {
		t.Fatalf("after hiding a recorded path = %q reason %q counts %+v, want incomparable absence-unproved", d.Outcome, d.Reason, d.Counts)
	}
	assertComparisonCompleteness(t, d, false)
	if d.Left == nil || d.Left.EvidenceBoundary.PathCoverage != "recorded" || d.Right.EvidenceBoundary.PathCoverage != "recorded" {
		t.Errorf("operand coverage = %+v / %+v, want recorded on both", d.Left, d.Right)
	}
}

// predecessorProject installs the schema-1 store a published release wrote (see
// testdata/predecessor-v1) into a fresh project, beside the worktree it ended with.
func predecessorProject(t *testing.T) string {
	t.Helper()
	root := initProject(t)
	src := filepath.Join("testdata", "predecessor-v1")
	copyTree(t, filepath.Join(src, "records"), filepath.Join(root, ".awa"))
	copyTree(t, filepath.Join(src, "worktree"), root)
	if err := os.Rename(filepath.Join(root, "dot-awaignore"), filepath.Join(root, ".awaignore")); err != nil {
		t.Fatal(err)
	}
	return root
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := os.ReadFile(p) //nolint:gosec // test fixture path
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copying %s: %v", from, err)
	}
}

const (
	predecessorCheckpoint = "y3e85ytwxtgm13etqp794x2brp6nqw18"
	predecessorRepeat     = "p0a2fnfzwctx591963qf76m7y07sew1w"
	predecessorSameRun    = "18dce8d56a78"
	predecessorRun        = "18dce8d56f39"
	predecessorRecovery   = "restore:18dce8d5745ae5d8ac5f8abb15414091:before"
)

func TestPredecessorStoreKeepsPositiveEvidenceAndBlocksOnlyAbsence(t *testing.T) {
	root := predecessorProject(t)

	if code, stdout, stderr := awa(t, root, "log"); code != 0 || !strings.Contains(stdout, "predecessor baseline") {
		t.Fatalf("log exit = %d stdout = %q stderr = %q", code, stdout, stderr)
	}
	if code, stdout, stderr := awa(t, root, "changes", predecessorCheckpoint+"..now"); code != 0 || !strings.Contains(stdout, "run-made.txt") {
		t.Errorf("changes exit = %d stdout = %q stderr = %q, want the observation delta", code, stdout, stderr)
	}
	if code, stdout, stderr := awa(t, root, "diff", predecessorCheckpoint+"..now", "--", "keep.txt"); code != 0 {
		t.Errorf("diff exit = %d stdout = %q stderr = %q", code, stdout, stderr)
	}

	writeAt(t, root, "keep.txt", "edited\n")
	code, positive := restoreJSON(t, root, "--apply", predecessorCheckpoint, "--", "keep.txt")
	if code != 0 || positive.Data.Outcome != "applied" || positive.Data.Boundary.SourcePathCoverage != "unknown" {
		t.Fatalf("positive restore exit = %d outcome = %q boundary = %+v", code, positive.Data.Outcome, positive.Data.Boundary)
	}
	if readAt(t, root, "keep.txt") != "keep\n" {
		t.Error("positive restore from a predecessor did not restore its owned bytes")
	}

	_, deletion := restoreJSON(t, root, predecessorCheckpoint, "--", "run-made.txt")
	if got := failureReasons(deletion, "run-made.txt"); !slices.Equal(got, []string{"policy-incompatible"}) {
		t.Errorf("deletion from a predecessor: reasons = %v, want policy-incompatible", got)
	}
	_, all := restoreJSON(t, root, "--all", predecessorCheckpoint)
	if all.Data.Complete || all.Data.Counts.DeleteFile != 0 {
		t.Errorf("--all from a predecessor counts = %+v complete = %v, want the dependent deletion to refuse the selection", all.Data.Counts, all.Data.Complete)
	}

	// The predecessor binary published this recovery after a partial apply that
	// stopped at a hidden existing z-hidden.txt. Removing the rule must not make that
	// covered-but-unproved path a deletion.
	writeAt(t, root, ".awaignore", "/kind-boundary/\n")
	_, undo := restoreJSON(t, root, predecessorRecovery, "--", "z-hidden.txt")
	if got := failureReasons(undo, "z-hidden.txt"); !slices.Equal(got, []string{"policy-incompatible"}) {
		t.Errorf("predecessor undo reasons = %v (counts %+v), want policy-incompatible", got, undo.Data.Counts)
	}
	if !existsAt(root, "z-hidden.txt") {
		t.Error("a preview removed a file")
	}

	_, resolved := resolveJSON(t, root, predecessorCheckpoint)
	if resolved.Outcome != "resolved" || resolved.State.EvidenceBoundary.PathCoverage != "unknown" {
		t.Errorf("resolve predecessor = %+v, want resolved with unknown coverage", resolved)
	}
	if d := compareJSON(t, root, predecessorCheckpoint+".."+predecessorCheckpoint); d.Outcome != "equal" {
		t.Errorf("same predecessor record compares %q, want equal", d.Outcome)
	}
	// The predecessor matched its exclusion text under other pattern semantics, so
	// its scan policy is not this binary's even where the text is identical.
	if d := compareJSON(t, root, predecessorCheckpoint+"..now"); d.Outcome != "incomparable" || d.Reason != "incompatible-identity-policy" {
		t.Errorf("predecessor..now = %q reason %q, want incomparable incompatible-identity-policy", d.Outcome, d.Reason)
	}
	for _, rng := range []string{
		predecessorCheckpoint + ".." + predecessorRepeat,
		"run:" + predecessorSameRun + ":before..run:" + predecessorSameRun + ":after",
	} {
		d := compareJSON(t, root, rng)
		if d.Left == nil || d.Right == nil || d.Left.TreeHash != d.Right.TreeHash {
			t.Fatalf("%s: fixture operands must share one tree hash: %+v %+v", rng, d.Left, d.Right)
		}
		if d.Outcome != "incomparable" || d.Reason != "absence-unproved" {
			t.Errorf("%s = %q reason %q, want incomparable absence-unproved despite equal tree hashes", rng, d.Outcome, d.Reason)
		}
	}
	sameRun := "run:" + predecessorSameRun + ":after"
	if d := compareJSON(t, root, sameRun+".."+sameRun); d.Outcome != "equal" {
		t.Errorf("same predecessor run observation compares %q, want equal", d.Outcome)
	}

	newID := checkpointID(t, root)
	if d := compareJSON(t, root, newID+"..now"); d.Outcome != "equal" {
		t.Errorf("new checkpoint..now = %q (%s), want equal", d.Outcome, d.Reason)
	}

	code, stdout, stderr := awa(t, root, "doctor", "--json")
	if strings.Contains(stdout, "corrupt") || strings.Contains(stdout, "incompatible") {
		t.Errorf("doctor exit = %d flags readable predecessors: %s %s", code, stdout, stderr)
	}
	if reasons := restoreGCReasons(t, root); slices.ContainsFunc(reasons, func(r string) bool {
		return strings.HasSuffix(r, "-corrupt") || strings.HasSuffix(r, "-incompatible")
	}) {
		t.Errorf("gc retains readable predecessors as unreadable: %v", reasons)
	}
}

func TestProofBearingRecordsDistinguishDamageFromUnsupportedGenerations(t *testing.T) {
	root := predecessorProject(t)
	damaged := checkpointID(t, root)
	frontier := filepath.Join(root, ".awa", "checkpoints", damaged, "frontier.jsonl")
	if err := os.Chmod(frontier, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(frontier, []byte(`{"path":"forged","limit":"subtree"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := awa(t, root, "run", "--record", "--", "true"); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}
	runFrontiers, err := filepath.Glob(filepath.Join(root, ".awa", "runs", "entries", "*", "*", "before.frontier.jsonl"))
	if err != nil || len(runFrontiers) != 1 {
		t.Fatalf("want one new run frontier, got %v (%v)", runFrontiers, err)
	}
	if err := os.Chmod(runFrontiers[0], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runFrontiers[0], nil, 0o600); err != nil {
		t.Fatal(err)
	}
	damagedRun := "run:" + filepath.Base(filepath.Dir(runFrontiers[0]))[:12] + ":before"
	if _, d := resolveJSON(t, root, damagedRun); d.Outcome != "unavailable" || d.Reason != "metadata-corrupt" {
		t.Errorf("resolve %s with a truncated frontier = %q reason %q, want unavailable metadata-corrupt", damagedRun, d.Outcome, d.Reason)
	}
	if d := compareJSON(t, root, damagedRun+".."+damagedRun); d.Outcome != "unavailable" || d.Reason != "metadata-corrupt" {
		t.Errorf("same damaged run compares %q reason %q, want unavailable metadata-corrupt", d.Outcome, d.Reason)
	}

	recMeta := filepath.Join(root, ".awa", "restores", strings.TrimSuffix(strings.TrimPrefix(predecessorRecovery, "restore:"), ":before"), "meta.json")
	data, err := os.ReadFile(recMeta) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(recMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(data), `"schema_version": 1,`, `"schema_version": 3,`+"\n  \"future\": true,", 1)
	if err := os.WriteFile(recMeta, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"changes", predecessorRecovery + "..now"},
		{"diff", predecessorRecovery + "..now"},
		{"restore", predecessorRecovery, "--all"},
	} {
		if code, _, stderr := awa(t, root, args...); code != 5 {
			t.Errorf("%v on an unsupported recovery generation exit = %d (stderr %q), want 5", args, code, stderr)
		}
	}

	_, stdout, _ := awa(t, root, "doctor", "--json")
	for _, want := range []string{"checkpoint-frontier-corrupt", "restore-recovery-incompatible-format", "before-observation exclusion frontier is corrupt"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor does not report %s:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "restore-recovery-corrupt") || strings.Contains(stdout, "checkpoint-incompatible") {
		t.Errorf("doctor misclassifies damage or an unsupported generation:\n%s", stdout)
	}
	gcCode, gcOut, _ := awa(t, root, "gc", "--dry-run", "--json")
	if gcCode != 5 || !strings.Contains(gcOut, `"restore-incompatible"`) || strings.Contains(gcOut, `"restore-corrupt"`) {
		t.Errorf("gc exit = %d, want state-action-required retaining restore-incompatible:\n%s", gcCode, gcOut)
	}

	writeAt(t, root, "run-made.txt", "edited\n")
	code, _, stderr := awa(t, root, "restore", "--json", damaged, "--all")
	if code == 0 || !strings.Contains(stderr, "frontier") {
		t.Errorf("restore from damaged proof exit = %d stderr = %q, want a corruption failure naming the frontier", code, stderr)
	}
}

func TestAbsenceCreateBelowAHiddenFileIsBlockedInPreview(t *testing.T) {
	root := initProject(t)
	writeAt(t, root, ".awaignore", "# none\n")
	writeAt(t, root, "d/x", "reviewed\n")
	id := checkpointID(t, root)
	if err := os.RemoveAll(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	writeAt(t, root, "d", "hidden file\n")
	writeAt(t, root, ".awaignore", "/d\n")

	_, preview := restoreJSON(t, root, id, "--", "d/x")
	if preview.Data.Counts.Create != 0 || preview.Data.Counts.Blocked != 1 {
		t.Fatalf("preview counts = %+v, want the create blocked", preview.Data.Counts)
	}
	if got := failureReasons(preview, "d/x"); !slices.Equal(got, []string{"ignored-boundary"}) {
		t.Errorf("reasons = %v, want ignored-boundary", got)
	}
	code, applied := restoreJSON(t, root, "--apply", id, "--", "d/x")
	if code == 0 || applied.Data.Outcome != "refused" {
		t.Errorf("apply exit = %d outcome = %q, want a refusal before any write", code, applied.Data.Outcome)
	}
	if got := readAt(t, root, "d"); got != "hidden file\n" {
		t.Errorf("hidden file = %q, want it untouched", got)
	}
}
