package restore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	domrestore "awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

type removalEvent struct {
	path string
	kind string
}

type removalRun struct {
	t       *testing.T
	pre     *removalPreflight
	settled map[string][]domrestore.Reason
	order   []string
	peak    int
}

func relPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatalf("rel path %q: %v", s, err)
	}
	return p
}

func recordedCoverage(t *testing.T, stream worktree.FrontierStream) worktree.Coverage {
	t.Helper()
	cur, err := stream.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := worktree.ReduceFrontier(blake3hash.New(), cur)
	if err != nil {
		t.Fatal(err)
	}
	return coverageOver(t, stream, digest)
}

func coverageOver(t *testing.T, stream worktree.FrontierStream, digest worktree.FrontierDigest) worktree.Coverage {
	t.Helper()
	c, err := worktree.RecordedCoverage(stream, digest)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *failingFrontier) coverage(t *testing.T) worktree.Coverage {
	t.Helper()
	_, digest, _ := recordedCoverage(t, scantest.FrontierStream(f.records...)).Frontier()
	return coverageOver(t, f, digest)
}

func frontierOf(t *testing.T, subtreeRoots []string, leaves ...string) worktree.Coverage {
	t.Helper()
	var records []worktree.FrontierRecord
	for _, p := range subtreeRoots {
		records = append(records, worktree.FrontierRecord{Path: relPath(t, p), Limit: worktree.LimitSubtree})
	}
	for _, p := range leaves {
		records = append(records, worktree.FrontierRecord{Path: relPath(t, p), Limit: worktree.LimitNode})
	}
	return recordedCoverage(t, scantest.FrontierStream(records...))
}

func removalOp(t *testing.T, path, kind string) domrestore.PlannedOperation {
	t.Helper()
	desired := domrestore.AbsentNode()
	switch kind {
	case "to-file":
		content, err := blake3hash.New().HashReader(strings.NewReader(path))
		if err != nil {
			t.Fatal(err)
		}
		if desired, err = domrestore.RegularNode(content, 0o644); err != nil {
			t.Fatal(err)
		}
	case "to-symlink":
		target, err := worktree.NewSymlinkTarget("elsewhere")
		if err != nil {
			t.Fatal(err)
		}
		if desired, err = domrestore.SymlinkNode(target); err != nil {
			t.Fatal(err)
		}
	}
	op, err := domrestore.NewPlannedOperation(relPath(t, path), domrestore.DirNode(), desired)
	if err != nil {
		t.Fatal(err)
	}
	if !removesDirectory(op) {
		t.Fatalf("%s %q is not a directory removal: %s", kind, path, op.Kind())
	}
	return op
}

