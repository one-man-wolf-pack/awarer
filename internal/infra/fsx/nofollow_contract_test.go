package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The no-follow contract this file pins is promised on every platform. Only its
// atomicity differs: darwin, linux, and freebsd refuse a symlink as part of the open
// itself and at every ancestor component, while the fallback used off those platforms
// refuses the final component with a pre-open Lstat. That difference is evidence of its
// own and lives in fsx_unix_test.go — asserting it here would demand semantics the
// fallback deliberately does not offer, and the Windows lane would be red for a promise
// nobody made. The fallback's own classification, which the native platforms do not
// share, is in nofollow_fallback_test.go.
//
// Before these files the fallback path — the one Windows actually runs — had no runtime
// coverage at all, so the lane that exists to prove the no-follow primitives never
// executed them.

// requireSymlinks proves the platform will create a symlink, or ends the test — fatally
// where symlink coverage is required, with a named skip otherwise.
//
// This mirrors the worktreemut helper of the same purpose so the Windows and FreeBSD
// lanes' AWA_REQUIRE_SYMLINK_TESTS reaches fsx too. Without it the cases that prove a
// symlink is refused would quietly skip on the very runners where the implementation
// under test is the one being proved, and the lane would report green having proved
// nothing.
func requireSymlinks(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if os.Getenv("AWA_REQUIRE_SYMLINK_TESTS") != "" {
		t.Fatalf("AWA_REQUIRE_SYMLINK_TESTS is set, so symlink coverage is required, but this platform will not create a symlink: %v", err)
	}
	t.Skipf("this platform will not create a symlink: %v", err)
}

// TestOpenNoFollowHonorsItsCommonContract exercises the outcomes every platform
// promises, against real filesystem errors rather than assertions about them.
func TestOpenNoFollowHonorsItsCommonContract(t *testing.T) {
	t.Run("an ordinary file opens", func(t *testing.T) {
		reg := filepath.Join(t.TempDir(), "reg")
		if err := os.WriteFile(reg, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := OpenNoFollow(reg)
		if err != nil {
			t.Fatalf("OpenNoFollow(regular) = %v, want success", err)
		}
		_ = f.Close()
	})

	// The swap window a separate shape check cannot close: a path that is a symlink at
	// open time — even one pointing at a file with identical bytes — must be refused
	// rather than followed.
	t.Run("a symlink is refused and recognized", func(t *testing.T) {
		dir := t.TempDir()
		reg := filepath.Join(dir, "reg")
		if err := os.WriteFile(reg, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		requireSymlinks(t, reg, link)

		f, err := OpenNoFollow(link)
		if err == nil {
			_ = f.Close()
			t.Fatal("OpenNoFollow(symlink) succeeded, want a no-follow rejection")
		}
		if !IsSymlinkOpenRejection(err) {
			t.Fatalf("OpenNoFollow(symlink) err = %v, want it recognized by IsSymlinkOpenRejection", err)
		}
	})

	// Absence is a normal answer that callers act on, so no platform may turn it into a
	// corruption sentinel. This is the half Windows makes easy to get wrong: it reports a
	// missing ancestor and a non-directory ancestor with one status, and a classifier
	// keying on that status alone would call every absent path corrupt.
	t.Run("an absent path keeps its absence", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{"missing", "dir/missing", "missing/deeper"} {
			_, err := OpenNoFollowAt(root, rel)
			if errors.Is(err, ErrNotDirectory) {
				t.Errorf("OpenNoFollowAt(root, %s) err = %v, want an absence, not ErrNotDirectory", rel, err)
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("OpenNoFollowAt(root, %s) err = %v, want it to keep its os.ErrNotExist identity", rel, err)
			}
		}
	})
}

// TestHasNonDirectoryAncestorUnder pins the discrimination the fallback depends on, on
// every host including the ones that never compile the fallback.
//
// The fallback cannot ask Windows whether an ancestor is a file — the status it gets back
// answers "a file or missing, cannot say which". This helper is what turns that into an
// answer, so it is the piece that has to be right; keeping it portable is what allows it
// to be proved somewhere other than the runner it was written for.
func TestHasNonDirectoryAncestorUnder(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		rel  string
		want bool
	}{
		{"directly under a regular file", "file/child", true},
		{"two levels under a regular file", "file/a/b", true},
		{"under a real directory", "dir/child", false},
		{"under a missing directory", "missing/child", false},
		{"an existing file itself", "file", false},
		{"a leaf beside existing ancestors", "anything", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasNonDirectoryAncestorUnder(root, c.rel); got != c.want {
				t.Errorf("hasNonDirectoryAncestorUnder(root, %s) = %v, want %v", c.rel, got, c.want)
			}
		})
	}

	// root itself is the trusted anchor and is never inspected. That matters on real
	// hosts: t.TempDir() sits under /var on darwin, which is a symlink, so a walk that
	// climbed past root would refuse to answer for any path here at all.
	t.Run("a link above root does not decide", func(t *testing.T) {
		if !hasNonDirectoryAncestorUnder(root, "file/child") {
			t.Error("the walk climbed above root; only the tree below it is the caller's to judge")
		}
	})
}

// TestHasNonDirectoryAncestorDoesNotFollowLinks covers the case the answer is withheld
// for.
//
// A symlink is not a directory by Lstat, but it may name one, and telling which would
// mean resolving it — which is the one thing a no-follow primitive cannot do to decide
// its own error. Reporting a symlinked ancestor as a non-directory would claim corruption
// on the strength of a link's own mode, so the honest answer is none, and the caller
// keeps the platform's original error.
//
// The nested case is the one that decides whether this is really implemented. Lstat does
// not follow the final component but does follow every earlier one, so inspecting a
// single ancestor of `link/file/child` resolves `link` and then reports on `file` — a
// true answer reached by following exactly what was not to be followed. Only a
// component-wise walk that stops at the first link refuses it, and only this witness
// tells the two implementations apart: the immediate-parent cases below pass under both.
func TestHasNonDirectoryAncestorUnderDoesNotFollowLinks(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(root, "realfile")
	if err := os.WriteFile(realFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A regular file INSIDE the linked directory, so the nested cases below reach a
	// genuine non-directory, but only by way of the link.
	if err := os.WriteFile(filepath.Join(realDir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	requireSymlinks(t, realDir, filepath.Join(root, "to-dir"))
	if err := os.Symlink(realFile, filepath.Join(root, "to-file")); err != nil {
		t.Fatalf("linking to a file: %v", err)
	}

	for _, c := range []struct{ name, rel string }{
		{"a symlinked ancestor naming a directory", "to-dir/child"},
		{"a symlinked ancestor naming a file", "to-file/child"},
		{"a non-directory reachable only through a symlinked ancestor", "to-dir/file/child"},
		{"a symlinked ancestor two levels up", "to-dir/file/a/b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if hasNonDirectoryAncestorUnder(root, c.rel) {
				t.Errorf("hasNonDirectoryAncestorUnder(root, %s) = true; a link's own mode is not proof about what it names, and resolving it would follow the link", c.rel)
			}
		})
	}
}
