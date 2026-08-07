package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the public-surface acceptance layer: it drives the built binary the way a
// user or coding agent with only the binary would, proving the embedded help and
// exit contract are true end to end. The command/topic lists here are literal on
// purpose — this is a black-box check of the public surface, and a change to that
// surface should require a deliberate edit here as well as to the in-process
// contract tests and the reference golden. Deliberate, but not optional:
// TestPublicInventoriesMatchExportedReference holds all three lists against the
// binary's own exported reference, so forgetting one of them fails loudly instead
// of quietly shrinking what everything below exercises.

// publicCommands is every top-level command a user can invoke.
var publicCommands = []string{
	"init", "status", "checkpoint", "log", "changes", "diff", "restore",
	"run", "gc", "doctor", "config", "state", "docs", "help", "version",
}

// publicSubcommands is every "<command> <sub>" pair the binary dispatches, whether
// its --help page is authored or derived from the summary.
var publicSubcommands = [][]string{
	{"run", "ls"}, {"run", "log"}, {"run", "show"}, {"run", "rm"}, {"run", "explain"},
	{"config", "path"}, {"config", "show"}, {"config", "effective"},
	{"config", "validate"}, {"config", "template"}, {"config", "init"},
	{"state", "resolve"}, {"state", "compare"},
	{"docs", "export"},
}

// publicTopics is every canonical operational help topic. Alias spellings resolve
// to these and are covered by the in-process help contract tests, so listing one
// here would read as a topic the binary does not publish.
var publicTopics = []string{
	"agents", "install", "quickstart", "workflows", "status",
	"run", "record", "inspect", "checkpoints", "diff", "restore", "refs",
	"config", "ignores", "doctor", "gc", "troubleshooting",
	"privacy", "integrations", "platform", "json", "exit-codes",
}

// TestHelpWorksOutsideProject proves the golden Unix property: every help surface
// succeeds with no project present, so an agent can discover the safe workflow
// from the binary alone before running anything. The temp dir is never
// initialized, so a help path that leaked into project resolution would fail here.
func TestHelpWorksOutsideProject(t *testing.T) {
	dir := t.TempDir()

	surfaces := [][]string{{"--help"}, {"help"}, {"help", "topics"}}
	for _, c := range publicCommands {
		surfaces = append(surfaces, []string{c, "--help"})
	}
	for _, sc := range publicSubcommands {
		surfaces = append(surfaces, append(append([]string{}, sc...), "--help"))
	}
	for _, topic := range publicTopics {
		surfaces = append(surfaces, []string{"help", topic})
	}

	for _, args := range surfaces {
		code, stdout, stderr := awa(t, dir, args...)
		if code != 0 {
			t.Errorf("awa %s outside a project: exit = %d, want 0; stderr=%q", strings.Join(args, " "), code, stderr)
		}
		if strings.TrimSpace(stdout) == "" {
			t.Errorf("awa %s: empty stdout", strings.Join(args, " "))
		}
		if stderr != "" {
			t.Errorf("awa %s: stderr = %q, want empty", strings.Join(args, " "), stderr)
		}
	}
}

// cliReference is the part of an exported reference/cli.json this file reads: the
// canonical command names, the subcommands each one owns, and the canonical help
// topics. Decoding only these fields keeps the parity check indifferent to the rest
// of the document's shape.
type cliReference struct {
	Commands []struct {
		Name        string `json:"name"`
		Subcommands []struct {
			Name string `json:"name"`
		} `json:"subcommands"`
	} `json:"commands"`
	HelpTopics struct {
		Canonical []struct {
			Name string `json:"name"`
		} `json:"canonical"`
	} `json:"help_topics"`
}

// exportedCLIReference makes the built binary publish its own machine reference
// into a fresh directory and decodes it. This is the one oracle the inventories are
// checked against: it is a public projection the binary already ships, so the check
// needs no product import and no human-help parser.
func exportedCLIReference(t *testing.T) cliReference {
	t.Helper()
	dir := t.TempDir()
	dest := filepath.Join(dir, "exported-docs")
	if code, _, stderr := awa(t, dir, "docs", "export", "--output", dest); code != 0 {
		t.Fatalf("docs export: exit = %d, want 0; stderr=%q", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "reference", "cli.json"))
	if err != nil {
		t.Fatalf("reading the exported machine reference: %v", err)
	}
	var ref cliReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("decode reference/cli.json: %v", err)
	}
	return ref
}