func startRemoval(t *testing.T, coverage worktree.Coverage) *removalRun {
	t.Helper()
	r := &removalRun{t: t, settled: map[string][]domrestore.Reason{}}
	pre, err := newRemovalPreflight(context.Background(), coverage, func(op domrestore.PlannedOperation, reasons []domrestore.Reason) error {
		path := op.Path().String()
		if _, twice := r.settled[path]; twice {
			t.Errorf("%s settled twice", path)
		}
		r.settled[path] = reasons
		r.order = append(r.order, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pre.close() })
	r.pre = pre
	return r
}

func (r *removalRun) feed(events ...removalEvent) {
	r.t.Helper()
	ctx := context.Background()
	for _, e := range events {
		path := relPath(r.t, e.path)
		if err := r.pre.reach(ctx, path); err != nil {
			r.t.Fatalf("reach %s: %v", e.path, err)
		}
		switch e.kind {
		case "delete":
			r.pre.candidate(removalOp(r.t, e.path, e.kind))
		case "to-file", "to-symlink":
			r.pre.candidate(removalOp(r.t, e.path, e.kind))
			r.pre.hold(path, false)
		case "gone":
		case string(domrestore.ReasonPathConflict):
			r.pre.hold(path, false)
		case string(domrestore.ReasonSkippedBoundary):
			r.pre.hold(path, true)
		default:
			r.t.Fatalf("unknown event kind %q", e.kind)
		}
		r.peak = max(r.peak, len(r.pre.open))
	}
}

func (r *removalRun) finish() {
	r.t.Helper()
	if err := r.pre.finish(context.Background()); err != nil {
		r.t.Fatalf("finish: %v", err)
	}
	if len(r.pre.open) != 0 {
		r.t.Errorf("%d directories left unsettled", len(r.pre.open))
	}
}

func TestDirectoryRemovalPreflightMatrix(t *testing.T) {
	const (
		conflict = string(domrestore.ReasonPathConflict)
		skipped  = string(domrestore.ReasonSkippedBoundary)
		unproved = string(domrestore.ReasonAbsenceUnproved)
	)
	none := frontierOf(t, nil)
	cases := []struct {
		name     string
		coverage worktree.Coverage
		events   []removalEvent
		want     map[string][]string
	}{
		{
			name:     "an empty directory stays ready",
			coverage: none,
			events:   []removalEvent{{"d", "delete"}},
			want:     map[string][]string{"d": nil},
		},
		{
			name:     "a directory emptied by ready removals stays ready",
			coverage: none,
			events:   []removalEvent{{"d", "delete"}, {"d/a", "gone"}, {"d/b", "gone"}},
			want:     map[string][]string{"d": nil},
		},
		{
			name:     "a hidden leaf blocks",
			coverage: frontierOf(t, nil, "d/a.log"),
			events:   []removalEvent{{"d", "delete"}},
			want:     map[string][]string{"d": {conflict}},
		},
		{
			name:     "a pruned subtree root blocks",
			coverage: frontierOf(t, []string{"d/cache"}),
			events:   []removalEvent{{"d", "delete"}, {"d/a", "gone"}},
			want:     map[string][]string{"d": {conflict}},
		},
		{
			name:     "a hidden leaf deep below blocks every enclosing removal",
			coverage: frontierOf(t, nil, "d/e/f/a.log"),
			events:   []removalEvent{{"d", "delete"}, {"d/e", "delete"}, {"d/e/f", "delete"}, {"d/g", "delete"}},
			want:     map[string][]string{"d": {conflict}, "d/e": {conflict}, "d/e/f": {conflict}, "d/g": nil},
		},
		{
			name:     "prefix siblings sort between a directory and its hidden child",
			coverage: frontierOf(t, nil, "d/a.log"),
			events:   []removalEvent{{"d", "delete"}, {"d-", "delete"}, {"d.file", "gone"}, {"d0", "delete"}},
			want:     map[string][]string{"d": {conflict}, "d-": nil, "d0": nil},
		},
		{
			name:     "unrelated frontier names do not block",
			coverage: frontierOf(t, []string{"d-/cache", "d0"}, "c/a.log", "d-other", "d.file", "da/x.log"),
			events:   []removalEvent{{"d", "delete"}, {"d-", "gone"}, {"d/a", "gone"}, {"d0x", "delete"}},
			want:     map[string][]string{"d": nil, "d0x": nil},
		},
		{
			name:     "a prefix sibling's own hidden child blocks only that sibling",
			coverage: frontierOf(t, nil, "d-/a.log"),
			events:   []removalEvent{{"d", "delete"}, {"d-", "delete"}, {"d/a", "gone"}},
			want:     map[string][]string{"d": nil, "d-": {conflict}},
		},
		{
			name:     "a surviving observed child blocks",
			coverage: none,
			events:   []removalEvent{{"d", "delete"}, {"d/a", "gone"}, {"d/kept", conflict}},
			want:     map[string][]string{"d": {conflict}},
		},
		{
			name:     "a skipped descendant is an evidence gap",
			coverage: none,
			events:   []removalEvent{{"d", "delete"}, {"d/unreadable", skipped}},
			want:     map[string][]string{"d": {skipped}},
		},
		{
			name:     "several impediments settle the directory once",
			coverage: frontierOf(t, []string{"d/cache"}, "d/a.log", "d/b.log"),
			events:   []removalEvent{{"d", "delete"}, {"d/kept", conflict}, {"d/other", conflict}, {"d/unreadable", skipped}},
			want:     map[string][]string{"d": {conflict, skipped}},
		},
		{
			name:     "a blocked child directory keeps its parent",
			coverage: frontierOf(t, nil, "d/e/a.log"),
			events:   []removalEvent{{"d", "delete"}, {"d/e", "delete"}},
			want:     map[string][]string{"d": {conflict}, "d/e": {conflict}},
		},
		{
			name:     "a directory skipped below keeps its ancestors with both facts",
			coverage: none,
			events:   []removalEvent{{"d", "delete"}, {"d/e", "delete"}, {"d/e/unreadable", skipped}},
			want:     map[string][]string{"d": {conflict, skipped}, "d/e": {skipped}},
		},
		{
			name:     "unknown current coverage cannot prove emptiness",
			coverage: worktree.UnknownCoverage(),
			events:   []removalEvent{{"d", "delete"}},
			want:     map[string][]string{"d": {unproved}},
		},
		{
			name:     "replacement by a file needs the same emptiness",
			coverage: frontierOf(t, nil, "d/a.log"),
			events:   []removalEvent{{"d", "to-file"}, {"e", "to-file"}, {"e/a", "gone"}},
			want:     map[string][]string{"d": {conflict}, "e": nil},
		},
		{
			name:     "replacement by a symlink needs the same emptiness",
			coverage: frontierOf(t, []string{"d/cache"}),
			events:   []removalEvent{{"d", "to-symlink"}, {"e", "to-symlink"}, {"e/a", "gone"}},
			want:     map[string][]string{"d": {conflict}, "e": nil},
		},
		{
			name:     "a replacement leaves a node in the directory above it",
			coverage: none,
			events:   []removalEvent{{"d", "delete"}, {"d/e", "to-file"}, {"d/e/a", "gone"}},
			want:     map[string][]string{"d": {conflict}, "d/e": nil},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := startRemoval(t, tc.coverage)
			r.feed(tc.events...)
			r.finish()
			got := map[string][]string{}
			for path, reasons := range r.settled {
				var tokens []string
				for _, reason := range reasons {
					tokens = append(tokens, string(reason))
				}
				got[path] = tokens
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("settled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDirectoryRemovalSettlesChildrenBeforeParents(t *testing.T) {
	r := startRemoval(t, frontierOf(t, nil))
	r.feed(removalEvent{"a", "delete"}, removalEvent{"a/b", "delete"}, removalEvent{"a/b/c", "delete"}, removalEvent{"z", "gone"})
	r.finish()
	if want := []string{"a/b/c", "a/b", "a"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("settle order = %v, want %v", r.order, want)
	}
}

func TestDirectoryRemovalPendingStateIsBoundedByPathShape(t *testing.T) {
	t.Run("wide", func(t *testing.T) {
		const dirs, children = 2000, 3
		r := startRemoval(t, frontierOf(t, nil))
		for i := range dirs {
			dir := fmt.Sprintf("d%05d", i)
			r.feed(removalEvent{dir, "delete"})
			for c := range children {
				r.feed(removalEvent{fmt.Sprintf("%s/f%d", dir, c), "gone"})
			}
		}
		r.finish()
		if r.peak != 1 {
			t.Errorf("peak pending directories = %d across %d sibling removals, want 1", r.peak, dirs)
		}
		if len(r.settled) != dirs {
			t.Errorf("settled %d directories, want %d", len(r.settled), dirs)
		}
	})
	t.Run("wide under one parent with prefix siblings", func(t *testing.T) {
		const dirs = 2000
		r := startRemoval(t, frontierOf(t, nil))
		r.feed(removalEvent{"root", "delete"})
		for i := range dirs {
			dir := fmt.Sprintf("root/d%05d", i)
			r.feed(removalEvent{dir, "delete"}, removalEvent{dir + "-", "delete"}, removalEvent{dir + "/f", "gone"})
		}
		r.finish()
		if r.peak != 3 {
			t.Errorf("peak pending directories = %d, want the parent, one directory and its prefix sibling", r.peak)
		}
	})
	t.Run("nested", func(t *testing.T) {
		const depth = 64
		r := startRemoval(t, frontierOf(t, nil))
		path := "n"
		for range depth {
			r.feed(removalEvent{path, "delete"})
			path += "/n"
		}
		r.feed(removalEvent{path, "gone"})
		r.finish()
		if r.peak != depth {
			t.Errorf("peak pending directories = %d, want the nesting depth %d", r.peak, depth)
		}
	})
}

type failingFrontier struct {
	records []worktree.FrontierRecord
	failAt  int
	err     error
	read    int
}

func (f *failingFrontier) Open(context.Context) (worktree.FrontierCursor, error) { return f, nil }

func (f *failingFrontier) Next() bool {
	if f.read >= len(f.records) || f.read == f.failAt {
		return false
	}
	f.read++
	return true
}

func (f *failingFrontier) Record() worktree.FrontierRecord { return f.records[f.read-1] }

func (f *failingFrontier) Err() error {
	if f.read == f.failAt {
		return f.err
	}
	return nil
}

func (f *failingFrontier) Close() error { return nil }

func frontierLeaves(t *testing.T, paths ...string) []worktree.FrontierRecord {
	t.Helper()
	var out []worktree.FrontierRecord
	for _, p := range paths {
		out = append(out, worktree.FrontierRecord{Path: relPath(t, p), Limit: worktree.LimitNode})
	}
	return out
}

func TestDirectoryRemovalReadsTheFrontierToItsEndAfterAConflict(t *testing.T) {
	records := frontierLeaves(t, "d/a.log", "x/1.log", "y/2.log", "z/3.log")
	stream := &failingFrontier{records: records, failAt: -1}
	r := startRemoval(t, stream.coverage(t))
	r.feed(removalEvent{"d", "delete"}, removalEvent{"e", "gone"})
	r.finish()
	if stream.read != len(records) {
		t.Errorf("read %d of %d frontier records", stream.read, len(records))
	}
	if got := r.settled["d"]; len(got) != 1 || got[0] != domrestore.ReasonPathConflict {
		t.Errorf("d settled with %v, want path-conflict", got)
	}
}

func TestDirectoryRemovalPropagatesATerminalFrontierError(t *testing.T) {
	boom := errors.New("frontier payload truncated")
	records := frontierLeaves(t, "a.log", "d/a.log", "z.log")
	for _, failAt := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("after %d records", failAt), func(t *testing.T) {
			stream := &failingFrontier{records: records, failAt: failAt, err: boom}
			r := startRemoval(t, stream.coverage(t))
			ctx := context.Background()
			err := r.pre.reach(ctx, relPath(t, "d"))
			if err == nil {
				r.pre.candidate(removalOp(t, "d", "delete"))
				err = r.pre.finish(ctx)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want the stream's terminal failure", err)
			}
			if len(r.settled) != 0 {
				t.Errorf("settled %v from a frontier that never completed", r.settled)
			}
		})
	}
}

func TestDirectoryRemovalStopsOnCancellation(t *testing.T) {
	r := startRemoval(t, frontierOf(t, nil, "d/a.log", "z.log"))
	r.feed(removalEvent{"d", "delete"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.pre.finish(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("finish = %v, want context.Canceled", err)
	}
	if len(r.settled) != 0 {
		t.Errorf("settled %v after cancellation", r.settled)
	}
}

func TestDirectoryRemovalRejectsAnOutOfOrderFrontier(t *testing.T) {
	records := frontierLeaves(t, "z.log", "d/a.log")
	stream := &failingFrontier{records: records, failAt: -1}
	r := startRemoval(t, stream.coverage(t))
	r.feed(removalEvent{"d", "delete"})
	if err := r.pre.finish(context.Background()); !errors.Is(err, worktree.ErrFrontierOutOfOrder) {
		t.Fatalf("finish = %v, want ErrFrontierOutOfOrder", err)
	}
}
