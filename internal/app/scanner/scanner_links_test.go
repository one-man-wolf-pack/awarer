package scanner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// TestSymlinkAsLinkDefault proves that with follow_symlinks=false a symlink is an
// entry recorded by its target, and changing the target's content does not change
// the symlink entry (only changing the link itself does).
func TestSymlinkAsLinkDefault(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "real.txt", "hello")
	scantest.Symlink(t, root, "link.txt", "real.txt")

	base := run(t, proj, config.Defaults())
	link, ok := findEntry(base, "link.txt")
	if !ok || link.Kind != worktree.KindSymlink {
		t.Fatalf("link.txt not recorded as symlink entry")
	}
	if link.Symlink.String() != "real.txt" {
		t.Errorf("target = %q, want real.txt", link.Symlink.String())
	}

	// Changing the target's content must not change the symlink entry: the link is
	// identified by where it points, not by what lives there.
	scantest.Write(t, root, "real.txt", "different content")
	after := run(t, proj, config.Defaults())
	link2, _ := findEntry(after, "link.txt")
	if link2.Symlink.String() != "real.txt" {
		t.Errorf("symlink entry changed when only target content changed")
	}
}

// TestSymlinkTargetChangeChangesEntry proves re-pointing the link changes its
// entry (and thus the tree hash).
func TestSymlinkTargetChangeChangesEntry(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "a.txt", "a")
	scantest.Write(t, root, "b.txt", "b")
	scantest.Symlink(t, root, "link.txt", "a.txt")
	base := run(t, proj, config.Defaults())

	// Re-point the link to b.txt.
	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	scantest.Symlink(t, root, "link.txt", "b.txt")
	after := run(t, proj, config.Defaults())

	if base.TreeHash().String() == after.TreeHash().String() {
		t.Errorf("re-pointing symlink did not change tree hash")
	}
}

// TestFollowSymlinkInsideRoot proves follow_symlinks=true reads through a symlink
// to a file (the content becomes part of the entry) and descends a dir symlink.
func TestFollowSymlinkInsideRoot(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "real.txt", "hello")
	scantest.Write(t, root, "dir/inner.txt", "inner")
	scantest.Symlink(t, root, "link.txt", "real.txt")
	scantest.Symlink(t, root, "dirlink", "dir")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	link, ok := findEntry(res, "link.txt")
	if !ok || link.Kind != worktree.KindRegular {
		t.Fatalf("followed file symlink should be a regular entry, got %+v ok=%v", link, ok)
	}
	if link.Content.IsZero() || !link.Traversal.Followed {
		t.Errorf("followed file should be hashed and flagged as traversed")
	}
	if link.Traversal.SourcePath.String() != "link.txt" || link.Traversal.Depth != 1 {
		t.Errorf("followed file provenance = %+v, want source=link.txt depth=1", link.Traversal)
	}
	inner, ok := findEntry(res, "dirlink/inner.txt")
	if !ok {
		t.Fatalf("follow mode did not descend into dir symlink")
	}
	// A node under a followed directory names the symlink it was reached through.
	if !inner.Traversal.Followed || inner.Traversal.SourcePath.String() != "dirlink" {
		t.Errorf("descendant provenance = %+v, want followed via dirlink", inner.Traversal)
	}
}

