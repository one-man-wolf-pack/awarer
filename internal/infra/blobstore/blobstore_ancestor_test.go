//go:build darwin || linux || freebsd

package blobstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/blob"
)

// TestBlobShardDirSymlinkRejected proves a blob is read from the store tree, not
// through a symlinked ancestor directory: replacing a shard directory with a symlink
// to an outside copy holding the same valid blob is rejected, even though the final
// file is a regular file with correct bytes.
func TestBlobShardDirSymlinkRejected(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("bytes reached through a shard directory")
	expected := hashOf(t, hasher, content)

	if _, _, err := store.Materialize(expected, opener(content)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if _, err := store.Open(expected); err != nil {
		t.Fatalf("Open before swap: %v", err)
	}

	bp, _ := blob.PathFor(expected)
	firstComp := strings.SplitN(bp.Rel(), "/", 2)[0] // the "<algo>" shard
	shard := filepath.Join(layout.BlobsDir(), firstComp)

	// Move the shard subtree outside the store, then symlink it back: the blob is
	// still a valid regular file, but it is now reached through a symlinked ancestor.
	outside := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(shard, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, shard); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, err := store.Open(expected); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Open through symlinked shard dir err = %v, want ErrCorruptBlob", err)
	}
	// Has must agree with Open: a blob reachable only through a symlinked shard is
	// not a present blob (#35).
	if _, err := store.Has(expected); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Has through symlinked shard dir err = %v, want ErrCorruptBlob", err)
	}
}

// TestMaterializeRejectsSymlinkedShardDir proves the write path fails closed: with a
// shard directory symlinked outside the store, Materialize must not publish a hard
// link through it.
func TestMaterializeRejectsSymlinkedShardDir(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("bytes that must not be published outside the store")
	expected := hashOf(t, hasher, content)

	// Create the first shard component, then replace it with a symlink to an outside
	// directory before any blob exists.
	bp, _ := blob.PathFor(expected)
	firstComp := strings.SplitN(bp.Rel(), "/", 2)[0]
	shard := filepath.Join(layout.BlobsDir(), firstComp)
	if err := os.MkdirAll(layout.BlobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-shard")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, shard); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, _, err := store.Materialize(expected, opener(content)); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Materialize into symlinked shard dir err = %v, want ErrCorruptBlob", err)
	}
	// Nothing was published through the symlink.
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("Materialize wrote %d entries outside the store, want 0", len(entries))
	}
}

// TestMaterializeRejectsNonDirectoryShard proves Materialize fails closed when a
// regular file sits where a shard directory belongs (#41).
func TestMaterializeRejectsNonDirectoryShard(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("bytes blocked by a non-directory shard")
	expected := hashOf(t, hasher, content)

	bp, _ := blob.PathFor(expected)
	firstComp := strings.SplitN(bp.Rel(), "/", 2)[0]
	if err := os.MkdirAll(layout.BlobsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a regular file where the first shard directory belongs.
	if err := os.WriteFile(filepath.Join(layout.BlobsDir(), firstComp), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.Materialize(expected, opener(content)); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Materialize into non-directory shard err = %v, want ErrCorruptBlob", err)
	}
}
