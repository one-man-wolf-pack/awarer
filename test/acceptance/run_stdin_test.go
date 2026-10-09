package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stdinPayload is the caller input the stdin scenarios offer awa. It is far smaller
// than any platform pipe buffer, so writing it never blocks the test.
const stdinPayload = "caller-input\n"

// awaWithStdin runs the built binary in dir with a pipe carrying stdinPayload as its
// stdin. When holdOpen is true the write end stays open until awa has exited, so a
// child that inherited the pipe could only finish by never reading it; the wait is
// bounded so such a regression fails instead of hanging. When false the write end is
// closed before awa starts and an inheriting child reads the payload then EOF.
func awaWithStdin(t *testing.T, dir string, holdOpen bool, args ...string) (int, string, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := w.WriteString(stdinPayload); err != nil {
		t.Fatal(err)
	}
	if !holdOpen {
		w.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, awaBinary(), args...)
	cmd.Dir = dir
	cmd.Stdin = r
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("awa %v did not finish while its stdin stayed open (child waiting on caller input?)\nstderr: %s", args, errBuf.String())
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running awa %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return code, out.String(), errBuf.String()
}

// runCacheEnvelope is the subset of the run --json envelope these scenarios check.
type runCacheEnvelope struct {
	Command string `json:"command"`
	Data    struct {
		Cache struct {
			Status string `json:"status"`
			Key    string `json:"key"`
			Reason string `json:"reason"`
		} `json:"cache"`
		Run struct {
			Reuse string `json:"reuse"`
		} `json:"run"`
	} `json:"data"`
}

// explainEnvelope is the subset of the run explain --json envelope checked here.
type explainEnvelope struct {
	Data struct {
		Outcome string `json:"outcome"`
		Current struct {
			Key        string `json:"key"`
			StdinMode  string `json:"stdin_mode"`
			TTYAllowed bool   `json:"tty_allowed"`
		} `json:"current"`
	} `json:"data"`
}

func decodeJSON[T any](t *testing.T, label, stdout string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Fatalf("%s: stdout is not one JSON document: %v\n%s", label, err, stdout)
	}
	return v
}

// TestRunExplicitNullStdinGivesChildEOF proves the delivered behavior end to end: with
// nonempty caller input whose pipe stays open, --stdin null still lets the child see
// EOF at once and finish, while the ordinary inherited-input control receives the
// supplied bytes and is not reusable because they are unkeyed.
func TestRunExplicitNullStdinGivesChildEOF(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	code, stdout, stderr := awaWithStdin(t, root, true, "run", "--stdin", "null", "--", h, "-read-stdin")
	if code != 0 || stdout != "STDIN\t0\n" {
		t.Fatalf("explicit null: exit %d stdout %q (want EOF with 0 bytes)\nstderr: %s", code, stdout, stderr)
	}

	code, stdout, stderr = awaWithStdin(t, root, false, "run", "--", h, "-read-stdin")
	want := "STDIN\t" + strconv.Itoa(len(stdinPayload)) + "\n"
	if code != 0 || stdout != want {
		t.Fatalf("inherited control: exit %d stdout %q, want %q\nstderr: %s", code, stdout, want, stderr)
	}
	if !strings.Contains(stderr, "stdin-not-keyed") {
		t.Errorf("inherited control should be non-reusable (stdin-not-keyed), stderr = %q", stderr)
	}
}

// TestRunExplicitNullStdinSharesNullIdentity proves explicit null is the same
// effective identity as an ordinary invocation already connected to the null device
// (the harness's default stdin): equal keys, and reuse across the two forms in both
// directions, even though the explicit form's caller stdin is a live pipe.
func TestRunExplicitNullStdinSharesNullIdentity(t *testing.T) {
	h := helper(t)

	t.Run("ordinary then explicit", func(t *testing.T) {
		root := initProject(t)
		_, out, stderr := awa(t, root, "run", "--json", "--", h, "-out", "same")
		first := decodeJSON[runCacheEnvelope](t, "ordinary run", out)
		if first.Data.Cache.Status != "miss" || first.Data.Cache.Reason != "" {
			t.Fatalf("ordinary run = %+v, want a reusable miss\nstderr: %s", first.Data.Cache, stderr)
		}
		_, out, stderr = awaWithStdin(t, root, true, "run", "--json", "--stdin", "null", "--", h, "-out", "same")
		second := decodeJSON[runCacheEnvelope](t, "explicit run", out)
		if second.Data.Cache.Status != "hit" || second.Data.Cache.Key != first.Data.Cache.Key {
			t.Errorf("explicit run = %+v, want a hit on key %s\nstderr: %s", second.Data.Cache, first.Data.Cache.Key, stderr)
		}
	})

	t.Run("explicit then ordinary", func(t *testing.T) {
		root := initProject(t)
		_, out, stderr := awaWithStdin(t, root, true, "run", "--json", "--stdin", "null", "--", h, "-out", "same")
		first := decodeJSON[runCacheEnvelope](t, "explicit run", out)
		if first.Data.Cache.Status != "miss" || first.Data.Cache.Reason != "" {
			t.Fatalf("explicit run = %+v, want a reusable miss\nstderr: %s", first.Data.Cache, stderr)
		}
		_, out, stderr = awa(t, root, "run", "--json", "--", h, "-out", "same")
		second := decodeJSON[runCacheEnvelope](t, "ordinary run", out)
		if second.Data.Cache.Status != "hit" || second.Data.Cache.Key != first.Data.Cache.Key {
			t.Errorf("ordinary run = %+v, want a hit on key %s\nstderr: %s", second.Data.Cache, first.Data.Cache.Key, stderr)
		}
	})
}

