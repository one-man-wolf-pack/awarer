package fsx

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenNoFollowAtRejectsUnsafeRel proves the root boundary is enforced by the
// helper, not left to caller discipline: empty, absolute, dot, dot-dot, and
// embedded dot-dot rels are rejected before any filesystem access.
func TestOpenNoFollowAtRejectsUnsafeRel(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"", ".", "..", "../outside/file", "dir/../escape", "/abs/file", "a//b", "a/./b", `a\b`} {
		if _, err := OpenNoFollowAt(root, rel); !errors.Is(err, ErrUnsafeRelPath) {
			t.Errorf("OpenNoFollowAt(%q) err = %v, want ErrUnsafeRelPath", rel, err)
		}
	}
}

// TestDirRelativeOpsRejectUnsafeName proves the directory-relative helpers refuse a
// name that is not a single path component, so a name argument can never escape the
// verified directory it is meant to act in.
func TestDirRelativeOpsRejectUnsafeName(t *testing.T) {
	root := t.TempDir()
	dir, err := MkdirAllNoFollow(root, "d", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	for _, name := range []string{"", ".", "..", "../escape", "nested/file", `back\slash`} {
		if err := PublishBytesNoFollow(root, "d", name, []byte("x"), 0o644); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("PublishBytesNoFollow(%q) err = %v, want ErrUnsafeName", name, err)
		}
		if err := ReplaceBytesNoFollow(root, "d", name, []byte("x"), 0o644); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("ReplaceBytesNoFollow(%q) err = %v, want ErrUnsafeName", name, err)
		}
		if err := LinkAt(dir, name, dir, "ok"); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("LinkAt(old=%q) err = %v, want ErrUnsafeName", name, err)
		}
		if err := RenameAt(dir, name, dir, "ok"); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("RenameAt(old=%q) err = %v, want ErrUnsafeName", name, err)
		}
		if err := RemoveAt(dir, name); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("RemoveAt(%q) err = %v, want ErrUnsafeName", name, err)
		}
		if err := RemoveDirAt(dir, name); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("RemoveDirAt(%q) err = %v, want ErrUnsafeName", name, err)
		}
		if _, err := OpenFileAt(dir, name); !errors.Is(err, ErrUnsafeName) {
			t.Errorf("OpenFileAt(%q) err = %v, want ErrUnsafeName", name, err)
		}
	}
}

