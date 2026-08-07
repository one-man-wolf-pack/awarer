//go:build darwin || linux || freebsd

package projfs

import (
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/paths"
)

// The permission proof on Unix: the .awa marker, every required state directory, and
// the awa-owned .gitignore guard are created owner-private, so local evidence is
// never group- or world-accessible by default. The project root itself is the user's
// directory and is not asserted.
func TestCreatedStateIsOwnerPrivate(t *testing.T) {
	root := t.TempDir()
	l := paths.New(root)

	if err := CreateMarker(l); err != nil {
		t.Fatalf("CreateMarker: %v", err)
	}
	if err := EnsureDirs(l); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	if _, err := EnsureStateGitignore(l); err != nil {
		t.Fatalf("EnsureStateGitignore: %v", err)
	}

	assertMode(t, l.AwaDir(), paths.DirPerm)
	for _, d := range l.RequiredDirs() {
		assertMode(t, d, paths.DirPerm)
	}
	assertMode(t, l.StateGitignore(), paths.FilePerm)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
	}
}
