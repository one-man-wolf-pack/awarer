package cli

import (
	"strings"
	"testing"
)

// topLevelMarker is a line unique to the top-level usage screen, used to prove a
// command-specific --help did NOT fall back to it.
const topLevelMarker = "awa [global options] <command>"

// commandHelpCase is one "<command> --help" expectation: the argv to run and the
// title line the page must lead with.
type commandHelpCase struct {
	name      string   // subtest label
	args      []string // argv passed to run(...)
	wantTitle string   // expected leading line
}

// commandHelpCases derives one case per command and subcommand from the same
// single source production dispatches from — the router registry and each
// command's own subcommands — so a newly added command or subcommand is covered
// automatically instead of needing a parallel hand-maintained list that could
// silently fall out of sync.
func commandHelpCases() []commandHelpCase {
	var cases []commandHelpCase
	for _, c := range newRouter().commands {
		cases = append(cases, commandHelpCase{c.name, []string{c.name, "--help"}, "awa " + c.name})
		for _, sc := range c.subcommands {
			label := c.name + " " + sc.name
			cases = append(cases, commandHelpCase{label, []string{c.name, sc.name, "--help"}, "awa " + label})
		}
	}
	return cases
}

// Every command and run subcommand has truthful command-specific --help that names
// that command's syntax and does not fall back to the top-level usage screen.
func TestCommandHelpIsCommandSpecific(t *testing.T) {
	for _, tc := range commandHelpCases() {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(tc.args...)
			if code != int(ExitSuccess) {
				t.Fatalf("%v exit = %d, want %d; stderr=%q", tc.args, code, ExitSuccess, stderr)
			}
			if stderr != "" {
				t.Errorf("%v stderr = %q, want empty", tc.args, stderr)
			}
			if !strings.HasPrefix(stdout, tc.wantTitle) {
				t.Errorf("%v: stdout should start with %q\n%s", tc.args, tc.wantTitle, stdout)
			}
			if strings.Contains(stdout, topLevelMarker) {
				t.Errorf("%v: --help fell back to top-level usage\n%s", tc.args, stdout)
			}
			if !strings.Contains(stdout, "usage:") {
				t.Errorf("%v: help missing a usage section\n%s", tc.args, stdout)
			}
		})
	}
}

// TestCommandHelpMetadataComplete is a structural guard: every registry command
// has authored help (usage + long). Subcommands with authored help (usage set)
// must also carry a long line; a subcommand may still rely on thin
// summary-derived help, so an empty help is allowed but a half-authored one is
// not.
func TestCommandHelpMetadataComplete(t *testing.T) {
	for _, c := range newRouter().commands {
		if len(c.help.usage) == 0 {
			t.Errorf("command %q has no help.usage", c.name)
		}
		if strings.TrimSpace(c.help.long) == "" {
			t.Errorf("command %q has no help.long", c.name)
		}
		for _, sc := range c.subcommands {
			// A subcommand may rely on thin summary-derived help (no authored usage);
			// but once it authors help it must be complete — a long line and at least
			// one flag or example — so a half-filled page cannot ship.
			if len(sc.help.usage) == 0 {
				continue
			}
			if strings.TrimSpace(sc.help.long) == "" || (len(sc.help.flags) == 0 && len(sc.help.examples) == 0) {
				t.Errorf("%s subcommand %q has incomplete authored help", c.name, sc.name)
			}
		}
	}
}

// TestCommandHelpNoForbiddenFlags pins that command help never advertises a
// removed flag (extends the top-level guard
// TestHelp_DoesNotAdvertiseRemovedFlags).
func TestCommandHelpNoForbiddenFlags(t *testing.T) {
	forbidden := []string{"--quiet", "--verbose", "--color", "--no-color"}
	for _, tc := range commandHelpCases() {
		_, stdout, _ := run(tc.args...)
		for _, bad := range forbidden {
			if strings.Contains(stdout, bad) {
				t.Errorf("%v help must not mention %q\n%s", tc.args, bad, stdout)
			}
		}
	}
}

// TestCommandHelpJSONMatchesCapabilities pins that "supports --json" appears in a
// command's help iff the command actually accepts --json, so the derived line can
// never drift from the validator.
func TestCommandHelpJSONMatchesCapabilities(t *testing.T) {
	for _, c := range newRouter().commands {
		if c.ownsHelp {
			continue // run/config render their own help; covered elsewhere
		}
		_, stdout, _ := run(c.name, "--help")
		says := strings.Contains(stdout, "supports --json")
		accepts := hasCap(c.capabilities, capJSON)
		if says != accepts {
			t.Errorf("command %q: help says --json=%v but capability=%v\n%s", c.name, says, accepts, stdout)
		}
	}
}

