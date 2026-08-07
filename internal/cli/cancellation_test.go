package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCtx invokes the CLI with an explicit root context, so a test can drive
// cancellation through the same path SIGINT uses.
func runCtx(ctx context.Context, args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = Run(ctx, args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestLogCancellationClassifiesAsInterrupt proves a cancelled root context during the
// checkpoint header walk is reported as an interruption (ExitInterrupted, "interrupted"),
// not dressed up as a storage or generic read failure. The store is healthy — the only
// reason the walk stops is the cancellation — so a non-interrupt exit would mean the CLI
// mistook an operator Ctrl+C for damage.
func TestLogCancellationClassifiesAsInterrupt(t *testing.T) {
	root := initProject(t)
	if code, _, stderr := run("checkpoint", "--root", root, "-m", "baseline"); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, _, stderr := runCtx(ctx, "log", "--root", root)
	if code != int(ExitInterrupted) {
		t.Fatalf("log exit = %d, want ExitInterrupted (%d); stderr = %q", code, ExitInterrupted, stderr)
	}
}

// TestDocsExportCancelledBeforeAnyWriteIsWired proves the wiring the mapping table in
// docs_test.go cannot: that a real invocation's root context reaches Publish at all, so
// a Ctrl+C arriving before the destination is reserved really does surface as an
// interruption and really does leave the path absent. Which failures then keep their
// message is docsExportFailure's decision, tested directly on the error identities.
func TestDocsExportCancelledBeforeAnyWriteIsWired(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, _, stderr := runCtx(ctx, "docs", "export", "--output", dest)
	if code != int(ExitInterrupted) {
		t.Fatalf("docs export exit = %d, want ExitInterrupted (%d); stderr = %q", code, ExitInterrupted, stderr)
	}
	if !strings.Contains(stderr, "interrupted") {
		t.Errorf("stderr = %q, want the standard interruption message", stderr)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a cancelled export created %s: %v", dest, err)
	}
}
