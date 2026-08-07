package worktreefs

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenVerifiedRegularDetectsSwap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	want := statSignature(info)

	// Swap the path for a different node between stat and open: a new file (new
	// inode, different size) replaces the one the walker observed.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("a different file entirely"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := openVerifiedRegular(p, want); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("err = %v, want a \"changed during scan\" rejection", err)
	}
}

func TestOpenVerifiedRegularDetectsSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	want := statSignature(info)

	// Replace the regular file with a symlink to another file: os.Open follows it,
	// but the opened node's identity no longer matches what was stat'd.
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("original"), 0o644); err != nil { // same bytes, different inode
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, p); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, err := openVerifiedRegular(p, want); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("err = %v, want a \"changed during scan\" rejection", err)
	}
}

func TestOpenVerifiedRegularDetectsSymlinkToSameInode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A hardlink shares p's inode; a symlink pointing at it resolves to the same
	// inode and content, so the opened-descriptor identity check alone would pass.
	hard := filepath.Join(dir, "hardlink")
	if err := os.Link(p, hard); err != nil {
		t.Skipf("hardlinks unsupported: %v", err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	want := statSignature(info)

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(hard, p); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	// The path is now a symlink, not a regular file; it must be rejected even
	// though the resolved target has the same inode/content.
	if _, err := openVerifiedRegular(p, want); err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("err = %v, want a \"changed during scan\" rejection", err)
	}
}

func TestOpenVerifiedRegularOpensUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("steady"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := openVerifiedRegular(p, statSignature(info))
	if err != nil {
		t.Fatalf("unchanged file rejected: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "steady" {
		t.Fatalf("content = %q, want %q", got, "steady")
	}
}
