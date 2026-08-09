package worktreefs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDecideBuiltinExclude proves a configured built-in exclude decides on its own,
// with no ignore file present.
func TestDecideBuiltinExclude(t *testing.T) {
	e := newIgnoreEngine([]string{"node_modules", "dist"}, true, true)
	if err := e.loadDirAt(t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if !e.ignores("node_modules", true) {
		t.Errorf("node_modules should be ignored by the built-in excludes")
	}
	if e.ignores("src", true) {
		t.Errorf("src should not be ignored")
	}
}

// TestDecideRootAndNestedGitignore proves both a root and a nested .gitignore apply,
// each to its own subtree: the nested file's pattern is rewritten to root-relative
// form and must not leak outside the directory that declared it.
func TestDecideRootAndNestedGitignore(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "# comment\n*.log\n")
	writeRaw(t, root, "sub/.gitignore", "*.tmp\n")

	e := newIgnoreEngine(nil, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.loadDirAt(filepath.Join(root, "sub"), "sub"); err != nil {
		t.Fatal(err)
	}

	if !e.ignores("app.log", false) {
		t.Errorf("app.log should be ignored by the root .gitignore")
	}
	if !e.ignores("sub/x.tmp", false) {
		t.Errorf("sub/x.tmp should be ignored by the nested .gitignore")
	}
	if e.ignores("x.tmp", false) {
		t.Errorf("x.tmp at the root should not be ignored by sub/.gitignore")
	}
}

// TestDecideNestedGitignoreYieldsToAwaignoreLayer proves the layer ordering a shared
// matcher must preserve: every .awaignore contribution is applied after every
// .gitignore contribution, whatever their relative depth. A shallow .awaignore
// therefore still overrides a deeper .gitignore in both directions — re-including a
// file the deeper .gitignore excluded, and excluding one the shallower .gitignore
// re-included. An implementation that appended a directory's local patterns onto its
// parent's compiled matcher would invert this, putting a local .gitignore after an
// inherited .awaignore.
func TestDecideNestedGitignoreYieldsToAwaignoreLayer(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "# binaries are usually vendored here\n!a/*.bin\n")
	writeRaw(t, root, ".awaignore", "# the checked-in sample log stays\n!a/keep.log\n")
	writeRaw(t, root, "a/.gitignore", "# drop build logs\n*.log\n")
	writeRaw(t, root, "a/.awaignore", "# this project's binaries are scan noise\n*.bin\n")

	e := newIgnoreEngine(nil, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.loadDirAt(filepath.Join(root, "a"), "a"); err != nil {
		t.Fatal(err)
	}

	if !e.ignores("a/app.log", false) {
		t.Errorf("a/app.log should be ignored by the nested .gitignore")
	}
	// The root .awaignore negation is applied after the deeper .gitignore exclusion.
	if e.ignores("a/keep.log", false) {
		t.Errorf("a/keep.log: the root .awaignore negation must outrank the nested .gitignore")
	}
	// And in the other direction: the nested .awaignore exclusion outranks the root
	// .gitignore negation.
	if !e.ignores("a/tool.bin", false) {
		t.Errorf("a/tool.bin: the nested .awaignore must outrank the root .gitignore negation")
	}
}

// TestDecideIgnoreFileNegationOutranksBuiltinExclude proves the built-in excludes are
// genuinely the lowest layer: they are compiled ahead of every ignore-file
// contribution, so a .gitignore negation re-includes a path the configured excludes
// dropped. Compiling them last instead would silently make a configured exclude
// impossible to negate.
func TestDecideIgnoreFileNegationOutranksBuiltinExclude(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "!keep.log\n")

	e := newIgnoreEngine([]string{"*.log"}, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}

	if !e.ignores("app.log", false) {
		t.Errorf("app.log should be ignored by the built-in exclude")
	}
	if e.ignores("keep.log", false) {
		t.Errorf("keep.log: a .gitignore negation must outrank a built-in exclude")
	}
}

