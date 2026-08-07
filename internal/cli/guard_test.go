package cli

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"awarer/internal/app/initcmd"
	"awarer/internal/infra/projfs"
	"awarer/internal/output"
)

func initGuardProject(t *testing.T) (projfs.Project, string) {
	t.Helper()
	root := t.TempDir()
	if _, err := initcmd.Run(initcmd.Request{Root: root, Profile: initcmd.ProfileDefault}); err != nil {
		t.Fatalf("init: %v", err)
	}
	proj, err := projfs.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	layout, err := proj.Paths()
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	return proj, layout.StateGitignore()
}

// A write command's preflight silently restores a deleted .awa/.gitignore so state
// never accumulates in an unguarded directory, and it says so on stderr.
func TestGuardPreflightRestoresMissingGuard(t *testing.T) {
	proj, guard := initGuardProject(t)
	if err := os.Remove(guard); err != nil {
		t.Fatalf("remove guard: %v", err)
	}

	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)
	if err := guardPreflight(w, proj, true); err != nil {
		t.Fatalf("strict preflight on a restorable guard = %v, want nil", err)
	}
	if _, err := os.Stat(guard); err != nil {
		t.Errorf("guard not restored: %v", err)
	}
	if !strings.Contains(stderr.String(), "restored") {
		t.Errorf("stderr = %q, want a restore notice", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("guard preflight must not touch stdout, got %q", stdout.String())
	}
}

// TestGuardPreflightStrictFailsWhenUnrestorable proves the fail-loud contract: when
// the guard cannot be restored, a write command's preflight errors rather than
// growing unprotected state, while a read-only command only warns.
func TestGuardPreflightStrictFailsWhenUnrestorable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write-permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	proj, guard := initGuardProject(t)
	layout, _ := proj.Paths()
	if err := os.Remove(guard); err != nil {
		t.Fatalf("remove guard: %v", err)
	}
	// Make .awa unwritable so the guard cannot be recreated.
	if err := os.Chmod(layout.AwaDir(), 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(layout.AwaDir(), 0o700) })

	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)
	if err := guardPreflight(w, proj, true); err == nil {
		t.Error("strict preflight on an unrestorable guard = nil, want an error")
	}

	var s2, e2 bytes.Buffer
	w2 := output.New(&s2, &e2)
	if err := guardPreflight(w2, proj, false); err != nil {
		t.Errorf("read-only preflight = %v, want a warning and nil", err)
	}
	if !strings.Contains(e2.String(), "warning") {
		t.Errorf("read-only stderr = %q, want a warning", e2.String())
	}
}
