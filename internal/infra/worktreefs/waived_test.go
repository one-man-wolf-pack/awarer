package worktreefs_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
)

func waivedScope(include []string, waived ...string) config.ScanScope {
	return config.ScanScope{Include: include, UseAwaignore: true, Waived: waived}
}

func TestWaivedDirectoryIsPrunedAndRecordedAsUnobserved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "gen/out.bin", "x")
	writeFile(t, root, "gen/deep/more.bin", "x")
	writeFile(t, root, "pkg/gen/kept.txt", "x")
	writeFile(t, root, "gen2/kept.txt", "x")
	writeFile(t, root, "src/main.go", "x")

	nodes, omitted := walkFrontier(t, root, waivedScope([]string{"."}, "gen"))

	if got, ok := omitted["gen"]; !ok || got != worktree.LimitSubtree {
		t.Fatalf("omitted[gen] = %v (present %v), want a subtree limit", got, ok)
	}
	for p := range nodes {
		if config.PathWithin(p, "gen") {
			t.Errorf("waived path %q was observed", p)
		}
	}
	for _, want := range []string{"pkg/gen/kept.txt", "gen2/kept.txt", "src/main.go"} {
		if !nodes[want] {
			t.Errorf("%q outside the waived directory was not observed", want)
		}
	}
}

func TestWaivedDirectoryIsNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory read permission is not enforceable through chmod on Windows")
	}
	root := t.TempDir()
	writeFile(t, root, "gen/secret/x", "x")
	writeFile(t, root, "src/main.go", "x")
	locked := filepath.Join(root, "gen")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("running with privileges that ignore directory permissions")
	}

	nodes, omitted := walkFrontier(t, root, waivedScope([]string{"."}, "gen"))
	if omitted["gen"] != worktree.LimitSubtree || !nodes["src/main.go"] {
		t.Fatalf("nodes=%v omitted=%v, want gen pruned unread and src observed", nodes, omitted)
	}
}

func TestWaiverOutranksANarrowedScopeButNotItsSiblings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "artifacts/cache/blob", "x")
	writeFile(t, root, "artifacts/report.txt", "x")

	nodes, omitted := walkFrontier(t, root, waivedScope([]string{"artifacts"}, "artifacts/cache"))
	if omitted["artifacts/cache"] != worktree.LimitSubtree || nodes["artifacts/cache/blob"] {
		t.Fatalf("narrowed scope reopened the waived directory: nodes=%v omitted=%v", nodes, omitted)
	}
	if !nodes["artifacts/report.txt"] {
		t.Error("the waived directory's sibling was not observed")
	}
}

// As a glob, ta[r]get would select target; a waiver names one literal directory.
func TestWaiverSelectsALiteralDirectoryOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "ta[r]get/a", "x")
	writeFile(t, root, "target/b", "x")
	writeFile(t, root, "out", "a file at the waived spelling")

	nodes, omitted := walkFrontier(t, root, waivedScope([]string{"."}, "ta[r]get", "out"))
	if omitted["ta[r]get"] != worktree.LimitSubtree || nodes["ta[r]get/a"] {
		t.Errorf("the literal directory was not waived: nodes=%v omitted=%v", nodes, omitted)
	}
	for _, want := range []string{"target/b", "out"} {
		if !nodes[want] {
			t.Errorf("%q was treated as waived", want)
		}
	}
}

func TestWaiverDoesNotApplyToASymlinkAtItsSpelling(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	writeFile(t, root, "real/x", "x")
	if err := os.Symlink("real", filepath.Join(root, "gen")); err != nil {
		t.Fatal(err)
	}
	nodes, omitted := walkFrontier(t, root, waivedScope([]string{"."}, "gen"))
	if _, ok := omitted["gen"]; ok || !nodes["gen"] {
		t.Fatalf("a symlink at the waived spelling lost ordinary handling: nodes=%v omitted=%v", nodes, omitted)
	}
}

// The physical tree is ignored, so alias/ is the only way the walk reaches it.
func followedAliasFixture(t *testing.T) string {
	t.Helper()
	requireSymlinks(t)
	root := t.TempDir()
	writeFile(t, root, ".awaignore", "/real/\n")
	writeFile(t, root, "real/cache/blob", "x")
	writeFile(t, root, "real/sibling.txt", "x")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestWaiverPrunesARealDirectoryBelowAFollowedLink(t *testing.T) {
	root := followedAliasFixture(t)
	nodes, omitted := walkFrontier(t, root, followingScope("alias/cache"))
	if omitted["alias/cache"] != worktree.LimitSubtree {
		t.Fatalf("omitted = %v, want alias/cache as a subtree limit", omitted)
	}
	if nodes["alias/cache/blob"] {
		t.Error("the waived directory was observed through the followed link")
	}
	if !nodes["alias/sibling.txt"] {
		t.Errorf("the waived directory's sibling was not observed: %v", nodes)
	}
}

func TestWaivedDirectoryBelowAFollowedLinkIsNeverRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory read permission is not enforceable through chmod on Windows")
	}
	root := followedAliasFixture(t)
	locked := filepath.Join(root, "real", "cache")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("running with privileges that ignore directory permissions")
	}
	if _, omitted := walkFrontier(t, root, followingScope("alias/cache")); omitted["alias/cache"] != worktree.LimitSubtree {
		t.Fatalf("omitted = %v, want alias/cache pruned unread", omitted)
	}
}

func TestWaiverIsNotInferredForAPhysicalAlias(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	writeFile(t, root, "real/cache/blob", "x")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	scope := followingScope("alias/cache")
	nodes, omitted := walkFrontier(t, root, scope)
	if omitted["alias/cache"] != worktree.LimitSubtree || !nodes["real/cache/blob"] {
		t.Fatalf("nodes=%v omitted=%v, want alias/cache waived and real/cache observed", nodes, omitted)
	}
}

func TestWaiverDoesNotApplyToANestedLinkAtItsSpellingBelowAFollowedLink(t *testing.T) {
	requireSymlinks(t)
	root := t.TempDir()
	writeFile(t, root, "real/target/x", "x")
	if err := os.Symlink("target", filepath.Join(root, "real", "cache")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	scope := followingScope("alias/cache")
	nodes, omitted := walkFrontier(t, root, scope)
	if _, ok := omitted["alias/cache"]; ok || !nodes["alias/cache/x"] {
		t.Fatalf("a link at the waived spelling lost ordinary handling: omitted=%v nodes=%v", omitted, nodes)
	}
}

func followingScope(waived ...string) config.ScanScope {
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	scope := cfg.HistoryScanScope()
	scope.Waived = waived
	return scope
}
