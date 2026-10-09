package runstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/runcache"
)

// TestGetWithinBoundIsDiagnosticNotValidity proves the bounded metadata read refuses
// an otherwise valid record over its budget without reading it, while the ordinary
// read keeps accepting the very same record; within budget it returns what Get
// returns and charges exactly the metadata bytes it read.
func TestGetWithinBoundIsDiagnosticNotValidity(t *testing.T) {
	r, _, h := newStore(t)
	entry := storeRecordOnly(t, r, h, "k1")
	info, err := os.Stat(filepath.Join(r.EntryPath(entry.ID), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	size := info.Size()

	got, n, err := r.GetWithin(entry.ID, size)
	if err != nil {
		t.Fatalf("GetWithin(exact size): %v", err)
	}
	if n != size {
		t.Errorf("bytes read = %d, want %d", n, size)
	}
	if got.ID != entry.ID || got.Reuse != entry.Reuse {
		t.Errorf("GetWithin decoded %v/%v, want %v/%v", got.ID, got.Reuse, entry.ID, entry.Reuse)
	}

	_, n, err = r.GetWithin(entry.ID, size-1)
	if !errors.Is(err, runcache.ErrReadBudgetExceeded) {
		t.Fatalf("GetWithin(size-1) err = %v, want ErrReadBudgetExceeded", err)
	}
	if errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("a budget refusal must not read as corruption: %v", err)
	}
	if n != 0 {
		t.Errorf("an over-budget record known from its size should cost no read, got %d bytes", n)
	}
	if _, err := r.Get(entry.ID); err != nil {
		t.Errorf("Get must still accept the record the bounded read refused: %v", err)
	}
}

// TestGetWithinClassifiesLikeGet pins that the bounded read shares Get's
// interpretation: a vanished, malformed, or foreign-schema record is reported with
// the same sentinel either way.
func TestGetWithinClassifiesLikeGet(t *testing.T) {
	r, _, h := newStore(t)
	corrupt := storeRecordOnly(t, r, h, "c")
	tamperMeta(t, r, corrupt.ID, func(m map[string]any) { m["unexpected"] = true })
	incompatible := storeRecordOnly(t, r, h, "i")
	tamperMeta(t, r, incompatible.ID, func(m map[string]any) { m["schema_version"] = 999 })
	vanished := storeRecordOnly(t, r, h, "v")
	if err := r.Delete(vanished.ID); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		id   runcache.RunID
		want error
	}{
		{"corrupt", corrupt.ID, runcache.ErrCorruptStore},
		{"incompatible", incompatible.ID, runcache.ErrIncompatibleEntry},
		{"vanished", vanished.ID, runcache.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Get(tc.id); !errors.Is(err, tc.want) {
				t.Fatalf("Get err = %v, want %v", err, tc.want)
			}
			if _, _, err := r.GetWithin(tc.id, 1<<20); !errors.Is(err, tc.want) {
				t.Errorf("GetWithin err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestHistoryPresentDistinguishesMissingEmptyAndBroken pins the history probe: no
// entries directory is known missing history, an existing directory is present even
// once emptied, and a non-directory in its place is corruption, not absence.
func TestHistoryPresentDistinguishesMissingEmptyAndBroken(t *testing.T) {
	r, layout, h := newStore(t)
	entries := filepath.Join(layout.RunsDir(), "entries")
	if present, err := r.HistoryPresent(); err != nil || present {
		t.Fatalf("fresh store: present=%v err=%v, want missing", present, err)
	}
	entry := storeRecordOnly(t, r, h, "k1")
	if present, err := r.HistoryPresent(); err != nil || !present {
		t.Fatalf("after a run: present=%v err=%v", present, err)
	}
	if err := r.Delete(entry.ID); err != nil {
		t.Fatal(err)
	}
	if present, err := r.HistoryPresent(); err != nil || !present {
		t.Errorf("emptied history: present=%v err=%v, want present", present, err)
	}

	if err := os.RemoveAll(entries); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entries, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.HistoryPresent(); !errors.Is(err, runcache.ErrCorruptStore) {
		t.Errorf("non-directory history: err=%v, want ErrCorruptStore", err)
	}
}
