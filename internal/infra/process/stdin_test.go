package process_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/runcache"
	"awarer/internal/infra/process"
)

// The stdin evidence is portable: it needs no shell or platform utility, so it runs
// on every shipped platform. Each case re-executes this test binary twice. The outer
// copy is a driver whose own stdin is a pipe the test controls; it calls the runner,
// which starts a reader copy that drains its stdin and reports the byte count. The
// driver therefore exercises the runner against a real caller descriptor without this
// test process ever touching its own os.Stdin.
const (
	stdinRoleEnv  = "AWA_PROCESS_STDIN_ROLE"
	stdinModeEnv  = "AWA_PROCESS_STDIN_MODE"
	stdinHelperRe = "^TestRunnerStdinHelperProcess$"
	readMarker    = "AWA-STDIN-READ "
)

// TestRunnerStdinHelperProcess is the driver/reader entry point for the stdin cases;
// run directly it does nothing.
func TestRunnerStdinHelperProcess(t *testing.T) {
	switch os.Getenv(stdinRoleEnv) {
	case "reader":
		n, err := io.Copy(io.Discard, os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "reading stdin:", err)
			os.Exit(1)
		}
		fmt.Printf("%s%d\n", readMarker, n)
		os.Exit(0)
	case "driver":
		mode, err := runcache.ParseStdinMode(os.Getenv(stdinModeEnv))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var out, errBuf bytes.Buffer
		res, err := process.New().Run(context.Background(), runcache.RunSpec{
			Argv:   []string{os.Args[0], "-test.run=" + stdinHelperRe},
			Env:    append(os.Environ(), stdinRoleEnv+"=reader"),
			Stdin:  mode,
			Stdout: &out,
			Stderr: &errBuf,
		})
		if err != nil || res.Exit.Code != 0 {
			fmt.Fprintf(os.Stderr, "runner: err=%v exit=%+v stderr=%s\n", err, res.Exit, errBuf.String())
			os.Exit(1)
		}
		os.Stdout.Write(out.Bytes())
		os.Exit(0)
	default:
		t.Skip("helper process for the runner stdin cases")
	}
}

// TestRunnerStdin proves how the runner wires a child's stdin on this platform: null
// connects the platform null device, so the child reads EOF at once even while the
// caller's pipe holds unread bytes and stays open; pipe inherits the caller's input,
// so the child receives exactly the supplied bytes.
func TestRunnerStdin(t *testing.T) {
	const payload = "caller-input\n"
	cases := []struct {
		mode     runcache.StdinMode
		holdOpen bool
		want     int
	}{
		{runcache.StdinNull, true, 0},
		{runcache.StdinPipe, false, len(payload)},
	}
	for _, tc := range cases {
		t.Run(tc.mode.String(), func(t *testing.T) {
			t.Parallel()
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			if _, err := w.WriteString(payload); err != nil {
				t.Fatal(err)
			}
			if !tc.holdOpen {
				w.Close()
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+stdinHelperRe)
			cmd.Env = append(os.Environ(), stdinRoleEnv+"=driver", stdinModeEnv+"="+tc.mode.String())
			cmd.Stdin = r
			var out, errBuf bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errBuf
			err = cmd.Run()
			if ctx.Err() != nil {
				t.Fatalf("driver did not finish while the caller pipe stayed open (child waiting on caller input?)")
			}
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("starting driver: %v", err)
				}
				t.Fatalf("driver failed: %v\nstdout: %s\nstderr: %s", err, out.String(), errBuf.String())
			}
			got := -1
			for _, line := range strings.Split(out.String(), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), readMarker); ok {
					got, _ = strconv.Atoi(rest)
				}
			}
			if got != tc.want {
				t.Errorf("child read %d stdin bytes, want %d\nstdout: %s\nstderr: %s", got, tc.want, out.String(), errBuf.String())
			}
		})
	}
}
