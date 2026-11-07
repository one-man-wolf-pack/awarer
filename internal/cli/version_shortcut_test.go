package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestParse_RootVersionShortcut pins where the parser recognizes root "--version":
// only before a command is named, and never after "--".
func TestParse_RootVersionShortcut(t *testing.T) {
	tests := []struct {
		name         string
		argv         []string
		wantCommand  string
		wantJSON     bool
		wantArgs     []string
		wantOperands []string
	}{
		{name: "alone", argv: []string{"--version"}, wantCommand: "version"},
		{name: "json before", argv: []string{"--json", "--version"}, wantCommand: "version", wantJSON: true},
		{name: "json after", argv: []string{"--version", "--json"}, wantCommand: "version", wantJSON: true},
		{name: "second command becomes an argument", argv: []string{"--version", "status"}, wantCommand: "version", wantArgs: []string{"status"}},
		{name: "after a command it is the command's token", argv: []string{"status", "--version"}, wantCommand: "status", wantArgs: []string{"--version"}},
		{name: "after the terminator it is an operand", argv: []string{"run", "--", "helper", "--version"}, wantCommand: "run", wantOperands: []string{"helper", "--version"}},
		{name: "terminator before any command", argv: []string{"--", "--version"}, wantOperands: []string{"--version"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv, err := parse(tt.argv)
			if err != nil {
				t.Fatalf("parse(%q): %v", tt.argv, err)
			}
			if inv.command != tt.wantCommand {
				t.Errorf("command = %q, want %q", inv.command, tt.wantCommand)
			}
			if inv.options.JSON != tt.wantJSON {
				t.Errorf("JSON = %v, want %v", inv.options.JSON, tt.wantJSON)
			}
			if strings.Join(inv.args, "\x00") != strings.Join(tt.wantArgs, "\x00") {
				t.Errorf("args = %q, want %q", inv.args, tt.wantArgs)
			}
			if strings.Join(inv.operands, "\x00") != strings.Join(tt.wantOperands, "\x00") {
				t.Errorf("operands = %q, want %q", inv.operands, tt.wantOperands)
			}
		})
	}
}

// TestRun_RootVersionMatchesVersionCommand proves the shortcut is the same operation
// end to end: identical stdout, stderr and exit for text and JSON, in either option
// order, with the canonical "version" command token.
func TestRun_RootVersionMatchesVersionCommand(t *testing.T) {
	tests := []struct {
		name      string
		canonical []string
		shortcut  []string
		json      bool
	}{
		{name: "text", canonical: []string{"version"}, shortcut: []string{"--version"}},
		{name: "json after", canonical: []string{"version", "--json"}, shortcut: []string{"--version", "--json"}, json: true},
		{name: "json before", canonical: []string{"--json", "version"}, shortcut: []string{"--json", "--version"}, json: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantCode, wantOut, wantErr := run(tt.canonical...)
			code, stdout, stderr := run(tt.shortcut...)

			if wantCode != int(ExitSuccess) || code != wantCode {
				t.Errorf("exit = %d, canonical %d, want both %d", code, wantCode, ExitSuccess)
			}
			if stdout == "" || stdout != wantOut {
				t.Errorf("stdout = %q, want canonical %q", stdout, wantOut)
			}
			if stderr != wantErr {
				t.Errorf("stderr = %q, want canonical %q", stderr, wantErr)
			}
			if tt.json {
				var env struct {
					SchemaVersion int             `json:"schema_version"`
					Command       string          `json:"command"`
					Data          json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal([]byte(stdout), &env); err != nil {
					t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
				}
				if env.Command != "version" || env.SchemaVersion != 1 {
					t.Errorf("envelope = {schema_version:%d command:%q}, want {1 \"version\"}", env.SchemaVersion, env.Command)
				}
			}
		})
	}
}