// TestFollowSymlinkRootEscapeRejected proves a symlink pointing outside the root
// is skipped by default and followed only with allow_symlink_root_escape.
func TestFollowSymlinkRootEscapeRejected(t *testing.T) {
	scantest.RequireSymlinks(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Symlink(t, root, "escape", filepath.Join(outside, "secret.txt"))

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	s, ok := findSkipped(res, "escape")
	if !ok || s.Reason != worktree.ReasonSymlinkRootEscape {
		t.Fatalf("root escape not rejected; skipped=%+v ok=%v", s, ok)
	}
	// The skipped symlink keeps its full model: target, the link's own stat/size,
	// and provenance (a top-level link is reached directly).
	if s.Symlink.String() != filepath.Join(outside, "secret.txt") {
		t.Errorf("skipped escape target = %q, want the link target", s.Symlink.String())
	}
	if s.Size <= 0 || s.Stat.MtimeNs == 0 {
		t.Errorf("skipped escape lost the link's stat/size: size=%d stat=%+v", s.Size, s.Stat)
	}
	if s.Traversal.Followed {
		t.Errorf("a top-level skipped link should be directly reached, got %+v", s.Traversal)
	}

	cfg.Scope.AllowSymlinkRootEscape = true
	res2 := run(t, proj, cfg)
	if e, ok := findEntry(res2, "escape"); !ok || e.Kind != worktree.KindRegular {
		t.Errorf("escape not followed when allowed; entry=%+v ok=%v", e, ok)
	}
}

// TestFollowSymlinkToMissingOutsideTargetRejected proves a link pointing outside
// the root is rejected as a root escape from the link alone, even when the target
// does not exist. The boundary must not depend on the state of files outside the
// project: an unreadable or deleted external target must not downgrade the escape
// to a plain (broken) symlink entry.
func TestFollowSymlinkToMissingOutsideTargetRejected(t *testing.T) {
	scantest.RequireSymlinks(t)
	outside := t.TempDir()
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Symlink(t, root, "escape", filepath.Join(outside, "missing.txt"))

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	s, ok := findSkipped(res, "escape")
	if !ok || s.Reason != worktree.ReasonSymlinkRootEscape {
		t.Fatalf("missing out-of-root target not rejected as escape; skipped=%+v ok=%v", s, ok)
	}
	if _, ok := findEntry(res, "escape"); ok {
		t.Errorf("escaping link must not be recorded as a plain symlink entry")
	}
}

// TestFollowSymlinkEscapeThroughSymlinkedDirRejected proves the root boundary
// resolves symlink components inside the target path: link -> dirlink/file, where
// dirlink is an in-root symlink to an outside directory, must be rejected as a root
// escape. A purely lexical check would be fooled by the in-root-looking string
// <root>/dirlink/file and read the escaped content.
func TestFollowSymlinkEscapeThroughSymlinkedDirRejected(t *testing.T) {
	scantest.RequireSymlinks(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	// dirlink (inside the root) points to the outside directory; link routes through
	// it to reach outside/file by an in-root-looking path.
	scantest.Symlink(t, root, "dirlink", outside)
	scantest.Symlink(t, root, "link", "dirlink/file")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	s, ok := findSkipped(res, "link")
	if !ok || s.Reason != worktree.ReasonSymlinkRootEscape {
		t.Fatalf("escape through a symlinked dir not rejected; skipped=%+v ok=%v", s, ok)
	}
	if e, ok := findEntry(res, "link"); ok && !e.Content.IsZero() {
		t.Errorf("escaped content leaked into an entry: %+v", e)
	}
}

// TestFollowSymlinkEscapeThroughSymlinkedDirMissingTargetRejected proves the root
// boundary holds even when the escape routes through a symlinked directory whose
// target does not exist: dirlink -> <outside>/missing-dir, link -> dirlink/file.
// The escape must be decided from the links alone, not downgraded to a broken
// symlink because the out-of-root location is absent.
func TestFollowSymlinkEscapeThroughSymlinkedDirMissingTargetRejected(t *testing.T) {
	scantest.RequireSymlinks(t)
	outside := t.TempDir()
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	// dirlink points outside the root to a directory that does not exist.
	scantest.Symlink(t, root, "dirlink", filepath.Join(outside, "missing-dir"))
	scantest.Symlink(t, root, "link", "dirlink/file")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	s, ok := findSkipped(res, "link")
	if !ok || s.Reason != worktree.ReasonSymlinkRootEscape {
		t.Fatalf("escape through a symlinked dir with a missing target not rejected; skipped=%+v ok=%v", s, ok)
	}
	if _, ok := findEntry(res, "link"); ok {
		t.Errorf("escaping link must not be recorded as a plain (broken) symlink entry")
	}
}

// TestFollowSymlinkInRootMissingTargetNotEscape proves the tolerant resolver does
// not over-reject: a link routed through a symlinked directory that points at a
// missing IN-root location is a broken link, not a root escape. The boundary must
// distinguish "missing and outside" from "missing and inside".
func TestFollowSymlinkInRootMissingTargetNotEscape(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	// dirlink points within the root to a directory that does not exist.
	scantest.Symlink(t, root, "dirlink", "missing-dir")
	scantest.Symlink(t, root, "link", "dirlink/file")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	if s, ok := findSkipped(res, "link"); ok && s.Reason == worktree.ReasonSymlinkRootEscape {
		t.Errorf("an in-root broken link was wrongly classified as a root escape")
	}
	// It is recorded as a plain (broken) symlink entry, since it never escapes.
	if e, ok := findEntry(res, "link"); !ok || e.Kind != worktree.KindSymlink {
		t.Errorf("in-root broken link not recorded as a symlink entry; entry=%+v ok=%v", e, ok)
	}
}

// TestSkippedSymlinkTargetAffectsTreeHash proves a skipped symlink carries its
// target into the tree hash: re-pointing a root-escaping link to a different
// out-of-root target changes the tree, even though the skip reason is unchanged.
func TestSkippedSymlinkTargetAffectsTreeHash(t *testing.T) {
	scantest.RequireSymlinks(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Symlink(t, root, "escape", filepath.Join(outside, "a.txt"))

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true // escapes are skipped (not allowed)
	base := run(t, proj, cfg)
	if s, ok := findSkipped(base, "escape"); !ok || s.Reason != worktree.ReasonSymlinkRootEscape {
		t.Fatalf("escape not skipped: %+v ok=%v", s, ok)
	}

	// Re-point the link to a different out-of-root target; same skip reason.
	if err := os.Remove(filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	scantest.Symlink(t, root, "escape", filepath.Join(outside, "b.txt"))
	after := run(t, proj, cfg)

	if base.TreeHash().String() == after.TreeHash().String() {
		t.Errorf("re-pointing a skipped symlink did not change the tree hash")
	}
}

// TestFollowSymlinkCycleRejected proves a cyclic dir symlink is skipped loudly
// rather than looping forever — and is caught on the link itself, before its
// target is descended and re-scanned as a duplicate.
func TestFollowSymlinkCycleRejected(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "a/keep.txt", "x")
	// a/loop points back to its own parent directory a, forming a cycle.
	scantest.Symlink(t, root, "a/loop", "../a")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	cycle, ok := findSkipped(res, "a/loop")
	if !ok || cycle.Reason != worktree.ReasonSymlinkCycle {
		t.Fatalf("expected a/loop to be a symlink-cycle skip, skipped=%v", res.MaterializeSkipped())
	}
	if cycle.Symlink.String() != "../a" {
		t.Errorf("cycle target = %q, want ../a", cycle.Symlink.String())
	}
	// The link is reached directly by the base walk (not through another symlink),
	// and the cycle is detected from the link alone, so it is not marked followed.
	if cycle.Traversal.Followed {
		t.Errorf("a directly-reached cyclic link must not be marked followed: %+v", cycle.Traversal)
	}
	// The cyclic link must not be descended: its target's contents must not reappear
	// duplicated under the link's path.
	if _, ok := findEntry(res, "a/loop"); ok {
		t.Errorf("cyclic link emitted as a directory")
	}
	if _, ok := findEntry(res, "a/loop/keep.txt"); ok {
		t.Errorf("cyclic link descended, duplicating its target's contents")
	}
}

// TestFollowSymlinkCycleThroughOrdinaryDir proves cycle detection tracks ordinary
// real subdirectories entered during a followed-symlink descent, not just
// symlink-target directories. outer -> realdir; inside realdir/sub a link self ->
// "." re-enters sub. The cyclic link must be skipped — and its contents must not be
// scanned again under the cyclic path.
func TestFollowSymlinkCycleThroughOrdinaryDir(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "realdir/sub/file.txt", "x")
	scantest.Symlink(t, root, "realdir/sub/self", ".")
	scantest.Symlink(t, root, "outer", "realdir")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	// Reached through the outer symlink, the self link inside the ordinary sub
	// directory must be a cycle skip.
	if s, ok := findSkipped(res, "outer/sub/self"); !ok || s.Reason != worktree.ReasonSymlinkCycle {
		t.Fatalf("self-link through an ordinary dir not detected as a cycle; skipped=%v", res.MaterializeSkipped())
	}
	// No path may descend through the cyclic link and duplicate sub's contents.
	for _, e := range res.MaterializeEntries() {
		if strings.Contains(e.Path.String(), "/self/") {
			t.Errorf("cyclic link was descended, duplicating contents at %q", e.Path.String())
		}
	}
}

// TestFollowSymlinkDepthLimit proves traversal stops at symlink_max_depth.
func TestFollowSymlinkDepthLimit(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "d1/d2/deep.txt", "x")
	// link1 -> d1; inside d1, link2 -> d2. With max depth 1, link2 is too deep.
	scantest.Symlink(t, root, "link1", "d1")
	scantest.Symlink(t, root, "d1/link2", "d2")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.SymlinkMaxDepth = 1
	res := run(t, proj, cfg)

	var deep worktree.SkippedInput
	for _, s := range res.MaterializeSkipped() {
		if s.Reason == worktree.ReasonSymlinkDepthExceeded {
			deep = s
		}
	}
	if deep.Reason != worktree.ReasonSymlinkDepthExceeded {
		t.Fatalf("expected a depth-exceeded skip, skipped=%v", res.MaterializeSkipped())
	}
	// The too-deep link keeps its target and provenance (reached through link1).
	if deep.Symlink.String() != "d2" {
		t.Errorf("depth-exceeded target = %q, want d2", deep.Symlink.String())
	}
	if !deep.Traversal.Followed || deep.Traversal.SourcePath.String() != "link1" || deep.Traversal.Depth != 1 {
		t.Errorf("depth-exceeded provenance = %+v, want followed via link1 depth 1", deep.Traversal)
	}
}

// TestFollowSymlinkChainDepthLimit proves symlink_max_depth counts every hop of a
// symlink-to-symlink chain, not just the first. a -> b -> c -> real.txt is three
// hops, so with max depth 1 it must be skipped as too deep — a single EvalSymlinks
// would have collapsed the chain and wrongly accepted it at depth 1.
func TestFollowSymlinkChainDepthLimit(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "real.txt", "hello")
	scantest.Symlink(t, root, "c", "real.txt")
	scantest.Symlink(t, root, "b", "c")
	scantest.Symlink(t, root, "a", "b")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.SymlinkMaxDepth = 1
	res := run(t, proj, cfg)

	s, ok := findSkipped(res, "a")
	if !ok || s.Reason != worktree.ReasonSymlinkDepthExceeded {
		t.Fatalf("symlink chain a->b->c->real not depth-limited; skipped=%+v ok=%v", s, ok)
	}
	if _, ok := findEntry(res, "a"); ok {
		t.Errorf("over-deep symlink chain should not be followed to an entry")
	}
}

