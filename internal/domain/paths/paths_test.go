package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutPaths(t *testing.T) {
	root := filepath.FromSlash("/repo")
	l := New(root)

	cases := map[string]string{
		l.AwaDir():         filepath.Join(root, ".awa"),
		l.ConfigFile():     filepath.Join(root, ".awa", "config.toml"),
		l.StoreDir():       filepath.Join(root, ".awa", "store"),
		l.BlobsDir():       filepath.Join(root, ".awa", "store", "blobs"),
		l.TmpDir():         filepath.Join(root, ".awa", "store", "tmp"),
		l.CheckpointsDir(): filepath.Join(root, ".awa", "checkpoints"),
		l.RunsDir():        filepath.Join(root, ".awa", "runs"),
		l.IndexesDir():     filepath.Join(root, ".awa", "indexes"),
		l.LocksDir():       filepath.Join(root, ".awa", "locks"),
		l.LogsDir():        filepath.Join(root, ".awa", "logs"),
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
	if l.Root() != root {
		t.Errorf("Root() = %q, want %q", l.Root(), root)
	}
}

func TestRequiredDirs(t *testing.T) {
	l := New(filepath.FromSlash("/repo"))
	dirs := l.RequiredDirs()

	want := []string{
		l.BlobsDir(), l.TmpDir(), l.CheckpointsDir(), l.RunsDir(),
		l.IndexesDir(), l.LocksDir(), l.LogsDir(),
	}
	if len(dirs) != len(want) {
		t.Fatalf("RequiredDirs() returned %d dirs, want %d", len(dirs), len(want))
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("RequiredDirs()[%d] = %q, want %q", i, dirs[i], want[i])
		}
	}
}

func TestConfigRelPath(t *testing.T) {
	if ConfigRelPath != ".awa/config.toml" {
		t.Errorf("ConfigRelPath = %q, want .awa/config.toml", ConfigRelPath)
	}
}

// TestResetGuidanceNeverDiscardsConfiguration is the guard behind every recovery
// hint. The reset it describes is destructive and users follow it literally, so the
// directories it names must not contain the configuration a reset is required to
// preserve — naming `.awa` itself would silently delete the private config layer
// along with the evidence.
func TestResetGuidanceNeverDiscardsConfiguration(t *testing.T) {
	l := New(string(filepath.Separator) + "project")
	preserved := map[string]bool{}
	for _, f := range PreservedResetFiles() {
		preserved[filepath.Join(l.Root(), filepath.FromSlash(f))] = true
	}
	if len(preserved) != 4 {
		t.Fatalf("PreservedResetFiles = %v, want the shared config, ignore file, local config, and git guard", PreservedResetFiles())
	}

	// No removed directory may be, or contain, a preserved file.
	for _, dir := range l.EvidenceDirs() {
		for p := range preserved {
			if p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) {
				t.Errorf("reset removes %s, which would take %s with it", dir, p)
			}
		}
		if dir == l.AwaDir() {
			t.Errorf("reset removes the whole state directory %s; it holds configuration that must survive", dir)
		}
	}

	// Every directory awa init creates must be reachable from the removal set, or a
	// reset would leave evidence behind and the store would come back half-populated.
	for _, req := range l.RequiredDirs() {
		var covered bool
		for _, dir := range l.EvidenceDirs() {
			if req == dir || strings.HasPrefix(req, dir+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("init creates %s but no reset directory covers it", req)
		}
	}

	// The sentence users actually read must name the preserved config and must not
	// tell them to delete the state directory.
	hint := ResetEvidenceHint()
	if !strings.Contains(hint, ConfigRelPath) {
		t.Errorf("reset hint %q does not say %s survives", hint, ConfigRelPath)
	}
	if strings.Contains(hint, "delete "+Dir+" ") || strings.HasSuffix(hint, "delete "+Dir) {
		t.Errorf("reset hint tells the user to delete the whole state directory: %q", hint)
	}
	// The instruction is destructive and project-relative, and it removes the lock
	// directory. Followed from the wrong directory it deletes nothing (or the wrong
	// project); followed while another awa process holds a lock it can leave a
	// half-recreated store. Both preconditions must travel with the paths.
	for _, want := range []string{"project root", "stop other awa processes"} {
		if !strings.Contains(hint, want) {
			t.Errorf("reset hint %q omits the precondition %q", hint, want)
		}
	}
}
