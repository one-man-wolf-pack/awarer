package cli

import (
	"os"
	"testing"
)

// TestMain isolates the cli tests from ambient awa-project discovery. Several router
// tests exercise the "no project" path (exit 3) by relying on the process working
// directory being outside any awa project. That is an ambient dependency: when the
// repository itself is an awa project — dogfooding awa on its own checkout — the
// package directory has a .awa ancestor, and those tests would wrongly discover it
// and pass through to a real project. Running the whole package from an isolated
// empty directory makes discovery deterministic regardless of where the checkout
// lives or whether it is an awa project. Tests that need a project pass an explicit
// --root, so they are unaffected by the chdir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "awa-cli-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
