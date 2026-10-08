//go:build darwin || linux || freebsd

package gc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gcdom "awarer/internal/domain/gc"
)

// TestFailedDeletionAfterMutationCreditsZero uses Unix directory permissions to make
// a run's deletion fail part-way: the store removes the key pointer, then cannot empty
// a read-only directory inside the entry. Something was already removed, yet the
// failed candidate credits nothing and the executed total is marked incomplete. The
// portable crediting rule is proved in the domain tests; this is the real-store case.
func TestFailedDeletionAfterMutationCreditsZero(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	e := newEnv(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	now := base.Add(60 * 24 * time.Hour)
	layout, _ := e.project.Paths()
	id := e.seedRun(t, "doomed", base)
	entry := e.mustRunPath(t, id)
	locked := filepath.Join(entry, "locked")
	writeLen(t, filepath.Join(locked, "pinned"), 400)
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	keysDir := filepath.Join(layout.RunsDir(), "keys")
	if regularBytes(t, keysDir) == 0 {
		t.Fatal("fixture run published no key pointer")
	}

	pl := e.plan(t, now, nil, defaultReq(t))
	c := onlyCandidate(t, candidatesOf(pl, gcdom.KindRun, gcdom.ActionDelete), "deleted run")
	assertExact(t, c, regularBytes(t, entry))

	res := e.execute(t, now, nil, pl)
	es := res.ExecSummary()
	if es.Failed != 1 || es.BytesDeleted != 0 || es.BytesDeletedComplete() {
		t.Fatalf("exec summary = %+v, want one failure credited 0 and an incomplete total", es)
	}
	if regularBytes(t, keysDir) != 0 {
		t.Fatal("fixture did not mutate before failing: the key pointer should already be gone")
	}
}
