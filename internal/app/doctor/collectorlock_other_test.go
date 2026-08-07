//go:build !darwin && !linux && !freebsd

package doctor

import (
	"os"
	"path/filepath"
	"testing"

	dom "awarer/internal/domain/doctor"
	"awarer/internal/infra/lockfile"
)

// TestUnrecognizedCollectorLockReported asserts the file-content fallback's doctor
// contract. Without flock, an unrecognized gc.lock is not self-healing — the acquire
// path refuses it rather than reacquiring, so it blocks every collector — and doctor
// surfaces it as a lock-unknown finding. Like any unknown lock it is never removed.
// (The flock backend treats it as a harmless leftover — see collectorlock_unix_test.go.)
func TestUnrecognizedCollectorLockReported(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	lockPath := filepath.Join(layout.LocksDir(), lockfile.CollectorLockName)
	if err := os.WriteFile(lockPath, []byte("garbage not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeLockUnknown); len(got) != 1 || got[0].Repairable() {
		t.Fatalf("want one non-repairable lock-unknown for an unrecognized gc.lock, got %+v", res.Findings())
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("unknown collector lock must not be removed: %v", err)
	}
}
