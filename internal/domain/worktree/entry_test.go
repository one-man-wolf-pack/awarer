package worktree_test

import (
	"testing"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

func TestNewRegularEntry(t *testing.T) {
	path := mustPath(t, "a.go")
	content := mustContent(t, hexA)

	if _, err := worktree.NewRegularEntry(path, content, worktree.StorageBlob, worktree.StatSignature{}, worktree.TraversalInfo{}); err != nil {
		t.Errorf("valid regular entry rejected: %v", err)
	}
	// Zero content is rejected.
	if _, err := worktree.NewRegularEntry(path, hashing.ContentHash{}, worktree.StorageBlob, worktree.StatSignature{}, worktree.TraversalInfo{}); err == nil {
		t.Errorf("regular entry with zero content accepted")
	}
	// Storage must be blob or hash-only.
	if _, err := worktree.NewRegularEntry(path, content, worktree.StorageNone, worktree.StatSignature{}, worktree.TraversalInfo{}); err == nil {
		t.Errorf("regular entry with storage none accepted")
	}
	// A negative size is an impossible state and must be rejected.
	if _, err := worktree.NewRegularEntry(path, content, worktree.StorageBlob, worktree.StatSignature{Size: -1}, worktree.TraversalInfo{}); err == nil {
		t.Errorf("regular entry with negative size accepted")
	}
}

func TestNewRegularEntryDerivesHardlinkFromStat(t *testing.T) {
	path := mustPath(t, "a.go")
	stat := worktree.StatSignature{Dev: 1, Ino: 2, Nlink: 2}
	e, err := worktree.NewRegularEntry(path, mustContent(t, hexA), worktree.StorageBlob, stat, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Hardlink().Known || e.Hardlink().Ino != 2 {
		t.Errorf("hardlink identity not derived from stat: %+v", e.Hardlink())
	}
}

// TestConstructorsValidateShape proves the constructors reject shape violations
// (bad traversal provenance, unknown omitted stat fields) at construction, not
// only later at tree construction.
func TestConstructorsValidateShape(t *testing.T) {
	path := mustPath(t, "a.go")
	content := mustContent(t, hexA)
	badTraversal := worktree.TraversalInfo{Followed: true} // no source, no depth
	badStat := worktree.StatSignature{Omitted: worktree.FieldSet(1 << 7)}

	if _, err := worktree.NewRegularEntry(path, content, worktree.StorageBlob, worktree.StatSignature{}, badTraversal); err == nil {
		t.Errorf("NewRegularEntry accepted invalid traversal")
	}
	if _, err := worktree.NewRegularEntry(path, content, worktree.StorageBlob, badStat, worktree.TraversalInfo{}); err == nil {
		t.Errorf("NewRegularEntry accepted unknown omitted stat fields")
	}
	if _, err := worktree.NewDirEntry(path, badTraversal); err == nil {
		t.Errorf("NewDirEntry accepted invalid traversal")
	}
	if _, err := worktree.NewSymlinkEntry(path, mustTarget(t, "../x"), badStat, worktree.TraversalInfo{}); err == nil {
		t.Errorf("NewSymlinkEntry accepted unknown omitted stat fields")
	}
}

func TestNewSymlinkEntry(t *testing.T) {
	path := mustPath(t, "link")
	if _, err := worktree.NewSymlinkEntry(path, mustTarget(t, "../x"), worktree.StatSignature{}, worktree.TraversalInfo{}); err != nil {
		t.Errorf("valid symlink entry rejected: %v", err)
	}
	if _, err := worktree.NewSymlinkEntry(path, worktree.SymlinkTarget{}, worktree.StatSignature{}, worktree.TraversalInfo{}); err == nil {
		t.Errorf("symlink entry with no target accepted")
	}
}

func TestNewSkippedInputDerivesKind(t *testing.T) {
	path := mustPath(t, "x")
	noTarget := worktree.SymlinkTarget{}

	s, err := worktree.NewSkippedInput(path, worktree.ReasonSymlinkCycle, 0, worktree.StatSignature{}, "", mustTarget(t, "../x"), worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != worktree.KindSymlink {
		t.Errorf("kind = %v, want symlink (derived from reason)", s.Kind)
	}

	if _, err := worktree.NewSkippedInput(path, worktree.ReasonLargeFileSkipPolicy, -1, worktree.StatSignature{}, "", noTarget, worktree.TraversalInfo{}); err == nil {
		t.Errorf("negative size accepted")
	}
	if _, err := worktree.NewSkippedInput(path, worktree.SkippedReason(99), 0, worktree.StatSignature{}, "", noTarget, worktree.TraversalInfo{}); err == nil {
		t.Errorf("invalid reason accepted")
	}
}

func TestSkippedInputOSErrorBinding(t *testing.T) {
	path := mustPath(t, "x")
	noTarget := worktree.SymlinkTarget{}

	// read-error requires an OS error category.
	if _, err := worktree.NewSkippedInput(path, worktree.ReasonReadError, 0, worktree.StatSignature{}, "", noTarget, worktree.TraversalInfo{}); err == nil {
		t.Errorf("read-error with empty OS error category accepted")
	}
	if _, err := worktree.NewSkippedInput(path, worktree.ReasonReadError, 0, worktree.StatSignature{}, "permission-denied", noTarget, worktree.TraversalInfo{}); err != nil {
		t.Errorf("valid read-error skip rejected: %v", err)
	}
	// non-read-error reasons must not carry an OS error category.
	if _, err := worktree.NewSkippedInput(path, worktree.ReasonSymlinkCycle, 0, worktree.StatSignature{}, "io-error", mustTarget(t, "../x"), worktree.TraversalInfo{}); err == nil {
		t.Errorf("non-read-error reason with OS error category accepted")
	}
}

func TestSkippedSymlinkRequiresTarget(t *testing.T) {
	path := mustPath(t, "link")
	// A symlink skip reason without a target is rejected.
	if _, err := worktree.NewSkippedInput(path, worktree.ReasonSymlinkCycle, 0, worktree.StatSignature{}, "", worktree.SymlinkTarget{}, worktree.TraversalInfo{}); err == nil {
		t.Errorf("symlink skip without target accepted")
	}
	// A non-symlink reason carrying a target is rejected.
	if _, err := worktree.NewSkippedInput(path, worktree.ReasonLargeFileSkipPolicy, 0, worktree.StatSignature{}, "", mustTarget(t, "../x"), worktree.TraversalInfo{}); err == nil {
		t.Errorf("non-symlink skip with target accepted")
	}
	// An invalid traversal (followed but no source/depth) is rejected, like an Entry.
	badTraversal := worktree.TraversalInfo{Followed: true}
	if _, err := worktree.NewSkippedInput(path, worktree.ReasonLargeFileSkipPolicy, 0, worktree.StatSignature{}, "", worktree.SymlinkTarget{}, badTraversal); err == nil {
		t.Errorf("skipped input with invalid traversal accepted")
	}
}