// TestCommandRegistryIsValid guards the top-level registry the same way
// TestSubcommandRegistriesAreValid guards the nested one: every shipped command
// has a dispatch handler and a non-empty summary, and names are unique so a
// duplicate cannot shadow another entry. A handlerless command is a
// construction defect — route calls cmd.run unconditionally — not a lifecycle
// state production has to model, so this is where that defect is caught.
func TestCommandRegistryIsValid(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range newRouter().commands {
		if strings.TrimSpace(c.name) == "" {
			t.Error("registry has a command with no name")
			continue
		}
		if c.run == nil {
			t.Errorf("command %q has no dispatch handler", c.name)
		}
		if strings.TrimSpace(c.summary) == "" {
			t.Errorf("command %q has no summary", c.name)
		}
		if seen[c.name] {
			t.Errorf("registry has a duplicate command %q", c.name)
		}
		seen[c.name] = true
	}
}

// TestSubcommandRegistriesAreValid guards every command's owned subcommand set:
// each subcommand has a dispatch handler and a non-empty summary, and names are
// unique within a command so a duplicate cannot silently shadow another entry.
func TestSubcommandRegistriesAreValid(t *testing.T) {
	for _, c := range newRouter().commands {
		seen := map[string]bool{}
		for _, sc := range c.subcommands {
			if sc.run == nil {
				t.Errorf("%s subcommand %q has no dispatch handler", c.name, sc.name)
			}
			if strings.TrimSpace(sc.summary) == "" {
				t.Errorf("%s subcommand %q has no summary", c.name, sc.name)
			}
			if seen[sc.name] {
				t.Errorf("%s has a duplicate subcommand %q", c.name, sc.name)
			}
			seen[sc.name] = true
		}
	}
}

// TestRunHelpListsSubcommands pins that "awa run --help" lists every run
// subcommand, so a new subcommand cannot ship undiscoverable from the wrapper help.
func TestRunHelpListsSubcommands(t *testing.T) {
	_, stdout, _ := run("run", "--help")
	for _, sc := range runSubcommands {
		if !strings.Contains(stdout, sc.name) {
			t.Errorf("awa run --help does not list subcommand %q\n%s", sc.name, stdout)
		}
	}
}

// TestRunGlobalBeforeChildIsNotAmbiguous pins that an awa-global flag before the
// wrapped child (short form or operand form) is treated as awa's, not flagged as
// ambiguous — the check must not false-positive. Outside a project these proceed
// to project resolution (ExitNotFound), never a usage error.
func TestRunGlobalBeforeChildIsNotAmbiguous(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--json", "mytool"},
		{"run", "--json", "--", "mytool"},
	} {
		code, _, stderr := run(args...)
		if code == int(ExitUsageError) {
			t.Errorf("%v exit = usage error, want it to proceed past the ambiguity check; stderr=%q", args, stderr)
		}
		if strings.Contains(stderr, "ambiguous flag") {
			t.Errorf("%v wrongly flagged as ambiguous: %q", args, stderr)
		}
	}
}

// TestRunHelpBeforeChildShowsRunHelp pins that "--help" before the wrapped command
// (an awa flag position) renders run's own help rather than erroring.
func TestRunHelpBeforeChildShowsRunHelp(t *testing.T) {
	code, stdout, stderr := run("run", "--help", "mytool")
	if code != int(ExitSuccess) {
		t.Fatalf("exit = %d, want success; stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "awa run") {
		t.Errorf("expected run's own help, got:\n%s", stdout)
	}
}

// For run explain, its --help must name the input/policy flags it actually accepts
// in command mode, and must NOT advertise the execution-only flags (--record,
// --display) it deliberately rejects. Without this, the earlier drift (explain help
// omitting implemented flags) could silently return.
func TestRunExplainHelpListsCommandModeFlags(t *testing.T) {
	_, stdout, _ := run("run", "explain", "--help")
	for _, want := range []string{
		"--scope", "--include", "--exclude", "--cwd",
		"--refresh", "--no-cache", "--no-cache-failures", "--allow-skipped-inputs", "--allow-tty",
		"--last", "--from-run", "--to-now",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run explain --help missing implemented flag %q\n%s", want, stdout)
		}
	}
	for _, absent := range []string{"--record", "--display"} {
		if strings.Contains(stdout, absent) {
			t.Errorf("run explain --help advertises execution-only flag %q it does not accept\n%s", absent, stdout)
		}
	}
}

// TestRunExplainHelpNotFalselyAmbiguous pins that the explain wrapper-help boundary
// check fires only when a global was stolen from after a wrapped child — never for a
// bare "run explain --help", a stored-run mode, or a --help placed before the child.
func TestRunExplainHelpNotFalselyAmbiguous(t *testing.T) {
	for _, args := range [][]string{
		{"run", "explain", "--help"},           // bare: explain's own help
		{"run", "explain", "--last", "--help"}, // stored-run mode, no child
		{"run", "explain", "--help", "mytool"}, // --help before the child boundary
	} {
		code, stdout, stderr := run(args...)
		if code != int(ExitSuccess) {
			t.Errorf("%v exit = %d, want success; stderr=%q", args, code, stderr)
		}
		if !strings.HasPrefix(stdout, "awa run explain") {
			t.Errorf("%v: expected run explain help, got:\n%s", args, stdout)
		}
	}
}