// TestRunExplainExplicitNullStdinMatchesRun proves command-mode explain applies the
// same selection: it models null/no-TTY with the ordinary null-device key, executes
// nothing, and publishes nothing.
func TestRunExplainExplicitNullStdinMatchesRun(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	child := []string{"--", h, "-fix", "marker.txt", "-content", "x"}

	_, out, stderr := awa(t, root, append([]string{"run", "explain", "--json"}, child...)...)
	ordinary := decodeJSON[explainEnvelope](t, "ordinary explain", out)
	_, out, stderr2 := awaWithStdin(t, root, true, append([]string{"run", "explain", "--json", "--stdin", "null"}, child...)...)
	explicit := decodeJSON[explainEnvelope](t, "explicit explain", out)

	if explicit.Data.Current.StdinMode != "null" || explicit.Data.Current.TTYAllowed {
		t.Errorf("explicit explain stdin = %q tty=%v, want null/false\nstderr: %s", explicit.Data.Current.StdinMode, explicit.Data.Current.TTYAllowed, stderr2)
	}
	if explicit.Data.Current.Key == "" || explicit.Data.Current.Key != ordinary.Data.Current.Key {
		t.Errorf("explicit key %q != ordinary key %q\nstderr: %s", explicit.Data.Current.Key, ordinary.Data.Current.Key, stderr)
	}
	if explicit.Data.Outcome != ordinary.Data.Outcome {
		t.Errorf("explicit outcome %q != ordinary outcome %q", explicit.Data.Outcome, ordinary.Data.Outcome)
	}

	// The same pipe without the flag is modeled as inherited, unkeyed input.
	_, out, _ = awaWithStdin(t, root, true, append([]string{"run", "explain", "--json"}, child...)...)
	piped := decodeJSON[explainEnvelope](t, "piped explain", out)
	if piped.Data.Current.StdinMode != "pipe" {
		t.Errorf("piped explain stdin = %q, want pipe", piped.Data.Current.StdinMode)
	}

	if _, err := os.Stat(filepath.Join(root, "marker.txt")); !os.IsNotExist(err) {
		t.Errorf("explain executed the command: marker stat err = %v", err)
	}
	_, logOut, _ := awa(t, root, "run", "log", "--json")
	var log struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(logOut), &log); err != nil {
		t.Fatalf("run log --json: %v\n%s", err, logOut)
	}
	if len(log.Data) != 0 {
		t.Errorf("explain published %d run(s), want none", len(log.Data))
	}

	// Explain's key is the key a real explicit-null run uses.
	_, out, _ = awaWithStdin(t, root, true, append([]string{"run", "--json", "--stdin", "null"}, child...)...)
	ran := decodeJSON[runCacheEnvelope](t, "explicit run", out)
	if ran.Data.Cache.Key != explicit.Data.Current.Key {
		t.Errorf("run key %q != explain key %q", ran.Data.Cache.Key, explicit.Data.Current.Key)
	}
}

// TestRunExplicitNullStdinKeepsRecordOnly proves the selection overrides no other
// admission condition: --record stays record-only, and JSON stdout carries only the
// existing envelope, never the child's bytes.
func TestRunExplicitNullStdinKeepsRecordOnly(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	code, out, stderr := awaWithStdin(t, root, true, "run", "--record", "--json", "--stdin", "null", "--", h, "-read-stdin", "-out", "child-bytes")
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr)
	}
	// The child's argv is echoed in the envelope, but its raw output line (with a
	// literal tab JSON would escape) must not be.
	if strings.Contains(out, "STDIN\t0") {
		t.Errorf("JSON stdout carries child output: %q", out)
	}
	env := decodeJSON[runCacheEnvelope](t, "record run", out)
	if env.Command != "run" || env.Data.Run.Reuse != "record-only" || env.Data.Cache.Status == "hit" {
		t.Errorf("record run = %+v, want a record-only run envelope", env)
	}
	if _, out, _ = awaWithStdin(t, root, true, "run", "--json", "--stdin", "null", "--", h, "-read-stdin", "-out", "child-bytes"); decodeJSON[runCacheEnvelope](t, "follow-up", out).Data.Cache.Status == "hit" {
		t.Error("a record-only run must never be offered as a hit")
	}
}
