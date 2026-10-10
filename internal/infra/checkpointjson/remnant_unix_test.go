//go:build darwin || linux || freebsd

package checkpointjson

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"awarer/internal/domain/paths"
)

func TestHeaderlessRemnantWithSpecialNodeIsPreserved(t *testing.T) {
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	id := stoppedPublication(t, layout, 0x81)
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	if err := os.Remove(filepath.Join(dir, frontierName)); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, frontierName), 0o600); err != nil {
		t.Fatal(err)
	}
	before := treeOf(t, dir)

	rem, err := repo.RemoveHeaderless(context.Background(), id)
	if err != nil || rem.State != RemnantUnsupported {
		t.Fatalf("RemoveHeaderless = state %d, err %v; want unsupported", rem.State, err)
	}
	assertSameTree(t, dir, before)
}

func TestUnreadableHeaderlessDirectoryIsUninspectedNotEligible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	id := stoppedPublication(t, layout, 0x82)
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	before := treeOf(t, dir)
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := repo.EachHeaderless(context.Background(), func(r Remnant) error {
		t.Errorf("an unreadable directory was classified: %+v", r)
		return nil
	})
	if err == nil {
		t.Fatal("EachHeaderless certified a store holding an unreadable id directory")
	}
	rem, err := repo.RemoveHeaderless(context.Background(), id)
	if err != nil || rem.State != RemnantUninspected {
		t.Fatalf("RemoveHeaderless = state %d, err %v; want uninspected", rem.State, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertSameTree(t, dir, before)
}

func TestRemoveHeaderlessUnlinkFailureRemovesNothingAndRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root unlinks from a read-only directory")
	}
	layout := paths.New(t.TempDir())
	repo := NewRepo(layout)
	id := stoppedPublication(t, layout, 0x83)
	dir := filepath.Join(layout.CheckpointsDir(), id.String())
	before := treeOf(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := repo.RemoveHeaderless(context.Background(), id); err == nil {
		t.Fatal("removal from a read-only directory reported success")
	}
	assertSameTree(t, dir, before)

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if rem, err := repo.RemoveHeaderless(context.Background(), id); err != nil || rem.State != RemnantEligible {
		t.Fatalf("retry = state %d, err %v", rem.State, err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("retry left the directory: %v", err)
	}
}
