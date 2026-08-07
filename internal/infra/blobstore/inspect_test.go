package blobstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListReturnsMaterializedBlobs(t *testing.T) {
	store, hasher, _ := newStore(t)
	a := []byte("alpha content")
	b := []byte("beta content longer")
	ha := hashOf(t, hasher, a)
	hb := hashOf(t, hasher, b)
	if _, _, err := store.Materialize(ha, opener(a)); err != nil {
		t.Fatalf("Materialize a: %v", err)
	}
	if _, _, err := store.Materialize(hb, opener(b)); err != nil {
		t.Fatalf("Materialize b: %v", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("List len = %d, want 2", len(infos))
	}
	bySize := map[string]int64{}
	for _, in := range infos {
		bySize[in.Ref.Hash().String()] = in.Bytes
	}
	if bySize[ha.String()] != int64(len(a)) {
		t.Errorf("size for a = %d, want %d", bySize[ha.String()], len(a))
	}
	if bySize[hb.String()] != int64(len(b)) {
		t.Errorf("size for b = %d, want %d", bySize[hb.String()], len(b))
	}
}

func TestListIsSortedByHash(t *testing.T) {
	store, hasher, _ := newStore(t)
	for _, c := range []string{"one", "two", "three", "four", "five"} {
		h := hashOf(t, hasher, []byte(c))
		if _, _, err := store.Materialize(h, opener([]byte(c))); err != nil {
			t.Fatalf("Materialize %q: %v", c, err)
		}
	}
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 1; i < len(infos); i++ {
		if infos[i-1].Ref.Hash().String() >= infos[i].Ref.Hash().String() {
			t.Fatalf("List not sorted by hash at %d: %q >= %q", i,
				infos[i-1].Ref.Hash().String(), infos[i].Ref.Hash().String())
		}
	}
}

func TestListAbsentStoreIsEmpty(t *testing.T) {
	store, _, _ := newStore(t)
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List on absent store: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("List len = %d, want 0", len(infos))
	}
}

func TestListSkipsMalformedFile(t *testing.T) {
	store, hasher, layout := newStore(t)
	a := []byte("alpha content")
	ha := hashOf(t, hasher, a)
	if _, _, err := store.Materialize(ha, opener(a)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// Drop a stray non-hash file directly into the identity namespace directory.
	stray := filepath.Join(layout.BlobsDir(), "blake3", "garbage.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	infos, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List len = %d, want 1 (stray skipped)", len(infos))
	}
}

func TestDeleteRemovesBlobIdempotently(t *testing.T) {
	store, hasher, _ := newStore(t)
	a := []byte("alpha content")
	ha := hashOf(t, hasher, a)
	if _, _, err := store.Materialize(ha, opener(a)); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if has, _ := store.Has(ha); !has {
		t.Fatal("blob should exist before delete")
	}
	if err := store.Delete(ha); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if has, _ := store.Has(ha); has {
		t.Fatal("blob should be gone after delete")
	}
	// Idempotent: deleting again is not an error.
	if err := store.Delete(ha); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}
