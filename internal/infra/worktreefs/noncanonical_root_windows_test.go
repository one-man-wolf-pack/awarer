//go:build windows

package worktreefs_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
)

// TestFollowedContentUnderNonCanonicalRoot is Windows-only although the walker does not
// branch on the platform: it needs a filesystem that holds a second spelling of a name
// (a folded case here, an 8.3 component in a path like C:\Users\RUNNER~1\...), and on
// the Unix systems the rest of the suite runs, a recorded terminal and a re-derived one
// agree by construction — so the same fixture would pass however they were resolved.
func TestFollowedContentUnderNonCanonicalRoot(t *testing.T) {
	requireSymlinks(t)

	base := t.TempDir()
	canonicalRoot := filepath.Join(base, "Project")
	if err := os.MkdirAll(filepath.Join(canonicalRoot, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalRoot, "real.txt"), []byte("followed bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalRoot, "dir", "inner.txt"), []byte("inner bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(canonicalRoot, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(canonicalRoot, "dirlink")); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(base, "PROJECT")
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", root, err)
	}
	if resolved == root {
		t.Fatalf("this volume resolved %s to itself, so the fixture carries no non-canonical spelling to prove anything with", root)
	}

	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	nodes := collectNodes(t, root, cfg)

	for _, tc := range []struct{ path, want string }{
		{"link.txt", "followed bytes\n"},
		{"dirlink/inner.txt", "inner bytes\n"},
	} {
		n, ok := nodes[tc.path]
		if !ok {
			t.Errorf("%s missing from the scan", tc.path)
			continue
		}
		if n.Skipped {
			t.Errorf("%s was skipped as %v; a followed entry under a non-canonical root is readable, not changed", tc.path, n.SkipReason)
			continue
		}
		if n.Kind != worktree.KindRegular || n.Open == nil {
			t.Errorf("%s surfaced as %v with opener present=%t, want a regular node with an opener", tc.path, n.Kind, n.Open != nil)
			continue
		}
		rc, err := n.Open()
		if err != nil {
			t.Errorf("Open(%s): %v", tc.path, err)
			continue
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Errorf("read %s: %v", tc.path, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s content = %q, want %q", tc.path, got, tc.want)
		}
	}
}
