package compare_test

import (
	"fmt"
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

const (
	hexA = "1111111111111111111111111111111111111111111111111111111111111111"
	hexB = "2222222222222222222222222222222222222222222222222222222222222222"
	hexC = "3333333333333333333333333333333333333333333333333333333333333333"
)

func relPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("ParseRelPath(%q): %v", s, err)
	}
	return p
}

func content(t *testing.T, hex string) hashing.ContentHash {
	t.Helper()
	h, err := hashing.ParseContentHash("blake3:" + hex)
	if err != nil {
		t.Fatalf("ParseContentHash(%q): %v", hex, err)
	}
	return h
}

func stat(mode uint32, size, mtime int64) worktree.StatSignature {
	return worktree.StatSignature{Size: size, MtimeNs: mtime, Mode: mode}
}

func regular(t *testing.T, path, hex string, storage worktree.ContentStorageIntent) worktree.Entry {
	t.Helper()
	e, err := worktree.NewRegularEntry(relPath(t, path), content(t, hex), storage, stat(0o644, 10, 1), worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewRegularEntry(%q): %v", path, err)
	}
	return e
}

func regularMode(t *testing.T, path, hex string, mode uint32) worktree.Entry {
	t.Helper()
	e, err := worktree.NewRegularEntry(relPath(t, path), content(t, hex), worktree.StorageBlob, stat(mode, 10, 1), worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewRegularEntry(%q): %v", path, err)
	}
	return e
}

func symlink(t *testing.T, path, target string) worktree.Entry {
	t.Helper()
	tgt, err := worktree.NewSymlinkTarget(target)
	if err != nil {
		t.Fatalf("NewSymlinkTarget(%q): %v", target, err)
	}
	e, err := worktree.NewSymlinkEntry(relPath(t, path), tgt, stat(0o777, 0, 1), worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewSymlinkEntry(%q): %v", path, err)
	}
	return e
}

func dir(t *testing.T, path string) worktree.Entry {
	t.Helper()
	e, err := worktree.NewDirEntry(relPath(t, path), worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewDirEntry(%q): %v", path, err)
	}
	return e
}

func skipped(t *testing.T, path string, reason worktree.SkippedReason, size, mtime int64) worktree.SkippedInput {
	t.Helper()
	s, err := worktree.NewSkippedInput(relPath(t, path), reason, size, stat(0o644, size, mtime), "", worktree.SymlinkTarget{}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewSkippedInput(%q): %v", path, err)
	}
	return s
}

// changeSet compares two canonical manifest cursors through the production entry
// point and returns the drained result. Each side is built with
// scantest.CanonicalCursor, which composes the same Ordered/NewSliceCursor
// primitives a real source does, so these
// tests exercise the one comparison implementation rather than a fixture-shaped
// alternative. A merge error is fatal: the fixtures below are duplicate-free and
// ordered by construction, so an error here means the merge itself is wrong.
func changeSet(t *testing.T, left, right worktree.ManifestCursor, opts compare.Options) compare.ChangeSet {
	t.Helper()
	cs, err := compare.CompareToChangeSet(left, right, opts)
	if err != nil {
		t.Fatalf("CompareToChangeSet: %v", err)
	}
	return cs
}

// find returns the single change for path (old or new), failing if not exactly one.
func find(t *testing.T, cs compare.ChangeSet, path string) compare.Change {
	t.Helper()
	var found []compare.Change
	for _, c := range cs.Changes {
		if c.OldPath.String() == path || c.NewPath.String() == path {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one change for %q, got %d: %+v", path, len(found), cs.Changes)
	}
	return found[0]
}

func TestEqualManifestsNoChanges(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "a.go", hexA, worktree.StorageBlob), dir(t, "pkg")}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "a.go", hexA, worktree.StorageBlob), dir(t, "pkg")}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	if len(cs.Changes) != 0 {
		t.Fatalf("want no changes, got %+v", cs.Changes)
	}
}

