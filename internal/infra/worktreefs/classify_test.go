package worktreefs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/scantest"
)

// TestClassifyRootFileAgreesWithWalk uses the full walk as the oracle: for every
// root-level name, the single-entry classification yields exactly the node a walk
// under the same scope records — or nothing when the walk records nothing — and a
// followed directory link yields no node and no descendants. The symlink cases run
// through the capability-aware fixture, so a lane requiring symlink coverage fails
// rather than skips them.
func TestClassifyRootFileAgreesWithWalk(t *testing.T) {
	t.Run("files", func(t *testing.T) {
		root := classifyFixture(t)
		requireClassifyParity(t, root, []string{".netrc", ".pgpass", "credentials", "missing"})
	})
	t.Run("symlinks", func(t *testing.T) {
		scantest.RequireSymlinks(t)
		root := classifyFixture(t)
		scantest.Symlink(t, root, "link-file", ".netrc")
		scantest.Symlink(t, root, "link-dir", "sub")
		requireClassifyParity(t, root, []string{".netrc", "link-file", "link-dir"})
	})
}

func classifyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range map[string]string{
		".netrc": "m", ".pgpass": "p", "credentials": "c", "sub/inner.txt": "i",
		".awaignore": ".pgpass\n", ".gitignore": "credentials\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireClassifyParity(t *testing.T, root string, names []string) {
	t.Helper()
	scopes := map[string]func(*config.Config){
		"defaults":  func(*config.Config) {},
		"gitignore": func(c *config.Config) { c.Scope.UseGitignore = true },
		"narrowed":  func(c *config.Config) { c.Scope.Include = []string{".pgpass", "link-file"} },
		"follow":    func(c *config.Config) { c.Scope.FollowSymlinks = true },
		"excluded":  func(c *config.Config) { c.Scope.ExtraExcludes = []string{".netrc"} },
	}
	layout := paths.New(root)
	for scopeName, mutate := range scopes {
		cfg := config.Defaults()
		mutate(&cfg)
		scope := cfg.HistoryScanScope()

		walked := map[string]worktree.Node{}
		if err := New().Walk(context.Background(), layout, scope, func(n worktree.Node) error {
			walked[n.Path.String()] = n
			return nil
		}, nil); err != nil {
			t.Fatal(err)
		}
		classified := map[string]worktree.Node{}
		for _, name := range names {
			if err := New().ClassifyRootFile(context.Background(), layout, scope, name, func(n worktree.Node) error {
				if _, dup := classified[n.Path.String()]; dup {
					t.Errorf("%s/%s classified twice", scopeName, n.Path)
				}
				classified[n.Path.String()] = n
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, n := range names {
			w, inWalk := walked[n]
			if inWalk && w.Kind == worktree.KindDir {
				inWalk = false // a followed directory link: never a leaf answer
			}
			c, inClass := classified[n]
			if inWalk != inClass {
				t.Errorf("%s/%s: walk recorded=%v, classification recorded=%v", scopeName, n, inWalk, inClass)
				continue
			}
			if inWalk && (w.Kind != c.Kind || w.Stat != c.Stat || w.Traversal != c.Traversal || w.Skipped != c.Skipped) {
				t.Errorf("%s/%s: classification %+v differs from walk %+v", scopeName, n, c, w)
			}
		}
		for p, n := range classified {
			if n.Kind == worktree.KindDir || filepath.Dir(p) != "." {
				t.Errorf("%s: classification descended or emitted a directory: %s", scopeName, p)
			}
		}
		if scope.FollowSymlinks {
			if w, ok := walked["link-file"]; ok && (!w.Traversal.Followed || classified["link-file"].Kind != worktree.KindRegular) {
				t.Errorf("follow: link-file walk=%+v classified=%+v, want a followed regular file", w, classified["link-file"])
			}
			if _, ok := walked["link-dir/inner.txt"]; ok {
				if _, leaked := classified["link-dir/inner.txt"]; leaked {
					t.Error("follow: classification descended a followed directory link")
				}
			}
		}
	}
}
