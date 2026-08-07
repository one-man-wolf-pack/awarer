package initcmd

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/configfile"
	"awarer/internal/infra/projfs"
)

func TestRunCreatesProject(t *testing.T) {
	root := t.TempDir()
	res, err := Run(Request{Root: root, Profile: ProfileDefault})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	l := paths.New(root)
	// The default profile writes no config file: an absent file means the
	// product defaults apply, so ConfigPath is empty and config.toml is absent.
	if res.ConfigPath != "" {
		t.Errorf("ConfigPath = %q, want empty (default profile writes no config)", res.ConfigPath)
	}
	if _, err := os.Stat(l.ConfigFile()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("default profile should not create config.toml, stat err = %v", err)
	}
	// The .awa directory itself is the project marker and must exist.
	if info, err := os.Stat(l.AwaDir()); err != nil || !info.IsDir() {
		t.Errorf(".awa directory missing: %v", err)
	}
	for _, d := range l.RequiredDirs() {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("required dir %q missing", d)
		}
	}
	// init must not create the SQLite file, only the indexes directory.
	if _, err := os.Stat(filepath.Join(l.IndexesDir(), "worktree.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree.sqlite should not exist after init")
	}
}

func TestRunStrictProfile(t *testing.T) {
	root := t.TempDir()
	if _, err := Run(Request{Root: root, Profile: ProfileStrict}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	l := paths.New(root)
	cfg, err := configfile.Decode(readFile(t, l.ConfigFile()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hashing.TrustMode.String() != "strict" {
		t.Errorf("strict trust_mode = %q, want strict", cfg.Hashing.TrustMode)
	}
}

func TestRunRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	if _, err := Run(Request{Root: root, Profile: ProfileDefault}); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	_, err := Run(Request{Root: root, Profile: ProfileDefault})
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Run error = %v, want ErrAlreadyInitialized", err)
	}
}

func TestRunConcurrentInitInitializesOnce(t *testing.T) {
	root := t.TempDir()
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, results[i] = Run(Request{Root: root, Profile: ProfileDefault})
		}(i)
	}
	wg.Wait()

	successes, alreadyInit := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrAlreadyInitialized):
			alreadyInit++
		default:
			t.Errorf("unexpected init error: %v", err)
		}
	}
	// The .awa marker is created exclusively, so exactly one racer wins and the
	// rest see an already-initialized project — never two successes.
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1", successes)
	}
	if alreadyInit != n-1 {
		t.Errorf("already-initialized = %d, want %d", alreadyInit, n-1)
	}
}

func TestRunDoesNotTouchRootGitignore(t *testing.T) {
	root := t.TempDir()
	if _, err := Run(Request{Root: root, Profile: ProfileDefault}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// init owns .awa/.gitignore but never creates or edits the project's root
	// .gitignore — that is the user's file.
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("root .gitignore should not be created by init")
	}
}

func TestRunPreservesExistingRootGitignore(t *testing.T) {
	root := t.TempDir()
	const original = "node_modules/\n*.log\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Request{Root: root, Profile: ProfileDefault}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("root .gitignore = %q, want it preserved byte-for-byte as %q", got, original)
	}
}

// TestRunCreatesStateGitignoreGuard pins that the guard is awa-owned state with no
// opt-out. Request carries no switch that could suppress it, and the profile must
// not become one either: local evidence has to be untracked by construction.
func TestRunCreatesStateGitignoreGuard(t *testing.T) {
	for _, profile := range []Profile{ProfileDefault, ProfileStrict} {
		root := t.TempDir()
		if _, err := Run(Request{Root: root, Profile: profile}); err != nil {
			t.Fatalf("Run(profile=%v): %v", profile, err)
		}
		got, err := os.ReadFile(paths.New(root).StateGitignore())
		if err != nil {
			t.Fatalf("profile=%v: reading .awa/.gitignore: %v", profile, err)
		}
		if string(got) != projfs.StateGitignoreContent {
			t.Errorf("profile=%v: guard = %q, want %q", profile, got, projfs.StateGitignoreContent)
		}
	}
}

func TestRunRejectsAwaFileAsNotADirectory(t *testing.T) {
	root := t.TempDir()
	// A stray regular file named .awa is not a project: discovery skips past it,
	// so init must report a real filesystem error, not "already initialized".
	if err := os.WriteFile(filepath.Join(root, ".awa"), []byte("stray"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Request{Root: root, Profile: ProfileDefault})
	if err == nil {
		t.Fatal("Run with a .awa file should error")
	}
	if errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("a .awa file must not be reported as already initialized: %v", err)
	}
	if !errors.Is(err, projfs.ErrNotDirectory) {
		t.Errorf("error = %v, want ErrNotDirectory", err)
	}
}

func TestRunRejectsAwaSymlink(t *testing.T) {
	external := t.TempDir()
	root := t.TempDir()
	// A .awa symlink to a real directory must not be adopted: state writes would
	// escape the project root. init reports a non-directory error, not "already
	// initialized", and leaves the symlink untouched.
	awa := filepath.Join(root, ".awa")
	if err := os.Symlink(external, awa); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Request{Root: root, Profile: ProfileDefault})
	if err == nil {
		t.Fatal("Run with a .awa symlink should error")
	}
	if errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("a .awa symlink must not be reported as already initialized: %v", err)
	}
	if !errors.Is(err, projfs.ErrNotDirectory) {
		t.Errorf("error = %v, want ErrNotDirectory", err)
	}
	// The symlink (and its target) must be left alone — no rollback of state we
	// did not create.
	if fi, lerr := os.Lstat(awa); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf(".awa symlink should be left intact, lstat err=%v", lerr)
	}
}

func TestRunRejectsUnknownProfile(t *testing.T) {
	root := t.TempDir()
	_, err := Run(Request{Root: root, Profile: Profile(999)})
	if err == nil {
		t.Fatal("Run with unknown profile should error")
	}
	// No side effects: an unknown profile must not half-initialize.
	if _, statErr := os.Stat(filepath.Join(root, ".awa")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error(".awa should not be created for an unknown profile")
	}
}

func TestRunRejectsEmptyRoot(t *testing.T) {
	if _, err := Run(Request{Root: "", Profile: ProfileDefault}); err == nil {
		t.Fatal("Run with empty root should error")
	}
}

func TestRunCanonicalizesRelativeRoot(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	res, err := Run(Request{Root: ".", Profile: ProfileDefault})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !filepath.IsAbs(res.Root) {
		t.Errorf("Result.Root = %q, want an absolute path", res.Root)
	}
	// The project marker (.awa) is created under the resolved root.
	if info, err := os.Stat(filepath.Join(root, ".awa")); err != nil || !info.IsDir() {
		t.Errorf(".awa not created under resolved root: %v", err)
	}
}

func TestParseProfile(t *testing.T) {
	if p, err := ParseProfile("strict"); err != nil || p != ProfileStrict {
		t.Errorf("ParseProfile(strict) = %v, %v", p, err)
	}
	if _, err := ParseProfile("paranoid"); err == nil {
		t.Error("ParseProfile(paranoid) should error")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