func TestAddModifyDelete(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{
		regular(t, "keep.go", hexA, worktree.StorageBlob),
		regular(t, "mod.go", hexA, worktree.StorageBlob),
		regular(t, "gone.go", hexA, worktree.StorageBlob),
	}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{
		regular(t, "keep.go", hexA, worktree.StorageBlob),
		regular(t, "mod.go", hexB, worktree.StorageBlob),
		regular(t, "new.go", hexC, worktree.StorageBlob),
	}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	if got := find(t, cs, "new.go").Status; got != compare.Added {
		t.Errorf("new.go: want Added, got %v", got)
	}
	if got := find(t, cs, "mod.go").Status; got != compare.Modified {
		t.Errorf("mod.go: want Modified, got %v", got)
	}
	if got := find(t, cs, "gone.go").Status; got != compare.Deleted {
		t.Errorf("gone.go: want Deleted, got %v", got)
	}
	if cs.Summary.Added != 1 || cs.Summary.Modified != 1 || cs.Summary.Deleted != 1 {
		t.Errorf("summary: %+v", cs.Summary)
	}
}

func TestModifiedContentDiffAvailability(t *testing.T) {
	// blob -> blob: diffable.
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "a.go", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "a.go", hexB, worktree.StorageBlob)}, nil)
	if !find(t, changeSet(t, left, right, compare.Options{}), "a.go").ContentDiffAvailable {
		t.Error("blob->blob modify should be content-diffable")
	}
	// hash-only on one side: not diffable, noted.
	left = scantest.CanonicalCursor([]worktree.Entry{regular(t, "b.bin", hexA, worktree.StorageHashOnly)}, nil)
	right = scantest.CanonicalCursor([]worktree.Entry{regular(t, "b.bin", hexB, worktree.StorageHashOnly)}, nil)
	ch := find(t, changeSet(t, left, right, compare.Options{}), "b.bin")
	if ch.Status != compare.Modified {
		t.Errorf("hash-only change: want Modified, got %v", ch.Status)
	}
	if ch.ContentDiffAvailable {
		t.Error("hash-only modify must not be content-diffable")
	}
	if ch.Note == "" {
		t.Error("hash-only modify should carry a note")
	}
}

func TestTypeChange(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "x", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{symlink(t, "x", "./target")}, nil)
	if got := find(t, changeSet(t, left, right, compare.Options{}), "x").Status; got != compare.TypeChanged {
		t.Errorf("want TypeChanged, got %v", got)
	}
}

func TestSymlinkTargetModification(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{symlink(t, "link", "./old")}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{symlink(t, "link", "./new")}, nil)
	ch := find(t, changeSet(t, left, right, compare.Options{}), "link")
	if ch.Status != compare.Modified {
		t.Errorf("want Modified, got %v", ch.Status)
	}
	if ch.ContentDiffAvailable {
		t.Error("symlink change is not a content diff")
	}
}

func TestStoragePolicyChangeIsNotAChange(t *testing.T) {
	// Same content, different storage intent (blob vs hash-only) — the tree hash
	// ignores storage policy, so comparison must too.
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "a.go", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "a.go", hexA, worktree.StorageHashOnly)}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	if len(cs.Changes) != 0 {
		t.Fatalf("storage-policy-only change must not be reported: %+v", cs.Changes)
	}
}

func TestTraversalProvenanceChangeIsAChange(t *testing.T) {
	plain := regular(t, "a.go", hexA, worktree.StorageBlob)
	followed := plain
	followed.Traversal = worktree.TraversalInfo{Followed: true, SourcePath: relPath(t, "link"), Depth: 1}
	left := scantest.CanonicalCursor([]worktree.Entry{plain}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{followed}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	if len(cs.Changes) != 1 || cs.Changes[0].Status != compare.Modified {
		t.Fatalf("traversal change should be Modified, got %+v", cs.Changes)
	}
}

func TestModeChangeIsModified(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regularMode(t, "s.sh", hexA, 0o644)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regularMode(t, "s.sh", hexA, 0o755)}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	if len(cs.Changes) != 1 || cs.Changes[0].Status != compare.Modified {
		t.Fatalf("mode change should be Modified, got %+v", cs.Changes)
	}
	ch := cs.Changes[0]
	// Content hash is unchanged, so there is no content diff to offer; the change
	// must be explained rather than presented as a diffable modification.
	if ch.ContentDiffAvailable {
		t.Error("mode-only change must not claim content-diff availability")
	}
	// Mode is carried structurally so it survives independently of content.
	if !ch.ModeChanged() || ch.OldMode != 0o644 || ch.NewMode != 0o755 {
		t.Errorf("mode fields wrong: old=%04o new=%04o changed=%v", ch.OldMode, ch.NewMode, ch.ModeChanged())
	}
}