// TestFollowSymlinkChainCycleRejected proves a symlink-to-symlink loop (a -> b ->
// a) is detected as a cycle, not mis-recorded as a plain link. EvalSymlinks would
// return an opaque resolve error here, dropping the link into the broken-link path
// instead of reporting ReasonSymlinkCycle.
func TestFollowSymlinkChainCycleRejected(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Symlink(t, root, "a", "b")
	scantest.Symlink(t, root, "b", "a")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	var sawCycle bool
	for _, s := range res.MaterializeSkipped() {
		if s.Reason == worktree.ReasonSymlinkCycle {
			sawCycle = true
		}
	}
	if !sawCycle {
		t.Fatalf("symlink-to-symlink cycle not reported as a cycle; skipped=%v", res.MaterializeSkipped())
	}
}

// TestScopeIncludeInsideFollowedSymlink proves a scoped include living beneath a
// followed directory symlink is reached: the symlink must be treated as a
// directory ancestor for scope pruning, or the include is silently dropped.
func TestScopeIncludeInsideFollowedSymlink(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "dir/inner.txt", "inner")
	scantest.Write(t, root, "dir/other.txt", "other")
	scantest.Symlink(t, root, "dirlink", "dir")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.Include = []string{"dirlink/inner.txt"}
	res := run(t, proj, cfg)

	if _, ok := findEntry(res, "dirlink/inner.txt"); !ok {
		t.Fatalf("scoped include under a followed symlink was dropped; entries=%v", res.MaterializeEntries())
	}
	// The narrower scope must still exclude the sibling not named in the include.
	if _, ok := findEntry(res, "dirlink/other.txt"); ok {
		t.Errorf("scope include leaked a sibling file outside the include")
	}
}

