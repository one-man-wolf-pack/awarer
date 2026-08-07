package restore

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// --- fixtures -------------------------------------------------------------

func hashOf(t *testing.T, seed byte) hashing.ContentHash {
	t.Helper()
	h, err := hashing.ParseContentHash("blake3:" + strings.Repeat(string("0123456789abcdef"[seed%16]), 64))
	if err != nil {
		t.Fatalf("parse content hash: %v", err)
	}
	return h
}

func relPath(t *testing.T, p string) worktree.RelPath {
	t.Helper()
	rp, err := worktree.ParseRelPath(p)
	if err != nil {
		t.Fatalf("parse rel path %q: %v", p, err)
	}
	return rp
}

func regular(t *testing.T, seed byte, mode uint32) NodeState {
	t.Helper()
	n, err := RegularNode(hashOf(t, seed), mode)
	if err != nil {
		t.Fatalf("regular node: %v", err)
	}
	return n
}

func symlink(t *testing.T, target string) NodeState {
	t.Helper()
	tgt, err := worktree.NewSymlinkTarget(target)
	if err != nil {
		t.Fatalf("symlink target: %v", err)
	}
	n, err := SymlinkNode(tgt)
	if err != nil {
		t.Fatalf("symlink node: %v", err)
	}
	return n
}

func testSource(t *testing.T) Source {
	t.Helper()
	s, err := NewSource(SourceCheckpoint, "gmqd3dbpvs42abcd", "gmqd3dbpvs42abcd", "latest")
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	return s
}

func testSelection(t *testing.T) Selection {
	t.Helper()
	sel, err := NewPathSelection([]worktree.RelPath{relPath(t, "generated/client")})
	if err != nil {
		t.Fatalf("selection: %v", err)
	}
	return sel
}

// --- Source ---------------------------------------------------------------

func TestNewSourceRejectsUnresolvedAndIncompleteIdentities(t *testing.T) {
	cases := []struct {
		name                     string
		kind                     SourceKind
		id, canonical, requested string
	}{
		{name: "unknown kind", kind: SourceKind(99), id: "abc", canonical: "abc", requested: "abc"},
		{name: "empty id", kind: SourceCheckpoint, id: "", canonical: "abc", requested: "abc"},
		{name: "blank id", kind: SourceCheckpoint, id: "   ", canonical: "abc", requested: "abc"},
		{name: "empty canonical ref", kind: SourceCheckpoint, id: "abc", canonical: "", requested: "abc"},
		{name: "id still means latest", kind: SourceCheckpoint, id: "latest", canonical: "abc", requested: "latest"},
		{name: "id still means now", kind: SourceCheckpoint, id: "now", canonical: "abc", requested: "now"},
		{name: "id still positional", kind: SourceCheckpoint, id: "@-2", canonical: "abc", requested: "@-2"},
		{name: "canonical ref still means latest", kind: SourceCheckpoint, id: "abc", canonical: "latest", requested: "latest"},
		{name: "canonical ref still positional", kind: SourceCheckpoint, id: "abc", canonical: "@-2", requested: "@-2"},
		{name: "no requested expression", kind: SourceCheckpoint, id: "abc", canonical: "abc", requested: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSource(tc.kind, tc.id, tc.canonical, tc.requested); err == nil {
				t.Fatalf("NewSource(%v, %q, %q, %q) succeeded, want rejection", tc.kind, tc.id, tc.canonical, tc.requested)
			}
		})
	}
}

func TestNewSourceKeepsRequestedExpressionSeparateFromIdentity(t *testing.T) {
	// The whole point of resolving once: a preview that says "latest" must also
	// name the immutable id an apply will act on, even if latest moves after.
	s, err := NewSource(SourceCheckpoint, "gmqd3dbpvs42abcd", "gmqd3dbpvs42abcd", "latest")
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if s.Requested() != "latest" {
		t.Errorf("Requested() = %q, want latest", s.Requested())
	}
	if s.ID() != "gmqd3dbpvs42abcd" {
		t.Errorf("ID() = %q, want the resolved id", s.ID())
	}
	if s.IsZero() {
		t.Error("a built source reports IsZero")
	}
}

func TestSourceKindTokensAreStable(t *testing.T) {
	want := map[SourceKind]string{
		SourceCheckpoint:          "checkpoint",
		SourceRunObservation:      "run-observation",
		SourceRecoveryObservation: "recovery-observation",
	}
	for k, tok := range want {
		if got := k.String(); got != tok {
			t.Errorf("SourceKind(%d).String() = %q, want %q", k, got, tok)
		}
		if !k.Valid() {
			t.Errorf("SourceKind %q is not Valid", tok)
		}
	}
	if SourceKind(0).Valid() || SourceKind(99).Valid() {
		t.Error("zero/unknown source kinds must not be Valid")
	}
}

// --- Selection ------------------------------------------------------------

func TestSelectionCannotBeNeitherOrBoth(t *testing.T) {
	if _, err := NewPathSelection(nil); err == nil {
		t.Error("an empty path selection was accepted; an omitted selection must never mean all")
	}
	if _, err := NewPathSelection([]worktree.RelPath{{}}); err == nil {
		t.Error("a selection containing an empty path was accepted")
	}
	var zero Selection
	if !zero.IsZero() {
		t.Error("the zero Selection must report IsZero: it is neither all nor a path set")
	}
	all := NewAllSelection()
	if all.IsZero() || !all.All() || len(all.Paths()) != 0 {
		t.Errorf("NewAllSelection = %+v, want all with no paths", all)
	}
	paths, err := NewPathSelection([]worktree.RelPath{relPath(t, "src")})
	if err != nil {
		t.Fatalf("NewPathSelection: %v", err)
	}
	if paths.All() {
		t.Error("a path selection must not report All: that is the both-at-once state")
	}
}

