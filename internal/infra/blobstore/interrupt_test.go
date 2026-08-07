package blobstore

import (
	"errors"
	"os"
	"testing"

	"awarer/internal/domain/paths"
)

// tmpEntries lists the store's tmp directory, treating absence as empty.
func tmpEntries(t *testing.T, layout paths.Layout) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(layout.TmpDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading tmp dir: %v", err)
	}
	return entries
}

// TestMaterializeInterruptedBeforeTempLeavesNothing proves an interruption before
// the temp file is even created publishes no blob and leaves the tmp dir clean.
func TestMaterializeInterruptedBeforeTempLeavesNothing(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("interrupt before temp")
	expected := hashOf(t, hasher, content)

	store.fail = func(stage failStage) error {
		if stage == failBeforeTemp {
			return errors.New("boom before temp")
		}
		return nil
	}
	if _, _, err := store.Materialize(expected, opener(content)); err == nil {
		t.Fatalf("expected interruption error")
	}
	if has, err := store.Has(expected); err != nil || has {
		t.Fatalf("blob must not exist: has=%v err=%v", has, err)
	}
	assertTmpEmpty(t, layout)
}

// TestMaterializeLiveErrorAfterTempRollsBack proves a plain error after the temp
// is written (before the link) rolls the temp file back: no blob, tmp clean.
func TestMaterializeLiveErrorAfterTempRollsBack(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("interrupt after temp, live error")
	expected := hashOf(t, hasher, content)

	store.fail = func(stage failStage) error {
		if stage == failAfterTempBeforeLink {
			return errors.New("boom after temp")
		}
		return nil
	}
	if _, _, err := store.Materialize(expected, opener(content)); err == nil {
		t.Fatalf("expected interruption error")
	}
	if has, err := store.Has(expected); err != nil || has {
		t.Fatalf("blob must not exist after live-error rollback: has=%v err=%v", has, err)
	}
	assertTmpEmpty(t, layout)
}

// TestMaterializeCrashAfterTempLeavesOrphanNotBlob proves a simulated crash after
// the temp write (before the link) leaves the temp blob as a reclaimable orphan but
// never a published blob — the interrupted write is not authoritative.
func TestMaterializeCrashAfterTempLeavesOrphanNotBlob(t *testing.T) {
	store, hasher, layout := newStore(t)
	content := []byte("interrupt after temp, crash")
	expected := hashOf(t, hasher, content)

	store.fail = func(stage failStage) error {
		if stage == failAfterTempBeforeLink {
			return errCrash
		}
		return nil
	}
	if _, _, err := store.Materialize(expected, opener(content)); err == nil {
		t.Fatalf("expected interruption error")
	}
	if has, err := store.Has(expected); err != nil || has {
		t.Fatalf("blob must not be published by a crashed write: has=%v err=%v", has, err)
	}
	// The orphan temp survives for gc/doctor to reclaim.
	entries := tmpEntries(t, layout)
	if len(entries) == 0 {
		t.Fatalf("expected an orphan temp file after a simulated crash")
	}

	// A fresh, un-faulted materialize recovers with no manual cleanup needed.
	store.fail = nil
	if _, written, err := store.Materialize(expected, opener(content)); err != nil || !written {
		t.Fatalf("re-run after crash should publish the blob: written=%v err=%v", written, err)
	}
	if has, err := store.Has(expected); err != nil || !has {
		t.Fatalf("blob must exist after recovery: has=%v err=%v", has, err)
	}
}