// TestFollowSymlinkChainProvenanceDepth proves a followed entry's provenance
// records the true number of symlink hops taken: a -> b -> real.txt is two hops,
// so the entry's Traversal.Depth must be 2, not the base depth 1.
func TestFollowSymlinkChainProvenanceDepth(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "real.txt", "hello")
	scantest.Symlink(t, root, "b", "real.txt")
	scantest.Symlink(t, root, "a", "b")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	res := run(t, proj, cfg)

	a, ok := findEntry(res, "a")
	if !ok || a.Kind != worktree.KindRegular {
		t.Fatalf("two-hop chain a->b->real.txt not followed to a regular entry; entry=%+v ok=%v", a, ok)
	}
	if !a.Traversal.Followed || a.Traversal.SourcePath.String() != "a" || a.Traversal.Depth != 2 {
		t.Errorf("chain provenance = %+v, want followed via a depth 2", a.Traversal)
	}
}

// TestScopeIncludeInsideNestedFollowedSymlink proves a scoped include living
// beneath a symlink that is itself nested inside an already-followed directory
// symlink is reached: the nested symlink must also be treated as a directory
// ancestor for scope pruning, or the include is silently dropped before the nested
// link is resolved.
func TestScopeIncludeInsideNestedFollowedSymlink(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "realdir/deep/file.txt", "deep")
	// nestedlink lives inside realdir and points to its sibling deep directory.
	scantest.Symlink(t, root, "realdir/nestedlink", "deep")
	// outer is the already-followed directory symlink.
	scantest.Symlink(t, root, "outer", "realdir")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.Include = []string{"outer/nestedlink/file.txt"}
	res := run(t, proj, cfg)

	if _, ok := findEntry(res, "outer/nestedlink/file.txt"); !ok {
		t.Fatalf("scoped include under a nested followed symlink was dropped; entries=%v", res.MaterializeEntries())
	}
	// The directly-reachable copy outside the include must stay pruned.
	if _, ok := findEntry(res, "outer/deep/file.txt"); ok {
		t.Errorf("scope include leaked the directly-reachable copy outside the include")
	}
}

