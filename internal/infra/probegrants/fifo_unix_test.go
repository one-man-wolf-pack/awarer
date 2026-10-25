//go:build darwin || linux || freebsd

package probegrants

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"awarer/internal/domain/hashing"
	"awarer/internal/infra/blake3hash"
)

// fifoChildEnv carries "store|root" to the child that checks the grant, so a regression
// to a blocking open hangs that bounded child rather than the whole test binary.
const fifoChildEnv = "AWA_PROBEGRANTS_FIFO_CHILD"

func TestFIFOCheckChild(t *testing.T) {
	arg := os.Getenv(fifoChildEnv)
	if arg == "" {
		t.Skip("helper for the FIFO regressions")
	}
	parts := strings.SplitN(arg, "|", 3)
	op, dir, root := parts[0], parts[1], parts[2]
	s := NewAt(dir, blake3hash.New())
	var result string
	switch op {
	case "check":
		got := s.Check(root, testContext("a"))
		result = string(got.State) + "/" + string(got.Cause)
	case "allow":
		_, err := s.Allow(root, []hashing.TreeHash{testContext("a")})
		result = outcome(err, true)
	case "revoke":
		_, removed, err := s.Revoke(root)
		result = outcome(err, removed)
	}
	_, _ = os.Stdout.WriteString("RESULT=" + result + "\n")
}

func outcome(err error, done bool) string {
	switch {
	case err != nil:
		return "error"
	case done:
		return "done"
	default:
		return "nothing"
	}
}

func inChild(t *testing.T, op, dir, root string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFIFOCheckChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), fifoChildEnv+"="+op+"|"+dir+"|"+root)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("%s waited on a FIFO instead of rejecting it", op)
	}
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if r, ok := strings.CutPrefix(line, "RESULT="); ok {
			return r
		}
	}
	t.Fatalf("child printed no result:\n%s", out)
	return ""
}

func checkInChild(t *testing.T, dir, root string) string {
	t.Helper()
	return inChild(t, "check", dir, root)
}

func TestAFIFOInTheGrantStoreNeverStallsOrAuthorizes(t *testing.T) {
	ctx := testContext("a")
	cases := map[string]func(t *testing.T, s *Store, dir, root string) string{
		"record":           func(t *testing.T, s *Store, dir, root string) string { return recordPath(t, s, dir, root) },
		"grants directory": func(_ *testing.T, _ *Store, dir, _ string) string { return filepath.Join(dir, "awa", "probe-grants") },
		"awa directory":    func(_ *testing.T, _ *Store, dir, _ string) string { return filepath.Join(dir, "awa") },
	}
	for name, at := range cases {
		t.Run(name, func(t *testing.T) {
			s, dir, root := newStore(t)
			fifo := at(t, s, dir, root)
			if err := os.MkdirAll(filepath.Dir(fifo), 0o700); err != nil {
				t.Fatal(err)
			}
			// The FIFO never has a writer: a blocking read open would wait forever.
			if err := unix.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			if got := checkInChild(t, dir, root); got != "unavailable/store-invalid" {
				t.Fatalf("check = %s, want unavailable/store-invalid", got)
			}
		})
	}

	// Regular-file control: the same read path still authorizes an ordinary record.
	s, dir, root := newStore(t)
	if _, err := s.Allow(root, []hashing.TreeHash{ctx}); err != nil {
		t.Fatal(err)
	}
	if got := checkInChild(t, dir, root); got != "granted/" {
		t.Fatalf("regular record = %s, want granted", got)
	}
}

func TestAFIFOInTheGrantStoreFailsAllowAndRevoke(t *testing.T) {
	for name, rel := range map[string]string{"awa directory": "awa", "grants directory": "awa/probe-grants"} {
		t.Run(name, func(t *testing.T) {
			_, dir, root := newStore(t)
			fifo := filepath.Join(dir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(fifo), 0o700); err != nil {
				t.Fatal(err)
			}
			// The FIFO never has a writer: a blocking open would wait forever.
			if err := unix.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			for _, op := range []string{"allow", "revoke"} {
				if got := inChild(t, op, dir, root); got != "error" {
					t.Errorf("%s = %s, want an error rather than success", op, got)
				}
			}
			if got := checkInChild(t, dir, root); got != "unavailable/store-invalid" {
				t.Errorf("check after a failed allow = %s, want unavailable/store-invalid", got)
			}
		})
	}

	// Ordinary controls through the same management paths.
	_, dir, root := newStore(t)
	for _, step := range []struct{ op, want string }{
		{"revoke", "nothing"}, {"allow", "done"}, {"check", "granted/"}, {"revoke", "done"}, {"check", "denied/no-grant"},
	} {
		if got := inChild(t, step.op, dir, root); got != step.want {
			t.Fatalf("%s = %s, want %s", step.op, got, step.want)
		}
	}
}