// TestOpenFileAtReadsThroughTheHeldDirectory covers the read counterpart of
// LstatAt: a caller that has proved a shape must be able to read the bytes of THAT
// object through the directory descriptor it already holds, so a name swapped into
// an ancestor cannot redirect the read. An absent name must be distinguishable
// from a real I/O failure, because a precondition guard reports the two very
// differently.
func TestOpenFileAtReadsThroughTheHeldDirectory(t *testing.T) {
	root := t.TempDir()
	dir, err := MkdirAllNoFollow(root, "d", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	if err := os.WriteFile(filepath.Join(root, "d", "file.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenFileAt(dir, "file.txt")
	if err != nil {
		t.Fatalf("OpenFileAt: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("read %q, want %q", got, "payload")
	}

	if _, err := OpenFileAt(dir, "missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenFileAt on an absent name = %v, want os.ErrNotExist", err)
	}
}

// TestRemoveDirAtRefusesToDescend pins what makes this the safe half of the removal
// pair: it reclaims a directory a caller emptied itself and refuses one that still
// holds anything, so an undo can never carry away content the caller did not write.
func TestRemoveDirAtRefusesToDescend(t *testing.T) {
	root := t.TempDir()
	dir, err := MkdirAllNoFollow(root, "d", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	if mErr := os.Mkdir(filepath.Join(root, "d", "occupied"), 0o755); mErr != nil {
		t.Fatal(mErr)
	}
	if wErr := os.WriteFile(filepath.Join(root, "d", "occupied", "keep.txt"), []byte("x"), 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	if rErr := RemoveDirAt(dir, "occupied"); rErr == nil {
		t.Error("RemoveDirAt on a non-empty directory = nil, want a refusal")
	}
	if _, sErr := os.Stat(filepath.Join(root, "d", "occupied", "keep.txt")); sErr != nil {
		t.Errorf("a refused RemoveDirAt took the content with it: %v", sErr)
	}

	if mErr := os.Mkdir(filepath.Join(root, "d", "empty"), 0o755); mErr != nil {
		t.Fatal(mErr)
	}
	if rErr := RemoveDirAt(dir, "empty"); rErr != nil {
		t.Errorf("RemoveDirAt on an empty directory = %v, want nil", rErr)
	}
}

// TestReadDirNoFollowRejectsNonDirectory proves a regular file where a directory is
// expected is reported as ErrNotDirectory, not a raw read error.
func TestReadDirNoFollowRejectsNonDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDirNoFollow(root, "f"); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("ReadDirNoFollow(regular file) err = %v, want ErrNotDirectory", err)
	}
}

// TestRemoveTreeAt proves the recursive, descriptor-pinned delete removes a whole
// subtree (including a read-only file), is idempotent on an absent name, and
// rejects an unsafe leaf name.
func TestRemoveTreeAt(t *testing.T) {
	root := t.TempDir()
	parent, err := MkdirAllNoFollow(root, "p", 0o755)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Close() }()

	// p/tree/{a/b/file, ro} where ro is read-only (a published payload's mode).
	base := filepath.Join(root, "p", "tree")
	if err := os.MkdirAll(filepath.Join(base, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "a", "b", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "ro"), []byte("y"), 0o444); err != nil {
		t.Fatal(err)
	}

	if err := RemoveTreeAt(parent, "tree"); err != nil {
		t.Fatalf("RemoveTreeAt: %v", err)
	}
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("subtree still present after RemoveTreeAt (stat err = %v)", err)
	}
	// Idempotent: removing an already-absent name is not an error.
	if err := RemoveTreeAt(parent, "tree"); err != nil {
		t.Errorf("RemoveTreeAt on absent name = %v, want nil", err)
	}
	// An unsafe leaf name is rejected before any filesystem op.
	if err := RemoveTreeAt(parent, "../escape"); !errors.Is(err, ErrUnsafeName) {
		t.Errorf("RemoveTreeAt(unsafe) err = %v, want ErrUnsafeName", err)
	}
}

// TestOpenNoFollowAtOpensNestedRel proves a clean nested rel opens normally.
func TestOpenNoFollowAtOpensNestedRel(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "c"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenNoFollowAt(root, "a/b/c")
	if err != nil {
		t.Fatalf("OpenNoFollowAt(nested) = %v, want success", err)
	}
	_ = f.Close()
}

// TestEachDirEntryNoFollowStreamsAcrossBatches proves the streaming directory iterator
// visits every entry even when the directory holds far more than one read batch, so a
// large shard is never silently truncated at the batch boundary.
func TestEachDirEntryNoFollowStreamsAcrossBatches(t *testing.T) {
	root := t.TempDir()
	dir := "d"
	if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Comfortably exceed dirEntryBatch so at least two batches are read.
	const n = dirEntryBatch*2 + 7
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(root, dir, fmtName(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	if err := EachDirEntryNoFollow(root, dir, func(e os.DirEntry) error {
		if seen[e.Name()] {
			t.Fatalf("entry %q visited twice", e.Name())
		}
		seen[e.Name()] = true
		return nil
	}); err != nil {
		t.Fatalf("EachDirEntryNoFollow: %v", err)
	}
	if len(seen) != n {
		t.Fatalf("visited %d entries, want %d (batch boundary dropped entries?)", len(seen), n)
	}
}

// TestEachDirEntryNoFollowPropagatesFnError proves a callback error stops the walk.
func TestEachDirEntryNoFollowPropagatesFnError(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("stop")
	if err := EachDirEntryNoFollow(root, "d", func(os.DirEntry) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the propagated callback error", err)
	}
}

func fmtName(i int) string {
	return "f" + string(rune('a'+i%26)) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
