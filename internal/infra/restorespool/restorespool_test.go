package restorespool

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
)

type harness struct {
	t      *testing.T
	root   string
	layout paths.Layout
	hasher hashing.Hasher
}

func setup(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	layout := paths.New(root)
	if err := os.MkdirAll(layout.TmpDir(), paths.DirPerm); err != nil {
		t.Fatalf("mkdir store tmp: %v", err)
	}
	hasher := blake3hash.New()
	return &harness{t: t, root: root, layout: layout, hasher: hasher}
}

func (h *harness) id() restore.OperationID {
	h.t.Helper()
	id, err := restore.NewOperationID(1, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8}))
	if err != nil {
		h.t.Fatalf("operation id: %v", err)
	}
	return id
}

func (h *harness) relPath(p string) worktree.RelPath {
	h.t.Helper()
	rp, err := worktree.ParseRelPath(p)
	if err != nil {
		h.t.Fatalf("rel path %q: %v", p, err)
	}
	return rp
}

func (h *harness) regular(seed byte, mode uint32) restore.NodeState {
	h.t.Helper()
	c, err := h.hasher.HashReader(bytes.NewReader([]byte{seed}))
	if err != nil {
		h.t.Fatalf("hash: %v", err)
	}
	n, err := restore.RegularNode(c, mode)
	if err != nil {
		h.t.Fatalf("regular node: %v", err)
	}
	return n
}

func (h *harness) symlink(target string) restore.NodeState {
	h.t.Helper()
	tgt, err := worktree.NewSymlinkTarget(target)
	if err != nil {
		h.t.Fatalf("symlink target: %v", err)
	}
	n, err := restore.SymlinkNode(tgt)
	if err != nil {
		h.t.Fatalf("symlink node: %v", err)
	}
	return n
}

func (h *harness) op(path string, current, desired restore.NodeState) restore.PlannedOperation {
	h.t.Helper()
	op, err := restore.NewPlannedOperation(h.relPath(path), current, desired)
	if err != nil {
		h.t.Fatalf("planned operation %q: %v", path, err)
	}
	return op
}

