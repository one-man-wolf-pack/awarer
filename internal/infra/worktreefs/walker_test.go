package worktreefs_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/worktreefs"
)

// writeFile creates a file (and parents) with content under root.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// walkPaths runs the walker and returns the rel-paths of regular-file nodes only.
func walkPaths(t *testing.T, root string, cfg config.Config) []string {
	t.Helper()
	return walkScope(t, root, cfg.HistoryScanScope())
}

func walkScope(t *testing.T, root string, scope config.ScanScope) []string {
	t.Helper()
	w := worktreefs.New()
	var got []string
	err := w.Walk(context.Background(), paths.New(root), scope, func(n worktree.Node) error {
		if n.Kind == worktree.KindRegular {
			got = append(got, n.Path.String())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Strings(got)
	return got
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}

func TestWalkIncludesAllByDefault(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "pkg/util.go", "package pkg")
	writeFile(t, root, "data/input.txt", "hello")

	got := walkPaths(t, root, config.Defaults())
	want := []string{"data/input.txt", "main.go", "pkg/util.go"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkExcludesBuiltinDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "node_modules/dep/index.js", "x")
	writeFile(t, root, ".git/config", "x")
	writeFile(t, root, "target/out.bin", "x")

	got := walkPaths(t, root, config.Defaults())
	want := []string{"main.go"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestWalkExplicitIncludeOverridesExcludes proves that an explicit, narrowed scope
// outranks configurable and default excludes: scoping to a path inside a
// default-excluded directory brings it back, while the same path stays excluded
// under the default whole-tree scope.
func TestWalkExplicitIncludeOverridesExcludes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "node_modules/keep/index.js", "x") // node_modules is a baseline exclude
	writeFile(t, root, "dist/bundle.js", "y")             // dist is a history-only default exclude

	// Default whole-tree scope: the excludes apply, so neither is scanned.
	def := walkPaths(t, root, config.Defaults())
	if contains(def, "node_modules/keep/index.js") || contains(def, "dist/bundle.js") {
		t.Errorf("default scope should exclude node_modules/dist, got %v", def)
	}

	// Explicit narrowed scope into the excluded directory brings exactly that
	// path back, without dragging in the rest of the tree.
	cfg := config.Defaults()
	cfg.Scope.Include = []string{"node_modules/keep"}
	got := walkPaths(t, root, cfg)
	if !contains(got, "node_modules/keep/index.js") {
		t.Errorf("explicit include should re-include node_modules/keep, got %v", got)
	}
	if contains(got, "main.go") {
		t.Errorf("narrowed scope must not scan out-of-scope paths, got %v", got)
	}
}

// TestWalkExplicitIncludeStillProtects proves protected paths remain a hard
// boundary even when a narrowed scope explicitly points into them.
func TestWalkExplicitIncludeStillProtects(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".git/config", "secret")

	cfg := config.Defaults()
	cfg.Scope.Include = []string{".git/config"}
	got := walkPaths(t, root, cfg)
	if contains(got, ".git/config") {
		t.Errorf("explicit include must not override the protected .git boundary, got %v", got)
	}
}

// TestWalkProtectedPathsResistNegation proves that .git and .awa are a hard scan
// boundary: an .awaignore (or .gitignore) negation cannot re-include VCS or awa
// state, so the "protected" invariant holds even against an explicit "!.git".
func TestWalkProtectedPathsResistNegation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, ".git/config", "secret")
	writeFile(t, root, ".git/HEAD", "ref")
	writeFile(t, root, ".awa/config.toml", "x")
	// Aggressively try to re-include protected state through both ignore sources.
	writeFile(t, root, ".awaignore", "!.git\n!.git/**\n!.awa\n!.awa/**\n")
	writeFile(t, root, ".gitignore", "!.git\n!.awa\n")

	cfg := config.Defaults()
	cfg.Scope.UseGitignore = true
	got := walkPaths(t, root, cfg)
	want := []string{".awaignore", ".gitignore", "main.go"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v (protected .git/.awa must never be re-included)", got, want)
	}
}

