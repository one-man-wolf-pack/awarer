package compare_test

import (
	"context"
	"errors"
	"testing"

	"awarer/internal/domain/compare"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

// --- fixtures ---

func frontier(path string, limit worktree.CoverageLimit) worktree.FrontierRecord {
	p, err := worktree.ParseRelPath(path)
	if err != nil {
		panic(err)
	}
	return worktree.FrontierRecord{Path: p, Limit: limit}
}

// recordedCoverage builds completed coverage over a frontier, optionally serving it
// through a stream whose cursor fails once its records are exhausted (a tampered
// tail, as a persisted frontier reports it on its final count/digest check).
func recordedCoverage(t *testing.T, tailErr error, records ...worktree.FrontierRecord) worktree.Coverage {
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
	served := stream
	if tailErr != nil {
		served = failingFrontier{inner: stream, err: tailErr}
	}
	c, err := worktree.RecordedCoverage(served, d)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type failingFrontier struct {
	inner worktree.FrontierStream
	err   error
}

func (f failingFrontier) Open(ctx context.Context) (worktree.FrontierCursor, error) {
	c, err := f.inner.Open(ctx)
	if err != nil {
		return nil, err
	}
	return &failingFrontierCursor{FrontierCursor: c, err: f.err}, nil
}

type failingFrontierCursor struct {
	worktree.FrontierCursor
	err  error
	done bool
}

func (c *failingFrontierCursor) Next() bool {
	if c.FrontierCursor.Next() {
		return true
	}
	c.done = true
	return false
}

func (c *failingFrontierCursor) Err() error {
	if c.done {
		return c.err
	}
	return nil
}

// pathSet is an ordered covered path set, closing-counted, optionally failing at EOF.
type pathSet struct {
	paths  []worktree.RelPath
	i      int
	err    error
	closed *int
}

func newPathSet(t *testing.T, tailErr error, closed *int, paths ...string) *pathSet {
	t.Helper()
	s := &pathSet{i: -1, err: tailErr, closed: closed}
	for _, p := range paths {
		s.paths = append(s.paths, relPath(t, p))
	}
	return s
}

func (s *pathSet) Next() bool {
	if s.i+1 >= len(s.paths) {
		s.i = len(s.paths)
		return false
	}
	s.i++
	return true
}

func (s *pathSet) Path() worktree.RelPath { return s.paths[s.i] }

func (s *pathSet) Err() error {
	if s.i >= len(s.paths) {
		return s.err
	}
	return nil
}

func (s *pathSet) Close() error {
	if s.closed != nil {
		*s.closed++
	}
	return nil
}

func evidence(t *testing.T, c worktree.Coverage, scope compare.PathSetCursor) compare.SideEvidence {
	t.Helper()
	tr, err := worktree.NewAbsenceTracker(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return compare.SideEvidence{Absence: tr, Scope: scope}
}

func skippedInput(t *testing.T, path string, reason worktree.SkippedReason) worktree.SkippedInput {
	t.Helper()
	var target worktree.SymlinkTarget
	if reason.Kind() == worktree.KindSymlink {
		var err error
		if target, err = worktree.NewSymlinkTarget("elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
	s, err := worktree.NewSkippedInput(relPath(t, path), reason, 0, worktree.StatSignature{}, "", target, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type qualified struct {
	path   string
	status compare.Status
	side   compare.Side
	qual   compare.Qualification
}

func qualifiedOf(c compare.Change) qualified {
	return qualified{c.PrimaryPath().String(), c.Status, c.Counterpart.Side, c.Counterpart.Qualification}
}

func runQualified(t *testing.T, left, right worktree.ManifestCursor, opts compare.Options, limit int) ([]qualified, error) {
	t.Helper()
	cur, err := compare.CompareStreamLimited(left, right, opts, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()
	var got []qualified
	for cur.Next() {
		got = append(got, qualifiedOf(cur.Change()))
	}
	return got, cur.Err()
}

func assertQualified(t *testing.T, got, want []qualified) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d:\n got  %+v\n want %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// boundedSide is one observation with an exclusion frontier and a skipped symlink:
// "ex.txt" excluded as a node, "gen" pruned as a subtree, "link" skipped (it may
// have led anywhere), and "z-tail" an exclusion after every reported path.
func boundedSide(t *testing.T) (entries []worktree.Entry, skips []worktree.SkippedInput, cov worktree.Coverage) {
	t.Helper()
	return []worktree.Entry{reg(t, "keep", hexA)},
		[]worktree.SkippedInput{skippedInput(t, "link", worktree.ReasonSymlinkCycle)},
		recordedCoverage(t, nil,
			frontier("ex.txt", worktree.LimitNode),
			frontier("gen", worktree.LimitSubtree),
			frontier("z-tail", worktree.LimitNode),
		)
}

// The one-sided paths the other observation records. In byte order "gen-b" and
// "gen.c" sort between "gen" and "gen/x", so the subtree boundary at "gen" must
// survive those siblings without covering them.
var oneSided = []string{"a", "ex.txt", "ex.txt/z", "gen-b", "gen.c", "gen/x", "link/y"}

func oneSidedEntries(t *testing.T) []worktree.Entry {
	var es []worktree.Entry
	for i, p := range oneSided {
		es = append(es, reg(t, p, []string{hexA, hexB, hexC}[i%3]))
	}
	return append(es, reg(t, "keep", hexA))
}

// wantOneSided is the qualification each one-sided path receives against
// boundedSide. A node-only exclusion leaves nothing below it unobserved.
func wantOneSided(status compare.Status, side compare.Side) []qualified {
	quals := []compare.Qualification{
		compare.QualAbsent,             // a
		compare.QualOutsideObservation, // ex.txt: excluded leaf
		compare.QualAbsent,             // ex.txt/z: below a node-only exclusion
		compare.QualAbsent,             // gen-b: byte-order sibling, not below gen
		compare.QualAbsent,             // gen.c
		compare.QualOutsideObservation, // gen/x: under a pruned subtree
		compare.QualSkippedBoundary,    // link/y: under a skipped symlink
	}
	var want []qualified
	for i, p := range oneSided {
		want = append(want, qualified{p, status, side, quals[i]})
	}
	return want
}

// insertSkip places the skipped "link" row (present on one side only) into an
// expected sequence at its canonical position, between "gen/x" and "link/y".
func insertSkip(want []qualified, q qualified) []qualified {
	out := append([]qualified(nil), want[:6]...)
	out = append(out, q)
	return append(out, want[6:]...)
}

// TestCounterpartQualifiesAddedEntriesFromTheLeftObservation covers every
// qualification for added entries, which consult the left observation.
func TestCounterpartQualifiesAddedEntriesFromTheLeftObservation(t *testing.T) {
	lEntries, lSkips, lCov := boundedSide(t)
	left := scantest.CanonicalCursor(lEntries, lSkips)
	right := scantest.CanonicalCursor(oneSidedEntries(t), nil)
	ev := &compare.CounterpartEvidence{
		Left:  evidence(t, lCov, nil),
		Right: evidence(t, recordedCoverage(t, nil), nil),
	}
	got, err := runQualified(t, left, right, compare.Options{Counterparts: ev}, compare.DefaultRenameBufferLimit)
	if err != nil {
		t.Fatal(err)
	}
	// The left-only skipped "link" is a delete; the right recorded nothing there and
	// has no boundary, so its absence on the right is proved.
	want := insertSkip(wantOneSided(compare.Added, compare.SideLeft),
		qualified{"link", compare.Deleted, compare.SideRight, compare.QualAbsent})
	assertQualified(t, got, want)
}

// TestCounterpartQualifiesDeletedEntriesFromTheRightObservation mirrors the added
// case: a delete consults the right observation.
func TestCounterpartQualifiesDeletedEntriesFromTheRightObservation(t *testing.T) {
	rEntries, rSkips, rCov := boundedSide(t)
	left := scantest.CanonicalCursor(oneSidedEntries(t), nil)
	right := scantest.CanonicalCursor(rEntries, rSkips)
	ev := &compare.CounterpartEvidence{
		Left:  evidence(t, recordedCoverage(t, nil), nil),
		Right: evidence(t, rCov, nil),
	}
	got, err := runQualified(t, left, right, compare.Options{DetectRenames: false, Counterparts: ev}, compare.DefaultRenameBufferLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := insertSkip(wantOneSided(compare.Deleted, compare.SideRight),
		qualified{"link", compare.Added, compare.SideLeft, compare.QualAbsent})
	assertQualified(t, got, want)
}

// TestCounterpartUnknownCoverageNeverProvesAbsence: a predecessor observation
// without coverage evidence qualifies its missing side as unknown, in both
// directions, while statuses and counts are untouched.
func TestCounterpartUnknownCoverageNeverProvesAbsence(t *testing.T) {
	left := scantest.CanonicalCursor([]worktree.Entry{reg(t, "gone", hexA), reg(t, "same", hexB)}, nil)
	right := scantest.CanonicalCursor([]worktree.Entry{reg(t, "new", hexC), reg(t, "same", hexB)}, nil)
	ev := &compare.CounterpartEvidence{
		Left:  evidence(t, worktree.UnknownCoverage(), nil),
		Right: evidence(t, worktree.UnknownCoverage(), nil),
	}
	got, err := runQualified(t, left, right, compare.Options{Counterparts: ev}, compare.DefaultRenameBufferLimit)
	if err != nil {
		t.Fatal(err)
	}
	assertQualified(t, got, []qualified{
		{"gone", compare.Deleted, compare.SideRight, compare.QualUnknown},
		{"new", compare.Added, compare.SideLeft, compare.QualUnknown},
	})
}

// TestCounterpartFilteredDescendantKeepsItsSkippedAncestor: the path filter selects
// only "link/y"; its skipped ancestor "link" is outside the selection yet still
// bounds it, and the frontier after the last selected change is still verified.
func TestCounterpartFilteredDescendantKeepsItsSkippedAncestor(t *testing.T) {
	lEntries, lSkips, lCov := boundedSide(t)
	left := scantest.CanonicalCursor(lEntries, lSkips)
	right := scantest.CanonicalCursor(oneSidedEntries(t), nil)
	ev := &compare.CounterpartEvidence{
		Left:  evidence(t, lCov, nil),
		Right: evidence(t, recordedCoverage(t, nil), nil),
	}
	opts := compare.Options{PathFilters: []worktree.RelPath{relPath(t, "link/y")}, Counterparts: ev}
	got, err := runQualified(t, left, right, opts, compare.DefaultRenameBufferLimit)
	if err != nil {
		t.Fatal(err)
	}
	assertQualified(t, got, []qualified{{"link/y", compare.Added, compare.SideLeft, compare.QualSkippedBoundary}})
}

// TestCounterpartTailFailureFailsTheComparison: a frontier that fails its terminal
// verification after the last selected change fails the stream; it never ends as a
// complete report with successful qualifications.
func TestCounterpartTailFailureFailsTheComparison(t *testing.T) {
	tampered := errors.New("frontier digest mismatch")
	for _, renames := range []bool{false, true} {
		for _, tamperRight := range []bool{false, true} {
			// The tampered side is the one missing "a", so it is the side consulted.
			present := scantest.CanonicalCursor([]worktree.Entry{reg(t, "a", hexA)}, nil)
			missing := scantest.CanonicalCursor(nil, nil)
			bad := evidence(t, recordedCoverage(t, tampered, frontier("zz", worktree.LimitNode)), nil)
			good := evidence(t, recordedCoverage(t, nil), nil)
			left, right := missing, present
			ev := &compare.CounterpartEvidence{Left: bad, Right: good}
			if tamperRight {
				left, right = present, missing
				ev = &compare.CounterpartEvidence{Left: good, Right: bad}
			}
			opts := compare.Options{DetectRenames: renames, PathFilters: []worktree.RelPath{relPath(t, "a")}, Counterparts: ev}
			_, err := runQualified(t, left, right, opts, compare.DefaultRenameBufferLimit)
			if !errors.Is(err, tampered) {
				t.Errorf("renames=%v right=%v: err = %v, want the frontier's terminal failure", renames, tamperRight, err)
			}
		}
	}
}

// TestCounterpartRecoveryScopeConfinesProof: a recovery observation proves absence
// only at its exact covered paths; elsewhere the lookup is outside observation. A
// predecessor recovery record keeps unknown inside its set. A covered set that
// fails its terminal check fails the comparison.
func TestCounterpartRecoveryScopeConfinesProof(t *testing.T) {
	right := func() worktree.ManifestCursor {
		return scantest.CanonicalCursor([]worktree.Entry{reg(t, "in", hexA), reg(t, "in/below", hexB), reg(t, "out", hexC)}, nil)
	}
	cases := []struct {
		name string
		cov  worktree.Coverage
		want compare.Qualification
	}{
		{"scoped", worktree.ScopedCoverage(), compare.QualAbsent},
		{"predecessor", worktree.UnknownCoverage(), compare.QualUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			ev := &compare.CounterpartEvidence{
				Left:  evidence(t, tc.cov, newPathSet(t, nil, &closed, "in", "zz-later")),
				Right: evidence(t, recordedCoverage(t, nil), nil),
			}
			got, err := runQualified(t, scantest.CanonicalCursor(nil, nil), right(), compare.Options{Counterparts: ev}, compare.DefaultRenameBufferLimit)
			if err != nil {
				t.Fatal(err)
			}
			assertQualified(t, got, []qualified{
				{"in", compare.Added, compare.SideLeft, tc.want},
				{"in/below", compare.Added, compare.SideLeft, compare.QualOutsideObservation},
				{"out", compare.Added, compare.SideLeft, compare.QualOutsideObservation},
			})
			if closed != 1 {
				t.Errorf("covered set closed %d times, want 1", closed)
			}
		})
	}

	broken := errors.New("covered scope count mismatch")
	ev := &compare.CounterpartEvidence{
		Left:  evidence(t, worktree.ScopedCoverage(), newPathSet(t, broken, nil, "in")),
		Right: evidence(t, recordedCoverage(t, nil), nil),
	}
	opts := compare.Options{PathFilters: []worktree.RelPath{relPath(t, "in")}, Counterparts: ev}
	if _, err := runQualified(t, scantest.CanonicalCursor(nil, nil), right(), opts, compare.DefaultRenameBufferLimit); !errors.Is(err, broken) {
		t.Errorf("err = %v, want the covered set's terminal failure", err)
	}
}

// TestCounterpartSurvivesRenamePairingAndItsLimit: paired renames carry no
// counterpart, retained adds/deletes keep theirs, and over the rename buffer limit
// the same membership, order and qualifications stream as without renames.
func TestCounterpartSurvivesRenamePairingAndItsLimit(t *testing.T) {
	sides := func() (worktree.ManifestCursor, worktree.ManifestCursor, *compare.CounterpartEvidence) {
		left := scantest.CanonicalCursor([]worktree.Entry{reg(t, "old", hexA), reg(t, "gone", hexB)}, nil)
		right := scantest.CanonicalCursor([]worktree.Entry{reg(t, "gen/new", hexA), reg(t, "added", hexC)}, nil)
		ev := &compare.CounterpartEvidence{
			Left:  evidence(t, recordedCoverage(t, nil, frontier("gen", worktree.LimitSubtree)), nil),
			Right: evidence(t, worktree.UnknownCoverage(), nil),
		}
		return left, right, ev
	}
	plain := []qualified{
		{"added", compare.Added, compare.SideLeft, compare.QualAbsent},
		{"gen/new", compare.Added, compare.SideLeft, compare.QualOutsideObservation},
		{"gone", compare.Deleted, compare.SideRight, compare.QualUnknown},
		{"old", compare.Deleted, compare.SideRight, compare.QualUnknown},
	}

	l, r, ev := sides()
	got, err := runQualified(t, l, r, compare.Options{Counterparts: ev}, compare.DefaultRenameBufferLimit)
	if err != nil {
		t.Fatal(err)
	}
	assertQualified(t, got, plain)

	l, r, ev = sides()
	got, err = runQualified(t, l, r, compare.Options{DetectRenames: true, Counterparts: ev}, compare.DefaultRenameBufferLimit)
	if err != nil {
		t.Fatal(err)
	}
	assertQualified(t, got, []qualified{
		{"added", compare.Added, compare.SideLeft, compare.QualAbsent},
		{"gen/new", compare.Renamed, 0, 0},
		{"gone", compare.Deleted, compare.SideRight, compare.QualUnknown},
	})

	l, r, ev = sides()
	got, err = runQualified(t, l, r, compare.Options{DetectRenames: true, Counterparts: ev}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertQualified(t, got, plain)
}

// TestCounterpartEvidenceClosesWithTheCursor: closing the comparison releases the
// evidence streams exactly once, even when Close is repeated.
func TestCounterpartEvidenceClosesWithTheCursor(t *testing.T) {
	for _, renames := range []bool{false, true} {
		closed := 0
		ev := &compare.CounterpartEvidence{
			Left:  evidence(t, worktree.ScopedCoverage(), newPathSet(t, nil, &closed, "a")),
			Right: evidence(t, recordedCoverage(t, nil), nil),
		}
		cur, err := compare.CompareStream(scantest.CanonicalCursor(nil, nil),
			scantest.CanonicalCursor([]worktree.Entry{reg(t, "a", hexA)}, nil),
			compare.Options{DetectRenames: renames, Counterparts: ev})
		if err != nil {
			t.Fatal(err)
		}
		_ = cur.Close()
		_ = cur.Close()
		if closed != 1 {
			t.Errorf("renames=%v: covered set closed %d times, want 1", renames, closed)
		}
	}
}

// TestCompareWithoutEvidenceLeavesCounterpartZero: callers that pass no evidence
// keep the previous change values exactly.
func TestCompareWithoutEvidenceLeavesCounterpartZero(t *testing.T) {
	cur, err := compare.CompareStream(scantest.CanonicalCursor([]worktree.Entry{reg(t, "a", hexA)}, nil),
		scantest.CanonicalCursor([]worktree.Entry{reg(t, "b", hexB)}, nil), compare.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	for _, c := range streamChanges(t, cur) {
		if c.Counterpart.Known() {
			t.Errorf("%s carries counterpart %+v without evidence", c.PrimaryPath(), c.Counterpart)
		}
	}
}
