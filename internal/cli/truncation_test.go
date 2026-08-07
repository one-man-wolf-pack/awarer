package cli

import (
	"bytes"
	"strings"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

// TestEmitTruncationBanner proves the run-footer banner names the truncated stream
// and the omitted byte count on stderr, and stays silent when nothing was cut.
func TestEmitTruncationBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)
	emitTruncationBanner(w, &apprun.Output{
		Stdout: apprun.StreamSummary{Truncated: true, OmittedBytes: 4096},
		Stderr: apprun.StreamSummary{},
	})
	got := stderr.String()
	if !strings.Contains(got, "truncated") || !strings.Contains(got, "stdout") || !strings.Contains(got, "4096") {
		t.Errorf("stderr = %q, want a stdout truncation notice with the omitted byte count", got)
	}
	if strings.Contains(got, "stderr") {
		t.Errorf("stderr stream must not be reported as truncated: %q", got)
	}
	if stdout.Len() != 0 {
		t.Errorf("banner must not touch stdout, got %q", stdout.String())
	}

	// No truncation → no banner, and a nil output (capture disabled) is a no-op.
	var s2, e2 bytes.Buffer
	w2 := output.New(&s2, &e2)
	emitTruncationBanner(w2, &apprun.Output{})
	emitTruncationBanner(w2, nil)
	if e2.Len() != 0 {
		t.Errorf("banner emitted for an untruncated/absent output: %q", e2.String())
	}
}

// TestEmitStoredTruncationBanner proves the run show banner reports a partial stored
// payload on stderr, keeping a piped --stdout dump clean.
func TestEmitStoredTruncationBanner(t *testing.T) {
	var stdout, stderr bytes.Buffer
	w := output.New(&stdout, &stderr)
	emitStoredTruncationBanner(w,
		runcache.OutputCapture{Truncated: true, OmittedBytes: 128},
		runcache.OutputCapture{})
	if !strings.Contains(stderr.String(), "128") || !strings.Contains(stderr.String(), "stdout") {
		t.Errorf("stderr = %q, want a stored stdout truncation notice", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("banner must stay off stdout, got %q", stdout.String())
	}
}
