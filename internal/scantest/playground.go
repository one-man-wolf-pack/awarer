// Package scantest builds deterministic worktree fixtures for scanner, index,
// checkpoint, and acceptance tests. It owns two fixture shapes: real filesystem
// playgrounds, whose builders create real files in real temporary directories so
// tests prove behavior against the OS and the SQLite database rather than against
// mocks; and bounded in-memory canonical manifest cursors/streams, which compose the
// worktree record, Ordered, and slice-cursor primitives so no test package restates
// that canonicalization. Playground helpers accept testing.TB so they serve both
// tests and benchmarks.
//
// This is test support: production packages do not import it.
package scantest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"awarer/internal/app/initcmd"
	"awarer/internal/infra/projfs"
)

// InitProject turns dir into an initialized awa project (the .awa marker and its
// directory layout; the default profile writes no config file) and returns the
// resolved project handle.
func InitProject(tb testing.TB, dir string) projfs.Project {
	tb.Helper()
	if _, err := initcmd.Run(initcmd.Request{Root: dir}); err != nil {
		tb.Fatalf("init project: %v", err)
	}
	proj, err := projfs.Open(dir)
	if err != nil {
		tb.Fatalf("open project: %v", err)
	}
	return proj
}

// Write creates a file (and its parent directories) with the given content under
// root, using a slash-separated rel path.
func Write(tb testing.TB, root, rel, content string) {
	tb.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		tb.Fatal(err)
	}
}

// BuildPlainGo creates the plain-go fixture: a non-git Go project that proves awa
// works without git. The shape mirrors the plain-Go acceptance scenario. It
// contains five regular files.
func BuildPlainGo(tb testing.TB, root string) {
	tb.Helper()
	Write(tb, root, "go.mod", "module example.com/plain\n\ngo 1.21\n")
	Write(tb, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int { return a + b }\n")
	Write(tb, root, "calc/calc_test.go", "package calc\n")
	Write(tb, root, "cmd/check/main.go", "package main\n\nfunc main() {}\n")
	Write(tb, root, "data/input.txt", "the quick brown fox\n")
}

// requireSymlinksEnv turns a missing symlink capability from a skip into a failure.
// The windows-portability and freebsd-portability lanes set it, because those lanes
// exist to prove platform symlink behavior: a fixture that quietly skipped every
// symlink case and reported green would be worse than no lane at all, since it would
// look like evidence. A developer running the suite on a machine without the
// privilege still gets a named skip.
const requireSymlinksEnv = "AWA_REQUIRE_SYMLINK_TESTS"

// endWithoutSymlinks ends a test that cannot create a symlink — fatally where
// symlink coverage is required, with a named skip otherwise.
func endWithoutSymlinks(tb testing.TB, err error) {
	tb.Helper()
	if os.Getenv(requireSymlinksEnv) != "" {
		tb.Fatalf("%s is set, so symlink coverage is required, but this platform will not create a symlink: %v",
			requireSymlinksEnv, err)
	}
	tb.Skipf("symlinks unavailable on this platform/sandbox: %v", err)
}

// Symlink creates a symlink at rel pointing to target (a raw link string), ending
// the test where the platform cannot create symlinks.
func Symlink(tb testing.TB, root, rel, target string) {
	tb.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		endWithoutSymlinks(tb, err)
	}
}

// Hardlink creates a hard link at rel pointing to an existing file at targetRel.
func Hardlink(tb testing.TB, root, rel, targetRel string) {
	tb.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, filepath.FromSlash(targetRel)), p); err != nil {
		tb.Fatalf("hardlink: %v", err)
	}
}

// RequireSymlinks creates a probe symlink and ends the test if the platform or
// sandbox cannot create one — fatally where symlink coverage is required, with a
// named skip otherwise.
func RequireSymlinks(tb testing.TB) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "awa-symlink-probe-")
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "t"), []byte("x"), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "t"), filepath.Join(dir, "l")); err != nil {
		endWithoutSymlinks(tb, err)
	}
}

// BuildLargeFiles creates the large-files fixture: a small file, a file exactly
// at the limit, and a file over the limit. Sizes are expressed in bytes so a test
// can set max_file_size accordingly.
func BuildLargeFiles(tb testing.TB, root string, limit int) {
	tb.Helper()
	Write(tb, root, "go.mod", "module example.com/large\n\ngo 1.21\n")
	Write(tb, root, "small.txt", strings.Repeat("a", limit/2))
	Write(tb, root, "exact-limit.bin", strings.Repeat("b", limit))
	Write(tb, root, "over-limit.bin", strings.Repeat("c", limit+1024))
}
