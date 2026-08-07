//go:build unix

package process_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/runcache"
	"awarer/internal/infra/process"
)

func run(t *testing.T, spec runcache.RunSpec) (runcache.RunResult, error) {
	t.Helper()
	// The runner rejects a nil env (the inherit-everything sentinel). These runner
	// tests are not about cache-env sanitization, so they opt into the real
	// environment explicitly.
	if spec.Env == nil {
		spec.Env = os.Environ()
	}
	return process.New().Run(context.Background(), spec)
}

func TestNormalExitAndStdout(t *testing.T) {
	var out, errBuf bytes.Buffer
	res, err := run(t, runcache.RunSpec{
		Argv:   []string{"/bin/echo", "hello"},
		Stdout: &out,
		Stderr: &errBuf,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit.Kind != runcache.ExitNormal || res.Exit.Code != 0 {
		t.Errorf("exit = %+v, want normal 0", res.Exit)
	}
	if out.String() != "hello\n" {
		t.Errorf("stdout = %q, want %q", out.String(), "hello\n")
	}
	if res.FinishedAt.Before(res.StartedAt) {
		t.Error("finished before started")
	}
}

func TestNonZeroExit(t *testing.T) {
	res, err := run(t, runcache.RunSpec{
		Argv:   []string{"/bin/sh", "-c", "exit 7"},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit.Code != 7 {
		t.Errorf("code = %d, want 7", res.Exit.Code)
	}
}

func TestStderrCaptured(t *testing.T) {
	var out, errBuf bytes.Buffer
	if _, err := run(t, runcache.RunSpec{
		Argv:   []string{"/bin/sh", "-c", "echo oops 1>&2"},
		Stdout: &out,
		Stderr: &errBuf,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if errBuf.String() != "oops\n" {
		t.Errorf("stderr = %q, want %q", errBuf.String(), "oops\n")
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestSignaledExit(t *testing.T) {
	res, err := run(t, runcache.RunSpec{
		Argv:   []string{"/bin/sh", "-c", "kill -KILL $$"},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit.Kind != runcache.ExitSignaled {
		t.Fatalf("kind = %s, want signaled", res.Exit.Kind)
	}
	if res.Exit.Code != 137 {
		t.Errorf("code = %d, want 137", res.Exit.Code)
	}
	if res.Exit.Signal == "" {
		t.Error("signal name should be recorded")
	}
}

func TestStartFailure(t *testing.T) {
	_, err := run(t, runcache.RunSpec{
		Argv:   []string{"/nonexistent/definitely-not-a-binary"},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	if !errors.Is(err, runcache.ErrStartFailed) {
		t.Errorf("err = %v, want ErrStartFailed", err)
	}
}

func TestNilEnvRejected(t *testing.T) {
	// Call the runner directly (not the helper, which fills in an env) to prove a nil
	// env is refused rather than silently inheriting the parent environment.
	_, err := process.New().Run(context.Background(), runcache.RunSpec{
		Argv:   []string{"/bin/echo", "x"},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("Run with a nil env should be rejected")
	}
}

func TestResolve(t *testing.T) {
	p := process.New()
	env := os.Environ()
	path, stat, ok := p.Resolve("/bin/echo", "/", env)
	if !ok {
		t.Fatal("expected to resolve /bin/echo")
	}
	if path != "/bin/echo" {
		t.Errorf("path = %q, want /bin/echo", path)
	}
	if stat == "" {
		t.Error("stat signature should not be empty")
	}
	if _, _, ok := p.Resolve("definitely-not-a-real-binary-xyz", "/", env); ok {
		t.Error("unknown executable should not resolve")
	}
}

func TestResolveRejectsNilEnv(t *testing.T) {
	p := process.New()
	// A nil env must not resolve, even a bare name: it would otherwise read PATH from
	// awa's own environment, keying against an unkeyed env.
	if _, _, ok := p.Resolve("echo", "/", nil); ok {
		t.Error("Resolve with a nil env should return ok=false")
	}
	// An absolute path is likewise refused with nil env — the contract is symmetric
	// with Run, which rejects a nil env outright.
	if _, _, ok := p.Resolve("/bin/echo", "/", nil); ok {
		t.Error("Resolve with a nil env should return ok=false even for an absolute path")
	}
}

// TestResolveAnchorsRelativePathAtDir verifies a bare command is resolved against
// the execution directory, not awa's cwd, when PATH has a relative entry — so
// "awa run --cwd sub" picks the tool reachable from sub.
func TestResolveAnchorsRelativePathAtDir(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(bin, "mytool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH comes from the command's own env (a relative entry), not the process
	// environment — proving resolution uses the effective env and anchors relative
	// entries at the execution dir.
	env := []string{"PATH=bin"}

	p := process.New()
	path, _, ok := p.Resolve("mytool", dir, env)
	if !ok {
		t.Fatal("expected to resolve mytool from the execution dir")
	}
	if path != tool {
		t.Errorf("path = %q, want %q", path, tool)
	}
	// From an unrelated directory the same relative PATH entry resolves nothing.
	if _, _, ok := p.Resolve("mytool", t.TempDir(), env); ok {
		t.Error("mytool should not resolve from a directory without bin/mytool")
	}
}