// assertSetParity compares one literal inventory with the corresponding exported
// catalog and names both directions of drift: a shipped member nobody listed stops
// being exercised, and a listed member the binary no longer ships is a stale
// spelling. Reporting the members rather than a count is what makes the failure
// actionable without rerunning anything.
func assertSetParity(t *testing.T, inventory string, listed, exported []string) {
	t.Helper()
	if len(exported) == 0 {
		t.Fatalf("the exported catalog for %s is empty; the parity check would be vacuous", inventory)
	}
	listedSet := map[string]bool{}
	for _, name := range listed {
		if listedSet[name] {
			t.Errorf("%s lists %q twice", inventory, name)
		}
		listedSet[name] = true
	}
	exportedSet := map[string]bool{}
	for _, name := range exported {
		exportedSet[name] = true
	}

	var missing, extra []string
	for _, name := range exported {
		if !listedSet[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range listed {
		if !exportedSet[name] {
			extra = append(extra, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s is missing shipped %v; add them so their surface stays exercised", inventory, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s names %v, which the binary no longer ships; remove the stale entries", inventory, extra)
	}
}

// TestPublicInventoriesMatchExportedReference proves the three literal inventories
// above name exactly the commands, subcommands, and help topics the built binary
// ships. They stay independent hand-maintained lists — that is what makes the tests
// driven by them a black-box statement about the public surface — while an omission
// can no longer pass as coverage.
func TestPublicInventoriesMatchExportedReference(t *testing.T) {
	ref := exportedCLIReference(t)

	var commands, pairs, topics []string
	for _, c := range ref.Commands {
		commands = append(commands, c.Name)
		for _, sc := range c.Subcommands {
			pairs = append(pairs, c.Name+" "+sc.Name)
		}
	}
	for _, topic := range ref.HelpTopics.Canonical {
		topics = append(topics, topic.Name)
	}

	listedPairs := make([]string, 0, len(publicSubcommands))
	for _, sc := range publicSubcommands {
		listedPairs = append(listedPairs, strings.Join(sc, " "))
	}

	assertSetParity(t, "publicCommands", publicCommands, commands)
	assertSetParity(t, "publicSubcommands", listedPairs, pairs)
	assertSetParity(t, "publicTopics", publicTopics, topics)
}

// TestUnknownInputsAreUsageErrors proves that unknown commands, topics, flags,
// values, and conflicting modes all fail with the usage exit code (2) and a
// message on stderr — never a silent success or a crash.
func TestUnknownInputsAreUsageErrors(t *testing.T) {
	root := initProject(t)
	cases := [][]string{
		{"not-a-command"},
		{"help", "not-a-topic"},
		{"status", "--not-a-flag"},
		{"log", "--time", "not-a-mode"},
		{"changes", "--stat", "--json"},       // human-only flag with --json
		{"run", "log", "--strict"},            // trust-mode on a history-only subcommand that ignores it
		{"config", "init"},                    // requires --shared or --local
		{"config", "not-a-subcommand"},        // unknown config subcommand
		{"gc", "--runs-only", "--blobs-only"}, // mutually exclusive filters
	}
	for _, args := range cases {
		code, _, stderr := awa(t, root, args...)
		if code != 2 {
			t.Errorf("awa %s: exit = %d, want 2 (usage); stderr=%q", strings.Join(args, " "), code, stderr)
		}
		if strings.TrimSpace(stderr) == "" {
			t.Errorf("awa %s: usage error with empty stderr", strings.Join(args, " "))
		}
	}
}

// Config init's layer flags must be named in its own --help, and executing the
// documented example must actually write the file.
func TestConfigInitFlagsDiscoverableAndWork(t *testing.T) {
	root := initProject(t)

	_, help, _ := awa(t, root, "config", "init", "--help")
	for _, flag := range []string{"--shared", "--local", "--force"} {
		if !strings.Contains(help, flag) {
			t.Errorf("config init --help does not document %s\n%s", flag, help)
		}
	}

	// The documented example runs and writes the private layer.
	if code, _, stderr := awa(t, root, "config", "init", "--local"); code != 0 {
		t.Fatalf("config init --local: exit = %d, stderr=%q", code, stderr)
	}
	if code, stdout, _ := awa(t, root, "config", "show", "local"); code != 0 || strings.TrimSpace(stdout) == "" {
		t.Errorf("config show local after init: exit=%d stdout=%q", code, stdout)
	}
}

// TestRunTrustModeAcceptedWhereStateResolved pins that the two run subcommands
// that resolve the cache decision against the current state — ls (reusable-now)
// and explain (modeled hit/miss) — accept --trust-mode/--strict exactly as
// "awa run" does, so the trust policy reaches the reusability computation instead
// of being rejected. Regression guard for the per-subcommand capability tightening.
func TestRunTrustModeAcceptedWhereStateResolved(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	for _, args := range [][]string{
		{"run", "ls", "--strict"},
		{"run", "ls", "--trust-mode", "fast"},
		{"run", "explain", "--strict", "--", h, "-out", "x"},
	} {
		if code, _, stderr := awa(t, root, args...); code == 2 {
			t.Errorf("awa %s: exit = 2 (usage), want it accepted; stderr=%q", strings.Join(args, " "), stderr)
		}
	}
}

// TestAgentsHelpBudgetBuiltBinary bounds the agent golden-path page against the
// built binary and pins the canonical example the page must teach, so the
// primary agent entry point stays compact and truthful in the shipped artifact.
// The byte/line limits deliberately mirror the in-process TestAgentsHelpWithinBudget
// (internal/cli); keep the two in step if the budget ever changes.
func TestAgentsHelpBudgetBuiltBinary(t *testing.T) {
	dir := t.TempDir()
	code, stdout, _ := awa(t, dir, "help", "agents")
	if code != 0 {
		t.Fatalf("help agents exit = %d", code)
	}
	if n := len(stdout); n > 8000 {
		t.Errorf("help agents = %d bytes, over the 8000-byte agent-context budget", n)
	}
	if lines := strings.Count(stdout, "\n") + 1; lines > 180 {
		t.Errorf("help agents = %d lines, over the 180-line budget", lines)
	}
	if !strings.Contains(stdout, "awa run --record") {
		t.Errorf("help agents must teach 'awa run --record'\n%s", stdout)
	}
}

// TestReferenceOriginFieldResolvesInEnvelope proves the CLI reference publishes the
// exact machine path of the run exit-origin field, not a shorthand: it reads the
// origin_field the committed reference advertises and walks that dotted path into a
// real "awa run --json" envelope. A shorthand like "run.exit_origin" would not
// resolve. This is the structural tie between the reference and the run envelope shape.
func TestReferenceOriginFieldResolvesInEnvelope(t *testing.T) {
	refBytes, err := os.ReadFile("../../internal/cli/testdata/reference.json")
	if err != nil {
		t.Fatalf("read reference golden: %v", err)
	}
	var ref struct {
		ExitCodes struct {
			RunChildExit struct {
				OriginField string `json:"origin_field"`
			} `json:"run_child_exit"`
		} `json:"exit_codes"`
	}
	if err := json.Unmarshal(refBytes, &ref); err != nil {
		t.Fatalf("decode reference: %v", err)
	}
	path := ref.ExitCodes.RunChildExit.OriginField
	if path == "" {
		t.Fatal("reference publishes no exit_codes.run_child_exit.origin_field")
	}

	root := initProject(t)
	h := helper(t)
	_, stdout, _ := awa(t, root, "run", "--json", "--", h, "-exit", "3")
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decode run envelope: %v\n%s", err, stdout)
	}

	got, ok := walkJSONPath(env, path)
	if !ok {
		t.Fatalf("origin_field %q does not resolve in a real run --json envelope\n%s", path, stdout)
	}
	if got != "child" {
		t.Errorf("%s = %v, want \"child\"", path, got)
	}
}

// walkJSONPath descends a decoded JSON object along a dot-separated path, returning
// the leaf value and whether the whole path resolved.
func walkJSONPath(root map[string]any, path string) (any, bool) {
	var cur any = root
	for _, key := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// TestNormativeJSONExampleParses proves a representative --json surface emits a
// clean schema-versioned envelope after normal completion, and — critically —
// that a non-zero wrapped-child exit is still a valid JSON document (a run
// protocol success carrying the child's failure), matching the exit contract.
func TestNormativeJSONExampleParses(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// A normal command envelope.
	_, stdout, _ := awa(t, root, "status", "--json")
	assertEnvelope(t, stdout, "status")

	// A wrapped child that exits non-zero: the run itself is a JSON success whose
	// data carries the child's exit code, so agents can branch on it without the
	// stream being treated as a protocol failure.
	code, stdout, _ := awa(t, root, "run", "--json", "--", h, "-exit", "3")
	if code != 3 {
		t.Errorf("run of a child exiting 3: awa exit = %d, want the child's 3", code)
	}
	assertEnvelope(t, stdout, "run")
}

// TestDocsExportWorksOutsideProjectWithMalformedConfig proves the export is pure
// with respect to project state end to end: it succeeds from a directory that has
// no .awa/, next to an unrelated awa.toml that would fail any config load, and
// publishes a manifest describing every file it wrote. A run that discovered a
// root or parsed a config layer would fail here rather than publish.
func TestDocsExportWorksOutsideProjectWithMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "awa.toml"), []byte("this is not = valid toml [[[\n"), 0o600); err != nil {
		t.Fatalf("seeding a malformed config: %v", err)
	}
	dest := filepath.Join(dir, "exported-docs")

	code, stdout, stderr := awa(t, dir, "docs", "export", "--output", dest)
	if code != 0 {
		t.Fatalf("docs export outside a project: exit = %d, stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "exported ") {
		t.Errorf("docs export printed no summary:\n%s", stdout)
	}

	raw, err := os.ReadFile(filepath.Join(dest, "manifest.json"))
	if err != nil {
		t.Fatalf("reading the published manifest: %v", err)
	}
	var m struct {
		ExportSchemaVersion int `json:"export_schema_version"`
		Product             struct {
			Version string `json:"version"`
		} `json:"product"`
		Documents []struct {
			Path string `json:"path"`
		} `json:"documents"`
		MachineReference struct {
			Path string `json:"path"`
		} `json:"machine_reference"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.ExportSchemaVersion == 0 || m.Product.Version == "" || len(m.Documents) == 0 {
		t.Fatalf("manifest is not a complete export: %+v", m)
	}
	for _, d := range m.Documents {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(d.Path))); err != nil {
			t.Errorf("manifest lists %q but it was not published: %v", d.Path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(m.MachineReference.Path))); err != nil {
		t.Errorf("machine reference was not published: %v", err)
	}
}
