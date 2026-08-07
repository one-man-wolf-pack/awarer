//go:build darwin || linux || freebsd

package fsx

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenNoFollowRejectsSymlinkAtomically is the native half of the symlink refusal.
// That a symlink is refused at all is the common contract and is proved for every
// platform in nofollow_contract_test.go; what is native-only is that the refusal is the
// open itself (O_NOFOLLOW), leaving no window between deciding and acting. The fallback
// checks with a separate Lstat first and says so, so this assertion belongs here rather
// than being weakened into something both could pass.
func TestOpenNoFollowRejectsSymlinkAtomically(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "reg")
	if err := os.WriteFile(reg, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link")
	requireSymlinks(t, reg, link)
	// The classification specifically: the kernel refused the open, rather than a
	// pre-open check having refused on the kernel's behalf. FreeBSD reports that refusal
	// as EMLINK and darwin/linux as ELOOP, which is exactly what the no-follow opens
	// normalize before anyone classifies it.
	f, err := OpenNoFollow(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("OpenNoFollow(symlink) succeeded, want a no-follow rejection")
	}
	if !IsSymlinkOpenRejection(err) {
		t.Fatalf("OpenNoFollow(symlink) err = %v, want a no-follow symlink rejection", err)
	}
}

// TestOpenFileAtRejectsASymlinkedLeaf proves the read a precondition guard makes
// is symlink-atomic: the name it opens must denote the object it checked, so a
// symlink swapped into that name — even one pointing at a file with identical
// bytes — is refused at the open rather than read through.
func TestOpenFileAtRejectsASymlinkedLeaf(t *testing.T) {
	root := t.TempDir()
	dir, err := MkdirAllNoFollow(root, "d", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	target := filepath.Join(root, "d", "target")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	requireSymlinks(t, "target", filepath.Join(root, "d", "link"))

	f, err := OpenFileAt(dir, "link")
	if err == nil {
		_ = f.Close()
		t.Fatal("OpenFileAt(symlink) succeeded, want a no-follow rejection")
	}
	if !IsSymlinkOpenRejection(err) {
		t.Fatalf("OpenFileAt(symlink) err = %v, want a no-follow symlink rejection", err)
	}
}

// TestOpenNoFollowAtRejectsAncestorSymlink proves the component-wise open refuses a
// symlink at an ancestor component, not only the final one.
func TestOpenNoFollowAtRejectsAncestorSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An unchanged path opens.
	f, err := OpenNoFollowAt(root, "dir/file")
	if err != nil {
		t.Fatalf("OpenNoFollowAt(unchanged) = %v, want success", err)
	}
	_ = f.Close()

	// Replace the ancestor directory with a symlink to an outside directory holding
	// the same file: the component-wise open refuses the ancestor symlink.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	requireSymlinks(t, outside, filepath.Join(root, "dir"))
	if _, err := OpenNoFollowAt(root, "dir/file"); !IsSymlinkOpenRejection(err) {
		t.Fatalf("OpenNoFollowAt(ancestor symlink) err = %v, want a no-follow symlink rejection", err)
	}
}

// TestRemoveTreeAtUnlinksSymlinkNotTarget proves a symlink leaf in the tree is
// removed as a link, never followed into its target directory, so the recursive
// delete cannot reach outside the tree.
func TestRemoveTreeAtUnlinksSymlinkNotTarget(t *testing.T) {
	root := t.TempDir()
	parent, err := MkdirAllNoFollow(root, "p", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()

	// An outside directory with a file that must survive the delete.
	outside := t.TempDir()
	survivor := filepath.Join(outside, "keep")
	if err := os.WriteFile(survivor, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// p/tree contains a symlink pointing at the outside directory.
	base := filepath.Join(root, "p", "tree")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	requireSymlinks(t, outside, filepath.Join(base, "link"))

	if err := RemoveTreeAt(parent, "tree"); err != nil {
		t.Fatalf("RemoveTreeAt: %v", err)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("tree still present after RemoveTreeAt (stat err = %v)", err)
	}
	// The symlink target and its contents must be untouched.
	if _, err := os.Stat(survivor); err != nil {
		t.Errorf("symlink target was followed and deleted: %v", err)
	}
}

// TestPublishBytesAtStaysWithTheOpenedDirectory is the runtime oracle for descriptor
// anchoring: it observes the property rather than inferring it from a build tag.
//
// A caller holds a directory it opened and verified. The directory is then moved and a
// different directory takes its pathname — the ordinary shape of a rename racing a
// publish. The write must land in the inode the caller holds, because that is the one it
// proved; the impostor at the old name must receive nothing.
//
// Only descriptor-relative primitives can answer that. PublishBytesAt creates its temp
// file with openat relative to the held fd and links it into the same fd, so the whole
// publish stays inside that inode. The joined-path fallback re-derives the destination
// from dir.Name() and therefore writes into whatever now answers to that path — which is
// why the counterpart in nofollow_fallback_test.go asserts the opposite outcome. A build
// that quietly compiled the fallback here would fail this test rather than pass it
// silently, which is the whole reason it exists.
//
// The substitution must land on every platform this file serves. A platform that refuses
// to rename a directory with an open descriptor is a different, non-Unix outcome that
// belongs to the fallback's counterpart; here the rename failing is a blocking failure,
// not a recorded platform property.
func TestPublishBytesAtStaysWithTheOpenedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "anchor"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDirNoFollow(root, "anchor")
	if err != nil {
		t.Fatalf("OpenDirNoFollow: %v", err)
	}
	defer func() { _ = dir.Close() }()

	// The pathname the caller used now denotes a different directory.
	if err := os.Rename(filepath.Join(root, "anchor"), filepath.Join(root, "moved")); err != nil {
		t.Fatalf("rename the opened directory aside: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "anchor"), 0o755); err != nil {
		t.Fatal(err)
	}

	want := []byte("anchored")
	if err := PublishBytesAt(dir, "witness", want, 0o600); err != nil {
		t.Fatalf("PublishBytesAt: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "moved", "witness"))
	if err != nil {
		t.Fatalf("the publish did not land in the directory the caller holds: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("witness bytes = %q, want %q", got, want)
	}
	if _, err := os.Lstat(filepath.Join(root, "anchor", "witness")); !os.IsNotExist(err) {
		t.Errorf("the publish reached the directory that took the anchor's pathname (lstat err = %v); "+
			"the descriptor was re-resolved by name instead of held", err)
	}
}
