//go:build darwin || linux || freebsd

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestRunSkippedInputs verifies that a skipped input (a FIFO special file) blocks
// caching by default and that --allow-skipped-inputs re-enables it.
func TestRunSkippedInputs(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	// A FIFO in scope is a special file the scanner records as a skipped input.
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.fifo"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	// By default a skipped input blocks caching: both runs execute uncached. The
	// footer names the run non-reusable with the skipped-inputs reason.
	_, _, stderr := awa(t, root, "run", "--", h, "-out", "x")
	if !strings.Contains(stderr, "not-reusable(skipped-inputs)") {
		t.Errorf("expected skipped-inputs non-reusable footer, stderr = %q", stderr)
	}
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("skipped inputs must block hits by default")
	}

	// With --allow-skipped-inputs the run caches and a later allowed run hits; the
	// allowed skip stays visible rather than disappearing into a clean footer.
	_, _, stderr = awa(t, root, "run", "--allow-skipped-inputs", "--", h, "-out", "x")
	if !strings.Contains(stderr, "warning: 1 input(s) skipped (not read); skipping permitted by --allow-skipped-inputs") {
		t.Errorf("allowed skipped inputs are not visible, stderr = %q", stderr)
	}
	if _, _, stderr := awa(t, root, "run", "--allow-skipped-inputs", "--", h, "-out", "x"); !isHit(stderr) {
		t.Error("--allow-skipped-inputs should enable caching")
	}
}

// TestRunFooterDeliberateRecordingKeepsObservationLimits confirms a deliberate
// recording still discloses incomplete observation from its own facts: an unreadable
// input and an unreadable watched effect root each earn a warning, while the summary
// stays record-only without a non-reuse reason and no change is claimed.
func TestRunFooterDeliberateRecordingKeepsObservationLimits(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not make files unreadable for root")
	}
	h := helper(t)
	unreadable := func(t *testing.T, path string) {
		t.Helper()
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
	}

	t.Run("skipped input", func(t *testing.T) {
		root := initProject(t)
		write(t, root, "secret.txt", "x")
		unreadable(t, filepath.Join(root, "secret.txt"))
		_, _, stderr := awa(t, root, "run", "--record", "--stdin", "null", "--", h, "-out", "x")
		if !strings.Contains(stderr, "warning: 1 input(s) skipped (not read); this run's observation is incomplete") {
			t.Errorf("skipped input hidden behind deliberate recording:\n%s", stderr)
		}
		if strings.Contains(stderr, "not-reusable(") {
			t.Errorf("deliberate recording shown with a non-reuse reason:\n%s", stderr)
		}
	})

	for _, flag := range []string{"--record", "--no-cache"} {
		t.Run("effect root "+flag, func(t *testing.T) {
			root := initProject(t)
			write(t, root, "target/sub/f", "y")
			unreadable(t, filepath.Join(root, "target", "sub"))
			_, _, stderr := awa(t, root, "run", flag, "--stdin", "null", "--", h, "-out", "x")
			if !strings.Contains(stderr, "warning: watched generated-output state could not be observed safely; its state after this run is unknown") {
				t.Errorf("unobservable effect state hidden behind %s:\n%s", flag, stderr)
			}
			if strings.Contains(stderr, "not-reusable(") || strings.Contains(stderr, "execution window") || strings.Contains(stderr, "compare:") {
				t.Errorf("%s footer shows a defect or claims a change:\n%s", flag, stderr)
			}
		})
	}
}
