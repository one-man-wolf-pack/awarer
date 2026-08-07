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

// TestPutManifestCrashAfterManifestBeforeHeaderInvisible proves an interruption
// after the manifest is written but before the header (the commit point) leaves a
// checkpoint that no reader can see, though the manifest file remains on disk for
// gc/doctor to reclaim.
func TestPutManifestCrashAfterManifestBeforeHeaderInvisible(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	s := buildCheckpoint(t, idFrom(t, 0x5a), time.Unix(1, 0).UTC(), false)

	repo.fail = func(stage failStage) error {
		if stage == failAfterManifestBeforeHeader {
			return errors.New("boom before header")
		}
		return nil
	}
	if _, err := repo.PutManifest(context.Background(), s.build, s.stream()); err == nil {
		t.Fatalf("expected PutManifest to abort before the header")
	}

	// A fresh repo over the same dir: the id is invisible to every reader.
	fresh := NewRepo(layout)
	ids, err := fresh.ListIDs(context.Background())
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	for _, id := range ids {
		if id == s.id() {
			t.Fatalf("header-less checkpoint must not be listed")
		}
	}
	if _, err := fresh.Header(s.id()); err == nil {
		t.Fatalf("Header should fail for a header-less checkpoint")
	}
	if err := readCheckpoint(fresh, s.id()); err == nil {
		t.Fatalf("Get should fail for a header-less checkpoint")
	}

	// The manifest leftover exists on disk (the reclaimable orphan).
	manifestPath := filepath.Join(layout.CheckpointsDir(), s.id().String(), manifestName)
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected an orphan manifest on disk: %v", err)
	}
}

// TestPutManifestReRunWithNewIDSucceeds proves a clean checkpoint after an
// interrupted one publishes a fully visible checkpoint with no manual cleanup: the
// orphan never blocks new work (checkpoint ids are unique per checkpoint).
func TestPutManifestReRunWithNewIDSucceeds(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)

	repo.fail = func(stage failStage) error {
		if stage == failAfterManifestBeforeHeader {
			return errors.New("boom before header")
		}
		return nil
	}
	orphan := buildCheckpoint(t, idFrom(t, 0x01), time.Unix(1, 0).UTC(), false)
	if _, err := repo.PutManifest(context.Background(), orphan.build, orphan.stream()); err == nil {
		t.Fatalf("expected first PutManifest to abort")
	}

	repo.fail = nil
	good := buildCheckpoint(t, idFrom(t, 0x02), time.Unix(2, 0).UTC(), false)
	if _, err := repo.PutManifest(context.Background(), good.build, good.stream()); err != nil {
		t.Fatalf("re-run PutManifest: %v", err)
	}

	fresh := NewRepo(layout)
	if err := readCheckpoint(fresh, good.id()); err != nil {
		t.Fatalf("Get(good): %v", err)
	}
	if err := readCheckpoint(fresh, orphan.id()); err == nil {
		t.Fatalf("orphan checkpoint must remain invisible")
	}
	assertNotListed(t, fresh, orphan.id())
}

func assertNotListed(t *testing.T, repo *Repo, id checkpoint.CheckpointID) {
	t.Helper()
	ids, err := repo.ListIDs(context.Background())
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	for _, got := range ids {
		if got == id {
			t.Fatalf("id %s should not be listed", id)
		}
	}
}
