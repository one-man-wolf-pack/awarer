package worktree_test

import (
	"context"
	"errors"
	"testing"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

func rel(t *testing.T, p string) worktree.RelPath {
	t.Helper()
	r, err := worktree.ParseRelPath(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func recorded(t *testing.T, records ...worktree.FrontierRecord) worktree.Coverage {
	t.Helper()
	stream := scantest.FrontierStream(records...)
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d, err := worktree.ReduceFrontier(blake3hash.New(), cur)
	if err != nil {
		t.Fatal(err)
	}
	c, err := worktree.RecordedCoverage(stream, d)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func tracker(t *testing.T, c worktree.Coverage) *worktree.AbsenceTracker {
	t.Helper()
	tr, err := worktree.NewAbsenceTracker(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func skippedRecord(t *testing.T, p string, reason worktree.SkippedReason) worktree.ManifestRecord {
	t.Helper()
	var target worktree.SymlinkTarget
	if reason.Kind() == worktree.KindSymlink {
		var err error
		if target, err = worktree.NewSymlinkTarget("elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
	s, err := worktree.NewSkippedInput(rel(t, p), reason, 0, worktree.StatSignature{}, "", target, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return worktree.SkippedRecord(s)
}

// In byte order "a-b" and "a.c" sort between "a" and "a/x", so the boundary at "a"
// must survive the sibling queries.
func TestAbsenceTrackerFollowsByteOrderAcrossInterleavedSiblings(t *testing.T) {
	tr := tracker(t, recorded(t,
		worktree.FrontierRecord{Path: rel(t, "a"), Limit: worktree.LimitSubtree},
		worktree.FrontierRecord{Path: rel(t, "a-b"), Limit: worktree.LimitNode},
		worktree.FrontierRecord{Path: rel(t, "z"), Limit: worktree.LimitNode},
	))
	want := []struct {
		path    string
		verdict worktree.AbsenceVerdict
	}{
		{"a", worktree.AbsenceExcluded},
		{"a-b", worktree.AbsenceExcluded},
		{"a-b/c", worktree.AbsenceProved},
		{"a.c", worktree.AbsenceProved},
		{"a/x", worktree.AbsenceExcluded},
		{"a/x/y", worktree.AbsenceExcluded},
		{"ab", worktree.AbsenceProved},
		{"z", worktree.AbsenceExcluded},
		{"z/under-a-node", worktree.AbsenceProved},
	}
	for _, w := range want {
		got, err := tr.Absent(rel(t, w.path))
		if err != nil {
			t.Fatal(err)
		}
		if got != w.verdict {
			t.Errorf("Absent(%q) = %v, want %v", w.path, got, w.verdict)
		}
	}
	if err := tr.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestAbsenceTrackerTreatsSkippedSymlinksAsSubtreeBoundaries(t *testing.T) {
	tr := tracker(t, recorded(t))
	if err := tr.Note(skippedRecord(t, "link", worktree.ReasonSymlinkCycle)); err != nil {
		t.Fatal(err)
	}
	if got, _ := tr.Absent(rel(t, "link/x")); got != worktree.AbsenceSkippedBoundary {
		t.Errorf("below a skipped symlink = %v, want skipped boundary", got)
	}
	if err := tr.Note(skippedRecord(t, "zz-large.bin", worktree.ReasonLargeFileSkipPolicy)); err != nil {
		t.Fatal(err)
	}
	if got, _ := tr.Absent(rel(t, "zz-large.bin/x")); got != worktree.AbsenceProved {
		t.Errorf("below a skipped regular file = %v, want proved (a file has no descendants)", got)
	}
}

func TestAbsenceTrackerWithoutRecordedCoverage(t *testing.T) {
	unknown := tracker(t, worktree.UnknownCoverage())
	if got, _ := unknown.Absent(rel(t, "anything")); got != worktree.AbsenceUnknown {
		t.Errorf("unknown coverage = %v, want unknown", got)
	}
	scoped := tracker(t, worktree.ScopedCoverage())
	if got, _ := scoped.Absent(rel(t, "anything")); got != worktree.AbsenceProved {
		t.Errorf("scoped coverage = %v, want proved", got)
	}
}

func TestAbsenceTrackerRejectsOutOfOrderQueries(t *testing.T) {
	tr := tracker(t, recorded(t))
	if _, err := tr.Absent(rel(t, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Absent(rel(t, "a")); err == nil {
		t.Fatal("a query before its predecessor was answered")
	}
}

func TestFrontierReducerRejectsDuplicatesAndDisorder(t *testing.T) {
	for name, records := range map[string][]worktree.FrontierRecord{
		"duplicate":    {{Path: rel(t, "a"), Limit: worktree.LimitNode}, {Path: rel(t, "a"), Limit: worktree.LimitSubtree}},
		"out of order": {{Path: rel(t, "b"), Limit: worktree.LimitNode}, {Path: rel(t, "a"), Limit: worktree.LimitNode}},
		"no limit":     {{Path: rel(t, "a")}},
	} {
		_, err := worktree.ReduceFrontier(blake3hash.New(), worktree.NewFrontierSliceCursor(records))
		if err == nil {
			t.Errorf("%s: reduced without error", name)
		}
	}
	_, err := worktree.ReduceFrontier(blake3hash.New(), worktree.OrderedFrontier(worktree.NewFrontierSliceCursor(
		[]worktree.FrontierRecord{{Path: rel(t, "b"), Limit: worktree.LimitNode}, {Path: rel(t, "a"), Limit: worktree.LimitNode}})))
	if !errors.Is(err, worktree.ErrFrontierOutOfOrder) {
		t.Errorf("ordered cursor error = %v, want ErrFrontierOutOfOrder", err)
	}
}

func TestFrontierDigestDependsOnNamesAndLimits(t *testing.T) {
	digest := func(records ...worktree.FrontierRecord) worktree.FrontierDigest {
		d, err := worktree.ReduceFrontier(blake3hash.New(), worktree.NewFrontierSliceCursor(records))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	base := digest(worktree.FrontierRecord{Path: rel(t, "a"), Limit: worktree.LimitNode})
	if base == digest(worktree.FrontierRecord{Path: rel(t, "a"), Limit: worktree.LimitSubtree}) {
		t.Error("limit does not reach the digest")
	}
	if base == digest(worktree.FrontierRecord{Path: rel(t, "b"), Limit: worktree.LimitNode}) {
		t.Error("name does not reach the digest")
	}
	if empty := digest(); empty.Count != 0 || empty.Hash.IsZero() {
		t.Errorf("empty frontier digest = %+v, want a real hash over no records", empty)
	}
}

func TestReachableTreatsAnExcludedNodeAncestorAsABoundary(t *testing.T) {
	tr := tracker(t, recorded(t, worktree.FrontierRecord{Path: rel(t, "d"), Limit: worktree.LimitNode}))
	if got, _ := tr.Reachable(rel(t, "d/x")); got != worktree.AbsenceExcluded {
		t.Errorf("Reachable below an excluded file = %v, want excluded", got)
	}
	if got, _ := tr.Absent(rel(t, "d/x")); got != worktree.AbsenceProved {
		t.Errorf("Absent below an excluded file = %v, want proved", got)
	}
}
