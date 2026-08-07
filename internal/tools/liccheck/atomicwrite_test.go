package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteNewFileGets0644(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notice")
	if err := atomicWrite(path, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("new file mode = %v, want 0644", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "hello\n" {
		t.Errorf("content = %q", data)
	}
}

func TestAtomicWritePreservesExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notice")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want preserved 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new\n" {
		t.Errorf("content = %q, want new", data)
	}
}

// TestAtomicWriteFailureLeavesTargetUnchanged makes the containing directory
// unwritable so the temp-file creation fails, and verifies the existing target is
// left byte-for-byte unchanged (no partial write).
func TestAtomicWriteFailureLeavesTargetUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notice")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := atomicWrite(path, []byte("replacement\n")); err == nil {
		t.Skip("directory is writable despite 0500 (running as root?); cannot exercise failure path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original\n" {
		t.Errorf("target changed on failed write: %q", data)
	}
}