func TestModeChangeToAndFromZeroIsDetected(t *testing.T) {
	// 0000 is valid permission bits, not "absent", so changes to and from it must
	// register as mode changes (the set flags, not a zero sentinel, decide).
	cases := []struct{ oldMode, newMode uint32 }{
		{0o000, 0o644},
		{0o644, 0o000},
	}
	for _, c := range cases {
		left := scantest.CanonicalCursor([]worktree.Entry{regularMode(t, "f", hexA, c.oldMode)}, nil)
		right := scantest.CanonicalCursor([]worktree.Entry{regularMode(t, "f", hexA, c.newMode)}, nil)
		cs := changeSet(t, left, right, compare.Options{})
		if len(cs.Changes) != 1 {
			t.Fatalf("%04o -> %04o: want one change, got %+v", c.oldMode, c.newMode, cs.Changes)
		}
		ch := cs.Changes[0]
		if !ch.ModeChanged() || ch.OldMode != c.oldMode || ch.NewMode != c.newMode {
			t.Errorf("%04o -> %04o: ModeChanged=%v old=%04o new=%04o", c.oldMode, c.newMode, ch.ModeChanged(), ch.OldMode, ch.NewMode)
		}
		if !ch.OldModeSet || !ch.NewModeSet {
			t.Errorf("%04o -> %04o: mode set flags should both be true", c.oldMode, c.newMode)
		}
	}
}

func TestContentAndModeChangeKeepsBoth(t *testing.T) {
	// Content and mode both change: the file is content-diffable, and the mode
	// change is still recorded structurally rather than being lost.
	left := scantest.CanonicalCursor([]worktree.Entry{regularMode(t, "s.sh", hexA, 0o644)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regularMode(t, "s.sh", hexB, 0o755)}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	if len(cs.Changes) != 1 {
		t.Fatalf("want one change, got %+v", cs.Changes)
	}
	ch := cs.Changes[0]
	if ch.Status != compare.Modified || !ch.ContentDiffAvailable {
		t.Errorf("content change should be a diffable modification: %+v", ch)
	}
	if !ch.ModeChanged() || ch.OldMode != 0o644 || ch.NewMode != 0o755 {
		t.Errorf("mode change lost alongside content: old=%04o new=%04o", ch.OldMode, ch.NewMode)
	}
}

func TestSkippedTransitions(t *testing.T) {
	// entry -> skipped is reported as S.
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "f", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor(nil, []worktree.SkippedInput{skipped(t, "f", worktree.ReasonLargeFileSkipPolicy, 99, 5)})
	if got := find(t, changeSet(t, left, right, compare.Options{}), "f").Status; got != compare.Skipped {
		t.Errorf("entry->skip: want Skipped, got %v", got)
	}
	// skipped reason change is reported as S.
	left = scantest.CanonicalCursor(nil, []worktree.SkippedInput{skipped(t, "f", worktree.ReasonLargeFileSkipPolicy, 99, 5)})
	right = scantest.CanonicalCursor(nil, []worktree.SkippedInput{skipped(t, "f", worktree.ReasonSpecialFile, 99, 5)})
	if got := find(t, changeSet(t, left, right, compare.Options{}), "f").Status; got != compare.Skipped {
		t.Errorf("skip reason change: want Skipped, got %v", got)
	}
	// identical skip is omitted.
	same := skipped(t, "f", worktree.ReasonLargeFileSkipPolicy, 99, 5)
	cs := changeSet(t, scantest.CanonicalCursor(nil, []worktree.SkippedInput{same}), scantest.CanonicalCursor(nil, []worktree.SkippedInput{same}), compare.Options{})
	if len(cs.Changes) != 0 {
		t.Fatalf("identical skip must be omitted: %+v", cs.Changes)
	}
}

func TestPathFilterExactFile(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "src/a.go", hexA, worktree.StorageBlob), regular(t, "src/b.go", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "src/a.go", hexB, worktree.StorageBlob), regular(t, "src/b.go", hexB, worktree.StorageBlob)}, nil)
	cs := changeSet(t, left, right, compare.Options{PathFilters: []worktree.RelPath{relPath(t, "src/a.go")}})
	if len(cs.Changes) != 1 || cs.Changes[0].NewPath.String() != "src/a.go" {
		t.Fatalf("exact-file filter: %+v", cs.Changes)
	}
}

