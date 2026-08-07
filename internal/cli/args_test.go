package cli

import (
	"testing"
)

func TestParse_CommandAndArgs(t *testing.T) {
	// Act
	inv, err := parse([]string{"changes", "before..now"})

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.command != "changes" {
		t.Errorf("command = %q, want %q", inv.command, "changes")
	}
	if len(inv.args) != 1 || inv.args[0] != "before..now" {
		t.Errorf("args = %v, want [before..now]", inv.args)
	}
}

func TestParse_GlobalFlagsAnyPosition(t *testing.T) {
	// Act: flags before and after the command, with both value forms.
	inv, err := parse([]string{"--root", "/repo", "diff", "--config=/c.toml", "@-2..@-1", "--json"})

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.command != "diff" {
		t.Errorf("command = %q, want %q", inv.command, "diff")
	}
	if inv.options.Root != "/repo" {
		t.Errorf("Root = %q, want %q", inv.options.Root, "/repo")
	}
	if inv.options.Config != "/c.toml" {
		t.Errorf("Config = %q, want %q", inv.options.Config, "/c.toml")
	}
	if !inv.options.JSON {
		t.Errorf("JSON = false, want true")
	}
	if len(inv.args) != 1 || inv.args[0] != "@-2..@-1" {
		t.Errorf("args = %v, want [@-2..@-1]", inv.args)
	}
}

