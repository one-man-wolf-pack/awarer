package projfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/paths"
)

// initProject creates a .awa directory (the project marker) with a config file
// under root. The config file is incidental — discovery keys on the directory —
// but tests that exercise the config helpers rely on it being present.
func initProject(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, paths.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, paths.ConfigFileName), []byte("# config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindRootFromNestedDir(t *testing.T) {
	root := t.TempDir()
	initProject(t, root)

	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindRoot(nested)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	// macOS temp dirs are under /private symlinks; compare resolved paths.
	if !sameDir(t, got, root) {
		t.Errorf("FindRoot = %q, want %q", got, root)
	}
}

func TestFindRootAtRootItself(t *testing.T) {
	root := t.TempDir()
	initProject(t, root)

	got, err := FindRoot(root)
	if err != nil {
		t.Fatalf("FindRoot: %v", err)
	}
	if !sameDir(t, got, root) {
		t.Errorf("FindRoot = %q, want %q", got, root)
	}
}

func TestFindRootNotFound(t *testing.T) {
	dir := t.TempDir() // no .awa anywhere up to the filesystem root
	_, err := FindRoot(dir)
	if !errors.Is(err, ErrRootNotFound) {
		t.Fatalf("FindRoot error = %v, want ErrRootNotFound", err)
	}
}

func TestEmptyInputIsRejected(t *testing.T) {
	// filepath.Abs("") would resolve to cwd; both entry points must refuse it
	// instead of silently adopting the current directory as a project.
	if _, err := FindRoot(""); err == nil {
		t.Error("FindRoot(\"\") should error")
	} else if errors.Is(err, ErrRootNotFound) {
		t.Error("FindRoot(\"\") should not be reported as ErrRootNotFound")
	}
	if _, err := Open(""); err == nil {
		t.Error("Open(\"\") should error")
	} else if errors.Is(err, ErrRootNotFound) {
		t.Error("Open(\"\") should not be reported as ErrRootNotFound")
	}
}

func TestEnsureDirsIdempotent(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)

	for i := 0; i < 2; i++ {
		if err := EnsureDirs(l); err != nil {
			t.Fatalf("EnsureDirs (call %d): %v", i, err)
		}
	}
	for _, d := range l.RequiredDirs() {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			t.Errorf("expected directory %q to exist", d)
		}
	}
}

func TestWriteNewFileAtomic(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sub", "config.toml")

	if err := WriteNewFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Errorf("contents = %q, want %q", got, "first")
	}

	// No leftover temp files in the directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestWriteNewFileAtomicRefusesExistingTarget(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.toml")
	// The target already exists when publish is attempted — the TOCTOU case a
	// concurrent init or external write would create.
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteNewFileAtomic(path, []byte("replacement"), 0o644)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want errors.Is(..., os.ErrExist)", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("contents = %q, want %q (must not clobber)", got, "original")
	}

	// The failed write must not leave a temp file behind.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.toml" {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestConfigExists(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)
	if ok, err := ConfigExists(l); err != nil || ok {
		t.Errorf("ConfigExists before init = (%v, %v), want (false, nil)", ok, err)
	}
	initProject(t, root)
	if ok, err := ConfigExists(l); err != nil || !ok {
		t.Errorf("ConfigExists after init = (%v, %v), want (true, nil)", ok, err)
	}
}

func TestConfigExistsSurfacesStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission checks")
	}
	root := t.TempDir()
	awa := filepath.Join(root, paths.Dir)
	if err := os.MkdirAll(awa, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make .awa unsearchable so stat of config.toml fails with a permission
	// error rather than "not found".
	if err := os.Chmod(awa, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(awa, 0o755) }) // let TempDir cleanup work

	ok, err := ConfigExists(paths.New(root))
	if err == nil {
		t.Fatal("ConfigExists should surface the permission error, not report (false, nil)")
	}
	if ok {
		t.Error("ConfigExists = true on stat error, want false")
	}
	if errors.Is(err, ErrRootNotFound) {
		t.Error("permission error must not be reported as ErrRootNotFound")
	}
}

func TestOpenRequiresAwaDir(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root); !errors.Is(err, ErrRootNotFound) {
		t.Errorf("Open on empty dir = %v, want ErrRootNotFound", err)
	}
	initProject(t, root)
	p, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	l := mustPaths(t, p)
	if !sameDir(t, l.Root(), root) {
		t.Errorf("Root() = %q, want %q", l.Root(), root)
	}
	if !sameDir(t, filepath.Dir(l.ConfigFile()), filepath.Join(root, paths.Dir)) {
		t.Errorf("ConfigFile() = %q, unexpected location", l.ConfigFile())
	}
}