// TestWalkAwaAsFileSkipsOnlyItself proves that when .awa is a stray file rather
// than the project directory, skipping it does not skip the rest of the root.
// Returning fs.SkipDir from a non-directory would skip every remaining sibling in
// the parent, silently omitting root files that sort after ".awa".
func TestWalkAwaAsFileSkipsOnlyItself(t *testing.T) {
	root := t.TempDir()
	// .awa is a plain file here; it sorts before the others, so a wrong SkipDir
	// would drop them.
	writeFile(t, root, ".awa", "stray")
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "zzz.txt", "last")

	got := walkPaths(t, root, config.Defaults())
	want := []string{"main.go", "zzz.txt"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v (.awa file must not skip its siblings)", got, want)
	}
}

func TestWalkNeverScansAwaDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, ".awa/config.toml", "x")
	writeFile(t, root, ".awa/indexes/worktree.sqlite", "db")

	// Even with .awa absent from the exclude list, it must never be scanned: it is
	// a protected hard boundary, not an ordinary exclude.
	got := walkScope(t, root, config.ScanScope{Include: []string{"."}, Exclude: []string{"node_modules"}})
	want := []string{"main.go"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkHonorsGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "secret.log", "secret")
	writeFile(t, root, "logs/a.log", "x")
	writeFile(t, root, ".gitignore", "*.log\nlogs/\n")

	// .gitignore is off by default now, so this test opts in explicitly.
	cfg := config.Defaults()
	cfg.Scope.UseGitignore = true
	got := walkPaths(t, root, cfg)
	want := []string{".gitignore", "main.go"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkGitignoreOffByDefault(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "secret.log", "secret")
	writeFile(t, root, ".gitignore", "*.log\n")

	// With the defaults, .gitignore does not participate, so a gitignored file
	// remains visible to the scan — the property awa relies on for run inputs.
	got := walkPaths(t, root, config.Defaults())
	want := []string{".gitignore", "main.go", "secret.log"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v (.gitignore must be off by default)", got, want)
	}
}

func TestWalkGitignoreDisabled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")
	writeFile(t, root, "secret.log", "secret")
	writeFile(t, root, ".gitignore", "*.log\n")

	cfg := config.Defaults()
	cfg.Scope.UseGitignore = false
	got := walkPaths(t, root, cfg)
	want := []string{".gitignore", "main.go", "secret.log"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkAwaignoreOverridesGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.log", "x")
	writeFile(t, root, "drop.log", "x")
	writeFile(t, root, ".gitignore", "*.log\n")
	// .awaignore re-includes keep.log, overriding the .gitignore exclusion.
	writeFile(t, root, ".awaignore", "!keep.log\n")

	// Opt into .gitignore so the override interaction can be exercised.
	cfg := config.Defaults()
	cfg.Scope.UseGitignore = true
	got := walkPaths(t, root, cfg)
	want := []string{".awaignore", ".gitignore", "keep.log"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkNestedGitignore(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "x")
	writeFile(t, root, "sub/b.txt", "x")
	writeFile(t, root, "sub/c.tmp", "x")
	writeFile(t, root, "sub/.gitignore", "*.tmp\n")

	cfg := config.Defaults()
	cfg.Scope.UseGitignore = true
	got := walkPaths(t, root, cfg)
	want := []string{"a.txt", "sub/.gitignore", "sub/b.txt"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkScopeInclude(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/a.go", "x")
	writeFile(t, root, "src/deep/b.go", "x")
	writeFile(t, root, "docs/readme.md", "x")

	cfg := config.Defaults()
	cfg.Scope.Include = []string{"src"}
	got := walkPaths(t, root, cfg)
	want := []string{"src/a.go", "src/deep/b.go"}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestWalkCapturesStatAndKinds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main")

	w := worktreefs.New()
	var regular worktree.Node
	var sawDir bool
	err := w.Walk(context.Background(), paths.New(root), config.Defaults().HistoryScanScope(), func(n worktree.Node) error {
		switch n.Kind {
		case worktree.KindRegular:
			regular = n
		case worktree.KindDir:
			sawDir = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	_ = sawDir
	if regular.Open == nil {
		t.Errorf("regular file node missing Open")
	}
	if regular.Stat.Size != int64(len("package main")) {
		t.Errorf("stat size = %d, want %d", regular.Stat.Size, len("package main"))
	}
}
