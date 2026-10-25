//go:build darwin || linux || freebsd

package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A FIFO where the grant store belongs must make allow and revoke fail rather than
// stall or report success, and leave inspection and run terminating with an unavailable
// grant: no probe runs and the command still runs, without reuse.
func TestAFIFOGrantStoreNeitherStallsNorAuthorizesConsumers(t *testing.T) {
	root := initProject(t)
	l := newProbeLauncher(t)
	write(t, root, "src.txt", "v1")
	write(t, root, "awa.toml", l.policy())
	home := t.TempDir()
	u := userConfigAt(home)
	userDir := filepath.Join(home, ".config")
	if runtime.GOOS == "darwin" {
		userDir = filepath.Join(home, "Library", "Application Support")
	}
	if err := os.MkdirAll(filepath.Join(userDir, "awa"), 0o700); err != nil {
		t.Fatal(err)
	}
	// No writer ever opens it: a blocking read open would wait forever.
	if err := unix.Mkfifo(filepath.Join(userDir, "awa", "probe-grants"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	bounded := func(args ...string) (int, string, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, awaBinary(), args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), u.env...)
		var out, errBuf strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errBuf
		err := cmd.Run()
		if ctx.Err() != nil {
			t.Fatalf("awa %v waited on the FIFO grant store", args)
		}
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, out.String(), errBuf.String()
	}

	if code, stdout, stderr := bounded("config", "probes", "allow"); code != 1 || strings.Contains(stdout, "approved") || !strings.Contains(stderr, "the approval is not confirmed") {
		t.Fatalf("allow: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if code, stdout, stderr := bounded("config", "probes", "revoke"); code != 1 || strings.Contains(stdout, "revoked") || strings.Contains(stdout, "no identity probe grant") {
		t.Fatalf("revoke: exit %d\n%s\n%s", code, stdout, stderr)
	}
	code, stdout, stderr := bounded("config", "effective", "--json")
	var doc probeAuthorizationDoc
	if code != 0 || json.Unmarshal([]byte(stdout), &doc) != nil || doc.Data.ProbeAuthorization == nil {
		t.Fatalf("config effective: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if a := doc.Data.ProbeAuthorization; a.State != "unavailable" || a.Cause != "store-invalid" {
		t.Fatalf("inspection = %+v, want unavailable store-invalid", a)
	}
	code, stdout, stderr = bounded("run", "--", "sh", "-c", validationBuild)
	if code != 0 || stdout != "checked\n" || !strings.Contains(stderr, "grant is unavailable (store-invalid)") {
		t.Fatalf("run: exit %d\n%s\n%s", code, stdout, stderr)
	}
	if l.callCount(t) != 0 {
		t.Fatalf("an unavailable grant executed %d probes", l.callCount(t))
	}
}
