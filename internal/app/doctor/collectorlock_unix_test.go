//go:build darwin || linux || freebsd

package doctor

import (
	"os"
	"path/filepath"
	"testing"

	dom "awarer/internal/domain/doctor"
	"awarer/internal/infra/lockfile"
)

// TestUnrecognizedCollectorLockNotReported asserts the flock backend's doctor
// contract. Under flock the collector lock's contents are advisory: a live holder is
// detected by flock, and an unparsable file with no holder is a harmless leftover the
// next collector simply reacquires. Doctor must not turn it into a finding, and must
// never remove it. (The file-content fallback makes the opposite call — see
// collectorlock_other_test.go.)
func TestUnrecognizedCollectorLockNotReported(t *testing.T) {
	e := newEnv(t)
	layout, _ := e.project.Paths()
	lockPath := filepath.Join(layout.LocksDir(), lockfile.CollectorLockName)
	if err := os.WriteFile(lockPath, []byte("garbage not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeLockUnknown); len(got) != 0 {
		t.Fatalf("an unheld leftover gc.lock must not be a finding, got %+v", got)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("collector lock must not be removed: %v", err)
	}
}
