//go:build darwin || linux || freebsd

// The one store-ancestor contract that rests on what the descriptor-relative no-follow
// implementation does rather than on product policy. Everything else in the ancestor suite
// is platform-neutral and lives in the untagged ancestor_test.go, which the Windows lane
// executes.

package checkpointjson

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/paths"
)

// TestRepoHeaderRejectsSymlinkedCheckpointDirAncestor proves a single header READ is
// refused when the checkpoint's own directory is a symlink to an outside copy, even
// though the header file itself is a regular file with correct contents. Header reads
// its record directly rather than enumerating the store, so the structural id-namespace
// check that guards List and ResolvePrefix never runs here: the ancestor traversal is
// the only thing standing between a tampered store and a foreign record read as its own.
//
// The mechanism, not the policy, is what confines this to the descriptor-relative build.
// Reading a record goes through fsx.OpenNoFollowAt: in fsx_unix.go that is a component-wise
// walk of openat(O_RDONLY|O_NOFOLLOW) calls, so a symlink at ANY ancestor fails the open and
// the store maps it to ErrCorruptStore. Everywhere else fsx_other.go serves the call and
// states the weaker contract it keeps — the final component is checked, ancestors are not —
// so this exact read succeeds there. That boundary is what the host's syscalls offer:
// darwin, linux, and freebsd all provide openat, and Windows is the one shipped target that
// does not. The corresponding no-follow guarantees that DO hold everywhere are asserted in
// ancestor_test.go over Put, List, and Delete. What those cases contract is the operation's
// refusal, not one specific guard's: the two builds reach it by different mechanisms, so
// they assert the sentinel rather than the origin.
//
// This test is therefore not evidence of a Windows defect and must not be mirrored by a
// Windows test asserting the successful traversal: that would enshrine the fallback's
// weaker reach as intended product behavior instead of the implementation limit it is.
func TestRepoHeaderRejectsSymlinkedCheckpointDirAncestor(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	if err := os.MkdirAll(layout.CheckpointsDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	// Publish a real checkpoint, then move its directory outside the store and symlink
	// it back. The header stays a valid regular file with correct contents — it is
	// simply reached through a symlinked ancestor now, which is the tampering the
	// no-follow policy exists to refuse.
	s := buildCheckpoint(t, idFrom(t, 0xcd), time.Unix(1, 0).UTC(), false)
	s.put(t, repo)
	dir := filepath.Join(layout.CheckpointsDir(), s.id().String())
	outside := filepath.Join(t.TempDir(), "moved-checkpoint")
	if err := os.Rename(dir, outside); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, dir)

	if _, err := repo.Header(s.id()); !errors.Is(err, checkpoint.ErrCorruptStore) {
		t.Fatalf("Header through symlinked checkpoint dir err = %v, want ErrCorruptStore", err)
	}
}
