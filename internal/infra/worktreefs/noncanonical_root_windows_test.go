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

// TestFollowedContentUnderNonCanonicalRoot proves a followed entry is still readable
// when the caller's root is spelled in a form the platform does not consider
// canonical. Windows keeps a second spelling of every name — a folded case here, an
// 8.3 component in a path like C:\Users\RUNNER~1\AppData\Local\Temp — and
// filepath.EvalSymlinks normalizes both while a hop-by-hop resolution preserves what
// the caller wrote. A walker that recorded a followed terminal through one of those
// and re-derived it through the other rejected every followed read as "changed during
// scan", and because a "now" scan tolerates unreadable inputs the file did not fail
// the scan: it silently left the manifest.
//
// The two shapes here are the two ways a followed regular entry is produced: a link
// straight to a file, whose terminal follow() canonicalizes, and a file under a
// followed directory symlink, whose real path is built from the canonicalized
// directory. Both are asserted by reading their content, because content is the only
// place the recorded and re-derived paths are compared.
//
// This is a Windows test not because the walker branches on the platform — it does
// not — but because the divergence it pins needs a filesystem that folds spelling,
// and on the Unix systems the rest of the suite runs the two resolutions agree by
// construction, so the same fixture would pass with the bug in place.
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

	// The scan is driven through a spelling of the same directory that the platform
	// resolves to a different string. Asserted rather than assumed: on a volume with
	// per-directory case sensitivity enabled this vehicle produces no divergence, and
	// a test that quietly walked a canonical root would report green having exercised
	// nothing the Unix lanes do not already cover.
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