func TestParse_StrictShortcut(t *testing.T) {
	inv, err := parse([]string{"--strict", "status"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.options.TrustMode != TrustStrict {
		t.Errorf("TrustMode = %v, want TrustStrict", inv.options.TrustMode)
	}
}

func TestParse_Enums(t *testing.T) {
	inv, err := parse([]string{"--trust-mode", "fast", "status"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.options.TrustMode != TrustFast {
		t.Errorf("TrustMode = %v, want TrustFast", inv.options.TrustMode)
	}
}

func TestParse_DoubleDashStopsFlagParsing(t *testing.T) {
	// Act: tokens after "--" are literal operands, even if they look like flags,
	// and are kept separate from args so no command parser re-reads them.
	inv, err := parse([]string{"run", "--", "--json", "ls"})

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.command != "run" {
		t.Errorf("command = %q, want %q", inv.command, "run")
	}
	if inv.options.JSON {
		t.Errorf("JSON = true, want false (after -- it is a literal operand)")
	}
	if len(inv.args) != 0 {
		t.Errorf("args = %v, want empty (post-terminator tokens are operands)", inv.args)
	}
	want := []string{"--json", "ls"}
	if len(inv.operands) != len(want) || inv.operands[0] != want[0] || inv.operands[1] != want[1] {
		t.Errorf("operands = %v, want %v", inv.operands, want)
	}
}

func TestParse_CommandFlagsPassThrough(t *testing.T) {
	// Act: unknown flags after the command belong to the command, not the
	// global parser. The diff numeric shortcut from the RFC is the headline case.
	inv, err := parse([]string{"diff", "-1", "--stat"})

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inv.command != "diff" {
		t.Errorf("command = %q, want %q", inv.command, "diff")
	}
	want := []string{"-1", "--stat"}
	if len(inv.args) != len(want) || inv.args[0] != want[0] || inv.args[1] != want[1] {
		t.Errorf("args = %v, want %v", inv.args, want)
	}
}

// TestParse_TailClassification guards the typed tail the run wrapper reads to catch a
// global stolen from the wrapped child: parse classifies each post-command token once (as a
// command-arg or an awa global) in its single pass, and a global's separate value is
// absorbed by the flag rather than recorded as its own token.
func TestParse_TailClassification(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []tailToken
	}{
		{"global after child", []string{"run", "mytool", "--json"},
			[]tailToken{{class: tailArg}, {class: tailGlobal, name: "--json"}}},
		{"global before child", []string{"run", "--json", "mytool"},
			[]tailToken{{class: tailGlobal, name: "--json"}, {class: tailArg}}},
		{"value-global absorbs its value", []string{"run", "--root", "/x", "mytool"},
			[]tailToken{{class: tailGlobal, name: "--root"}, {class: tailArg}}},
		{"command-local flag is a command-arg", []string{"run", "--refresh", "mytool"},
			[]tailToken{{class: tailArg}, {class: tailArg}}},
		{"pre-command global leaves an empty tail", []string{"--json", "status"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := parse(tt.argv)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := inv.tail; !equalTail(got, tt.want) {
				t.Errorf("tail = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestAmbiguousTailGlobal_Boundary guards the run wrapper's boundary check over the typed
// tail: a global at or after the child's index is ambiguous, a global before it is awa's.
// childPos is the child's index in inv.args (command-arg space) as run's flag parser
// computes it — 0 when only globals precede the child, since parse strips them from args.
func TestAmbiguousTailGlobal_Boundary(t *testing.T) {
	arg := tailToken{class: tailArg}
	glob := func(name string) tailToken { return tailToken{class: tailGlobal, name: name} }
	tests := []struct {
		name     string
		tail     []tailToken
		childPos int
		wantErr  bool
	}{
		{"global after child is ambiguous", []tailToken{arg, glob("--json")}, 0, true},
		{"global before child is awa's", []tailToken{glob("--json"), arg}, 0, false},
		{"global after a run-local flag and child", []tailToken{arg, arg, glob("--json")}, 1, true},
		{"value-global before child is awa's", []tailToken{glob("--root"), arg}, 0, false},
		{"value-global after child is ambiguous", []tailToken{arg, glob("--root")}, 0, true},
		{"non-global flag after child is the child's", []tailToken{arg, arg}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ambiguousTailGlobal(tt.tail, tt.childPos)
			if tt.wantErr && err == nil {
				t.Errorf("ambiguousTailGlobal(%+v, %d) = nil, want ambiguity error", tt.tail, tt.childPos)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ambiguousTailGlobal(%+v, %d) = %v, want nil", tt.tail, tt.childPos, err)
			}
		})
	}
}

func equalTail(a, b []tailToken) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParse_UnknownFlagBeforeCommandIsError(t *testing.T) {
	// A flag with no command to own it is a usage error.
	_, err := parse([]string{"--bogus", "status"})
	if err == nil {
		t.Fatal("expected usage error for unknown flag before command, got nil")
	}
	ce, ok := err.(*codedError)
	if !ok || ce.Code() != ExitUsageError {
		t.Errorf("error = %v, want usage error", err)
	}
}

// TestCapabilityRegistriesAreConsistent guards the several capability lists
// from drifting apart: the ordered set (allCapabilities), the help catalog
// (capabilityInfo), the command/subcommand declarations, and what the parser
// actually marks. A new capability that is added to one but not the others would
// silently degrade help or validation; this turns that into a test failure.
func TestCapabilityRegistriesAreConsistent(t *testing.T) {
	known := map[capability]bool{}
	for _, c := range allCapabilities {
		if known[c] {
			t.Errorf("duplicate capability %q in allCapabilities", c)
		}
		known[c] = true
	}

	// allCapabilities and capabilityInfo must describe exactly the same set,
	// and every catalog entry must carry non-empty help metadata.
	if len(allCapabilities) != len(capabilityInfo) {
		t.Errorf("allCapabilities has %d entries, capabilityInfo has %d", len(allCapabilities), len(capabilityInfo))
	}
	for c := range capabilityInfo {
		if !known[c] {
			t.Errorf("capabilityInfo has %q absent from allCapabilities", c)
		}
	}
	for _, c := range allCapabilities {
		info, ok := capabilityInfo[c]
		if !ok {
			t.Errorf("capability %q missing from capabilityInfo", c)
			continue
		}
		if info.display() == "" || info.help == "" || len(info.spellings) == 0 {
			t.Errorf("capability %q has empty help metadata: %+v", c, info)
		}
	}

	// Every capability a command or any of its subcommands declares must be known,
	// and — the other direction — every cataloged capability must be declared by a
	// top-level command. The catalog is a catalog of shipped behavior: top-level
	// help renders every entry as usable and the generated global-options page
	// lists who accepts it, so one with no executable consumer would advertise a
	// global option no invocation can act on.
	//
	// The accepting set is the top-level one on purpose, because that is the set
	// route validates an invocation against before it ever reaches a subcommand: a
	// capability only a subcommand declared would be rejected for every command
	// line, and commandsAccepting would render it accepted by nobody.
	accepted := map[capability]bool{}
	for _, cmd := range newRouter().commands {
		for _, c := range cmd.capabilities {
			if !known[c] {
				t.Errorf("command %q declares capability %q absent from allCapabilities", cmd.name, c)
			}
			accepted[c] = true
		}
		for _, sc := range cmd.subcommands {
			for _, c := range sc.caps {
				if !known[c] {
					t.Errorf("%s %q declares capability %q absent from allCapabilities", cmd.name, sc.name, c)
				}
				// A subcommand's set must stay inside its parent's, because route
				// validates the invocation against the parent before dispatch: a
				// capability only the subcommand declared is rejected for every command
				// line, while its own help still advertises the flag as supported.
				if !hasCap(cmd.capabilities, c) {
					t.Errorf("%s %q declares capability %q that %q does not accept; route would reject it before dispatch", cmd.name, sc.name, c, cmd.name)
				}
			}
		}
	}
	for _, c := range allCapabilities {
		if !accepted[c] {
			t.Errorf("capability %q is cataloged but no command accepts it", c)
		}
	}

	// Every global flag the parser marks must map to a known capability, so a
	// new flag cannot mark an uncataloged capability (which requireAccepted,
	// iterating allCapabilities, would never check — silently accepting it).
	flagCap := map[string]capability{
		"--root":       capRoot,
		"--config":     capConfig,
		"--json":       capJSON,
		"--trust-mode": capTrustMode,
		"--strict":     capTrustMode,
	}
	valueFor := func(flag string) []string {
		switch flag {
		case "--root", "--config":
			return []string{flag, "x"}
		case "--trust-mode":
			return []string{flag, "normal"}
		default:
			return []string{flag}
		}
	}
	for flag, want := range flagCap {
		inv, err := parse(append(valueFor(flag), "status"))
		if err != nil {
			t.Fatalf("parse %q: %v", flag, err)
		}
		if !inv.options.has(want) {
			t.Errorf("flag %q did not mark capability %q", flag, want)
		}
		if !known[want] {
			t.Errorf("flag %q marks capability %q absent from allCapabilities", flag, want)
		}
	}
}

func TestParse_MissingFlagValue(t *testing.T) {
	_, err := parse([]string{"--root"})
	if err == nil {
		t.Fatal("expected error for missing flag value, got nil")
	}
	ce, ok := err.(*codedError)
	if !ok || ce.Code() != ExitUsageError {
		t.Errorf("error = %v, want usage error", err)
	}
}