// TestScopeIncludeSymlinkResolvingToFileExcluded proves a symlink admitted only as
// a provisional directory ancestor of an include (include=["link/child.txt"]) does
// not smuggle its own content into a narrow scan when it actually resolves to a
// file. The link is outside the file-level scope, so it must not appear at all.
func TestScopeIncludeSymlinkResolvingToFileExcluded(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "real.txt", "secret content")
	scantest.Symlink(t, root, "link", "real.txt")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.Include = []string{"link/child.txt"} // treats link as a directory
	res := run(t, proj, cfg)

	if e, ok := findEntry(res, "link"); ok {
		t.Errorf("out-of-scope symlink resolving to a file leaked into an entry: %+v", e)
	}
	if s, ok := findSkipped(res, "link"); ok {
		t.Errorf("out-of-scope symlink leaked into a skipped input: %+v", s)
	}
}

// TestScopeIncludeNestedSymlinkResolvingToFileExcluded proves the same guard holds
// for a symlink nested inside an already-followed directory: a link resolving to a
// file, admitted only as an ancestor, must not be scanned.
func TestScopeIncludeNestedSymlinkResolvingToFileExcluded(t *testing.T) {
	scantest.RequireSymlinks(t)
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "realdir/real.txt", "secret content")
	scantest.Symlink(t, root, "realdir/innerlink", "real.txt")
	scantest.Symlink(t, root, "outer", "realdir")

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.Include = []string{"outer/innerlink/child.txt"}
	res := run(t, proj, cfg)

	if e, ok := findEntry(res, "outer/innerlink"); ok {
		t.Errorf("out-of-scope nested symlink resolving to a file leaked into an entry: %+v", e)
	}
	if s, ok := findSkipped(res, "outer/innerlink"); ok {
		t.Errorf("out-of-scope nested symlink leaked into a skipped input: %+v", s)
	}
}

