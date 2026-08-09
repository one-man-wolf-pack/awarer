package worktreefs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	ignore "github.com/sabhiram/go-gitignore"
)

// loadDirs loads the ignore files of dirs (given shallow-to-deep, as a walk visits
// them) into e, creating each directory first so a directory with no ignore file is
// loaded the same way a real walk loads it.
func loadDirs(t *testing.T, e *ignoreEngine, root string, dirs []string) {
	t.Helper()
	for _, d := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(abs, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := e.loadDirAt(abs, d); err != nil {
			t.Fatal(err)
		}
	}
}

// uniqueCompiled counts the distinct compiled matchers deciding for dirs. It is the
// structural claim this packet makes: the expensive regexp state lives in these
// instances, so their count — not the number of directories — is what a walk retains.
func uniqueCompiled(e *ignoreEngine, dirs []string) int {
	seen := map[*ignore.GitIgnore]bool{}
	for _, d := range dirs {
		seen[e.combinedFor(d)] = true
	}
	return len(seen)
}

// TestNoRuleChainSharesOneCompiledMatcher proves a long chain of directories that
// contribute no ignore patterns of their own decides through the single matcher
// compiled at the root boundary, rather than each compiling an identical copy.
func TestNoRuleChainSharesOneCompiledMatcher(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")

	dirs := []string{""}
	cur := ""
	for i := 0; i < 12; i++ {
		if cur == "" {
			cur = "d00"
		} else {
			cur = fmt.Sprintf("%s/d%02d", cur, i)
		}
		dirs = append(dirs, cur)
	}

	e := newIgnoreEngine([]string{"node_modules/"}, true, true)
	loadDirs(t, e, root, dirs)

	want := e.combinedFor("")
	for _, d := range dirs {
		if got := e.combinedFor(d); got != want {
			t.Errorf("directory %q compiled its own matcher; want the root boundary's instance", d)
		}
	}
	if n := uniqueCompiled(e, dirs); n != 1 {
		t.Errorf("unique compiled matchers = %d over %d directories, want 1 (the only rule boundary)", n, len(dirs))
	}
	// The shared matcher still decides: the root rule reaches the deepest directory.
	deep := dirs[len(dirs)-1]
	if !e.ignores(deep+"/app.log", false) {
		t.Errorf("%s/app.log should be ignored by the root rule through the shared matcher", deep)
	}
	if e.ignores(deep+"/main.go", false) {
		t.Errorf("%s/main.go should not be ignored", deep)
	}
}

// TestNestedRuleCreatesOneSharedBoundary proves a directory that does contribute
// patterns compiles exactly one new matcher, that its rule-free descendants reuse that
// instance, and that a rule-free sibling stays on its own nearest ancestor boundary
// instead of being pulled into the new one.
func TestNestedRuleCreatesOneSharedBoundary(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")
	writeRaw(t, root, "a/b/.gitignore", "*.tmp\n")

	dirs := []string{"", "a", "a/b", "a/b/c", "a/b/c/d", "a/x", "a/x/y"}
	e := newIgnoreEngine(nil, true, true)
	loadDirs(t, e, root, dirs)

	rootM := e.combinedFor("")
	boundary := e.combinedFor("a/b")
	if boundary == rootM {
		t.Fatalf("a/b contributes a pattern and must compile its own matcher")
	}
	for _, d := range []string{"a/b/c", "a/b/c/d"} {
		if got := e.combinedFor(d); got != boundary {
			t.Errorf("descendant %q does not reuse the a/b boundary matcher", d)
		}
	}
	for _, d := range []string{"a", "a/x", "a/x/y"} {
		if got := e.combinedFor(d); got != rootM {
			t.Errorf("rule-free %q left the root boundary; a sibling's rule must not move it", d)
		}
	}
	if n := uniqueCompiled(e, dirs); n != 2 {
		t.Errorf("unique compiled matchers = %d over %d directories, want 2 (root and a/b)", n, len(dirs))
	}

	// Sharing routes the decision, it does not widen it: the nested rule applies below
	// a/b and nowhere else, while the root rule still reaches both subtrees.
	if !e.ignores("a/b/c/d/scratch.tmp", false) {
		t.Errorf("a/b/c/d/scratch.tmp should be ignored by a/b's rule")
	}
	if e.ignores("a/x/y/scratch.tmp", false) {
		t.Errorf("a/x/y/scratch.tmp should not be ignored by a sibling subtree's rule")
	}
	if !e.ignores("a/x/y/app.log", false) {
		t.Errorf("a/x/y/app.log should still be ignored by the root rule")
	}
}