func TestSelectionPathsCannotBeMutatedThroughTheAccessor(t *testing.T) {
	original := relPath(t, "generated/client")
	sel, err := NewPathSelection([]worktree.RelPath{original})
	if err != nil {
		t.Fatalf("NewPathSelection: %v", err)
	}
	got := sel.Paths()
	got[0] = relPath(t, "src/secret")
	if sel.Paths()[0].String() != "generated/client" {
		t.Errorf("mutating the returned slice changed the validated selection: %v", sel.Paths())
	}
	// The input slice must not alias the stored one either.
	input := []worktree.RelPath{original}
	sel2, err := NewPathSelection(input)
	if err != nil {
		t.Fatalf("NewPathSelection: %v", err)
	}
	input[0] = relPath(t, "src/secret")
	if sel2.Paths()[0].String() != "generated/client" {
		t.Errorf("mutating the input slice changed the validated selection: %v", sel2.Paths())
	}
}

func TestSelectionCoversPathAndDescendantsOnly(t *testing.T) {
	sel, err := NewPathSelection([]worktree.RelPath{relPath(t, "src/api")})
	if err != nil {
		t.Fatalf("NewPathSelection: %v", err)
	}
	covered := []string{"src/api", "src/api/handler.go", "src/api/v1/x.go"}
	for _, p := range covered {
		if !sel.Covers(relPath(t, p)) {
			t.Errorf("Covers(%q) = false, want true", p)
		}
	}
	// "src/apiv2" shares a textual prefix but is a sibling, not a descendant.
	notCovered := []string{"src/apiv2", "src/apiv2/x.go", "src", "other/api"}
	for _, p := range notCovered {
		if sel.Covers(relPath(t, p)) {
			t.Errorf("Covers(%q) = true, want false", p)
		}
	}
	if !NewAllSelection().Covers(relPath(t, "anything/at/all")) {
		t.Error("--all must cover every path")
	}
}

// --- NodeState ------------------------------------------------------------

func TestNodeConstructorsRejectImpossibleShapes(t *testing.T) {
	if _, err := RegularNode(hashing.ContentHash{}, 0o644); err == nil {
		t.Error("a regular node with no content hash was accepted")
	}
	if _, err := SymlinkNode(worktree.SymlinkTarget{}); err == nil {
		t.Error("a symlink node with no target was accepted")
	}
	if AbsentNode().Present() {
		t.Error("AbsentNode reports Present")
	}
	if !DirNode().Present() || DirNode().Kind() != worktree.KindDir {
		t.Error("DirNode is not a present directory")
	}
}

func TestNodeFromEntryRefusesUnsupportedKinds(t *testing.T) {
	// A special file is never an Entry in production, but the projection is the
	// boundary a hostile or future record would arrive through: it must refuse
	// rather than approximate.
	e := worktree.Entry{Path: relPath(t, "dev/null"), Kind: worktree.KindSpecial}
	if _, err := NodeFromEntry(e); err == nil {
		t.Error("NodeFromEntry accepted a special file; restore must refuse unsupported kinds")
	}
}

func TestNodeEqualityMirrorsTreeIdentity(t *testing.T) {
	a := regular(t, 1, 0o644)
	same := regular(t, 1, 0o644)
	otherContent := regular(t, 2, 0o644)
	otherMode := regular(t, 1, 0o755)

	if !a.Equal(same) {
		t.Error("identical regular nodes compare unequal")
	}
	if a.Equal(otherContent) {
		t.Error("different content compares equal")
	}
	if a.Equal(otherMode) {
		t.Error("different permission bits compare equal: a mode change is a real change")
	}
	if a.Equal(AbsentNode()) || AbsentNode().Equal(a) {
		t.Error("present and absent compare equal")
	}
	if !AbsentNode().Equal(AbsentNode()) {
		t.Error("two absences compare unequal")
	}
	if a.Equal(DirNode()) {
		t.Error("a file and a directory compare equal")
	}
	if !symlink(t, "../target").Equal(symlink(t, "../target")) {
		t.Error("identical symlink targets compare unequal")
	}
	if symlink(t, "../target").Equal(symlink(t, "../other")) {
		t.Error("different symlink targets compare equal")
	}
}

// --- operation kind derivation -------------------------------------------

func TestOperationKindIsDerivedFromProvedStates(t *testing.T) {
	cases := []struct {
		name    string
		current NodeState
		desired NodeState
		want    OperationKind
	}{
		{"create a file", AbsentNode(), regular(t, 1, 0o644), OpCreate},
		{"create a directory", AbsentNode(), DirNode(), OpCreate},
		{"create a symlink", AbsentNode(), symlink(t, "../a"), OpRestoreSymlink},
		{"replace a file", regular(t, 2, 0o644), regular(t, 1, 0o644), OpReplace},
		{"replace a mode", regular(t, 1, 0o755), regular(t, 1, 0o644), OpReplace},
		{"replace a symlink", symlink(t, "../b"), symlink(t, "../a"), OpRestoreSymlink},
		{"file becomes directory", regular(t, 1, 0o644), DirNode(), OpTypeChange},
		{"directory becomes file", DirNode(), regular(t, 1, 0o644), OpTypeChange},
		{"symlink becomes file", symlink(t, "../a"), regular(t, 1, 0o644), OpTypeChange},
		{"delete a file", regular(t, 1, 0o644), AbsentNode(), OpDeleteFile},
		{"delete a symlink", symlink(t, "../a"), AbsentNode(), OpDeleteFile},
		{"delete a directory", DirNode(), AbsentNode(), OpDeleteEmptyDirectory},
		{"equal files", regular(t, 1, 0o644), regular(t, 1, 0o644), OpEqual},
		{"equal directories", DirNode(), DirNode(), OpEqual},
		{"equal symlinks", symlink(t, "../a"), symlink(t, "../a"), OpEqual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, err := NewPlannedOperation(relPath(t, "p/x"), tc.current, tc.desired)
			if err != nil {
				t.Fatalf("NewPlannedOperation: %v", err)
			}
			if op.Kind() != tc.want {
				t.Errorf("kind = %s, want %s", op.Kind(), tc.want)
			}
			if op.Availability() != Ready {
				t.Errorf("availability = %s, want ready", op.Availability())
			}
			if len(op.Reasons()) != 0 {
				t.Errorf("a ready operation carries reasons: %v", op.Reasons())
			}
		})
	}
}