// TestDiscoveryMarkerIsDirectoryNotConfig locks the marker contract: a
// project is marked by the .awa directory alone, with no config file, and a
// path named .awa that is not a directory must not be treated as a root.
func TestDiscoveryMarkerIsDirectoryNotConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, paths.Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// No config.toml: discovery must still succeed on the directory alone.
	if got, err := FindRoot(root); err != nil || !sameDir(t, got, root) {
		t.Errorf("FindRoot with .awa dir and no config = (%q, %v), want root", got, err)
	}
	if _, err := Open(root); err != nil {
		t.Errorf("Open with .awa dir and no config = %v, want success", err)
	}

	// A .awa that is a regular file does not mark a root; discovery keeps walking.
	fileRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(fileRoot, paths.Dir), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindRoot(fileRoot); !errors.Is(err, ErrRootNotFound) {
		t.Errorf("FindRoot with a .awa file = %v, want ErrRootNotFound", err)
	}
}

// TestMarkerRejectsSymlink locks the hard state-boundary: a .awa symlink to a
// real external directory must not be accepted as a project marker, because all
// project state would then be written through the symlink, outside the root.
func TestMarkerRejectsSymlink(t *testing.T) {
	external := t.TempDir() // a real directory the symlink will point at
	root := t.TempDir()
	awa := filepath.Join(root, paths.Dir)
	if err := os.Symlink(external, awa); err != nil {
		t.Fatal(err)
	}

	// FindRoot must not adopt the symlinked .awa: it keeps walking and reports no
	// project (the symlink target happens to be a real dir, which os.Stat would
	// have wrongly accepted).
	if _, err := FindRoot(root); !errors.Is(err, ErrRootNotFound) {
		t.Errorf("FindRoot with a .awa symlink = %v, want ErrRootNotFound", err)
	}
	// Open on the same root rejects it as a non-directory marker, not a project.
	if _, err := Open(root); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("Open with a .awa symlink = %v, want ErrNotDirectory", err)
	}
}

func TestFindResolvesProjectFromNestedDir(t *testing.T) {
	root := t.TempDir()
	initProject(t, root)
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Find(nested)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if !sameDir(t, mustPaths(t, p).Root(), root) {
		t.Errorf("Root() = %q, want %q", mustPaths(t, p).Root(), root)
	}
	if !p.IsResolved() {
		t.Error("Find result should be resolved")
	}
}

func TestZeroProjectIsNotResolved(t *testing.T) {
	var p Project
	if p.IsResolved() {
		t.Error("zero Project must report IsResolved() == false")
	}
	if _, err := p.Paths(); !errors.Is(err, ErrUnresolvedProject) {
		t.Errorf("zero Project Paths() = %v, want ErrUnresolvedProject", err)
	}
}

func TestOpenNormalizesToAbsoluteRoot(t *testing.T) {
	root := t.TempDir()
	initProject(t, root)

	// Open from a relative path: the resolved root must be absolute, not
	// dependent on the caller's working directory.
	restore := chdir(t, root)
	defer restore()

	p, err := Open(".")
	if err != nil {
		t.Fatalf("Open(\".\"): %v", err)
	}
	l := mustPaths(t, p)
	if !filepath.IsAbs(l.Root()) {
		t.Errorf("Root() = %q, want an absolute path", l.Root())
	}
	if !sameDir(t, l.Root(), root) {
		t.Errorf("Root() = %q, want %q", l.Root(), root)
	}
}

// mustPaths returns the resolved project's layout, failing the test if the
// project is unresolved.
func mustPaths(t *testing.T, p Project) paths.Layout {
	t.Helper()
	l, err := p.Paths()
	if err != nil {
		t.Fatalf("Paths(): %v", err)
	}
	return l
}

func TestDirExists(t *testing.T) {
	root := t.TempDir()
	if ok, err := DirExists(filepath.Join(root, "nope")); err != nil || ok {
		t.Errorf("DirExists(missing) = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := DirExists(root); err != nil || !ok {
		t.Errorf("DirExists(dir) = (%v, %v), want (true, nil)", ok, err)
	}

	// A path that exists but is a file is distinct from "missing".
	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := DirExists(file)
	if ok {
		t.Error("DirExists(file) = true, want false")
	}
	if !errors.Is(err, ErrNotDirectory) {
		t.Errorf("DirExists(file) err = %v, want ErrNotDirectory", err)
	}
}

// chdir changes the working directory for a test and returns a restore func.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(prev) }
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	return ra == rb
}

