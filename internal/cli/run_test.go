package cli

import (
	"os"
	"strings"
	"testing"
)

// TestTimeFlagRejectedWithJSON pins the uniform contract that --time (a human
// display preference) cannot be combined with --json in any command that accepts it:
// JSON always emits machine UTC timestamps, so --time would otherwise silently no-op.
// The check fires before project resolution, so no scaffolded project is needed.
func TestTimeFlagRejectedWithJSON(t *testing.T) {
	cases := [][]string{
		{"log", "--json", "--time", "relative"},
		{"run", "log", "--json", "--time", "relative"},
		{"run", "ls", "--json", "--time", "relative"},
		{"run", "show", "--last", "--json", "--time", "relative"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := run(args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d (usage); stderr=%q", code, ExitUsageError, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty (no JSON emitted)", stdout)
			}
			if !strings.Contains(stderr, "human time display") {
				t.Errorf("stderr = %q, want the shared --time/--json message", stderr)
			}
		})
	}
}

// TestRunUsageErrors covers the run command's parse-time usage errors, which are
// reported before any project is resolved, so they need no scaffolded project.
func TestRunUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing command", []string{"run"}, "requires a command"},
		{"missing command after terminator", []string{"run", "--"}, "requires a command"},
		{"unknown run flag", []string{"run", "--bogus", "--", "echo"}, "unknown flag"},
		{"unknown short-form flag", []string{"run", "-x"}, "use -- to pass it to the command"},
		{"no-cache with refresh", []string{"run", "--no-cache", "--refresh", "--", "echo"}, "cannot be combined"},
		{"stray token before terminator", []string{"run", "foo", "--", "echo"}, "before --"},
		{"show without id", []string{"run", "show"}, "requires a run id"},
		{"show extra id", []string{"run", "show", "a", "b"}, "single run id"},
		{"show json with stdout", []string{"run", "show", "--json", "--stdout", "abc"}, "cannot be combined with --stdout"},
		{"show meta with stdout", []string{"run", "show", "--meta", "--stdout", "abc"}, "mutually exclusive"},
		{"show stdout with stderr", []string{"run", "show", "--stdout", "--stderr", "abc"}, "mutually exclusive"},
		{"show meta with tail", []string{"run", "show", "--meta", "--tail", "5", "abc"}, "cannot be combined with --tail"},
		{"show id and last", []string{"run", "show", "--last", "abc"}, "either a run id or --last"},
		{"show no id no last", []string{"run", "show", "--tail", "5"}, "requires a run id or --last"},
		{"show invalid tail", []string{"run", "show", "--tail", "x", "abc"}, "positive integer"},
		{"show invalid grep", []string{"run", "show", "--grep", "(", "abc"}, "invalid grep pattern"},
		{"show time with json", []string{"run", "show", "--json", "--time", "relative", "--last"}, "human time display"},
		{"show stdout invalid time", []string{"run", "show", "--stdout", "--time", "bogus", "abc"}, "time"},
		{"show filtered invalid time", []string{"run", "show", "--tail", "5", "--time", "bogus", "abc"}, "time"},
		{"display invalid mode", []string{"run", "--display", "bogus", "--", "echo"}, "invalid display mode"},
		{"display invalid tail", []string{"run", "--display", "tail:0", "--", "echo"}, "positive integer"},
		{"rm without target", []string{"run", "rm"}, "requires a run id or a filter"},
		{"log bad limit", []string{"run", "log", "-n", "abc"}, "positive integer"},
		{"log unknown flag", []string{"run", "log", "--bogus"}, "unknown flag"},
		{"ls bad limit", []string{"run", "ls", "-n", "abc"}, "positive integer"},
		{"ls unknown flag", []string{"run", "ls", "--bogus"}, "unknown flag"},
		{"ls all with value", []string{"run", "ls", "--all=yes"}, "takes no value"},
		// An awa-global flag after the wrapped child command in short form is
		// ambiguous — it must fail loudly and point at "--", not be silently stolen.
		{"child json short form", []string{"run", "mytool", "--json"}, "ambiguous flag"},
		{"child help short form", []string{"run", "mytool", "--help"}, "ambiguous flag"},
		{"child root short form", []string{"run", "mytool", "--root", "/x"}, "ambiguous flag"},
		{"child json after run-local", []string{"run", "--refresh", "mytool", "--json"}, "ambiguous flag"},
		{"explain child json short form", []string{"run", "explain", "mycmd", "--json"}, "ambiguous flag"},
		// --help after the explain-wrapped child is ambiguous too: it must not silently
		// show explain's help when it may have been meant for the wrapped command.
		{"explain child help short form", []string{"run", "explain", "mycmd", "--help"}, "ambiguous flag"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(tc.args...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d (usage)\nstderr: %s", code, ExitUsageError, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want to contain %q", stderr, tc.wantErr)
			}
		})
	}
}

