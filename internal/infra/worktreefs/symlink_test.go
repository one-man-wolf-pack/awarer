package worktreefs_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/worktreefs"
)

// requireSymlinksEnv turns a missing symlink capability from a skip into a failure.
// The windows-portability lane sets it and now runs this package, whose followed-
// symlink traversal is exactly what that lane exists to prove: a run that quietly
// skipped every symlink case and reported green would look like evidence while
// proving nothing. A developer without the privilege still gets a named skip.
const requireSymlinksEnv = "AWA_REQUIRE_SYMLINK_TESTS"

// requireSymlinks proves the platform will create a symlink, or ends the test —
// fatally where symlink coverage is required, with a named skip otherwise. Windows
// grants the privilege only in developer mode or to an elevated process.
func requireSymlinks(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := os.Symlink(target, filepath.Join(dir, "link"))
	if err == nil {
		return
	}
	if os.Getenv(requireSymlinksEnv) != "" {
		t.Fatalf("%s is set, so symlink coverage is required, but this platform will not create a symlink: %v",
			requireSymlinksEnv, err)
	}
	t.Skipf("this platform will not create a symlink: %v", err)
}

// collectNodes runs the walker and returns nodes keyed by rel-path.
func collectNodes(t *testing.T, root string, cfg config.Config) map[string]worktree.Node {
	t.Helper()
	w := worktreefs.New()
	out := map[string]worktree.Node{}
	err := w.Walk(context.Background(), paths.New(root), cfg.HistoryScanScope(), func(n worktree.Node) error {
		out[n.Path.String()] = n
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}

// TestFollowedFileOpenDetectsSwap proves that a followed-symlink regular file
// also verifies its resolved target at open time: a target swapped between the
// walk and the content read is rejected, not hashed under the stale stat.
func TestFollowedFileOpenDetectsSwap(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true

	followed, ok := collectNodes(t, root, cfg)["link.txt"]
	if !ok || followed.Kind != worktree.KindRegular || followed.Open == nil {
		t.Fatalf("followed file not surfaced as a regular node with an opener: %+v", followed)
	}

	// Swap the resolved target between walk and open.
	target := filepath.Join(root, "real.txt")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("a totally different payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := followed.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after target swap err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestFollowedFileOpenDetectsRepoint proves that a followed file is read through
// its virtual path: repointing the symlink to a different target after the walk
// (target left unchanged) is rejected, not served from the old resolved target.
func TestFollowedFileOpenDetectsRepoint(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true

	node, ok := collectNodes(t, root, cfg)["link.txt"]
	if !ok || node.Kind != worktree.KindRegular || node.Open == nil {
		t.Fatalf("followed file not surfaced: %+v", node)
	}

	// Repoint link.txt -> b.txt; a.txt is untouched.
	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after repoint err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestFollowedFileOpenDetectsRootEscapeRepoint proves the chain is re-validated by
// physical path, not just terminal identity: repointing a followed link to an
// out-of-root hard link sharing the in-root file's inode is rejected, even though
// dev/ino/size/mtime/mode all match.
func TestFollowedFileOpenDetectsRootEscapeRepoint(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, outside the project root

	inFile := filepath.Join(root, "a.txt")
	if err := os.WriteFile(inFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	outHard := filepath.Join(outside, "hard")
	if err := os.Link(inFile, outHard); err != nil { // same inode/content, outside root
		t.Skipf("hardlinks unsupported: %v", err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults() // AllowSymlinkRootEscape is false by default
	cfg.Scope.FollowSymlinks = true

	node, ok := collectNodes(t, root, cfg)["link.txt"]
	if !ok || node.Kind != worktree.KindRegular || node.Open == nil {
		t.Fatalf("followed file not surfaced: %+v", node)
	}

	// Repoint the link to the out-of-root hard link (same inode as a.txt).
	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outHard, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after root-escape repoint err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestFollowedFileOpenDetectsHopInsertion proves the observed chain is replayed, not
// just the terminal path: repointing a followed link to the same terminal through an
// extra symlink hop (link -> real becomes link -> hop -> real) is rejected, because
// the recorded link's raw target changes from "real.txt" to "hop", even though the
// resolved terminal is unchanged. A fresh walk under a tighter symlink_max_depth
// would now resolve more hops, so serving the file from the stale entry would bypass
// the depth policy.
func TestFollowedFileOpenDetectsHopInsertion(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.SymlinkMaxDepth = 1 // link -> real.txt is exactly one hop

	node, ok := collectNodes(t, root, cfg)["link.txt"]
	if !ok || node.Kind != worktree.KindRegular || node.Open == nil {
		t.Fatalf("followed file not surfaced: %+v", node)
	}

	// Repoint link.txt -> hop -> real.txt: same terminal, one extra hop. Under
	// SymlinkMaxDepth=1 a fresh walk would now skip it as depth-exceeded.
	if err := os.Symlink("real.txt", filepath.Join(root, "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after hop insertion err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestFollowedFileOpenDetectsChainShortcut proves the whole observed chain is
// replayed, not only its endpoints: in a two-hop chain link -> mid -> real,
// repointing the first link straight to the terminal (link -> real, dropping the
// middle hop) is rejected even though the resolved terminal is unchanged, because
// the recorded first link's raw target changed from "mid" to "real.txt".
func TestFollowedFileOpenDetectsChainShortcut(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "mid")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mid", filepath.Join(root, "link.txt")); err != nil { // link -> mid -> real.txt
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true

	node, ok := collectNodes(t, root, cfg)["link.txt"]
	if !ok || node.Kind != worktree.KindRegular || node.Open == nil {
		t.Fatalf("followed file not surfaced: %+v", node)
	}

	// Repoint link.txt straight to real.txt, bypassing mid: same terminal, one fewer
	// hop. Only replaying the recorded chain (link.txt's target was "mid") catches it.
	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after chain shortcut err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestFollowedDirChildOpenDetectsRepoint proves the same for a file reached under
// a followed directory symlink: repointing the directory symlink is caught.
func TestFollowedDirChildOpenDetectsRepoint(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	for dir, content := range map[string]string{"dirA": "AAA", "dirB": "BBBB"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "file.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("dirA", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true

	node, ok := collectNodes(t, root, cfg)["dirlink/file.txt"]
	if !ok || node.Kind != worktree.KindRegular || node.Open == nil {
		t.Fatalf("followed dir child not surfaced: %+v", node)
	}

	// Repoint dirlink -> dirB.
	if err := os.Remove(filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dirB", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after dir repoint err = %v, want a \"changed during scan\" rejection", err)
	}
}

// TestSymlinkRecordedAsLinkByDefault proves that with follow_symlinks=false a
// symlink is recorded as a link by its target, not followed into its content.
func TestSymlinkRecordedAsLinkByDefault(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}

	nodes := collectNodes(t, root, config.Defaults())
	link, ok := nodes["link.txt"]
	if !ok {
		t.Fatalf("symlink not surfaced")
	}
	if link.Kind != worktree.KindSymlink {
		t.Errorf("kind = %v, want symlink", link.Kind)
	}
	if link.Symlink.String() != "real.txt" {
		t.Errorf("target = %q, want real.txt", link.Symlink.String())
	}
	if link.Open != nil {
		t.Errorf("symlink node must not expose content opener")
	}
}

// TestSymlinkToDirNotFollowed proves a symlink to a directory is recorded as a
// link and not descended into.
func TestSymlinkToDirNotFollowed(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}

	nodes := collectNodes(t, root, config.Defaults())
	if _, ok := nodes["dirlink"]; !ok {
		t.Fatalf("dir symlink not surfaced")
	}
	if nodes["dirlink"].Kind != worktree.KindSymlink {
		t.Errorf("dirlink kind = %v, want symlink", nodes["dirlink"].Kind)
	}
	// The walker must not descend through the symlink to inner.txt under dirlink.
	if _, ok := nodes["dirlink/inner.txt"]; ok {
		t.Errorf("walker followed symlink into directory")
	}
}