func TestStateGitignoreEffective(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"canonical", StateGitignoreContent, true},
		{"bare-star", "*\n", true},
		{"star-with-comment-and-blanks", "# note\n\n*\n", true},
		{"star-with-trailing-space", "  *  \n", true},
		{"empty", "", false},
		{"only-comment", "# just a comment\n", false},
		{"only-blank", "\n\n", false},
		{"unrelated-pattern", "*.log\nbuild/\n", false},
		{"star-then-negation", "*\n!keep\n", false},
		{"negation-then-star", "!keep\n*\n", false},
		{"double-star", "**\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StateGitignoreEffective([]byte(tc.content)); got != tc.want {
				t.Errorf("StateGitignoreEffective(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestEnsureStateGitignoreCreatesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)
	if err := os.MkdirAll(l.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureStateGitignore(l)
	if err != nil {
		t.Fatalf("first EnsureStateGitignore: %v", err)
	}
	if !changed {
		t.Errorf("first call changed = false, want true (guard was missing)")
	}
	got, err := os.ReadFile(l.StateGitignore())
	if err != nil {
		t.Fatalf("reading guard: %v", err)
	}
	if string(got) != StateGitignoreContent {
		t.Errorf("guard content = %q, want %q", got, StateGitignoreContent)
	}

	// A second call on a healthy guard writes nothing.
	changed, err = EnsureStateGitignore(l)
	if err != nil {
		t.Fatalf("second EnsureStateGitignore: %v", err)
	}
	if changed {
		t.Errorf("second call changed = true, want false (guard already effective)")
	}
}

func TestEnsureStateGitignoreReplacesIneffective(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)
	if err := os.MkdirAll(l.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// An existing but ineffective guard (no ignore-all) must be repaired.
	if err := os.WriteFile(l.StateGitignore(), []byte("# stale, ignores nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureStateGitignore(l)
	if err != nil {
		t.Fatalf("EnsureStateGitignore: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true (ineffective guard replaced)")
	}
	got, err := os.ReadFile(l.StateGitignore())
	if err != nil {
		t.Fatalf("reading guard: %v", err)
	}
	if string(got) != StateGitignoreContent {
		t.Errorf("guard content = %q, want canonical %q", got, StateGitignoreContent)
	}
}

func TestInspectStateGitignore(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)
	if err := os.MkdirAll(l.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	if st, err := InspectStateGitignore(l); err != nil || st != StateGitignoreMissing {
		t.Errorf("missing: state = %v, err = %v; want StateGitignoreMissing", st, err)
	}

	if err := os.WriteFile(l.StateGitignore(), []byte("# nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := InspectStateGitignore(l); err != nil || st != StateGitignoreIneffective {
		t.Errorf("ineffective: state = %v, err = %v; want StateGitignoreIneffective", st, err)
	}

	if err := os.WriteFile(l.StateGitignore(), []byte(StateGitignoreContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := InspectStateGitignore(l); err != nil || st != StateGitignoreOK {
		t.Errorf("ok: state = %v, err = %v; want StateGitignoreOK", st, err)
	}
}

func TestInspectStateGitignoreRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)
	if err := os.MkdirAll(l.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A target whose body is a valid guard, reached only through a symlink. git does
	// not honor a symlinked .gitignore, so .awa stays exposed — this must classify as
	// ineffective (repairable), not OK, even though the target content is "*".
	target := filepath.Join(root, "real-gitignore")
	if err := os.WriteFile(target, []byte(StateGitignoreContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, l.StateGitignore()); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if st, err := InspectStateGitignore(l); err != nil || st != StateGitignoreIneffective {
		t.Fatalf("symlink guard: state = %v, err = %v; want StateGitignoreIneffective", st, err)
	}

	// Repair replaces the symlink with a real, canonical guard file.
	changed, err := EnsureStateGitignore(l)
	if err != nil {
		t.Fatalf("EnsureStateGitignore: %v", err)
	}
	if !changed {
		t.Errorf("changed = false, want true (symlink guard replaced)")
	}
	info, err := os.Lstat(l.StateGitignore())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Errorf("guard mode = %v, want a regular file after repair", info.Mode())
	}
	got, err := os.ReadFile(l.StateGitignore())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != StateGitignoreContent {
		t.Errorf("guard content = %q, want canonical %q", got, StateGitignoreContent)
	}
	// The symlink target must be left untouched — repair replaces the link, not what
	// it pointed at.
	if tgt, err := os.ReadFile(target); err != nil || string(tgt) != StateGitignoreContent {
		t.Errorf("symlink target changed: %q, err %v", tgt, err)
	}
}

func TestInspectStateGitignoreRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)
	if err := os.MkdirAll(l.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory at the guard path cannot be read or safely overwritten in place:
	// report it as an error (doctor turns it into a non-repairable "unreadable").
	if err := os.Mkdir(l.StateGitignore(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectStateGitignore(l); err == nil {
		t.Fatal("a directory at the guard path should surface an error, not a state")
	}
}
