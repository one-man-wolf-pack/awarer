//go:build darwin || linux || freebsd

package checkpointjson

import (
	"os"
	"testing"
	"time"

	"awarer/internal/domain/paths"
)

// TestPublishedCheckpointFilesArePrivate proves a published checkpoint header and
// manifest are owner-only: they hold project evidence (paths, modes, hashes), so
// their file mode — not just the enclosing directory — must be private.
func TestPublishedCheckpointFilesArePrivate(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)

	s := buildCheckpoint(t, idFrom(t, 0xa1), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)

	for _, path := range []string{repo.headerFor(s.id()), repo.manifestFor(s.id())} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("lstat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != paths.ReadOnlyFilePerm {
			t.Errorf("%s mode = %o, want %o (owner-only read-only)", path, got, paths.ReadOnlyFilePerm)
		}
	}
}