// TestRun_RootVersionInvalidUsage proves the shortcut selects an operation rather
// than modifying another command: a second command, operand, unknown flag, inline
// value or unaccepted global is usage exit 2 with empty stdout, and the other command
// is never run.
func TestRun_RootVersionInvalidUsage(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{name: "second command", argv: []string{"--version", "status"}, wantErr: `unexpected argument "status" for version`},
		{name: "second command after json", argv: []string{"--json", "--version", "init"}, wantErr: `unexpected argument "init" for version`},
		{name: "operand", argv: []string{"--version", "extra"}, wantErr: `unexpected argument "extra" for version`},
		{name: "terminator operand", argv: []string{"--version", "--", "extra"}, wantErr: `unexpected argument "extra" for version`},
		{name: "repeated", argv: []string{"--version", "--version"}, wantErr: `unknown flag "--version" for version`},
		{name: "unknown flag", argv: []string{"--version", "--bogus"}, wantErr: `unknown flag "--bogus" for version`},
		{name: "unknown flag first", argv: []string{"--bogus", "--version"}, wantErr: `unknown flag "--bogus"`},
		{name: "inline true", argv: []string{"--version=true"}, wantErr: `flag "--version" does not take a value`},
		{name: "inline empty", argv: []string{"--version="}, wantErr: `flag "--version" does not take a value`},
		{name: "bad global value", argv: []string{"--version", "--trust-mode", "bogus"}, wantErr: `invalid --trust-mode value`},
		{name: "unaccepted global", argv: []string{"--version", "--root", "/nonexistent"}, wantErr: "--root"},
		{name: "after a command", argv: []string{"status", "--version"}, wantErr: `"--version"`},
		{name: "after version", argv: []string{"version", "--version"}, wantErr: `unknown flag "--version" for version`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An empty, undiscoverable cwd: "status" would fail as not-found (exit 3),
			// so a usage exit also proves the second command was not run.
			t.Chdir(t.TempDir())
			code, stdout, stderr := run(tt.argv...)

			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want %d (stderr %q)", code, ExitUsageError, stderr)
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

// TestRun_RootVersionHelp keeps the established help behavior: "--help" with the
// shortcut shows the version command page, and root help advertises the shortcut
// as a root synopsis, not as a global option every command accepts.
func TestRun_RootVersionHelp(t *testing.T) {
	_, want, _ := run("version", "--help")
	for _, argv := range [][]string{{"--version", "--help"}, {"--help", "--version"}} {
		code, stdout, stderr := run(argv...)
		if code != int(ExitSuccess) || stdout != want {
			t.Errorf("%q: exit %d stdout %q stderr %q, want version --help output %q", argv, code, stdout, stderr, want)
		}
	}
	if !strings.Contains(want, "awa --version [--json]") {
		t.Errorf("version --help = %q, want the root shortcut synopsis", want)
	}

	_, root, _ := run("--help")
	usage, globals, ok := strings.Cut(root, "global options:")
	if !ok {
		t.Fatalf("root help has no global options section:\n%s", root)
	}
	if !strings.Contains(usage, "  awa --version [--json]\n") {
		t.Errorf("root help usage = %q, want the root shortcut synopsis", usage)
	}
	if strings.Contains(globals, "--version") {
		t.Errorf("root help lists --version as a global option:\n%s", globals)
	}
}

// TestRun_RootVersionOutsideProject proves the shortcut needs no project and
// creates none.
func TestRun_RootVersionOutsideProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	code, stdout, stderr := run("--version")
	if code != int(ExitSuccess) || stdout == "" {
		t.Fatalf("exit = %d stdout %q stderr %q, want success with output", code, stdout, stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cwd gained %d entries (first %q), want none", len(entries), entries[0].Name())
	}
}

// TestRun_RootVersionIgnoresMalformedProjectConfig runs the shortcut inside a
// genuinely recognized project whose shared or private config is malformed. A
// control proves the project and the bad config are discovered by an ordinary
// command; the shortcut still answers like "awa version" and leaves .awa/ untouched.
func TestRun_RootVersionIgnoresMalformedProjectConfig(t *testing.T) {
	_, wantOut, _ := run("version")
	_, wantJSON, _ := run("version", "--json")

	for _, layer := range []string{"awa.toml", ".awa/config.toml"} {
		t.Run(layer, func(t *testing.T) {
			root := initProject(t)
			writeFile(t, root, layer, "[run\nnot toml")
			t.Chdir(root)

			if code, _, stderr := run("status"); code != int(ExitConfigError) {
				t.Fatalf("control: status exit = %d (stderr %q), want config error %d", code, stderr, ExitConfigError)
			}
			before := snapshotTree(t, filepath.Join(root, ".awa"))

			for _, tc := range []struct {
				argv []string
				want string
			}{
				{[]string{"--version"}, wantOut},
				{[]string{"--version", "--json"}, wantJSON},
			} {
				code, stdout, stderr := run(tc.argv...)
				if code != int(ExitSuccess) || stdout != tc.want || stderr != "" {
					t.Errorf("%q: exit %d stdout %q stderr %q, want canonical %q", tc.argv, code, stdout, stderr, tc.want)
				}
			}
			// An explicit config locator does not make it load config: the version
			// operation rejects --config as usage, exactly like "awa version --config".
			_, _, wantErr := run("version", "--config", layer)
			code, stdout, stderr := run("--version", "--config", layer)
			if code != int(ExitUsageError) || stdout != "" || stderr != wantErr {
				t.Errorf("--config: exit %d stdout %q stderr %q, want usage with canonical %q", code, stdout, stderr, wantErr)
			}

			if after := snapshotTree(t, filepath.Join(root, ".awa")); after != before {
				t.Errorf(".awa/ changed under the shortcut:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// snapshotTree renders every entry under dir with its mode, size and modification
// time, so any write, creation or removal changes the result.
func snapshotTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		fmt.Fprintf(&b, "%s %s %d %s\n", rel, info.Mode(), info.Size(), info.ModTime().Format(time.RFC3339Nano))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
