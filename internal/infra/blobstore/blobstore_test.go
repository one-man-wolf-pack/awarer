package blobstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/domain/blob"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/fsx"
)

func newStore(t *testing.T) (*FS, *blake3hash.Hasher, paths.Layout) {
	t.Helper()
	hasher := blake3hash.New()
	layout := paths.New(t.TempDir())
	return New(layout, hasher), hasher, layout
}

func hashOf(t *testing.T, hasher *blake3hash.Hasher, content []byte) hashing.ContentHash {
	t.Helper()
	h, err := hasher.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	return h
}

func opener(content []byte) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(content)), nil }
}

func TestMaterializeFirstWriteStoresBlobAtShardedPath(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("hello blob")
	expected := hashOf(t, hasher, content)

	ref, written, err := store.Materialize(expected, opener(content))
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if !written {
		t.Fatal("expected written=true on first write")
	}
	if ref.Hash() != expected {
		t.Fatalf("ref hash = %v, want %v", ref.Hash(), expected)
	}

	bp, _ := blob.PathFor(expected)
	full := filepath.Join(layout.BlobsDir(), filepath.FromSlash(bp.Rel()))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("reading stored blob: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("stored bytes = %q, want %q", got, content)
	}
}

func TestMaterializeIsIdempotent(t *testing.T) {
	store, hasher, _ := newStore(t)
	content := []byte("dedupe me")
	expected := hashOf(t, hasher, content)

	if _, written, err := store.Materialize(expected, opener(content)); err != nil || !written {
		t.Fatalf("first write: written=%v err=%v", written, err)
	}
	// Second write of identical content must not rewrite the blob.
	_, written, err := store.Materialize(expected, func() (io.ReadCloser, error) {
		t.Fatal("source should not be opened when blob already exists")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if written {
		t.Fatal("expected written=false on idempotent second write")
	}
}

func TestMaterializeWrongBytesRejected(t *testing.T) {
	store, hasher, layout := newStore(t)
	expected := hashOf(t, hasher, []byte("the real content"))

	_, _, err := store.Materialize(expected, opener([]byte("different content")))
	if !errors.Is(err, blob.ErrHashMismatch) {
		t.Fatalf("err = %v, want ErrHashMismatch", err)
	}
	// No blob published.
	if has, _ := store.Has(expected); has {
		t.Fatal("blob must not exist after a hash mismatch")
	}
	// No temp file left behind.
	assertTmpEmpty(t, layout)
}

func TestHasAndOpen(t *testing.T) {
	store, hasher, _ := newStore(t)
	content := []byte("openable")
	expected := hashOf(t, hasher, content)

	if has, _ := store.Has(expected); has {
		t.Fatal("Has should be false before materialize")
	}
	if _, _, err := store.Materialize(expected, opener(content)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if has, err := store.Has(expected); err != nil || !has {
		t.Fatalf("Has after materialize: has=%v err=%v", has, err)
	}
	rc, err := store.Open(expected)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Fatalf("Open bytes = %q, want %q", got, content)
	}
}

func TestMaterializeRejectsCorruptExistingBlob(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("trustworthy content")
	expected := hashOf(t, hasher, content)
	if _, _, err := store.Materialize(expected, opener(content)); err != nil {
		t.Fatalf("initial Materialize: %v", err)
	}

	// Corrupt the stored blob in place (blobs are read-only, so widen first).
	bp, _ := blob.PathFor(expected)
	full := filepath.Join(layout.BlobsDir(), filepath.FromSlash(bp.Rel()))
	if err := os.Chmod(full, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("tampered bytes"), 0o444); err != nil {
		t.Fatal(err)
	}

	// Even with a correct source available, the corrupt stored blob must be
	// rejected rather than silently reused.
	_, _, err := store.Materialize(expected, opener(content))
	if !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("err = %v, want ErrCorruptBlob", err)
	}
}

func TestMaterializeVerifiesRaceWinnerBlob(t *testing.T) {
	hasher := blake3hash.New()
	layout := paths.New(t.TempDir())
	content := []byte("trustworthy content")
	expected := hashOf(t, hasher, content)

	// Inject a publish primitive that simulates a concurrent writer landing corrupt
	// bytes in the window between the existence check and the link: create the
	// target first, then let the real link run and lose with ErrExist.
	store := newWithLink(layout, hasher, func(oldDir *os.File, oldName string, newDir *os.File, newName string) error {
		target := filepath.Join(newDir.Name(), newName)
		if err := os.WriteFile(target, []byte("loser race bytes"), 0o444); err != nil {
			t.Fatal(err)
		}
		return fsx.LinkAt(oldDir, oldName, newDir, newName)
	})

	// The race winner's bytes are corrupt, so reuse must be rejected rather than
	// trusted on os.ErrExist alone.
	_, _, err := store.Materialize(expected, opener(content))
	if !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("err = %v, want ErrCorruptBlob", err)
	}
}

func TestBlobPathRejectsSymlink(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("real bytes")
	expected := hashOf(t, hasher, content)

	// A decoy regular file holding the correct bytes, somewhere outside the store.
	decoy := filepath.Join(t.TempDir(), "decoy")
	if err := os.WriteFile(decoy, content, 0o444); err != nil {
		t.Fatal(err)
	}
	// Plant a symlink at the blob's content address pointing at the decoy: it would
	// hash correctly today, but its target could be swapped later.
	bp, _ := blob.PathFor(expected)
	full := filepath.Join(layout.BlobsDir(), filepath.FromSlash(bp.Rel()))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, full); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// Every access path must reject the symlink as store corruption rather than
	// treating it as a valid blob.
	if _, err := store.Has(expected); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Has at symlink err = %v, want ErrCorruptBlob", err)
	}
	if _, err := store.Open(expected); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Open at symlink err = %v, want ErrCorruptBlob", err)
	}
	if _, _, err := store.Materialize(expected, opener(content)); !errors.Is(err, blob.ErrCorruptBlob) {
		t.Fatalf("Materialize over symlink err = %v, want ErrCorruptBlob", err)
	}
}

func TestMaterializeOpenFailureLeavesNoBlob(t *testing.T) {
	store, hasher, layout := newStore(t)
	expected := hashOf(t, hasher, []byte("never read"))
	_, _, err := store.Materialize(expected, func() (io.ReadCloser, error) {
		return nil, errors.New("disk gone")
	})
	if err == nil || strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected open error, got %v", err)
	}
	if has, _ := store.Has(expected); has {
		t.Fatal("blob must not exist after open failure")
	}
	assertTmpEmpty(t, layout)
}

func assertTmpEmpty(t *testing.T, layout paths.Layout) {
	t.Helper()
	entries, err := os.ReadDir(layout.TmpDir())
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("reading tmp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp dir not empty: %v", entries)
	}
}
