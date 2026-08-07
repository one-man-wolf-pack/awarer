//go:build windows

package process_test

import (
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/infra/process"
)

// TestResolveWindowsPATHEXTAndCwd verifies the Windows resolver applies PATHEXT
// and searches the execution directory before PATH for a bare command, using the
// command's own env rather than the process environment.
func TestResolveWindowsPATHEXTAndCwd(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "mytool.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	// PATH is empty: resolution must still find the tool in the execution dir
	// (cwd-first), and PATHEXT supplies the .EXE suffix for the bare name.
	env := []string{"PATHEXT=.EXE", "PATH="}

	p := process.New()
	path, _, ok := p.Resolve("mytool", dir, env)
	if !ok {
		t.Fatal("expected to resolve mytool via PATHEXT in the execution dir")
	}
	if path != exe {
		t.Errorf("path = %q, want %q", path, exe)
	}

	// A relative PATH entry is anchored at the execution dir, not the process cwd.
	sub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	subExe := filepath.Join(sub, "other.exe")
	if err := os.WriteFile(subExe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, _, ok = p.Resolve("other", dir, []string{"PATHEXT=.EXE", "PATH=bin"})
	if !ok || path != subExe {
		t.Errorf("relative PATH resolve = %q ok=%v, want %q", path, ok, subExe)
	}
}