// TestLoadingRuleLeavesLoadedSubtreeMatchersIntact proves a rule found in one subtree
// neither discards nor rebuilds a matcher already compiled for an unrelated subtree —
// the property that lets the engine drop global cache invalidation.
func TestLoadingRuleLeavesLoadedSubtreeMatchersIntact(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")
	writeRaw(t, root, "c/.gitignore", "*.tmp\n")

	e := newIgnoreEngine(nil, true, true)
	loadDirs(t, e, root, []string{"", "a", "a/b"})
	before := e.combinedFor("a/b")

	// A later subtree contributes a rule, exactly as a walk would find it.
	loadDirs(t, e, root, []string{"c", "c/d"})

	if after := e.combinedFor("a/b"); after != before {
		t.Errorf("a/b's matcher was rebuilt after an unrelated subtree loaded a rule")
	}
	if e.combinedFor("c") == e.combinedFor("") {
		t.Errorf("c contributes a pattern and must compile its own matcher")
	}
	if e.ignores("a/b/scratch.tmp", false) {
		t.Errorf("c's rule must not reach the unrelated a/b subtree")
	}
	if !e.ignores("c/d/scratch.tmp", false) {
		t.Errorf("c/d/scratch.tmp should be ignored by c's rule")
	}
}

// TestNonPatternIgnoreFilesCreateNoBoundary proves an empty or comment-only ignore
// file leaves a directory on its ancestor's matcher: only an effective pattern makes a
// boundary.
func TestNonPatternIgnoreFilesCreateNoBoundary(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")
	writeRaw(t, root, "a/.gitignore", "# nothing to ignore here yet\n\n   \n")
	writeRaw(t, root, "a/.awaignore", "")

	e := newIgnoreEngine(nil, true, true)
	loadDirs(t, e, root, []string{"", "a"})

	if e.combinedFor("a") != e.combinedFor("") {
		t.Errorf("a has only comment/empty ignore files and must not become a boundary")
	}
	if !e.ignores("a/app.log", false) {
		t.Errorf("a/app.log should still be ignored by the root rule")
	}
}

// TestVirtualDirectoryRuleOwnsItsBoundary proves a rule read from a real path but
// attributed to a virtual directory — how a followed-symlink subtree is loaded —
// creates its boundary at the virtual directory. Its virtual descendants share that
// instance, and the real path the file was read from owns no rules of its own.
func TestVirtualDirectoryRuleOwnsItsBoundary(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")
	// The ignore file lives under real/, while its rules apply at the virtual link/.
	writeRaw(t, root, "real/.gitignore", "*.tmp\n")
	if err := os.MkdirAll(filepath.Join(root, "real", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}

	e := newIgnoreEngine(nil, true, true)
	loadDirs(t, e, root, []string{""})
	if err := e.loadDirAt(filepath.Join(root, "real"), "link"); err != nil {
		t.Fatal(err)
	}
	if err := e.loadDirAt(filepath.Join(root, "real", "inner"), "link/inner"); err != nil {
		t.Fatal(err)
	}

	boundary := e.combinedFor("link")
	if boundary == e.combinedFor("") {
		t.Fatalf("the virtual directory contributes a pattern and must compile its own matcher")
	}
	if got := e.combinedFor("link/inner"); got != boundary {
		t.Errorf("a virtual descendant does not reuse its virtual ancestor's matcher")
	}
	if !e.ignores("link/inner/x.tmp", false) {
		t.Errorf("link/inner/x.tmp should be ignored under the virtual boundary")
	}
	// The rule is owned by the virtual path it was attributed to, not by the real
	// directory the file was physically read from.
	if e.ignores("real/x.tmp", false) {
		t.Errorf("real/x.tmp must not be ignored; the rule applies at the virtual path only")
	}
}

// TestAwaignoreOnlyRuleCreatesItsOwnBoundary proves a directory whose only
// contribution is an .awaignore is an effective rule boundary in its own right, the
// half of that contract the .gitignore fixtures above cannot show.
func TestAwaignoreOnlyRuleCreatesItsOwnBoundary(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")
	writeRaw(t, root, "a/.awaignore", "*.tmp\n")

	e := newIgnoreEngine(nil, true, true)
	loadDirs(t, e, root, []string{"", "a", "a/b"})

	boundary := e.combinedFor("a")
	if boundary == e.combinedFor("") {
		t.Fatalf("a contributes an .awaignore pattern and must compile its own matcher")
	}
	if got := e.combinedFor("a/b"); got != boundary {
		t.Errorf("a/b does not reuse the a boundary matcher")
	}
	if !e.ignores("a/b/scratch.tmp", false) {
		t.Errorf("a/b/scratch.tmp should be ignored by a's .awaignore")
	}
	if e.ignores("scratch.tmp", false) {
		t.Errorf("the root must not inherit a's .awaignore rule")
	}
}
