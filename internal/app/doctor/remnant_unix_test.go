//go:build darwin || linux || freebsd

package doctor

import (
	"os"
	"testing"

	dom "awarer/internal/domain/doctor"
)

func TestFailedRemnantRemovalIsNotRepairedAndRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	e := newEnv(t)
	dir, _ := e.headerlessCheckpoint(t, "payload")
	before := fileBytes(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	res := e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeRepairFailed); len(got) != 1 || got[0].Path() != dir {
		t.Fatalf("want one repair-failed at %s, got %+v", dir, res.Findings())
	}
	if got := findingsByCode(res, dom.CodeIncompletePublication); len(got) != 1 || got[0].Repaired() || !got[0].Repairable() {
		t.Fatalf("want the remnant still repairable and unrepaired, got %+v", res.Findings())
	}
	if res.Summary().Repaired != 0 || !res.NeedsAttention() {
		t.Fatalf("a failed removal reads as repaired: %+v", res.Summary())
	}
	assertFileBytes(t, dir, before)

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	res = e.run(t, noGit(), Request{Repair: true})
	if got := findingsByCode(res, dom.CodeIncompletePublication); len(got) != 1 || !got[0].Repaired() {
		t.Fatalf("retry did not repair: %+v", res.Findings())
	}
	if res.NeedsAttention() {
		t.Fatalf("retry left attention needed: %+v", res.Findings())
	}
}
