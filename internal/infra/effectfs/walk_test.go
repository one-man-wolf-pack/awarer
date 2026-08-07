package effectfs_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"awarer/internal/app/effectobserve"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
)

func newWalker(t *testing.T) *effectfs.Walker {
	t.Helper()
	h := blake3hash.New()
	return effectfs.New(h)
}

// newWalkerLimited builds a Walker with tiny entry budgets so the fail-closed paths
// can be exercised without creating enormous directories.
func newWalkerLimited(t *testing.T, perRoot, total, discovery int) *effectfs.Walker {
	t.Helper()
	h := blake3hash.New()
	return effectfs.NewWithLimits(h, perRoot, total, discovery)
}

// byPath indexes discovered roots by their project-relative path.
func byPath(ds []effectobserve.DiscoveredRoot) map[string]effectobserve.DiscoveredRoot {
	out := make(map[string]effectobserve.DiscoveredRoot, len(ds))
	for _, d := range ds {
		out[d.Path] = d
	}
	return out
}

func TestDiscoverFindsRootAndNested(t *testing.T) {
	root := t.TempDir()
	// A root-level watched dir and a nested one under a monorepo package.
	mustWrite(t, filepath.Join(root, "node_modules", "a", "index.js"), "x")
	mustWrite(t, filepath.Join(root, "packages", "api", "target", "app"), "y")
	// A non-watched source file must not be observed as a root.
	mustWrite(t, filepath.Join(root, "src", "main.go"), "z")

	got, err := newWalker(t).Discover(root, []string{"node_modules", "target"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	idx := byPath(got)
	if _, ok := idx["node_modules"]; !ok {
		t.Errorf("root-level node_modules not discovered: %v", idx)
	}
	if _, ok := idx["packages/api/target"]; !ok {
		t.Errorf("nested packages/api/target not discovered: %v", idx)
	}
	if len(got) != 2 {
		t.Errorf("discovered %d roots, want exactly 2 (root node_modules + nested target)", len(got))
	}
}

func TestDiscoverDoesNotDescendIntoMatchedRoot(t *testing.T) {
	root := t.TempDir()
	// A watched dir nested inside another watched dir must not be reported: the outer
	// match is not descended into.
	mustWrite(t, filepath.Join(root, "node_modules", "pkg", "dist", "bundle.js"), "x")
	got, err := newWalker(t).Discover(root, []string{"node_modules", "dist"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Path != "node_modules" {
		t.Errorf("discovered %v, want only the outer node_modules (no descent into a matched root)", got)
	}
}

func TestDiscoverSkipsProtected(t *testing.T) {
	root := t.TempDir()
	// A watched name inside .git must never be observed.
	mustWrite(t, filepath.Join(root, ".git", "node_modules", "x"), "x")
	mustWrite(t, filepath.Join(root, "target", "app"), "y")
	got, err := newWalker(t).Discover(root, []string{"node_modules", "target"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Path != "target" {
		t.Errorf("discovered %v, want only target (protected .git must be skipped)", got)
	}
}

func TestDiscoverDeterministicAndDetectsChange(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "target", "app"), "v1")
	w := newWalker(t)

	a, err := w.Discover(root, []string{"target"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	b, err := w.Discover(root, []string{"target"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if a[0].SubHash.String() != b[0].SubHash.String() {
		t.Errorf("two observations of the same dir differ: %s vs %s", a[0].SubHash, b[0].SubHash)
	}

	// Change the content: the sub-hash must move.
	mustWrite(t, filepath.Join(root, "target", "app"), "v2-longer")
	c, _ := w.Discover(root, []string{"target"})
	if a[0].SubHash.String() == c[0].SubHash.String() {
		t.Error("a changed file under a watched dir must change its sub-hash")
	}
}

func TestDiscoverOverBudgetFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "node_modules")
	// More entries than the tiny per-root budget: the directory cannot be fully
	// observed, so Discover fails closed rather than returning a partial signature.
	for i := 0; i < 40; i++ {
		mustWrite(t, filepath.Join(dir, "pkg-"+strconv.Itoa(i)), "x")
	}
	if _, err := newWalkerLimited(t, 8, 100, 100_000).Discover(root, []string{"node_modules"}); err == nil {
		t.Error("a directory over the entry budget must fail Discover closed, not observe partially")
	}
}

// TestDiscoverOverBudgetNamesRoot proves the fail-closed budget overrun surfaces a typed
// OverBudgetError carrying the offending root's project-relative path and the budget it
// crossed, so a latency diagnostic can name the huge root even though the observation
// itself is unavailable. The true count is deliberately not carried (the walk stopped at
// the budget), only the crossed threshold.
func TestDiscoverOverBudgetNamesRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "packages", "api", "node_modules")
	for i := 0; i < 40; i++ {
		mustWrite(t, filepath.Join(dir, "pkg-"+strconv.Itoa(i)), "x")
	}
	_, err := newWalkerLimited(t, 8, 100, 100_000).Discover(root, []string{"node_modules"})
	var over *effectobserve.OverBudgetError
	if !errors.As(err, &over) {
		t.Fatalf("err = %v, want an *effectobserve.OverBudgetError", err)
	}
	if over.Path != "packages/api/node_modules" {
		t.Errorf("over-budget path = %q, want packages/api/node_modules", over.Path)
	}
	if over.Limit != 8 {
		t.Errorf("over-budget limit = %d, want 8", over.Limit)
	}
}

func TestDiscoverDeepOverBudgetFailsClosed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "target")
	// Files nested deep (small top level) still exceed the deep budget, so a deep
	// change could hide behind an immediate child's unchanged stat — the directory
	// must fail closed rather than be observed on a top-level signature.
	for i := 0; i < 40; i++ {
		mustWrite(t, filepath.Join(dir, "deep", "f"+strconv.Itoa(i)), "x")
	}
	if _, err := newWalkerLimited(t, 8, 100, 100_000).Discover(root, []string{"target"}); err == nil {
		t.Error("a deep directory over the entry budget must fail Discover closed")
	}
}

func TestDiscoverTraversalBudgetFailsClosed(t *testing.T) {
	root := t.TempDir()
	// A single flat directory with many ordinary (non-matching) files and no watched
	// directory at all. The chunked traversal must count entries as it streams and stop
	// within the budget — well before reading the whole directory — rather than
	// materializing and sorting the full entry list the way os.ReadDir would.
	for i := 0; i < 300; i++ {
		mustWrite(t, filepath.Join(root, "src", "file"+strconv.Itoa(i)+".go"), "x")
	}
	if _, err := newWalkerLimited(t, 100, 100, 10).Discover(root, []string{"node_modules"}); err == nil {
		t.Error("discovery over its traversal budget must fail closed, not read the whole directory")
	}

	// With a generous discovery budget the same tree succeeds (and finds nothing).
	got, err := newWalkerLimited(t, 100, 100, 100_000).Discover(root, []string{"node_modules"})
	if err != nil {
		t.Fatalf("Discover with a generous budget: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("discovered %v, want none (no watched directory present)", got)
	}
}

func TestDiscoverDoesNotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret"), "should-not-be-read")
	mustMkdir(t, filepath.Join(root, "build"))
	if err := os.Symlink(outside, filepath.Join(root, "build", "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, err := newWalker(t).Discover(root, []string{"build"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0].Path != "build" {
		t.Errorf("discovered %v, want a single build observation (symlink folded as a leaf, not followed)", got)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