// TestHardlinkMetadataAndContent proves hard-linked paths share a content hash and
// each carries hardlink identity metadata.
func TestHardlinkMetadataAndContent(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "hardlink-a.txt", "shared content")
	scantest.Hardlink(t, root, "hardlink-b.txt", "hardlink-a.txt")

	res := run(t, proj, config.Defaults())
	a, okA := findEntry(res, "hardlink-a.txt")
	b, okB := findEntry(res, "hardlink-b.txt")
	if !okA || !okB {
		t.Fatalf("hardlinks missing: a=%v b=%v", okA, okB)
	}
	if a.Content.String() != b.Content.String() {
		t.Errorf("hardlinks should share a content hash: %q vs %q", a.Content, b.Content)
	}
	if a.Hardlink().Known {
		// Where the platform supplies inode metadata, hardlinks share dev/ino and
		// report nlink >= 2.
		if a.Hardlink().Ino != b.Hardlink().Ino || a.Hardlink().Dev != b.Hardlink().Dev {
			t.Errorf("hardlinks should share dev/ino")
		}
		if a.Hardlink().Nlink < 2 {
			t.Errorf("hardlink nlink = %d, want >= 2", a.Hardlink().Nlink)
		}
	}
}

// TestHardlinkContentChangeObservedThroughAllPaths proves a change made through
// one hard-linked path is observed for every scoped path referring to it.
func TestHardlinkContentChangeObservedThroughAllPaths(t *testing.T) {
	root := t.TempDir()
	proj := scantest.InitProject(t, root)
	scantest.Write(t, root, "h-a.txt", "original")
	scantest.Hardlink(t, root, "h-b.txt", "h-a.txt")

	svc, _ := openIndexedService(t, proj)
	first := scanWith(t, svc, proj, config.Defaults())

	// Modify through one path; both paths see the new content (same inode).
	if err := os.WriteFile(filepath.Join(root, "h-a.txt"), []byte("changed via a"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := scanWith(t, svc, proj, config.Defaults())

	if first.TreeHash().String() == second.TreeHash().String() {
		t.Errorf("hardlink content change not observed")
	}
	a, _ := findEntry(second, "h-a.txt")
	b, _ := findEntry(second, "h-b.txt")
	if a.Content.String() != b.Content.String() {
		t.Errorf("hard-linked paths diverged after a shared-content change")
	}
}