// TestDecideDeeperGitignoreOutranksShallowerOne proves the shallow-to-deep order
// inside one layer: a nested .gitignore is compiled after its ancestors', so its
// negation re-includes a file the root .gitignore excluded, while the root rule still
// governs paths the nested file says nothing about. Compiling the chain deep-to-shallow
// would invert which file wins.
func TestDecideDeeperGitignoreOutranksShallowerOne(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".gitignore", "*.log\n")
	writeRaw(t, root, "a/.gitignore", "!app.log\n")

	e := newIgnoreEngine(nil, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.loadDirAt(filepath.Join(root, "a"), "a"); err != nil {
		t.Fatal(err)
	}

	if e.ignores("a/app.log", false) {
		t.Errorf("a/app.log: the deeper .gitignore negation must outrank the root .gitignore")
	}
	if !e.ignores("a/other.log", false) {
		t.Errorf("a/other.log should still be ignored by the root .gitignore")
	}
	if !e.ignores("app.log", false) {
		t.Errorf("app.log at the root should be ignored; the nested negation applies below a/ only")
	}
}

// TestDecideUnmatched proves a path no rule mentions is not ignored.
func TestDecideUnmatched(t *testing.T) {
	e := newIgnoreEngine([]string{"node_modules"}, true, true)
	if err := e.loadDirAt(t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if e.ignores("main.go", false) {
		t.Errorf("main.go should not be ignored")
	}
}

// TestDecideDirectoryOnlyPatternMatchesDirectoryEntry proves a directory-only
// pattern (".rezonator/") excludes the directory entry itself and its contents, but
// not a like-named file. This is red on the pre-fix engine, where the decision had no
// isDir parameter and queried the bare path, so a dir-only pattern matched only the
// descendants — leaving the directory entry to be emitted and appear as "added" the
// first time it exists after a baseline.
func TestDecideDirectoryOnlyPatternMatchesDirectoryEntry(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".awaignore", ".rezonator/\n")
	e := newIgnoreEngine(nil, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}
	if !e.ignores(".rezonator", true) {
		t.Errorf("dir-only pattern must exclude the directory entry itself")
	}
	if !e.ignores(".rezonator/ledger.sqlite", false) {
		t.Errorf("dir-only pattern must exclude the directory's contents")
	}
	// A dir-only pattern must never match a like-named regular file.
	if e.ignores(".rezonator", false) {
		t.Errorf("dir-only pattern must not exclude a like-named file")
	}
}

// TestDecideNestedDirectoryOnlyPattern proves the same for a directory-only pattern
// in a nested .awaignore, which is rewritten to a root-relative form: the trailing
// slash and its directory-entry semantics survive the rewrite.
func TestDecideNestedDirectoryOnlyPattern(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, "a/.awaignore", "logs/\n")
	e := newIgnoreEngine(nil, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.loadDirAt(filepath.Join(root, "a"), "a"); err != nil {
		t.Fatal(err)
	}
	if !e.ignores("a/logs", true) {
		t.Errorf("nested dir-only pattern must exclude the directory entry")
	}
	if !e.ignores("a/logs/app.log", false) {
		t.Errorf("nested dir-only pattern must exclude the directory's contents")
	}
	if e.ignores("a/logs", false) {
		t.Errorf("nested dir-only pattern must not exclude a like-named file")
	}
}

// TestDecideDirectoryOnlyPatternPreservesFileNegation proves the directory-aware
// query does not disturb file-level negation and precedence: alongside a dir-only
// exclude, a later "!keep.tmp" still re-includes a file a broad "*.tmp" excluded, and
// the dir-only pattern does not spuriously catch a file. (Re-including a path *under*
// a directory-only exclude is intentionally not attempted here: gitignore forbids
// re-including anything beneath an excluded directory, and the walk prunes such a
// directory outright — the behavior the primary walk oracle covers.)
func TestDecideDirectoryOnlyPatternPreservesFileNegation(t *testing.T) {
	root := t.TempDir()
	writeRaw(t, root, ".awaignore", "cache/\n*.tmp\n!keep.tmp\n")
	e := newIgnoreEngine(nil, true, true)
	if err := e.loadDirAt(root, ""); err != nil {
		t.Fatal(err)
	}
	if !e.ignores("cache", true) {
		t.Errorf("cache/ must exclude the cache directory")
	}
	if !e.ignores("scratch.tmp", false) {
		t.Errorf("*.tmp must exclude a temp file")
	}
	if e.ignores("keep.tmp", false) {
		t.Errorf("!keep.tmp must re-include the file")
	}
}

func writeRaw(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