func TestPlannedOperationRejectsImpossibleInputs(t *testing.T) {
	if _, err := NewPlannedOperation(worktree.RelPath{}, AbsentNode(), regular(t, 1, 0o644)); err == nil {
		t.Error("an operation with no path was accepted")
	}
	if _, err := NewPlannedOperation(relPath(t, "p/x"), AbsentNode(), AbsentNode()); err == nil {
		t.Error("an operation with neither a current nor a desired state was accepted")
	}
}

func TestBlockedOperationRequiresAnAllowedReason(t *testing.T) {
	p := relPath(t, "p/x")
	if _, err := NewBlockedOperation(p, AbsentNode(), regular(t, 1, 0o644)); err == nil {
		t.Error("a blocked operation with no reason was accepted; a silent refusal is exactly what restore removes")
	}
	if _, err := NewBlockedOperation(p, AbsentNode(), regular(t, 1, 0o644), Reason("invented")); err == nil {
		t.Error("a reason outside the closed vocabulary was accepted")
	}
	// An apply-time fact explains an outcome, never a planned operation: planning
	// happens before the first mutation and cannot observe a destination moving.
	for _, r := range []Reason{ReasonPreconditionMismatch, ReasonIOFailure, ReasonCancelled, ReasonPartialApply, ReasonRecoveryIncomplete} {
		if _, err := NewBlockedOperation(p, regular(t, 2, 0o644), regular(t, 1, 0o644), r); err == nil {
			t.Errorf("apply-time reason %q was accepted for a blocked operation", r)
		}
	}
	op, err := NewBlockedOperation(p, AbsentNode(), regular(t, 1, 0o644), ReasonHashOnlyContent)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	if op.Availability() != Blocked || len(op.Reasons()) != 1 || op.Reasons()[0] != ReasonHashOnlyContent {
		t.Errorf("blocked operation = %+v, want blocked with hash-only-content", op)
	}
}

func TestBlockedOperationReasonsCannotBeMutatedThroughTheAccessor(t *testing.T) {
	op, err := NewBlockedOperation(relPath(t, "p/x"), AbsentNode(), regular(t, 1, 0o644), ReasonBlobMissing)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	got := op.Reasons()
	got[0] = ReasonCancelled
	if op.Reasons()[0] != ReasonBlobMissing {
		t.Errorf("mutating the returned slice changed the validated operation: %v", op.Reasons())
	}
}

