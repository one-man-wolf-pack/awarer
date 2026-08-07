package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// awaEnv runs the built binary in dir with extra environment entries, returning exit
// code, stdout, and stderr. It is the env-aware sibling of awa, used to prove the
// run-cache keys an allowlisted variable's value without persisting it raw.
func awaEnv(t *testing.T, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(awaBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running awa %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errBuf.String()
}

// grepTree reports every file under dir whose bytes contain needle, so a test can prove
// a secret never lands anywhere in the durable store.
func grepTree(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil // a transient/permission read is not the subject of this test
		}
		if strings.Contains(string(b), needle) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return hits
}

// The production-path privacy proof: an allowlisted environment variable
// participates in the cache key (as a redacted identity), but its raw value must
// never be written into durable run metadata. It runs a real child with a
// secret-bearing allowlisted variable, then greps the entire .awa tree for the
// secret and requires zero hits.
func TestRunEnvAllowlistDoesNotPersistRawSecret(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	// Allowlist a custom variable so its value reaches the child and is keyed.
	write(t, root, filepath.Join(".awa", "config.toml"), "[run]\nenv_allowlist = [\"AWA_TEST_SECRET\"]\n")

	const secret = "s3cr3t-do-not-persist-9f8a7b6c5d4e"
	code, _, stderr := awaEnv(t, root, []string{"AWA_TEST_SECRET=" + secret}, "run", "--", h, "-out", "ok")
	if code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}

	// The secret keyed the run, but its raw value must not be recoverable from disk.
	if hits := grepTree(t, filepath.Join(root, ".awa"), secret); len(hits) != 0 {
		t.Errorf("raw allowlisted secret found on disk in %v; env values must be keyed as redacted identities, never persisted raw", hits)
	}

	// A second identical run (same secret) must hit: the value's identity is keyed even
	// though the raw value is not stored, so reuse still works.
	code2, _, stderr2 := awaEnv(t, root, []string{"AWA_TEST_SECRET=" + secret}, "run", "--", h, "-out", "ok")
	if code2 != 0 {
		t.Fatalf("second run exit = %d, stderr = %q", code2, stderr2)
	}
	if !isHit(stderr2) {
		t.Errorf("second run with the same allowlisted value should hit; stderr=%q", stderr2)
	}
}

// The path-safety proof at the CLI edge: an absolute
// --cwd escapes the project scope the cache key is computed over, so it must be refused
// with a usage error rather than silently keyed. The domain enforces this; this pins it
// through the real binary.
func TestRunAbsoluteCWDRejected(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	code, stdout, stderr := awa(t, root, "run", "--cwd", "/tmp", "--", h, "-out", "ok")
	if code == 0 {
		t.Fatalf("run with an absolute --cwd exited 0; stdout=%q stderr=%q", stdout, stderr)
	}
	if code != 2 { // ExitUsageError
		t.Errorf("absolute --cwd should be a usage error (exit 2), got %d; stderr=%q", code, stderr)
	}
}
