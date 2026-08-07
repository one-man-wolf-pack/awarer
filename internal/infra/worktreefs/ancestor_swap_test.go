//go:build darwin || linux || freebsd

package worktreefs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
)

// TestDirectFileOpenDetectsAncestorSymlinkSwap proves a direct (non-followed)
// regular entry is read through a symlink-free path, not just a symlink-free final
// component: replacing an ancestor directory with a symlink to an outside directory
// holding a hard link of the same inode and content is rejected, even though the
// final component is still a regular file and its descriptor identity matches.
func TestDirectFileOpenDetectsAncestorSymlinkSwap(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	inFile := filepath.Join(root, "dir", "file")
	if err := os.WriteFile(inFile, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	node, ok := collectNodes(t, root, config.Defaults())["dir/file"]
	if !ok || node.Kind != worktree.KindRegular || node.Open == nil {
		t.Fatalf("direct file not surfaced as a regular node with an opener: %+v", node)
	}

	// An outside directory whose "file" is a hard link of the same inode/content.
	outside := t.TempDir()
	if err := os.Link(inFile, filepath.Join(outside, "file")); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}
	// Swap the ancestor directory root/dir for a symlink to that outside directory.
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}

	if _, err := node.Open(); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("Open after ancestor symlink swap err = %v, want a \"changed during scan\" rejection", err)
	}
}
