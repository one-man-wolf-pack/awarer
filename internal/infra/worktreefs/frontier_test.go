package worktreefs_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/worktreefs"
)

func walkFrontier(t *testing.T, root string, scope config.ScanScope) (map[string]bool, map[string]worktree.CoverageLimit) {
	t.Helper()
	nodes := map[string]bool{}
	omitted := map[string]worktree.CoverageLimit{}
	err := worktreefs.New().Walk(context.Background(), paths.New(root), scope, func(n worktree.Node) error {
		nodes[n.Path.String()] = true
		return nil
	}, func(r worktree.FrontierRecord) error {
		if _, dup := omitted[r.Path.String()]; dup {
			t.Errorf("frontier records %q twice", r.Path)
		}
		omitted[r.Path.String()] = r.Limit
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for p := range omitted {
		if nodes[p] {
			t.Errorf("%q is both observed and omitted", p)
		}
	}
	return nodes, omitted
}

func TestFrontierRecordsEveryIgnoreAndProtectedOmission(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".awaignore", "/kind-boundary/\n*.env\n")
	writeFile(t, root, "kind-boundary/child", "x")
	writeFile(t, root, "secret.env", "x")
	writeFile(t, root, "node_modules/pkg/index.js", "x")
	writeFile(t, root, "sub/.git", "gitdir: elsewhere")
	writeFile(t, root, "sub/.awaignore", ".awaignore\nscratch/\n")
	writeFile(t, root, "sub/scratch/x", "x")
	writeFile(t, root, "sub/kept.txt", "x")

	nodes, omitted := walkFrontier(t, root, config.Defaults().HistoryScanScope())

	want := map[string]worktree.CoverageLimit{
		"kind-boundary":  worktree.LimitSubtree,
		"node_modules":   worktree.LimitSubtree,
		"secret.env":     worktree.LimitNode,
		"sub/.git":       worktree.LimitNode,
		"sub/.awaignore": worktree.LimitNode,
		"sub/scratch":    worktree.LimitSubtree,
	}
	for p, limit := range want {
		if got, ok := omitted[p]; !ok || got != limit {
			t.Errorf("omitted[%q] = %v (present %v), want %v", p, got, ok, limit)
		}
	}
	for p := range omitted {
		if _, ok := want[p]; !ok {
			t.Errorf("unexpected frontier record %q", p)
		}
	}
	if !nodes["sub/kept.txt"] || nodes["sub/scratch/x"] {
		t.Errorf("a self-excluded rule file must still apply its rules: nodes = %v", nodes)
	}
}

func TestFrontierNeverEnumeratesAPrunedSubtree(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 50; i++ {
		writeFile(t, root, filepath.Join("node_modules", "pkg", "f"+string(rune('a'+i%26))+string(rune('a'+i/26))), "x")
	}
	// Entering the pruned tree would fail the walk on this unreadable directory.
	unreadable := filepath.Join(root, "node_modules", "locked")
	if err := os.MkdirAll(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(unreadable, 0o700) }()

	_, omitted := walkFrontier(t, root, config.Defaults().HistoryScanScope())
	if len(omitted) != 1 || omitted["node_modules"] != worktree.LimitSubtree {
		t.Errorf("omitted = %v, want only node_modules", omitted)
	}
}

func TestFrontierRecordsNarrowedScopeSiblings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.go", "x")
	writeFile(t, root, "docs/readme", "x")
	writeFile(t, root, "README.md", "x")
	writeFile(t, root, "src-old/b.go", "x")
	cfg := config.Defaults()
	cfg.Scope.Include = []string{"src"}

	nodes, omitted := walkFrontier(t, root, cfg.HistoryScanScope())
	if !nodes["src/a.go"] {
		t.Fatalf("the included path was not observed: %v", nodes)
	}
	for p, limit := range map[string]worktree.CoverageLimit{
		"docs": worktree.LimitSubtree, "README.md": worktree.LimitNode, "src-old": worktree.LimitSubtree,
	} {
		if omitted[p] != limit {
			t.Errorf("omitted[%q] = %v, want %v (all %v)", p, omitted[p], limit, omitted)
		}
	}
}

func TestFrontierRecordsOmissionsInsideFollowedDirectories(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	writeFile(t, root, ".awaignore", "*.log\n")
	writeFile(t, root, "real/keep.txt", "x")
	writeFile(t, root, "real/debug.log", "x")
	writeFile(t, root, "real/node_modules/m.js", "x")
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true

	nodes, omitted := walkFrontier(t, root, cfg.HistoryScanScope())
	if !nodes["link/keep.txt"] {
		t.Fatalf("followed directory was not traversed: %v", nodes)
	}
	for p, limit := range map[string]worktree.CoverageLimit{
		"link/debug.log":    worktree.LimitNode,
		"link/node_modules": worktree.LimitSubtree,
		"real/debug.log":    worktree.LimitNode,
		"real/node_modules": worktree.LimitSubtree,
	} {
		if omitted[p] != limit {
			t.Errorf("omitted[%q] = %v, want %v (all %v)", p, omitted[p], limit, omitted)
		}
	}
}

func TestFrontierRecordsAProvisionalLinkThatIsNotADirectory(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	writeFile(t, root, "target.txt", "x")
	if err := os.Symlink("target.txt", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.Include = []string{"dirlink/inner.txt"}

	nodes, omitted := walkFrontier(t, root, cfg.HistoryScanScope())
	if nodes["dirlink"] {
		t.Fatalf("a provisional ancestor resolving to a file was observed: %v", nodes)
	}
	if omitted["dirlink"] != worktree.LimitSubtree {
		t.Errorf("omitted[dirlink] = %v, want subtree (all %v)", omitted["dirlink"], omitted)
	}
}