// drain pulls a cursor into (path, kind) pairs. Production filters the same
// cursor by rank and depth; this materializer is test-only by design.
func drain(t *testing.T, s *Spool) []string {
	t.Helper()
	cur, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var out []string
	for cur.Next() {
		op := cur.Operation()
		out = append(out, op.Path().String()+":"+op.Kind().String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return out
}

func TestSpoolRoundTripsEveryMutatingShape(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()

	// Canonical ascending path order, one of every mutating kind.
	ops := []restore.PlannedOperation{
		h.op("a/created.txt", restore.AbsentNode(), h.regular(1, 0o644)),
		h.op("b/link", restore.AbsentNode(), h.symlink("../a/created.txt")),
		h.op("c/replaced.txt", h.regular(2, 0o644), h.regular(3, 0o755)),
		h.op("d/typechanged", restore.DirNode(), h.regular(4, 0o600)),
		h.op("e/deleted.txt", h.regular(5, 0o644), restore.AbsentNode()),
		h.op("f/dir", restore.DirNode(), restore.AbsentNode()),
	}
	for _, op := range ops {
		if err := s.Add(op); err != nil {
			t.Fatalf("Add %q: %v", op.Path(), err)
		}
	}
	if err := s.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if s.Count() != len(ops) {
		t.Errorf("Count() = %d, want %d", s.Count(), len(ops))
	}

	got := drain(t, s)
	want := []string{
		"a/created.txt:create",
		"b/link:restore-symlink",
		"c/replaced.txt:replace",
		"d/typechanged:type-change",
		"e/deleted.txt:delete-file",
		"f/dir:delete-empty-directory",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("spool round trip = %v, want %v", got, want)
	}

	// A cursor is re-openable: apply walks the spool once per rank and once per
	// directory depth, so a single-use stream would silently truncate the commit.
	if again := drain(t, s); strings.Join(again, ",") != strings.Join(want, ",") {
		t.Errorf("second cursor = %v, want %v", again, want)
	}
}

func TestSpoolPreservesNodeIdentityExactly(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()

	current := h.regular(2, 0o600)
	desired := h.regular(3, 0o755)
	if err := s.Add(h.op("x.txt", current, desired)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(h.op("y", restore.AbsentNode(), h.symlink("../target"))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	cur, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = cur.Close() }()

	if !cur.Next() {
		t.Fatal("no operations read back")
	}
	got := cur.Operation()
	// The precondition and desired state are what apply guards and writes; a codec
	// that dropped the mode or the hash would let apply write the wrong bytes or
	// pass a guard it should have failed.
	if !got.Current().Equal(current) {
		t.Errorf("current precondition did not survive the spool: %s want %s", got.Current().Describe(), current.Describe())
	}
	if !got.Desired().Equal(desired) {
		t.Errorf("desired state did not survive the spool: %s want %s", got.Desired().Describe(), desired.Describe())
	}
	if got.Desired().Content() != desired.Content() {
		t.Errorf("desired content hash did not survive: %s want %s", got.Desired().Content(), desired.Content())
	}

	if !cur.Next() {
		t.Fatal("second operation missing")
	}
	link := cur.Operation()
	if link.Desired().SymlinkTarget().String() != "../target" {
		t.Errorf("symlink target did not survive: %q", link.Desired().SymlinkTarget())
	}
}

func TestSpoolRefusesWorkThatMustNeverBeReplayed(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()

	blocked, err := restore.NewBlockedOperation(h.relPath("a"), restore.AbsentNode(), h.regular(1, 0o644), restore.ReasonHashOnlyContent)
	if err != nil {
		t.Fatalf("NewBlockedOperation: %v", err)
	}
	if err := s.Add(blocked); err == nil {
		t.Error("a blocked operation was spooled for replay")
	}
	equal := h.regular(1, 0o644)
	if err := s.Add(h.op("b", equal, equal)); err == nil {
		t.Error("an equal operation was spooled as work")
	}
}

func TestSpoolRequiresCanonicalOrder(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()

	if err := s.Add(h.op("b.txt", restore.AbsentNode(), h.regular(1, 0o644))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(h.op("a.txt", restore.AbsentNode(), h.regular(1, 0o644))); err == nil {
		t.Error("an out-of-order operation was accepted; the executor's rank and depth passes depend on canonical order")
	}
	if err := s.Add(h.op("b.txt", restore.AbsentNode(), h.regular(2, 0o644))); err == nil {
		t.Error("a duplicate path was accepted")
	}
}

func TestMaxDirectoryDepthTracksDeletionsOnly(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()

	if s.MaxDirectoryDepth() != 0 {
		t.Errorf("an empty spool reports directory depth %d", s.MaxDirectoryDepth())
	}
	// A deeply nested *write* must not raise the directory-deletion depth: the
	// executor uses it to bound its descending deletion passes, and an inflated
	// value would make it walk passes that can never match.
	if err := s.Add(h.op("a/b/c/d/e/deep.txt", restore.AbsentNode(), h.regular(1, 0o644))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s.MaxDirectoryDepth() != 0 {
		t.Errorf("a write raised MaxDirectoryDepth to %d", s.MaxDirectoryDepth())
	}
	if err := s.Add(h.op("x/y/z", restore.DirNode(), restore.AbsentNode())); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s.MaxDirectoryDepth() != 3 {
		t.Errorf("MaxDirectoryDepth() = %d, want 3", s.MaxDirectoryDepth())
	}
}

func TestSpoolLifecycleGuards(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	if _, err := s.Open(context.Background()); err == nil {
		t.Error("an unsealed spool was readable; a cursor could see a partial flush")
	}
	if err := s.Add(h.op("a.txt", restore.AbsentNode(), h.regular(1, 0o644))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := s.Seal(); err != nil {
		t.Errorf("Seal is not idempotent: %v", err)
	}
	if err := s.Add(h.op("b.txt", restore.AbsentNode(), h.regular(1, 0o644))); err == nil {
		t.Error("a sealed spool accepted more work")
	}
	if err := s.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := s.Discard(); err != nil {
		t.Errorf("Discard is not idempotent: %v", err)
	}
}

func TestDiscardRemovesOnlyAwaOwnedTemp(t *testing.T) {
	h := setup(t)
	id := h.id()
	s, err := Open(h.layout, id)
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	if err := s.Add(h.op("a.txt", restore.AbsentNode(), h.regular(1, 0o644))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// A foreign artifact sharing the temp directory must survive: the spool cleans
	// its own directory, never the temp area.
	foreign := filepath.Join(h.layout.TmpDir(), "not-ours.tmp")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write foreign temp: %v", err)
	}
	spoolDir := filepath.Join(h.layout.TmpDir(), dirPrefix+id.String())
	if _, err := os.Stat(spoolDir); err != nil {
		t.Fatalf("spool directory missing: %v", err)
	}
	if err := s.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(spoolDir); !os.IsNotExist(err) {
		t.Errorf("spool directory survived Discard: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("Discard removed a foreign temp artifact: %v", err)
	}
	if _, err := os.Stat(h.layout.TmpDir()); err != nil {
		t.Errorf("Discard removed the shared temp directory: %v", err)
	}
}

func TestSpoolLandsUnderTheStoreTempDirectory(t *testing.T) {
	h := setup(t)
	id := h.id()
	s, err := Open(h.layout, id)
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()
	// gc and doctor already classify everything under .awa/store/tmp by age, so
	// landing here is what makes an abandoned spool reclaimable without any new
	// sweeping logic.
	want := filepath.Join(h.layout.TmpDir(), dirPrefix+id.String(), opsName)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("spool is not at the expected temp location %s: %v", want, err)
	}
}

func TestOpenRefusesToAdoptAnExistingSpoolFile(t *testing.T) {
	h := setup(t)
	id := h.id()
	s, err := Open(h.layout, id)
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()
	if _, err := Open(h.layout, id); err == nil {
		t.Error("a second spool for the same id was opened; leftover state must never be silently extended")
	}
}

func TestCursorRejectsATamperedSpool(t *testing.T) {
	cases := []struct {
		name    string
		rewrite func(original string) string
	}{
		{
			name:    "truncated",
			rewrite: func(string) string { return "" },
		},
		{
			name: "kind disagrees with the states it describes",
			rewrite: func(s string) string {
				return strings.Replace(s, `"kind":"create"`, `"kind":"delete-file"`, 1)
			},
		},
		{
			name: "unknown field",
			rewrite: func(s string) string {
				return strings.Replace(s, `{"path":`, `{"surprise":1,"path":`, 1)
			},
		},
		{
			name: "path escapes the project root",
			rewrite: func(s string) string {
				return strings.Replace(s, `"path":"a.txt"`, `"path":"../outside"`, 1)
			},
		},
		{
			name: "absent node carries state",
			rewrite: func(s string) string {
				return strings.Replace(s, `"current":{"present":false}`, `"current":{"present":false,"mode":420}`, 1)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			id := h.id()
			s, err := Open(h.layout, id)
			if err != nil {
				t.Fatalf("Open spool: %v", err)
			}
			defer func() { _ = s.Discard() }()
			if err := s.Add(h.op("a.txt", restore.AbsentNode(), h.regular(1, 0o644))); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if err := s.Seal(); err != nil {
				t.Fatalf("Seal: %v", err)
			}

			opsPath := filepath.Join(h.layout.TmpDir(), dirPrefix+id.String(), opsName)
			data, err := os.ReadFile(opsPath) //nolint:gosec // test fixture path
			if err != nil {
				t.Fatalf("read spool: %v", err)
			}
			// The encoder writes compact JSON; normalize so the rewrites above match.
			original := string(data)
			mutated := tc.rewrite(original)
			if tc.name != "truncated" && mutated == original {
				t.Fatalf("rewrite %q did not change the spool; fixture drifted from the codec:\n%s", tc.name, original)
			}
			if err := os.WriteFile(opsPath, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write spool: %v", err)
			}

			cur, err := s.Open(context.Background())
			if err != nil {
				t.Fatalf("Open cursor: %v", err)
			}
			defer func() { _ = cur.Close() }()
			for cur.Next() { //nolint:revive // draining is the point
			}
			if cur.Err() == nil {
				t.Fatalf("a %s spool was read without error", tc.name)
			}
		})
	}
}

func TestCursorHonorsCancellation(t *testing.T) {
	h := setup(t)
	s, err := Open(h.layout, h.id())
	if err != nil {
		t.Fatalf("Open spool: %v", err)
	}
	defer func() { _ = s.Discard() }()
	if err := s.Add(h.op("a.txt", restore.AbsentNode(), h.regular(1, 0o644))); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cur, err := s.Open(ctx)
	if err != nil {
		t.Fatalf("Open cursor: %v", err)
	}
	defer func() { _ = cur.Close() }()
	if cur.Next() {
		t.Error("a cancelled cursor produced an operation")
	}
	if cur.Err() == nil {
		t.Error("a cancelled cursor reported a clean end of stream")
	}
}

func TestOpenRequiresAnOperationID(t *testing.T) {
	h := setup(t)
	if _, err := Open(h.layout, restore.OperationID{}); err == nil {
		t.Error("a spool was opened without an operation id")
	}
}
