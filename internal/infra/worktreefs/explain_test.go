package worktreefs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDecideExplainsBuiltin proves a built-in exclude reports which layer and
// pattern decided — the diagnostic "why is this path excluded?" hook.
func TestDecideExplainsBuiltin(t *testing.T) {
	e := newIgnoreEngine([]string{"node_modules", "dist"}, true, true)
	_ = e.loadDirAt(t.TempDir(), "")
	d := e.decide("node_modules", true)
	if !d.Ignored {
		t.Fatalf("node_modules should be ignored")
	}
	if d.Rule.Layer != layerBuiltin || d.Rule.Pattern != "node_modules" {
		t.Errorf("rule = %+v, want builtin/node_modules", d.Rule)
	}
}

// TestDecideExplainsGitignoreSource proves a .gitignore decision attributes the
// originating file and original line number.
func TestDecideExplainsGitignoreSource(t *testing.T) {
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

	d := e.decide("app.log", false)
	if !d.Ignored || d.Rule.Source != ".gitignore" || d.Rule.LineNo != 2 {
		t.Errorf("root rule = %+v, want .gitignore line 2", d.Rule)
	}
	d2 := e.decide("sub/x.tmp", false)
	if !d2.Ignored || d2.Rule.Source != "sub/.gitignore" {
		t.Errorf("nested rule = %+v, want sub/.gitignore", d2.Rule)
	}
}

// TestDecideUnmatched proves an unmatched path reports no rule.
func TestDecideUnmatched(t *testing.T) {
	e := newIgnoreEngine([]string{"node_modules"}, true, true)
	_ = e.loadDirAt(t.TempDir(), "")
	d := e.decide("main.go", false)
	if d.Ignored || d.Rule.Matched {
		t.Errorf("main.go should be unmatched, got %+v", d)
	}
}

// TestDecideDirectoryOnlyPatternMatchesDirectoryEntry proves a directory-only
// pattern (".rezonator/") excludes the directory entry itself and its contents, but
// not a like-named file. This is red on the pre-fix engine, where decide had no
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
	if d := e.decide(".rezonator", true); !d.Ignored {
		t.Errorf("dir-only pattern must exclude the directory entry itself, got %+v", d)
	}
	if d := e.decide(".rezonator/ledger.sqlite", false); !d.Ignored {
		t.Errorf("dir-only pattern must exclude the directory's contents, got %+v", d)
	}
	// A dir-only pattern must never match a like-named regular file.
	if d := e.decide(".rezonator", false); d.Ignored {
		t.Errorf("dir-only pattern must not exclude a like-named file, got %+v", d)
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
	if d := e.decide("a/logs", true); !d.Ignored {
		t.Errorf("nested dir-only pattern must exclude the directory entry, got %+v", d)
	}
	if d := e.decide("a/logs/app.log", false); !d.Ignored {
		t.Errorf("nested dir-only pattern must exclude the directory's contents, got %+v", d)
	}
	if d := e.decide("a/logs", false); d.Ignored {
		t.Errorf("nested dir-only pattern must not exclude a like-named file, got %+v", d)
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
	if d := e.decide("cache", true); !d.Ignored {
		t.Errorf("cache/ must exclude the cache directory, got %+v", d)
	}
	if d := e.decide("scratch.tmp", false); !d.Ignored {
		t.Errorf("*.tmp must exclude a temp file, got %+v", d)
	}
	if d := e.decide("keep.tmp", false); d.Ignored {
		t.Errorf("!keep.tmp must re-include the file, got %+v", d)
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
