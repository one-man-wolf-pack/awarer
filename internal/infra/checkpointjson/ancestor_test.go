// Store-ancestor and no-follow contracts that hold on every platform awa ships to. They
// live in an untagged file so the Windows lane executes them too: what a store enumeration
// accepts and refuses is product behavior, not a unix detail. The one ancestor assertion
// that instead rests on what the darwin/linux no-follow implementation does lives in
// ancestor_unix_test.go.
//
// Symlink setup goes through mustSymlink, so an unavailable symlink privilege fails the
// job under AWA_REQUIRE_SYMLINK_TESTS rather than quietly removing these cases.

package checkpointjson

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
)

// TestRepoListRejectsSymlinkedCheckpointsDir proves a symlinked checkpoints directory
// pointing at an empty outside directory is reported as corruption, not as an empty
// store: listing must not silently traverse the symlink (#37).
func TestRepoListRejectsSymlinkedCheckpointsDir(t *testing.T) {
	layout := paths.New(t.TempDir())
	symlinkCheckpointsDir(t, layout) // checkpoints -> empty outside directory
	repo := NewRepo(layout)
	if _, err := repo.ListHeaders(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("List through symlinked checkpoints dir err = %v, want ErrCorruptStore", err)
	}
}

// TestRepoListRejectsNonDirectoryCheckpoints proves a regular file where the checkpoints
// directory belongs is reported as corruption, not a generic error (#40).
func TestRepoListRejectsNonDirectoryCheckpoints(t *testing.T) {
	layout := paths.New(t.TempDir())
	if err := os.MkdirAll(layout.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.CheckpointsDir(), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRepo(layout).ListHeaders(context.Background()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("List with a file at checkpoints/ err = %v, want ErrCorruptStore", err)
	}
}

// TestRepoPutRejectsNonDirectoryCheckpoints proves Put fails closed when a regular
// file sits where the checkpoints directory belongs (#41).
func TestRepoPutRejectsNonDirectoryCheckpoints(t *testing.T) {
	layout := paths.New(t.TempDir())
	if err := os.MkdirAll(layout.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.CheckpointsDir(), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := buildCheckpoint(t, idFrom(t, 0x05), time.Unix(1, 0).UTC(), false)
	if err := s.putErr(NewRepo(layout)); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Put with a file at checkpoints/ err = %v, want ErrCorruptStore", err)
	}
}

// TestRepoDeleteRejectsNonDirectoryCheckpoints proves Delete fails closed when a
// regular file sits where the checkpoints directory belongs, rather than failing with
// a raw ENOTDIR (#42).
func TestRepoDeleteRejectsNonDirectoryCheckpoints(t *testing.T) {
	layout := paths.New(t.TempDir())
	if err := os.MkdirAll(layout.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.CheckpointsDir(), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewRepo(layout).Delete(idFrom(t, 0x07)); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Delete with a file at checkpoints/ err = %v, want ErrCorruptStore", err)
	}
}

// symlinkCheckpointsDir creates a real .awa with the checkpoints/ directory replaced by
// a symlink to an outside directory, returning the outside path. It models a store
// whose checkpoints directory has been tampered into pointing outside the project.
func symlinkCheckpointsDir(t *testing.T, layout paths.Layout) string {
	t.Helper()
	if err := os.MkdirAll(layout.AwaDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-checkpoints")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, layout.CheckpointsDir())
	return outside
}

// TestRepoPutRejectsSymlinkedCheckpointsDir proves the write path fails closed: Put
// must not write a checkpoint record through a symlinked checkpoints directory.
func TestRepoPutRejectsSymlinkedCheckpointsDir(t *testing.T) {
	layout := paths.New(t.TempDir())
	outside := symlinkCheckpointsDir(t, layout)
	repo := NewRepo(layout)

	s := buildCheckpoint(t, idFrom(t, 0x01), time.Unix(1, 0).UTC(), false)
	if err := s.putErr(repo); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Put through symlinked checkpoints dir err = %v, want ErrCorruptStore", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("Put wrote %d entries outside the store, want 0", len(entries))
	}
}

// TestRepoDeleteRejectsSymlinkedCheckpointsDir proves Delete fails closed rather than
// removing a file through a symlinked checkpoints directory.
func TestRepoDeleteRejectsSymlinkedCheckpointsDir(t *testing.T) {
	layout := paths.New(t.TempDir())
	outside := symlinkCheckpointsDir(t, layout)
	repo := NewRepo(layout)

	id := idFrom(t, 0x02)
	// Plant a record in the outside target at the checkpoint's address: Delete must
	// refuse to remove it through the symlink.
	victim := filepath.Join(outside, id.String(), headerName)
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(id); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Delete through symlinked checkpoints dir err = %v, want ErrCorruptStore", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("Delete removed the outside file through the symlink: %v", err)
	}
}