func TestRequiresContentOnlyForMutatingRegularWrites(t *testing.T) {
	cases := []struct {
		name    string
		current NodeState
		desired NodeState
		want    bool
	}{
		{"create a file", AbsentNode(), regular(t, 1, 0o644), true},
		{"replace a file", regular(t, 2, 0o644), regular(t, 1, 0o644), true},
		{"directory becomes file", DirNode(), regular(t, 1, 0o644), true},
		{"create a symlink", AbsentNode(), symlink(t, "../a"), false},
		{"create a directory", AbsentNode(), DirNode(), false},
		{"delete a file", regular(t, 1, 0o644), AbsentNode(), false},
		{"equal files", regular(t, 1, 0o644), regular(t, 1, 0o644), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, err := NewPlannedOperation(relPath(t, "p/x"), tc.current, tc.desired)
			if err != nil {
				t.Fatalf("NewPlannedOperation: %v", err)
			}
			if got := op.RequiresContent(); got != tc.want {
				t.Errorf("RequiresContent() = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- staging capability ---------------------------------------------------

func TestNewStagedPayloadRejectsIncompleteCapabilities(t *testing.T) {
	if _, err := NewStagedPayload(hashing.ContentHash{}, 1, "h"); err == nil {
		t.Error("a staged payload with no content hash was accepted")
	}
	if _, err := NewStagedPayload(hashOf(t, 1), -1, "h"); err == nil {
		t.Error("a staged payload with negative size was accepted")
	}
	if _, err := NewStagedPayload(hashOf(t, 1), 1, ""); err == nil {
		t.Error("a staged payload with no staging handle was accepted; a hash alone is not staged bytes")
	}
	if !(StagedPayload{}).IsZero() {
		t.Error("the zero StagedPayload must report IsZero")
	}
}

func TestStagedOperationCannotWriteWithoutVerifiedContent(t *testing.T) {
	p := relPath(t, "generated/client/openapi.json")
	desired := regular(t, 1, 0o644)
	ready, err := NewPlannedOperation(p, AbsentNode(), desired)
	if err != nil {
		t.Fatalf("NewPlannedOperation: %v", err)
	}

	// The central invariant: a ready regular-file write with no staged bytes is
	// not a value the executor can be handed.
	if _, err := NewStagedOperation(ready, StagedPayload{}); err == nil {
		t.Fatal("a regular-file write was staged with no verified content")
	}

	// Bytes staged for a different file cannot be attached to this one.
	wrong, err := NewStagedPayload(hashOf(t, 2), 10, "stage/aaa")
	if err != nil {
		t.Fatalf("NewStagedPayload: %v", err)
	}
	if _, err := NewStagedOperation(ready, wrong); err == nil {
		t.Fatal("staged bytes whose identity differs from the desired content were accepted")
	}

	right, err := NewStagedPayload(desired.Content(), 10, "stage/bbb")
	if err != nil {
		t.Fatalf("NewStagedPayload: %v", err)
	}
	staged, err := NewStagedOperation(ready, right)
	if err != nil {
		t.Fatalf("NewStagedOperation: %v", err)
	}
	if staged.Payload().Content() != desired.Content() {
		t.Error("staged payload identity does not match the desired content")
	}
}

func TestStagedOperationRejectsPayloadForContentFreeWork(t *testing.T) {
	del, err := NewPlannedOperation(relPath(t, "p/x"), regular(t, 1, 0o644), AbsentNode())
	if err != nil {
		t.Fatalf("NewPlannedOperation: %v", err)
	}
	payload, err := NewStagedPayload(hashOf(t, 1), 3, "stage/ccc")
	if err != nil {
		t.Fatalf("NewStagedPayload: %v", err)
	}
	if _, err := NewStagedOperation(del, payload); err == nil {
		t.Error("a deletion was given staged content")
	}
	if _, err := NewStagedOperation(del, StagedPayload{}); err != nil {
		t.Errorf("a deletion with no payload was rejected: %v", err)
	}
}

func TestStagedOperationRejectsUnavailableWork(t *testing.T) {
	blocked, err := NewBlockedOperation(relPath(t, "p/x"), AbsentNode(), regular(t, 1, 0o644), ReasonHashOnlyContent)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	if _, err := NewStagedOperation(blocked, StagedPayload{}); err == nil {
		t.Error("a blocked operation was staged for execution")
	}
	if _, err := NewStagedOperation(PlannedOperation{}, StagedPayload{}); err == nil {
		t.Error("an unbuilt operation was staged for execution")
	}
}

// --- apply order ----------------------------------------------------------

// TestApplyOrderIsDeterministicAndDependencySafe drives Rank and PathDepth exactly
// the way the executor does — one pass per rank, then one descending pass per
// directory depth — rather than sorting a materialized set. That is deliberate: a
// comparator the executor does not call could drift from the real commit order
// without any test noticing, and the whole point of the rank/depth pair is that no
// operation set is ever sorted.
func TestApplyOrderIsDeterministicAndDependencySafe(t *testing.T) {
	mk := func(path string, current, desired NodeState) PlannedOperation {
		t.Helper()
		p, err := NewPlannedOperation(relPath(t, path), current, desired)
		if err != nil {
			t.Fatalf("NewPlannedOperation(%s): %v", path, err)
		}
		return p
	}

	// Spooled in canonical ascending path order, which is the only order the
	// executor's cursor ever yields.
	//
	// "dir" is the case the order exists for: a directory the source proves is a
	// file. Emptying it is what the child deletions below do, so the replacement can
	// only run after them — and it sorts FIRST by path, so an order derived from the
	// operation kind alone would run it first and fail on a directory its own plan
	// was about to empty.
	ops := []PlannedOperation{
		mk("a", DirNode(), AbsentNode()),           // delete shallow dir
		mk("a/b", DirNode(), AbsentNode()),         // delete mid dir
		mk("a/b/c", DirNode(), AbsentNode()),       // delete deep dir
		mk("dir", DirNode(), regular(t, 3, 0o644)), // type change over a directory
		mk("dir/child.txt", regular(t, 4, 0o644), AbsentNode()),
		mk("m/new.txt", AbsentNode(), regular(t, 2, 0o644)),  // create
		mk("z/file.txt", regular(t, 1, 0o644), AbsentNode()), // delete file
	}

	maxDepth := 0
	for _, o := range ops {
		if o.Rank() == RankDeleteDirectory && o.PathDepth() > maxDepth {
			maxDepth = o.PathDepth()
		}
	}

	type pass struct{ rank, depth int }
	passes := []pass{{RankWrite, 0}, {RankDeleteFile, 0}}
	for d := maxDepth; d >= 1; d-- {
		passes = append(passes, pass{RankDeleteDirectory, d})
	}
	passes = append(passes, pass{RankReplaceDirectory, 0})

	var got []string
	for _, p := range passes {
		for _, o := range ops {
			if o.Rank() != p.rank {
				continue
			}
			if p.depth > 0 && o.PathDepth() != p.depth {
				continue
			}
			got = append(got, o.Path().String())
		}
	}

	want := []string{
		"m/new.txt",                   // writes
		"dir/child.txt", "z/file.txt", // file deletions
		"a/b/c", "a/b", "a", // directory deletions, deepest first
		"dir", // and only now the directory replacement
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("apply order = %v, want %v (writes, file deletions, directories deepest-first, then directory replacements)", got, want)
	}
}

// TestTypeChangeRankFollowsTheProvedShapes pins the rule the order above depends
// on: a type change is a first-phase write when it puts something where a file or a
// link used to be, and a last-phase directory replacement when the destination is
// currently a directory. Deriving the rank from the kind would collapse the two.
func TestTypeChangeRankFollowsTheProvedShapes(t *testing.T) {
	cases := []struct {
		name             string
		current, desired NodeState
		want             int
	}{
		{"file becomes a directory", regular(t, 1, 0o644), DirNode(), RankWrite},
		{"file becomes a symlink", regular(t, 1, 0o644), symlink(t, "elsewhere"), RankWrite},
		{"symlink becomes a file", symlink(t, "elsewhere"), regular(t, 1, 0o644), RankWrite},
		{"directory becomes a file", DirNode(), regular(t, 1, 0o644), RankReplaceDirectory},
		{"directory becomes a symlink", DirNode(), symlink(t, "elsewhere"), RankReplaceDirectory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op, err := NewPlannedOperation(relPath(t, "thing"), tc.current, tc.desired)
			if err != nil {
				t.Fatalf("NewPlannedOperation: %v", err)
			}
			if op.Kind() != OpTypeChange {
				t.Fatalf("kind = %s, want type-change", op.Kind())
			}
			if got := op.Rank(); got != tc.want {
				t.Errorf("rank = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- counts and plan ------------------------------------------------------

func TestCountsFoldEveryKindAndAvailability(t *testing.T) {
	var c Counts
	add := func(path string, current, desired NodeState) {
		t.Helper()
		op, err := NewPlannedOperation(relPath(t, path), current, desired)
		if err != nil {
			t.Fatalf("NewPlannedOperation: %v", err)
		}
		c.Add(op)
	}
	add("a", AbsentNode(), regular(t, 1, 0o644))         // create
	add("b", regular(t, 2, 0o644), regular(t, 1, 0o644)) // replace
	add("c", DirNode(), regular(t, 1, 0o644))            // type change
	add("d", AbsentNode(), symlink(t, "../x"))           // symlink
	add("e", regular(t, 1, 0o644), AbsentNode())         // delete file
	add("f", DirNode(), AbsentNode())                    // delete dir
	add("g", regular(t, 1, 0o644), regular(t, 1, 0o644)) // equal
	blocked, err := NewBlockedOperation(relPath(t, "h"), AbsentNode(), regular(t, 1, 0o644), ReasonHashOnlyContent)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	c.Add(blocked)

	want := Counts{Create: 1, Replace: 1, TypeChange: 1, Symlink: 1, DeleteFile: 1, DeleteDirectory: 1, Equal: 1, Blocked: 1}
	if c != want {
		t.Fatalf("counts = %+v, want %+v", c, want)
	}
	if c.Delete() != 2 {
		t.Errorf("Delete() = %d, want 2", c.Delete())
	}
	if c.Mutating() != 6 {
		t.Errorf("Mutating() = %d, want 6", c.Mutating())
	}
	if c.Unavailable() != 1 {
		t.Errorf("Unavailable() = %d, want 1", c.Unavailable())
	}
	if c.Total() != 8 {
		t.Errorf("Total() = %d, want 8", c.Total())
	}
}

func TestNewPlanRejectsCompletePlusUnavailable(t *testing.T) {
	src, sel := testSource(t), testSelection(t)
	blocked, err := NewBlockedOperation(relPath(t, "h"), AbsentNode(), regular(t, 1, 0o644), ReasonHashOnlyContent)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	sample, err := NewBlockedSample(blocked)
	if err != nil {
		t.Fatalf("NewBlockedSample: %v", err)
	}
	counts := Counts{Blocked: 1}

	if _, err := NewPlan(src, sel, counts, Boundary{}, []BlockedSample{sample}, true); err == nil {
		t.Error("a plan claiming completeness while holding a blocked operation was accepted")
	}
	if _, err := NewPlan(src, sel, counts, Boundary{}, nil, false); err == nil {
		t.Error("a plan with a blocked count and no explanatory sample was accepted")
	}
	if _, err := NewPlan(Source{}, sel, Counts{}, Boundary{}, nil, true); err == nil {
		t.Error("a plan with no source identity was accepted")
	}
	if _, err := NewPlan(src, Selection{}, Counts{}, Boundary{}, nil, true); err == nil {
		t.Error("a plan with an unbuilt selection was accepted")
	}
	over := make([]BlockedSample, MaxBlockedSamples+1)
	for i := range over {
		over[i] = sample
	}
	if _, err := NewPlan(src, sel, Counts{Blocked: len(over)}, Boundary{}, over, false); err == nil {
		t.Errorf("a plan carrying more than %d samples was accepted; the bound is the point", MaxBlockedSamples)
	}
}

func TestNewBlockedSampleRejectsReadyOperations(t *testing.T) {
	ready, err := NewPlannedOperation(relPath(t, "p/x"), AbsentNode(), regular(t, 1, 0o644))
	if err != nil {
		t.Fatalf("NewPlannedOperation: %v", err)
	}
	if _, err := NewBlockedSample(ready); err == nil {
		t.Error("a ready operation was accepted as a blocked sample")
	}
}

func TestPlanSamplesCannotBeMutatedThroughTheAccessor(t *testing.T) {
	blocked, err := NewBlockedOperation(relPath(t, "h"), AbsentNode(), regular(t, 1, 0o644), ReasonBlobMissing)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	sample, err := NewBlockedSample(blocked)
	if err != nil {
		t.Fatalf("NewBlockedSample: %v", err)
	}
	plan, err := NewPlan(testSource(t), testSelection(t), Counts{Blocked: 1}, Boundary{}, []BlockedSample{sample}, false)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	got := plan.BlockedSamples()
	got[0].Path = relPath(t, "elsewhere")
	got[0].Reasons[0] = ReasonCancelled
	after := plan.BlockedSamples()
	if after[0].Path.String() != "h" || after[0].Reasons[0] != ReasonBlobMissing {
		t.Errorf("mutating the returned samples changed the validated plan: %+v", after)
	}
}

func TestPlanApplicabilityDistinguishesNoOpFromWork(t *testing.T) {
	src, sel := testSource(t), testSelection(t)
	empty, err := NewPlan(src, sel, Counts{Equal: 3}, Boundary{PolicyCompatible: true}, nil, true)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if !empty.Complete() {
		t.Error("an all-equal plan is not complete")
	}
	if empty.Applicable() {
		t.Error("an all-equal plan reports Applicable; it is a no-op")
	}
	work, err := NewPlan(src, sel, Counts{Replace: 1}, Boundary{PolicyCompatible: true}, nil, true)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if !work.Applicable() {
		t.Error("a complete plan with mutating work is not Applicable")
	}
}

func TestBoundaryIgnoredOutsideEvidenceIsConstant(t *testing.T) {
	// It is a method, not a field, so no code path can report that ignored paths
	// are inside awa evidence.
	if !(Boundary{}).IgnoredOutsideEvidence() {
		t.Error("IgnoredOutsideEvidence must always be true")
	}
}

// --- outcomes -------------------------------------------------------------

func TestApplyResultRejectsDishonestCombinations(t *testing.T) {
	src, sel := testSource(t), testSelection(t)
	id, err := NewOperationID(1, bytes.NewReader(bytes.Repeat([]byte{7}, 8)))
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}

	cases := []struct {
		name string
		in   ResultInput
	}{
		{"no source", ResultInput{Selection: sel, Outcome: OutcomePreview}},
		{"no selection", ResultInput{Source: src, Outcome: OutcomePreview}},
		{"unknown outcome", ResultInput{Source: src, Selection: sel, Outcome: Outcome(99)}},
		{"preview claiming mutations", ResultInput{Source: src, Selection: sel, Outcome: OutcomePreview, Completed: 1}},
		{"preview carrying a recovery observation", ResultInput{Source: src, Selection: sel, Outcome: OutcomePreview, Recovery: id}},
		{"applied with remaining work", ResultInput{Source: src, Selection: sel, Outcome: OutcomeApplied, Completed: 2, Remaining: 1, Recovery: id}},
		{"applied with nothing completed", ResultInput{Source: src, Selection: sel, Outcome: OutcomeApplied, Recovery: id}},
		{"applied with no recovery observation", ResultInput{Source: src, Selection: sel, Outcome: OutcomeApplied, Completed: 1}},
		{"partial with nothing completed and no incomplete mutation", ResultInput{Source: src, Selection: sel, Outcome: OutcomePartial, Remaining: 1, Recovery: id}},
		// The four outcomes below all promise the worktree was not touched, so an
		// incomplete mutation may not hide behind one of them: that is exactly the
		// machine-readable lie an applier's reported effect exists to prevent.
		{"conflict hiding an incomplete mutation", ResultInput{Source: src, Selection: sel, Outcome: OutcomeConflict, Remaining: 1, IncompleteMutation: true}},
		{"cancelled hiding an incomplete mutation", ResultInput{Source: src, Selection: sel, Outcome: OutcomeCancelled, Remaining: 1, IncompleteMutation: true}},
		{"refused hiding an incomplete mutation", ResultInput{Source: src, Selection: sel, Outcome: OutcomeRefused, Reasons: []Reason{ReasonIOFailure}, IncompleteMutation: true}},
		{"no-op hiding an incomplete mutation", ResultInput{Source: src, Selection: sel, Outcome: OutcomeNoOp, IncompleteMutation: true}},
		{"partial with an incomplete mutation but no recovery observation", ResultInput{Source: src, Selection: sel, Outcome: OutcomePartial, Remaining: 1, IncompleteMutation: true}},
		{"partial with nothing remaining", ResultInput{Source: src, Selection: sel, Outcome: OutcomePartial, Completed: 1, Recovery: id}},
		{"partial with no recovery observation", ResultInput{Source: src, Selection: sel, Outcome: OutcomePartial, Completed: 1, Remaining: 1}},
		{"conflict claiming completed work", ResultInput{Source: src, Selection: sel, Outcome: OutcomeConflict, Completed: 1}},
		{"cancelled claiming completed work", ResultInput{Source: src, Selection: sel, Outcome: OutcomeCancelled, Completed: 1}},
		{"refused with no explanation", ResultInput{Source: src, Selection: sel, Outcome: OutcomeRefused}},
		{"negative counts", ResultInput{Source: src, Selection: sel, Outcome: OutcomePreview, Remaining: -1}},
		{"invented reason", ResultInput{Source: src, Selection: sel, Outcome: OutcomeRefused, Reasons: []Reason{"invented"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewApplyResult(tc.in); err == nil {
				t.Fatalf("NewApplyResult accepted %s", tc.name)
			}
		})
	}
}

func TestApplyResultAcceptsHonestCombinations(t *testing.T) {
	src, sel := testSource(t), testSelection(t)
	id, err := NewOperationID(1, bytes.NewReader(bytes.Repeat([]byte{7}, 8)))
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}
	cases := []struct {
		name string
		in   ResultInput
	}{
		{"preview", ResultInput{Source: src, Selection: sel, Outcome: OutcomePreview, Counts: Counts{Replace: 1}}},
		{"applied", ResultInput{Source: src, Selection: sel, Outcome: OutcomeApplied, Completed: 3, Recovery: id}},
		{"no-op", ResultInput{Source: src, Selection: sel, Outcome: OutcomeNoOp, Counts: Counts{Equal: 4}}},
		{"partial", ResultInput{Source: src, Selection: sel, Outcome: OutcomePartial, Completed: 1, Remaining: 2, Recovery: id, Reasons: []Reason{ReasonPartialApply}}},
		// An install that removed the old node and then failed: nothing completed, but the
		// worktree moved. Partial is the only honest name for it, and it still carries the
		// recovery observation the change can be undone from.
		{"partial with only an incomplete mutation", ResultInput{Source: src, Selection: sel, Outcome: OutcomePartial, Remaining: 1, Recovery: id, IncompleteMutation: true, Reasons: []Reason{ReasonPartialApply}}},
		{"refused with a reason", ResultInput{Source: src, Selection: sel, Outcome: OutcomeRefused, Reasons: []Reason{ReasonRecoveryIncomplete}}},
		{"conflict", ResultInput{Source: src, Selection: sel, Outcome: OutcomeConflict, Remaining: 2}},
		{"cancelled", ResultInput{Source: src, Selection: sel, Outcome: OutcomeCancelled, Reasons: []Reason{ReasonCancelled}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := NewApplyResult(tc.in)
			if err != nil {
				t.Fatalf("NewApplyResult(%s): %v", tc.name, err)
			}
			if res.IsZero() {
				t.Error("a built result reports IsZero")
			}
			if res.Outcome().Mutated() && res.Recovery().IsZero() {
				t.Error("a mutating outcome has no recovery observation")
			}
		})
	}
}

func TestPreviewResultDerivesEverythingFromThePlan(t *testing.T) {
	plan, err := NewPlan(testSource(t), testSelection(t), Counts{Replace: 2, Equal: 1}, Boundary{PolicyCompatible: true}, nil, true)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	res, err := PreviewResult(plan)
	if err != nil {
		t.Fatalf("PreviewResult: %v", err)
	}
	if res.Outcome() != OutcomePreview {
		t.Errorf("outcome = %s, want preview", res.Outcome())
	}
	if res.Completed() != 0 || !res.Recovery().IsZero() {
		t.Error("a preview reported mutations or a recovery observation")
	}
	if res.Counts() != plan.Counts() {
		t.Errorf("preview counts %+v differ from the plan's %+v", res.Counts(), plan.Counts())
	}
	if _, err := PreviewResult(Plan{}); err == nil {
		t.Error("PreviewResult accepted an unbuilt plan")
	}
}

func TestApplyResultFailuresCannotBeMutatedThroughTheAccessor(t *testing.T) {
	f, err := NewFailure(relPath(t, "p/x"), OpReplace, ReasonIOFailure)
	if err != nil {
		t.Fatalf("NewFailure: %v", err)
	}
	id, err := NewOperationID(1, bytes.NewReader(bytes.Repeat([]byte{7}, 8)))
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}
	res, err := NewApplyResult(ResultInput{
		Source: testSource(t), Selection: testSelection(t), Outcome: OutcomePartial,
		Completed: 1, Remaining: 1, Recovery: id, Reasons: []Reason{ReasonPartialApply}, Failures: []Failure{f},
	})
	if err != nil {
		t.Fatalf("NewApplyResult: %v", err)
	}
	got := res.Failures()
	got[0].Path = relPath(t, "elsewhere")
	got[0].Reasons[0] = ReasonCancelled
	rs := res.Reasons()
	rs[0] = ReasonCancelled
	after := res.Failures()
	if after[0].Path.String() != "p/x" || after[0].Reasons[0] != ReasonIOFailure {
		t.Errorf("mutating the returned failures changed the validated result: %+v", after)
	}
	if res.Reasons()[0] != ReasonPartialApply {
		t.Errorf("mutating the returned reasons changed the validated result: %v", res.Reasons())
	}
}

func TestNewFailureRequiresAnExplanation(t *testing.T) {
	if _, err := NewFailure(relPath(t, "p/x"), OpReplace); err == nil {
		t.Error("a failure with no reason was accepted")
	}
	if _, err := NewFailure(worktree.RelPath{}, OpReplace, ReasonIOFailure); err == nil {
		t.Error("a failure with no path was accepted")
	}
	if _, err := NewFailure(relPath(t, "p/x"), OpReplace, Reason("invented")); err == nil {
		t.Error("a failure with a reason outside the closed vocabulary was accepted")
	}
}

// --- vocabulary coverage --------------------------------------------------

func TestReasonVocabularyIsClosedAndStable(t *testing.T) {
	// The token list is a published contract: agents branch on these strings. A
	// change here must be a deliberate contract change, not a rename side effect.
	want := []Reason{
		ReasonHashOnlyContent, ReasonBlobMissing, ReasonBlobCorrupt, ReasonSkippedBoundary,
		ReasonIgnoredBoundary, ReasonPolicyIncompatible, ReasonObservationUnstable, ReasonOutOfProvenScope,
		ReasonPathConflict, ReasonRootEscape, ReasonSymlinkAncestor, ReasonUnsupportedEntryKind,
		ReasonPreconditionMismatch,
		ReasonRecoveryIncomplete, ReasonCancelled, ReasonIOFailure, ReasonPartialApply,
	}
	got := Reasons()
	if len(got) != len(want) {
		t.Fatalf("Reasons() has %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Reasons()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if Reason("not-a-reason").Valid() {
		t.Error("an unknown token reports Valid")
	}
	for _, r := range got {
		if !r.Valid() {
			t.Errorf("catalogued reason %q is not Valid", r)
		}
		if r.String() != string(r) {
			t.Errorf("String() for %q is not the token itself", r)
		}
	}
}

func TestReasonsReturnsACopy(t *testing.T) {
	first := Reasons()
	first[0] = Reason("tampered")
	if Reasons()[0] == Reason("tampered") {
		t.Error("Reasons() exposes the package catalog; a consumer reordered what everyone sees")
	}
}

// TestMutationResultAnswersFromTheEffectNotTheError pins the distinction the
// (effect, error) tuple could not enforce: whether an operation completed is a
// property of what happened to the destination, not of whether the call returned an
// error. A result that reached the desired state and then failed on something else is
// still a completed operation — read from the error it would be counted as a stop, and
// the commit would call a changed worktree a conflict.
func TestMutationResultAnswersFromTheEffectNotTheError(t *testing.T) {
	if got := Completed(); !got.Done() || !got.Changed() || got.Err() != nil {
		t.Errorf("Completed() = %+v, want done, changed, no failure", got)
	}
	boom := errors.New("boom")
	if got := Interrupted(boom); got.Done() || !got.Changed() || !errors.Is(got.Err(), boom) {
		t.Errorf("Interrupted() = %+v, want not done, changed, carrying the failure", got)
	}
	if got := Untouched(boom); got.Done() || got.Changed() || !errors.Is(got.Err(), boom) {
		t.Errorf("Untouched() = %+v, want not done, unchanged, carrying the failure", got)
	}
	// The case the tuple got wrong: a desired state that was reached alongside a
	// failure. Constructed by hand because no constructor produces it — which is the
	// point — and Done must still be true.
	reachedThenFailed := MutationResult{effect: EffectComplete, err: boom}
	if !reachedThenFailed.Done() {
		t.Error("a result whose destination holds the desired state is not reported as completed")
	}
}

func TestMutationResultRejectsUnreadableReports(t *testing.T) {
	if err := Completed().Validate(); err != nil {
		t.Errorf("Completed() is invalid: %v", err)
	}
	if err := Untouched(errors.New("x")).Validate(); err != nil {
		t.Errorf("Untouched(err) is invalid: %v", err)
	}
	cases := map[string]MutationResult{
		// An applier that said nothing at all: the worktree's state is unknown, which must
		// never be read as "nothing happened".
		"unbuilt": {},
		// A failure-shaped effect with no failure to explain it is not a report, it is a
		// contradiction.
		"changed without a failure":   {effect: EffectPartial},
		"untouched without a failure": {effect: EffectNone},
	}
	for name, r := range cases {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate accepted %s", name)
		}
	}
}

func TestOperationKindAndAvailabilityTokensAreStable(t *testing.T) {
	kinds := map[OperationKind]string{
		OpCreate: "create", OpReplace: "replace", OpTypeChange: "type-change",
		OpRestoreSymlink: "restore-symlink", OpDeleteFile: "delete-file",
		OpDeleteEmptyDirectory: "delete-empty-directory", OpEqual: "equal",
	}
	for k, tok := range kinds {
		if k.String() != tok {
			t.Errorf("OperationKind(%d).String() = %q, want %q", k, k.String(), tok)
		}
		if !k.Valid() {
			t.Errorf("kind %q is not Valid", tok)
		}
	}
	if OperationKind(0).Valid() || OperationKind(99).Valid() {
		t.Error("zero/unknown operation kinds must not be Valid")
	}
	if OpEqual.Mutates() {
		t.Error("equal must not report Mutates")
	}
	for k := range kinds {
		if k != OpEqual && !k.Mutates() {
			t.Errorf("%s must report Mutates", k)
		}
	}

	avails := map[Availability]string{Ready: "ready", Blocked: "blocked"}
	for a, tok := range avails {
		if a.String() != tok {
			t.Errorf("Availability(%d).String() = %q, want %q", a, a.String(), tok)
		}
		if !a.Valid() {
			t.Errorf("availability %q is not Valid", tok)
		}
	}
	if Availability(0).Valid() {
		t.Error("the zero availability must not be Valid")
	}
}

func TestOutcomeTokensAreStableAndMutatedIsExact(t *testing.T) {
	tokens := map[Outcome]string{
		OutcomePreview: "preview", OutcomeApplied: "applied", OutcomeNoOp: "no-op",
		OutcomeRefused: "refused", OutcomeConflict: "conflict", OutcomePartial: "partial",
		OutcomeCancelled: "cancelled",
	}
	for o, tok := range tokens {
		if o.String() != tok {
			t.Errorf("Outcome(%d).String() = %q, want %q", o, o.String(), tok)
		}
		if !o.Valid() {
			t.Errorf("outcome %q is not Valid", tok)
		}
	}
	if Outcome(0).Valid() || Outcome(99).Valid() {
		t.Error("zero/unknown outcomes must not be Valid")
	}
	// Exactly the two outcomes that may have written to the worktree.
	for o, tok := range tokens {
		want := tok == "applied" || tok == "partial"
		if o.Mutated() != want {
			t.Errorf("Outcome(%q).Mutated() = %v, want %v", tok, o.Mutated(), want)
		}
	}
}

// --- operation id ---------------------------------------------------------

func TestOperationIDGrammarAndReference(t *testing.T) {
	id, err := NewOperationID(1_700_000_000_000_000_000, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}
	if len(id.String()) != operationIDHexLen {
		t.Fatalf("id %q has length %d, want %d", id, len(id.String()), operationIDHexLen)
	}
	if got := id.BeforeRef(); got != "restore:"+id.String()+":before" {
		t.Errorf("BeforeRef() = %q, want the full-id restore reference", got)
	}
	if (OperationID{}).BeforeRef() != "" {
		t.Error("the zero id produced a reference")
	}
	if len(id.Short()) != operationIDShortLen {
		t.Errorf("Short() = %q, want %d characters", id.Short(), operationIDShortLen)
	}

	parsed, err := ParseOperationID(id.String())
	if err != nil || parsed != id {
		t.Fatalf("ParseOperationID round-trip failed: %v (%v)", err, parsed)
	}
	for _, bad := range []string{"", "short", strings.Repeat("z", operationIDHexLen), strings.ToUpper(id.String())} {
		if _, err := ParseOperationID(bad); err == nil {
			t.Errorf("ParseOperationID(%q) succeeded, want rejection", bad)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", operationIDHexLen+1), "zz"} {
		if err := ValidateOperationIDPrefix(bad); err == nil {
			t.Errorf("ValidateOperationIDPrefix(%q) succeeded, want rejection", bad)
		}
	}
	if err := ValidateOperationIDPrefix(id.Short()); err != nil {
		t.Errorf("a short id is not a valid prefix: %v", err)
	}
}

func TestOperationIDsSortChronologically(t *testing.T) {
	rand := bytes.Repeat([]byte{0}, 8)
	older, err := NewOperationID(1, bytes.NewReader(rand))
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}
	newer, err := NewOperationID(2, bytes.NewReader(rand))
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}
	if older.String() >= newer.String() {
		t.Errorf("ids do not sort chronologically: %q >= %q", older, newer)
	}
}