func TestPathFilterDirectoryDescendants(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "src/a.go", hexA, worktree.StorageBlob), regular(t, "doc/x.md", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "src/a.go", hexB, worktree.StorageBlob), regular(t, "doc/x.md", hexB, worktree.StorageBlob)}, nil)
	cs := changeSet(t, left, right, compare.Options{PathFilters: []worktree.RelPath{relPath(t, "src")}})
	if len(cs.Changes) != 1 || cs.Changes[0].NewPath.String() != "src/a.go" {
		t.Fatalf("directory filter: %+v", cs.Changes)
	}
}

func TestRenameDetectionUniqueContent(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "old.go", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "new.go", hexA, worktree.StorageBlob)}, nil)
	cs := changeSet(t, left, right, compare.Options{DetectRenames: true})
	if len(cs.Changes) != 1 || cs.Changes[0].Status != compare.Renamed {
		t.Fatalf("want one Renamed, got %+v", cs.Changes)
	}
	r := cs.Changes[0]
	if r.OldPath.String() != "old.go" || r.NewPath.String() != "new.go" {
		t.Errorf("rename paths: %q -> %q", r.OldPath, r.NewPath)
	}
}

func TestRenameDetectionDisabled(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "old.go", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "new.go", hexA, worktree.StorageBlob)}, nil)
	cs := changeSet(t, left, right, compare.Options{DetectRenames: false})
	if cs.Summary.Renamed != 0 || cs.Summary.Added != 1 || cs.Summary.Deleted != 1 {
		t.Fatalf("no-renames: want add+delete, got %+v", cs.Summary)
	}
}

func TestRenameAmbiguousStaysAddDelete(t *testing.T) {
	// Two deletes and two adds share content hexA: ambiguous, so no rename.
	left := scantest.CanonicalCursor([]worktree.Entry{regular(t, "old1.go", hexA, worktree.StorageBlob), regular(t, "old2.go", hexA, worktree.StorageBlob)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{regular(t, "new1.go", hexA, worktree.StorageBlob), regular(t, "new2.go", hexA, worktree.StorageBlob)}, nil)
	cs := changeSet(t, left, right, compare.Options{DetectRenames: true})
	if cs.Summary.Renamed != 0 {
		t.Fatalf("ambiguous rename should stay add/delete, got %+v", cs.Summary)
	}
	if cs.Summary.Added != 2 || cs.Summary.Deleted != 2 {
		t.Fatalf("want 2 add + 2 delete, got %+v", cs.Summary)
	}
}

func TestRenameSymlinkByTarget(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{symlink(t, "old-link", "./target")}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{symlink(t, "new-link", "./target")}, nil)
	cs := changeSet(t, left, right, compare.Options{DetectRenames: true})
	if len(cs.Changes) != 1 || cs.Changes[0].Status != compare.Renamed {
		t.Fatalf("symlink rename: %+v", cs.Changes)
	}
}

func TestChangesAreSortedByPath(t *testing.T) {
	left := scantest.CanonicalCursor(nil, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{
		regular(t, "z.go", hexA, worktree.StorageBlob),
		regular(t, "a.go", hexB, worktree.StorageBlob),
		regular(t, "m.go", hexC, worktree.StorageBlob),
	}, nil)
	cs := changeSet(t, left, right, compare.Options{})
	var order []string
	for _, c := range cs.Changes {
		order = append(order, c.PrimaryPath().String())
	}
	want := []string{"a.go", "m.go", "z.go"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}