// TestRunStdinSelectionUsageErrors pins the --stdin contract shared by run and
// command-mode explain: only null is accepted, it conflicts with --allow-tty in either
// order, and the stored-run explain modes reject it even though null is an existing
// mode. Each failure must be a usage error raised before configuration is read, so
// every case runs both outside any project and against a project whose config is
// invalid — either would surface as a different failure if config were reached first.
func TestRunStdinSelectionUsageErrors(t *testing.T) {
	broken := initProject(t)
	writeFile(t, broken, ".awa/config.toml", "this is not toml [\n")
	// Control: a valid explicit selection does reach configuration and fails there,
	// so a usage exit below cannot be the config failure in disguise.
	if code, _, stderr := run("--root", broken, "run", "explain", "--stdin", "null", "--", "echo"); code == int(ExitUsageError) || code == 0 {
		t.Fatalf("control exit = %d, want a configuration failure; stderr=%q", code, stderr)
	}
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing value", []string{"run", "--stdin"}, "requires a value"},
		{"empty inline value", []string{"run", "--stdin=", "--", "echo"}, "--stdin"},
		{"persisted pipe token", []string{"run", "--stdin", "pipe", "--", "echo"}, "only null is accepted"},
		{"persisted tty token", []string{"run", "--stdin=tty", "--", "echo"}, "only null is accepted"},
		{"case variant", []string{"run", "--stdin", "NULL", "--", "echo"}, "only null is accepted"},
		{"null then allow-tty", []string{"run", "--stdin", "null", "--allow-tty", "--", "echo"}, "cannot be combined"},
		{"allow-tty then null", []string{"run", "--allow-tty", "--stdin", "null", "--", "echo"}, "cannot be combined"},
		{"explain unsupported value", []string{"run", "explain", "--stdin", "pipe", "--", "echo"}, "only null is accepted"},
		{"explain null then allow-tty", []string{"run", "explain", "--stdin=null", "--allow-tty", "--", "echo"}, "cannot be combined"},
		{"explain allow-tty then null", []string{"run", "explain", "--allow-tty", "--stdin=null", "--", "echo"}, "cannot be combined"},
		{"explain last override", []string{"run", "explain", "--last", "--stdin", "null"}, "not valid with --last or --from-run"},
		{"explain from-run override", []string{"run", "explain", "--from-run", "abc", "--to-now", "--stdin", "null"}, "not valid with --last or --from-run"},
	}
	for _, tc := range tests {
		for _, where := range []struct {
			name   string
			prefix []string
		}{{"nonproject", nil}, {"invalid config", []string{"--root", broken}}} {
			t.Run(tc.name+"/"+where.name, func(t *testing.T) {
				args := append(append([]string{}, where.prefix...), tc.args...)
				code, stdout, stderr := run(args...)
				if code != int(ExitUsageError) {
					t.Errorf("exit = %d, want %d (usage)\nstderr: %s", code, ExitUsageError, stderr)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want empty", stdout)
				}
				if !strings.Contains(stderr, tc.wantErr) {
					t.Errorf("stderr = %q, want to contain %q", stderr, tc.wantErr)
				}
			})
		}
	}
}

// TestRunStdinTokensAfterTerminatorReachTheChild proves the mandatory "--" keeps a
// child's own --stdin tokens literal: they are neither parsed nor validated by awa.
func TestRunStdinTokensAfterTerminatorReachTheChild(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("requires /bin/echo")
	}
	root := initProject(t)

	code, stdout, stderr := run("run", "--root", root, "--cwd", root, "--stdin", "null", "--", "/bin/echo", "--stdin", "pipe", "--allow-tty")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.TrimSpace(stdout) != "--stdin pipe --allow-tty" {
		t.Errorf("child stdout = %q, want its literal argv", stdout)
	}
}

// TestRunWritesToProvidedStreams verifies the run command streams a wrapped
// command's output — on both a miss and a hit replay — to the writers passed to
// cli.Run, never to the process globals. Run with --root/--cwd so no chdir is
// needed; the command executes for real, so it requires /bin/echo.
func TestRunWritesToProvidedStreams(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("requires /bin/echo")
	}
	root := initProject(t)

	// Miss: the command's stdout must land in the provided stdout buffer.
	code, stdout, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/echo", "hello-streams")
	if code != 0 {
		t.Fatalf("first run exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "hello-streams") {
		t.Errorf("miss: command stdout = %q, want it to contain hello-streams (leaked to os.Stdout?)", stdout)
	}

	// Hit: the replayed output must use the provided stdout buffer too.
	code, stdout, stderr = run("run", "--root", root, "--cwd", root, "--", "/bin/echo", "hello-streams")
	if code != 0 {
		t.Fatalf("second run exit = %d", code)
	}
	if !strings.Contains(stderr, "awa run: hit,") {
		t.Fatalf("expected a hit, stderr = %q", stderr)
	}
	if !strings.Contains(stdout, "hello-streams") {
		t.Errorf("hit: replayed stdout = %q, want it to contain hello-streams (leaked to os.Stdout?)", stdout)
	}
}

// TestRunPassesGlobalLookingFlagsToChildAfterTerminator proves the "--" boundary keeps
// awa-global-looking flags opaque: after "--", tokens like --json/--root are the wrapped
// command's argv verbatim, never reinterpreted as awa flags, so they reach the child and
// awa does not emit its own JSON envelope.
func TestRunPassesGlobalLookingFlagsToChildAfterTerminator(t *testing.T) {
	if _, err := os.Stat("/bin/echo"); err != nil {
		t.Skip("requires /bin/echo")
	}
	root := initProject(t)

	code, stdout, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/echo", "--json", "--root", "payload")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	// The child echoed its argv unchanged, including the global-looking flags.
	for _, want := range []string{"--json", "--root", "payload"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("child stdout = %q, want it to contain %q (flag stolen from the child?)", stdout, want)
		}
	}
	// awa did not turn --json on for itself: stdout is the child's echo, not a JSON envelope.
	if strings.Contains(stdout, "\"schema_version\"") {
		t.Errorf("stdout = %q, want the child echo, not an awa JSON envelope (--json leaked to awa)", stdout)
	}
}

// TestRunCapabilityRejection verifies the run command rejects global options it
// does not act on, via the central capability check.
func TestRunCapabilityRejection(t *testing.T) {
	code, _, stderr := run("run", "--quiet", "--", "echo")
	if code != int(ExitUsageError) {
		t.Errorf("exit = %d, want usage", code)
	}
	if !strings.Contains(stderr, "--quiet") {
		t.Errorf("stderr = %q, want mention of --quiet", stderr)
	}
}
