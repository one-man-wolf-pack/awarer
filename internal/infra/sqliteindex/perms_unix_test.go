//go:build darwin || linux || freebsd

package sqliteindex_test

import (
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/paths"
	"awarer/internal/infra/sqliteindex"
)

// TestOpenPrivatizesIndexFiles proves the worktree index database and its WAL/SHM
// sidecars are owner-private after Open, so index evidence does not rely on the
// parent directory's mode alone.
func TestOpenPrivatizesIndexFiles(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	defer func() { _ = idx.Close() }()

	checked := 0
	for _, name := range sqliteindex.OwnedFiles() {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			continue // a sidecar (e.g. -wal/-shm) may not exist on every filesystem
		}
		checked++
		if got := info.Mode().Perm(); got != paths.FilePerm {
			t.Errorf("%s mode = %o, want %o (owner-private)", name, got, paths.FilePerm)
		}
	}
	if checked == 0 {
		t.Fatal("no owned index files found to check")
	}
}
