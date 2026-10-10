//go:build darwin || linux || freebsd

package lockfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanDirKeepsUnknownForUnreadableOrSymlinkedProtection(t *testing.T) {
	cases := map[string]func(t *testing.T, locksDir string){
		"record made unreadable": func(t *testing.T, locksDir string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file regardless of its mode")
			}
			if err := os.Chmod(filepath.Join(locksDir, "released.lock"), 0); err != nil {
				t.Error(err)
			}
		},
		"record replaced by a dangling symlink": func(t *testing.T, locksDir string) {
			p := filepath.Join(locksDir, "released.lock")
			if err := os.Remove(p); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(filepath.Join(locksDir, "nowhere"), p); err != nil {
				t.Error(err)
			}
		},
		"locks directory made unsearchable": func(t *testing.T, locksDir string) {
			if os.Geteuid() == 0 {
				t.Skip("root traverses a directory regardless of its mode")
			}
			if err := os.Chmod(locksDir, 0); err != nil {
				t.Error(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locksDir, 0o755) })
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			root, locksDir, now := scanFixture(t)
			got := scanStates(t, root, locksDir, now, func() { mutate(t, locksDir) })
			if got["released.lock"] != EntryUnknown || got["held.lock"] == 0 {
				t.Errorf("states = %v, want released.lock unknown and held.lock still classified", got)
			}
		})
	}
}
