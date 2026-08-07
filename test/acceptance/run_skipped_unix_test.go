//go:build darwin || linux || freebsd

package acceptance

import (
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
	if !strings.Contains(stderr, "not reusable (skipped-inputs)") {
		t.Errorf("expected skipped-inputs non-reusable footer, stderr = %q", stderr)
	}
	// A non-reusable run still points at the deeper cache-decision command so the user
	// is not left to guess why it will not reuse.
	if !strings.Contains(stderr, "run explain --") {
		t.Errorf("skipped-inputs footer should point to run explain, stderr = %q", stderr)
	}
	if _, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); isHit(stderr) {
		t.Error("skipped inputs must block hits by default")
	}

	// With --allow-skipped-inputs the run caches and a later allowed run hits.
	awa(t, root, "run", "--allow-skipped-inputs", "--", h, "-out", "x")
	if _, _, stderr := awa(t, root, "run", "--allow-skipped-inputs", "--", h, "-out", "x"); !isHit(stderr) {
		t.Error("--allow-skipped-inputs should enable caching")
	}
}
