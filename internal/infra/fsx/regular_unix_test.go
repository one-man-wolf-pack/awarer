//go:build darwin || linux || freebsd

package fsx

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fifoChildEnv carries "root|rel" to the child that performs the open, so a regression
// to a blocking open hangs that bounded child rather than the whole test binary.
const fifoChildEnv = "AWA_FSX_REGULAR_OPEN_CHILD"

func TestOpenRegularNoFollowAtChild(t *testing.T) {
	arg := os.Getenv(fifoChildEnv)
	if arg == "" {
		t.Skip("helper for the FIFO regressions")
	}
	op, rest, _ := strings.Cut(arg, "|")
	root, rel, _ := strings.Cut(rest, "|")
	var f *os.File
	var err error
	switch op {
	case "open-dir":
		f, err = OpenDirOnlyNoFollow(root, rel)
	case "mkdir":
		f, err = MkdirAllDirOnlyNoFollow(root, rel, 0o700)
	default:
		f, err = OpenRegularNoFollowAt(root, rel)
	}
	switch {
	case err == nil:
		_ = f.Close()
		_, _ = os.Stdout.WriteString("RESULT=opened\n")
	case errors.Is(err, ErrNotRegular):
		_, _ = os.Stdout.WriteString("RESULT=not-regular\n")
	case errors.Is(err, ErrNotDirectory):
		_, _ = os.Stdout.WriteString("RESULT=not-directory\n")
	default:
		_, _ = os.Stdout.WriteString("RESULT=other " + err.Error() + "\n")
	}
}

func openInChild(t *testing.T, root, rel string) string {
	t.Helper()
	return inChild(t, "regular", root, rel)
}

func inChild(t *testing.T, op, root, rel string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOpenRegularNoFollowAtChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), fifoChildEnv+"="+op+"|"+root+"|"+rel)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s %s waited on a special node instead of rejecting it", op, rel)
	}
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if r, ok := strings.CutPrefix(line, "RESULT="); ok {
			return r
		}
	}
	t.Fatalf("child printed no result:\n%s", out)
	return ""
}

func TestOpenRegularNoFollowAtRejectsAFIFOWithoutWaiting(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Neither FIFO ever has a writer: a blocking read open would wait forever.
	if err := unix.Mkfifo(filepath.Join(root, "dir", "leaf"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := openInChild(t, root, "dir/leaf"); got != "not-regular" {
		t.Errorf("FIFO leaf = %s, want not-regular", got)
	}
	if got := openInChild(t, root, "pipe/leaf"); got != "not-directory" {
		t.Errorf("FIFO component = %s, want not-directory", got)
	}
}

func TestOpenRegularNoFollowAtOpensARegularFileAndRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRegularNoFollowAt(root, "dir/file")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(data) != "payload" {
		t.Fatalf("read = %q, %v", data, err)
	}

	if err := os.Symlink(filepath.Join(root, "dir", "file"), filepath.Join(root, "dir", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "dir"), filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"dir/link", "dirlink/file"} {
		if _, err := OpenRegularNoFollowAt(root, rel); !IsSymlinkOpenRejection(err) {
			t.Errorf("%s = %v, want a symlink rejection", rel, err)
		}
	}
	if _, err := OpenRegularNoFollowAt(root, "dir/absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent leaf = %v, want not-exist", err)
	}
	if _, err := OpenRegularNoFollowAt(root, "gone/file"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent component = %v, want not-exist", err)
	}
	if _, err := OpenRegularNoFollowAt(root, "dir"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory leaf = %v, want not-regular", err)
	}
}

func TestDirOnlyTraversalRejectsAFIFOWithoutWaiting(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	// No writer ever opens these FIFOs: a blocking open would wait forever.
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "dir", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"open-dir", "mkdir"} {
		for _, rel := range []string{"pipe", "pipe/sub", "dir/pipe", "dir/pipe/sub"} {
			if got := inChild(t, op, root, rel); got != "not-directory" {
				t.Errorf("%s %s = %s, want not-directory", op, rel, got)
			}
		}
	}
}

func TestDirOnlyTraversalKeepsDirectorySymlinkAndErrorContracts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	f, err := OpenDirOnlyNoFollow(root, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := f.Stat(); err != nil || !info.IsDir() {
		t.Fatalf("opened = %v, %v, want a directory", info, err)
	}
	_ = f.Close()
	if _, err := OpenDirOnlyNoFollow(root, "a/absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("absent = %v, want not-exist", err)
	}
	for _, rel := range []string{"link", "link/b"} {
		for name, open := range map[string]func(string, string) (*os.File, error){
			"open":  OpenDirOnlyNoFollow,
			"mkdir": func(r, d string) (*os.File, error) { return MkdirAllDirOnlyNoFollow(r, d, 0o700) },
		} {
			if _, err := open(root, rel); !IsSymlinkOpenRejection(err) {
				t.Errorf("%s %s = %v, want a symlink rejection", name, rel, err)
			}
		}
	}
	for _, rel := range []string{"a/file", "a/file/sub"} {
		if _, err := OpenDirOnlyNoFollow(root, rel); !errors.Is(err, ErrNotDirectory) {
			t.Errorf("open %s = %v, want not-directory", rel, err)
		}
		if _, err := MkdirAllDirOnlyNoFollow(root, rel, 0o700); !errors.Is(err, ErrNotDirectory) {
			t.Errorf("mkdir %s = %v, want not-directory", rel, err)
		}
	}
	d, err := MkdirAllDirOnlyNoFollow(root, "a/new/deeper", 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if info, err := os.Lstat(filepath.Join(root, "a", "new", "deeper")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("created = %v, %v", info, err)
	}
	if d, err = MkdirAllDirOnlyNoFollow(root, "a/new/deeper", 0o700); err != nil {
		t.Fatalf("existing chain = %v", err)
	}
	defer func() { _ = d.Close() }()
	for _, body := range []string{"one", "two"} {
		if err := ReplaceBytesAt(d, "x", []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(root, "a", "new", "deeper", "x")); err != nil || string(got) != body {
			t.Fatalf("replaced = %q, %v, want %q", got, err, body)
		}
	}
}
