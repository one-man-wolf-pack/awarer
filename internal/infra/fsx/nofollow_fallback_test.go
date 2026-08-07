//go:build !darwin && !linux && !freebsd

package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The fallback classifies one thing the native platforms do not: a non-directory
// ancestor becomes ErrNotDirectory rather than the raw kernel errno. On darwin, linux,
// and freebsd OpenNoFollow is a single O_NOFOLLOW open that hands back whatever the
// kernel said, so these assertions would be false there. They are the fallback's
// contract, not the common one, and they run on the platform that actually uses it.

// TestOpenNoFollowReportsANonDirectoryAncestor proves the fallback maps a file standing
// where a directory belongs onto the store-corruption sentinel, instead of leaving a raw
// platform error for callers to guess at.
func TestOpenNoFollowReportsANonDirectoryAncestor(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"file/child", "file/a/b"} {
		if _, err := OpenNoFollowAt(root, rel); !errors.Is(err, ErrNotDirectory) {
			t.Errorf("OpenNoFollowAt(root, %s) err = %v, want ErrNotDirectory", rel, err)
		}
	}
}

// TestAbsenceIsNeverReportedAsANonDirectory asserts the one distinction the fallback's
// classification must never lose, and records the platform statuses behind it.
//
// Only the classification is asserted; the statuses are logged, deliberately. syscall.
// ENOTDIR is not a POSIX stand-in on Windows — syscall/zerrors_windows.go defines it as
// ERROR_PATH_NOT_FOUND, a status the OS really returns — but it is not exclusive, since a
// missing ancestor can carry it too. That ambiguity is why the classification asks the
// filesystem instead of reading a status, and pinning which status a given host picks
// would assert a fact the implementation is designed not to depend on. The log line is
// there so a failure below can be read without a second run.
func TestAbsenceIsNeverReportedAsANonDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	underFile := filepath.Join(file, "child")
	underMissing := filepath.Join(root, "missing", "child")

	_, statUnderFile := os.Lstat(underFile)
	_, statUnderMissing := os.Lstat(underMissing)
	t.Logf("under a regular file: %v (ENOTDIR=%v, NotExist=%v)",
		statUnderFile, errors.Is(statUnderFile, syscall.ENOTDIR), errors.Is(statUnderFile, os.ErrNotExist))
	t.Logf("under a missing directory: %v (ENOTDIR=%v, NotExist=%v)",
		statUnderMissing, errors.Is(statUnderMissing, syscall.ENOTDIR), errors.Is(statUnderMissing, os.ErrNotExist))

	// A symlinked ancestor is the case the ancestor check declines to answer, because
	// deciding it would mean resolving the link. The absence must survive that decline
	// rather than being upgraded to corruption on the strength of the link's own mode.
	//
	// A subtest, so requireSymlinks governs this case alone: it is the only one here that
	// needs the privilege, and the assertions after it must still run without one. Under
	// AWA_REQUIRE_SYMLINK_TESTS the lane fails instead of skipping, which is the point —
	// this file runs only on the platform whose symlink privilege is conditional, so a
	// case that quietly drops itself there would be worth nothing.
	t.Run("under a symlinked ancestor", func(t *testing.T) {
		requireSymlinks(t, filepath.Join(root, "dir"), filepath.Join(root, "link"))
		if _, err := OpenNoFollowAt(root, "link/missing"); errors.Is(err, ErrNotDirectory) {
			t.Errorf("OpenNoFollowAt(root, link/missing) err = %v, want an absence, not ErrNotDirectory", err)
		}
	})

	// Whatever the statuses are, these two must not be conflated.
	for _, c := range []struct {
		name string
		rel  string
	}{
		{"a missing entry beside a real directory", "dir/child"},
		{"an entry under a missing directory", "missing/child"},
	} {
		_, err := OpenNoFollowAt(root, c.rel)
		if errors.Is(err, ErrNotDirectory) {
			t.Errorf("OpenNoFollowAt(root, %s) err = %v, want an absence, not ErrNotDirectory", c.name, err)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("OpenNoFollowAt(root, %s) err = %v, want it to keep its os.ErrNotExist identity", c.name, err)
		}
	}
}

// TestPublishBytesAtReResolvesTheAnchorPathname is the counterpart of the native
// anchoring oracle in fsx_unix_test.go, and its whole job is to prove that oracle
// discriminates: the same scenario has the opposite outcome here, so a native platform
// that quietly compiled this file would fail there rather than pass.
//
// A caller holds a directory it opened; the directory is moved and another takes its
// pathname. Without openat there is nothing to hold — PublishBytesAt re-derives its
// destination from dir.Name() — so the bytes land in whatever answers to that name now.
// That is the documented weaker guarantee of this file, asserted rather than assumed.
//
// This platform may refuse to rename a directory while a descriptor on it is open, and
// that refusal is itself an answer: the pathname cannot be replaced underneath the
// caller, so the publish reaches the directory the name still denotes. Three outcomes
// are therefore kept apart — a successful substitution, a known platform refusal, and an
// unrecognized setup failure — and only the first two are evidence. The third fails the
// test rather than being recorded as a platform property, and nothing skips: a lane that
// exists to prove this file's behavior must not report green for having run nothing.
//
// A FreeBSD build reaching this test at all would mean the build constraint failed,
// which is why freebsd is excluded by the tag rather than tolerated here.
func TestPublishBytesAtReResolvesTheAnchorPathname(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "anchor"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenDirNoFollow(root, "anchor")
	if err != nil {
		t.Fatalf("OpenDirNoFollow: %v", err)
	}
	defer func() { _ = dir.Close() }()

	renamed := false
	switch err := os.Rename(filepath.Join(root, "anchor"), filepath.Join(root, "moved")); {
	case err == nil:
		renamed = true
		if err := os.Mkdir(filepath.Join(root, "anchor"), 0o755); err != nil {
			t.Fatal(err)
		}
	case isRenameDenied(err):
		t.Logf("this platform refused to replace the anchor's pathname while a descriptor "+
			"on it is open: %v", err)
	default:
		t.Fatalf("the substitution could not be set up, and the failure is not a recognized "+
			"platform refusal: %v", err)
	}

	want := []byte("re-resolved")
	if err := PublishBytesAt(dir, "witness", want, 0o600); err != nil {
		t.Fatalf("PublishBytesAt: %v", err)
	}

	// Either way the bytes are at the anchor's pathname: after a successful rename that
	// is the impostor, and after a refusal it is the original directory.
	if _, err := os.Lstat(filepath.Join(root, "anchor", "witness")); err != nil {
		t.Errorf("the publish did not land at the anchor's pathname (renamed = %v): %v", renamed, err)
	}
	if renamed {
		if _, err := os.Lstat(filepath.Join(root, "moved", "witness")); !os.IsNotExist(err) {
			t.Errorf("the publish followed the opened directory (lstat err = %v); this file cannot "+
				"anchor by identity, and a caller relying on that must be told so", err)
		}
	}
}
