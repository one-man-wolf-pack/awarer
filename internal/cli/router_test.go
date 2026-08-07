package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// run invokes Run with captured streams so tests can assert on exit code and on
// stdout/stderr separately.
func run(args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = Run(context.Background(), args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestRun_Routing(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		// wantOut/wantErr, when set, must appear on the respective stream.
		wantOut string
		wantErr string
		// emptyOut/emptyErr assert the stream is empty.
		emptyOut bool
		emptyErr bool
	}{
		{
			name:     "version on stdout",
			args:     []string{"version"},
			wantCode: int(ExitSuccess),
			wantOut:  "awa 0.0.0-dev",
			emptyErr: true,
		},
		{
			name:     "version json envelope",
			args:     []string{"version", "--json"},
			wantCode: int(ExitSuccess),
			wantOut:  `"schema_version": 1`,
			emptyErr: true,
		},
		{
			name:     "long help flag",
			args:     []string{"--help"},
			wantCode: int(ExitSuccess),
			wantOut:  "usage:",
			emptyErr: true,
		},
		{
			name:     "short help flag",
			args:     []string{"-h"},
			wantCode: int(ExitSuccess),
			wantOut:  "usage:",
			emptyErr: true,
		},
		{
			name:     "unknown command is usage error",
			args:     []string{"bogus"},
			wantCode: int(ExitUsageError),
			wantErr:  `unknown command "bogus"`,
			emptyOut: true,
		},
		{
			name:     "unknown flag is usage error",
			args:     []string{"--bogus", "version"},
			wantCode: int(ExitUsageError),
			wantErr:  "unknown flag",
			emptyOut: true,
		},
		{
			name:     "removed reserved flag --color is an unknown flag",
			args:     []string{"--color", "purple", "version"},
			wantCode: int(ExitUsageError),
			wantErr:  "unknown flag",
			emptyOut: true,
		},
		{
			name:     "invalid trust-mode value is usage error",
			args:     []string{"--trust-mode", "paranoid", "version"},
			wantCode: int(ExitUsageError),
			wantErr:  "invalid --trust-mode",
			emptyOut: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(tt.args...)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if tt.wantOut != "" && !strings.Contains(stdout, tt.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}
			if tt.emptyOut && stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if tt.emptyErr && stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}
		})
	}
}

func TestHelp_DoesNotAdvertiseRemovedFlags(t *testing.T) {
	// The pre-release clean-up removed the never-implemented --quiet/--verbose/
	// --color flags rather than shipping them as API surface, so help must not
	// mention them at all.
	code, stdout, _ := run("--help")
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d", code)
	}
	for _, gone := range []string{"--quiet", "--verbose", "--color", "--no-color"} {
		if strings.Contains(stdout, gone) {
			t.Errorf("--help still advertises removed flag %q:\n%s", gone, stdout)
		}
	}
	// Implemented flags are still advertised.
	if !strings.Contains(stdout, "--root <path>") {
		t.Errorf("--help must still advertise --root:\n%s", stdout)
	}
}

func TestRun_FlagAfterCommand(t *testing.T) {
	// Arrange + Act: --json given after the command must still be recognized.
	code, stdout, stderr := run("version", "--json")

	// Assert
	if code != int(ExitSuccess) {
		t.Fatalf("exit code = %d, want %d", code, ExitSuccess)
	}
	if !strings.Contains(stdout, `"command": "version"`) {
		t.Errorf("stdout = %q, want json envelope", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestRun_CommandFlagRoutesToCommand(t *testing.T) {
	// "awa diff -1" must route to diff and let diff parse "-1" as its range
	// shortcut, not fail at the global parser as an unknown flag. Outside a
	// project it then fails as not-found (exit 3), not a usage error.
	code, _, stderr := run("diff", "-1")

	if code != int(ExitNotFound) {
		t.Errorf("exit code = %d, want %d", code, ExitNotFound)
	}
	if strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr = %q, command flag must not be rejected globally", stderr)
	}
}

func TestRun_VersionRejectsArguments(t *testing.T) {
	// version accepts no input; stray tokens are usage errors, not silently
	// ignored. The message must distinguish a flag from a positional argument.
	tests := []struct {
		name    string
		arg     string
		wantErr string
	}{
		{name: "positional", arg: "extra", wantErr: `unexpected argument "extra"`},
		{name: "flag", arg: "--bogus", wantErr: `unknown flag "--bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run("version", tt.arg)

			if code != int(ExitUsageError) {
				t.Errorf("exit code = %d, want %d", code, ExitUsageError)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantErr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestRun_OptionTerminatorBeforeCommandIsError(t *testing.T) {
	// "awa -- version" passes "version" as an operand after the option
	// terminator, with no command to own it. That is malformed, not a silent
	// bare invocation.
	code, stdout, stderr := run("--", "version")

	if code != int(ExitUsageError) {
		t.Errorf("exit code = %d, want %d", code, ExitUsageError)
	}
	if !strings.Contains(stderr, "expected a command") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "expected a command")
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

func TestRun_CommandBeforeTerminatorRoutes(t *testing.T) {
	// "awa run -- pytest" names a command before "--", so the operands belong to
	// it and the run handler takes over. Run from a non-project directory, it must
	// reach project resolution and fail with not-found — proving it routed past the
	// "--" terminator rather than erroring on it.
	code, _, stderr := run("run", "--", "pytest")

	if code != int(ExitNotFound) {
		t.Errorf("exit code = %d, want %d", code, ExitNotFound)
	}
	if !strings.Contains(stderr, "not an awa project") {
		t.Errorf("stderr = %q, want it to contain %q", stderr, "not an awa project")
	}
}

// failWriter fails every write, simulating a closed stdout pipe.
type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRun_StdoutWriteFailureIsNonZero(t *testing.T) {
	// A broken stdout must never yield a success exit code. Run surfaces the
	// Writer's error centrally, so this holds for every payload path without
	// each handler checking it — both the help path and a command handler.
	for _, args := range [][]string{
		{"--help"},
		{"version"},
		{"version", "--json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var errBuf bytes.Buffer
			code := Run(context.Background(), args, failWriter{}, &errBuf)

			if code == int(ExitSuccess) {
				t.Errorf("exit code = %d, want non-zero on stdout write failure", code)
			}
		})
	}
}

func TestErrorPrefixedOnStderr(t *testing.T) {
	// Act
	_, _, stderr := run("bogus")

	// Assert: user errors are prefixed so they are recognizably from awa.
	if !strings.HasPrefix(stderr, "awa: ") {
		t.Errorf("stderr = %q, want it to start with %q", stderr, "awa: ")
	}
}
