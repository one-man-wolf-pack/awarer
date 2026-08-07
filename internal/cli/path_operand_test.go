package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The path-operand contract for the comparison commands: for an ordinary changed
// path, "<range> <path>" and "<range> -- <path>" name the same filter and produce
// the same report on both changes and diff, in human and JSON form. A path-filtered
// comparison that silently drops its operand reports "no changes", which reads
// exactly like proof that nothing changed — the false-conclusion chain this stops.
// "--" stays the disambiguator for a token that looks like a flag or a range, and a
// token the command cannot classify is a usage error, never an empty comparison.

// operandProject initializes a project whose human output is deterministic
// (absolute UTC timestamps rather than relative ages, so two invocations
// milliseconds apart cannot render different headers), records a baseline
// checkpoint over two files, then dirties both. The returned path is the absolute
// path of the file the operand tests filter on; filters are resolved from the
// invocation cwd, and these tests run from an isolated temp dir, so they pass an
// absolute path rather than mutating the process working directory.
func operandProject(t *testing.T) (root, changedAbs string) {
	t.Helper()
	root = initProject(t)
	writeFile(t, root, ".awa/config.toml", "[ui]\ntime = \"utc\"\n")
	writeFile(t, root, "generated/client/openapi.json", "{\"paths\":{}}\n")
	writeFile(t, root, "src/service.go", "package src\n\nfunc A() int { return 1 }\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	// Both files drift, so a filtered report is strictly narrower than an
	// unfiltered one: the equivalence below cannot pass by both sides being empty.
	writeFile(t, root, "generated/client/openapi.json", "{\"paths\":{\"/v2\":{}}}\n")
	writeFile(t, root, "src/service.go", "package src\n\nfunc A() int { return 2 }\n")
	return root, filepath.Join(root, "generated", "client", "openapi.json")
}

// TestPathOperandFormsAgree proves the bare and terminated operand forms resolve
// to the same filter for changes and diff, in human and JSON output.
func TestPathOperandFormsAgree(t *testing.T) {
	root, changed := operandProject(t)

	for _, cmd := range []string{"changes", "diff"} {
		for _, jsonMode := range []bool{false, true} {
			name := cmd
			if jsonMode {
				name += "-json"
			}
			t.Run(name, func(t *testing.T) {
				// --json goes before the terminator: everything after "--" is an
				// operand and is never re-read as a flag.
				lead := []string{cmd, "--root", root}
				if jsonMode {
					lead = append(lead, "--json")
				}
				bare := append(append([]string{}, lead...), "latest..now", changed)
				term := append(append([]string{}, lead...), "latest..now", "--", changed)

				bareCode, bareOut, bareErr := run(bare...)
				termCode, termOut, termErr := run(term...)

				if bareCode != int(ExitSuccess) {
					t.Fatalf("bare form exit = %d, stderr = %q", bareCode, bareErr)
				}
				if termCode != int(ExitSuccess) {
					t.Fatalf("terminated form exit = %d, stderr = %q", termCode, termErr)
				}
				if bareOut != termOut {
					t.Errorf("path operand forms disagree on stdout:\nbare:\n%s\nterminated:\n%s", bareOut, termOut)
				}
				// The oracle is independent of the production filter field: it asserts on
				// the rendered report. The selected path must be reported and the
				// unselected one must not, so a dropped operand (which would widen the
				// report to both files) fails here even though both forms would still
				// agree with each other.
				if !strings.Contains(bareOut, "generated/client/openapi.json") {
					t.Errorf("filtered report omits the selected path:\n%s", bareOut)
				}
				if strings.Contains(bareOut, "src/service.go") {
					t.Errorf("filtered report leaked an unselected path:\n%s", bareOut)
				}
			})
		}
	}
}

// TestPathOperandFilterIsNarrowerThanUnfiltered proves the filter actually
// narrows: without an operand both dirty files are reported. Without this, a
// filter implementation that reported everything would still satisfy the
// bare-vs-terminated equivalence above.
func TestPathOperandFilterIsNarrowerThanUnfiltered(t *testing.T) {
	root, _ := operandProject(t)
	code, stdout, stderr := run("changes", "--root", root, "latest..now")
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{"generated/client/openapi.json", "src/service.go"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("unfiltered report omits %s:\n%s", want, stdout)
		}
	}
}

// TestPathOperandTerminatorDisambiguates proves "--" is what lets a path token
// that itself looks like a range be named as a path. Without the terminator the
// token is read as a second range and rejected; with it, it is a path filter.
func TestPathOperandTerminatorDisambiguates(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "a..b/file.txt", "one\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	writeFile(t, root, "a..b/file.txt", "two\n")
	rangeLike := filepath.Join(root, "a..b")

	if code, _, _ := run("changes", "--root", root, "latest..now", rangeLike); code != int(ExitUsageError) {
		t.Errorf("range-looking path without -- exit = %d, want %d", code, ExitUsageError)
	}
	code, stdout, stderr := run("changes", "--root", root, "latest..now", "--", rangeLike)
	if code != int(ExitSuccess) {
		t.Fatalf("range-looking path after -- exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "a..b/file.txt") {
		t.Errorf("terminated range-looking path was not used as a filter:\n%s", stdout)
	}
}

// TestUnclassifiableOperandIsUsageError proves a token neither command can
// classify fails loudly. The failure mode this guards against is the dangerous
// one: a silently ignored operand that degrades into an empty comparison
// indistinguishable from "no changes".
func TestUnclassifiableOperandIsUsageError(t *testing.T) {
	root, _ := operandProject(t)

	cases := []struct {
		name string
		args []string
	}{
		{"changes second range", []string{"changes", "--root", root, "latest..now", "@-2..now"}},
		{"diff second range", []string{"diff", "--root", root, "latest..now", "@-2..now"}},
		{"changes flag-like token", []string{"changes", "--root", root, "latest..now", "-x.go"}},
		{"diff flag-like token", []string{"diff", "--root", root, "latest..now", "-x.go"}},
		{"changes outside root", []string{"changes", "--root", root, "latest..now", "--", "/definitely/outside"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, _ := run(tc.args...)
			if code != int(ExitUsageError) {
				t.Fatalf("exit = %d, want %d (stdout = %q)", code, ExitUsageError, stdout)
			}
			if strings.Contains(stdout, "no changes") {
				t.Errorf("unclassifiable operand degraded into an empty comparison:\n%s", stdout)
			}
		})
	}
}
