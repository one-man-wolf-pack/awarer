package cli

import (
	_ "embed"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"awarer/internal/app/help"
)

// referenceGolden is the committed CLI reference. It is embedded (resolved at
// compile time relative to this source file) rather than read at runtime because
// the package's TestMain chdirs to an isolated temp directory, so a relative
// testdata path would not resolve.
//
//go:embed testdata/reference.json
var referenceGolden string

// This file is the embedded-help contract audit: it proves the parser and the
// built-in help agree on the flag set of every command, that the exit-code catalog
// and its help page cannot diverge, that the agent golden-path page stays within a
// bounded context budget, and that the deterministic CLI reference matches its
// committed golden.
//
// Proof boundary: the flag contract is now proven mechanically in BOTH directions.
// help->parser lives here (every documented flag is accepted); parser->help lives in
// parser_vocabulary_test.go, which reads the accepted spellings out of the parsers'
// own switch statements and probes each against each command. Operands are covered
// both ways by TestOperandContractMatchesParser.
//
// The reference JSON and the exported Markdown are awa's single public CLI source,
// so an undocumented flag is a hole in a published document. Checking the reverse
// direction needs no unified descriptor and no parser rewrite: the per-command
// switches are already the declaration; they only had to be read as data.

// flagProbe is one command/subcommand's documented flag surface, derived from the
// same authored help metadata dispatch and rendering use, plus the argv prefix
// that routes tokens to that command's own parser.
type flagProbe struct {
	label  string      // subtest name
	prefix []string    // argv that selects the command (e.g. {"run","ls"})
	help   commandHelp // the authored help whose flag rows we probe
}

// flagProbes enumerates every command and subcommand that documents local flags,
// derived from the live registry and each command's own subcommands so a new one
// is covered automatically. A subcommand that documents no flags (its contract is
// fully described by its summary) has nothing to probe and is skipped.
func flagProbes() []flagProbe {
	var probes []flagProbe
	for _, c := range newRouter().commands {
		if len(c.help.flags) > 0 {
			probes = append(probes, flagProbe{c.name, []string{c.name}, c.help})
		}
		for _, sc := range c.subcommands {
			if len(sc.help.flags) == 0 {
				continue
			}
			probes = append(probes, flagProbe{c.name + " " + sc.name, []string{c.name, sc.name}, sc.help})
		}
	}
	return probes
}

// isUnknownFlagRejection reports whether stderr is the parser rejecting a token it
// does not recognize (as opposed to any other usage error, such as a missing
// companion flag or an out-of-project failure). We match the parser's own wording.
func isUnknownFlagRejection(stderr string) bool {
	return strings.Contains(stderr, "unknown flag") || strings.Contains(stderr, "unexpected argument")
}

// TestHelpFlagsAreAcceptedByParser proves the help→parser direction: every flag a
// command's --help documents is accepted by that command's real parser. Each flag
// is fed through Run with a fresh empty --root so resolution is hermetic and
// side-effect free: a *known* flag parses and then fails for a downstream reason
// (no project, a missing companion flag, a bad dummy value) — never as an unknown
// flag; an *unknown* flag is rejected before any of that. A sentinel negative
// control per command guarantees the probe actually reaches flag parsing, so a
// command that never parsed flags could not pass vacuously.
func TestHelpFlagsAreAcceptedByParser(t *testing.T) {
	for _, p := range flagProbes() {
		t.Run(p.label, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range p.help.flags {
				for _, tok := range flagHelpTokens(f.display) {
					args := []string{"--root", root}
					args = append(args, p.prefix...)
					args = append(args, tok)
					if strings.ContainsRune(f.display, '<') {
						args = append(args, "0") // dummy value for a value-taking flag
					}
					_, _, stderr := run(args...)
					if isUnknownFlagRejection(stderr) {
						t.Errorf("%v: documented flag %q rejected as unknown by the parser: %s", p.prefix, tok, stderr)
					}
				}
			}
			// Sentinel: an undocumented flag MUST be rejected, proving the probe
			// above reached the flag parser rather than short-circuiting earlier.
			args := append([]string{"--root", root}, p.prefix...)
			args = append(args, "--zzz-not-a-real-flag")
			code, _, stderr := run(args...)
			if code != int(ExitUsageError) {
				t.Errorf("%v: sentinel unknown flag not rejected (code=%d); the parser probe may be vacuous: %s", p.prefix, code, stderr)
			}
		})
	}
}

// globalFlagTokens is the set of flag spellings that belong to global options, not
// to any command, so the reverse scan below does not require them in a command's
// local flag rows. Derived from the capability catalog plus the always-present
// help/strict spellings.
func globalFlagTokens() map[string]bool {
	set := map[string]bool{"--help": true, "-h": true}
	for _, info := range capabilityInfo {
		for _, s := range info.spellings {
			set[s.flag] = true
		}
	}
	return set
}

// helpFlagTokenRe extracts flag spellings from RENDERED help text, so it differs
// from the production flagHelpTokens (which reads authored flagHelp.display fields):
// it treats "(" as a word boundary because help wraps flags as "(--shared | ...)",
// and it requires a letter after the dashes so a numeric synopsis token like a
// "-N" range placeholder is not mistaken for a flag.
var helpFlagTokenRe = regexp.MustCompile(`(?:^|[\s(])(-{1,2}[A-Za-z][A-Za-z0-9-]*)`)

// TestHelpTextAdvertisesOnlyDocumentedFlags proves that no flag named anywhere in a
// command's rendered --help (usage synopsis, prose, exit note) lacks a backing
// flag row — otherwise it would be advertised but never probed against the parser.
// Global options and the wrapped-command region after "--" are excluded (the former
// are legitimately not command-local; the latter belong to the child), and the
// examples section is skipped because it shows full command lines, not this
// command's flag contract.
func TestHelpTextAdvertisesOnlyDocumentedFlags(t *testing.T) {
	globals := globalFlagTokens()
	for _, p := range flagProbes() {
		t.Run(p.label, func(t *testing.T) {
			documented := map[string]bool{}
			for _, tok := range commandFlagTokens(p.help) {
				documented[tok] = true
			}
			args := append(append([]string{}, p.prefix...), "--help")
			_, stdout, _ := run(args...)
			if i := strings.Index(stdout, "\nexamples:"); i >= 0 {
				stdout = stdout[:i]
			}
			for _, line := range strings.Split(stdout, "\n") {
				// Drop the wrapped-command region: everything from " -- " onward is
				// the child command's syntax, not awa's.
				if i := strings.Index(line, " -- "); i >= 0 {
					line = line[:i]
				}
				for _, m := range helpFlagTokenRe.FindAllStringSubmatch(line, -1) {
					tok := m[1]
					if globals[tok] || documented[tok] {
						continue
					}
					t.Errorf("%v --help advertises flag %q with no backing flag row (so it is never checked against the parser)\nline: %q", p.prefix, tok, line)
				}
			}
		})
	}
}

// TestHelpTopicsDoNotShadowCommands is a cross-layer check, and the only place
// that can be one: the catalog cannot see the command registry, and the registry
// cannot see the catalog.
//
// A help spelling that matches a command name is a promise to the user, because
// `awa help <command>` is how anyone looks up a command they just ran. Such a
// spelling is legitimate only when it lands on the operational topic that command
// itself points at — `awa help checkpoint` reaching "checkpoints" and `awa help
// changes` reaching "diff" are exactly right. It is NOT legitimate when it lands
// somewhere else, because then the two surfaces answer the same question
// differently and neither is visibly wrong on its own.
//
// Both halves of the help vocabulary are checked. Aliases are the obvious case,
// but a CANONICAL slug can spell a command name too — `run`, `diff`, `config`,
// `doctor`, `gc`, and `status` all do — and those are the more dangerous half,
// because nothing about declaring a page named after a command forces the command
// to point back at it.
func TestHelpTopicsDoNotShadowCommands(t *testing.T) {
	topics := map[string]string{} // command name -> topic it declares
	for _, c := range newRouter().commands {
		topics[c.name] = c.help.topic
	}
	for spelling, slug := range helpSpellings() {
		declared, isCommand := topics[spelling]
		if !isCommand {
			continue
		}
		switch {
		case declared == "":
			t.Errorf("help topic spelling %q matches the %q command, which declares no operational topic: "+
				"`awa help %s` answers with %q while `awa %s --help` points nowhere",
				spelling, spelling, spelling, slug, spelling)
		case declared != slug:
			t.Errorf("help topic spelling %q resolves to %q but the %q command declares topic %q: "+
				"`awa help %s` and `awa %s --help` would send a user to different pages",
				spelling, slug, spelling, declared, spelling, spelling)
		}
	}
}

// helpSpellings returns every token `awa help <token>` accepts — canonical slugs
// and aliases alike — mapped to the canonical slug it resolves to. It builds its
// own map rather than writing into the one Aliases() hands back, so the merge
// cannot depend on that accessor's copy-safety.
func helpSpellings() map[string]string {
	out := map[string]string{}
	for alias, slug := range help.Aliases() {
		out[alias] = slug
	}
	for _, topic := range help.Topics() {
		out[topic.Slug.String()] = topic.Slug.String()
	}
	return out
}

// TestOperandContractMatchesParser is a parser->inventory check for operand syntax:
// every command's AND every subcommand's registry operand kind (which the
// reference publishes) must match what the parser actually does with a token after
// "--". One that does not accept operands rejects it as an unexpected argument;
// one that does routes it onward (failing later for its own reasons, never as an
// unexpected argument). This catches operand syntax being added or removed without
// updating the checked model.
//
// Subcommands are covered explicitly because they are where the contract is
// easiest to get wrong: route can only validate the parent's kind, so "awa run
// show -- x" reaches run's handler on run's wrapped-command allowance and would
// otherwise discard the token in silence.
func TestOperandContractMatchesParser(t *testing.T) {
	root := t.TempDir()
	for _, c := range newRouter().commands {
		assertOperandParsing(t, root, c.operands, c.name)
		for _, sc := range c.subcommands {
			assertOperandParsing(t, root, sc.operands, c.name, sc.name)
		}
	}
}

// assertOperandParsing feeds one token after "--" to the named command path and
// checks the parser's treatment against the declared kind.
func assertOperandParsing(t *testing.T, root string, kind operandKind, path ...string) {
	t.Helper()
	argv := append([]string{"--root", root}, path...)
	argv = append(argv, "--", "x")
	_, _, stderr := run(argv...)
	rejected := strings.Contains(stderr, "unexpected argument")
	if rejected == kind.accepts() {
		t.Errorf("%q: operands=%s but parser %s a token after --",
			strings.Join(path, " "), kind, map[bool]string{true: "rejected", false: "accepted"}[rejected])
	}
}

// TestRunSubcommandOperandSplit pins the deliberate per-subcommand operand
// classification. TestOperandContractMatchesParser can only prove the parser
// enforces what the registry declares — for subcommands the declaration IS the
// enforcement, so it cannot tell a right declaration from a wrong one. This does:
// explain is the only run subcommand that takes a wrapped command line, and the
// rest reject a token after "--" rather than discarding it. A new run subcommand
// must be classified here rather than defaulting in silently.
//
// The behavioural backstop is separate: the acceptance suite drives
// "awa run explain -- <cmd>" end to end, so a declaration that starved explain of
// its operands fails there too.
func TestRunSubcommandOperandSplit(t *testing.T) {
	want := map[string]operandKind{
		"ls":      operandNone,
		"log":     operandNone,
		"show":    operandNone,
		"rm":      operandNone,
		"explain": operandWrappedCommand,
	}
	for _, sc := range runSubcommands {
		expected, ok := want[sc.name]
		if !ok {
			t.Errorf("run subcommand %q is not classified by the operand split test", sc.name)
			continue
		}
		if sc.operands != expected {
			t.Errorf("run %s: operands = %s, want %s", sc.name, sc.operands, expected)
		}
	}
}

// TestSubcommandsOutsideRunTakeNoOperands pins the other half: no subcommand of a
// command that rejects operands may quietly declare that it takes them, which
// would advertise syntax route rejects before dispatch ever runs.
func TestSubcommandsOutsideRunTakeNoOperands(t *testing.T) {
	for _, c := range newRouter().commands {
		if c.operands.accepts() {
			continue
		}
		for _, sc := range c.subcommands {
			if sc.operands.accepts() {
				t.Errorf("%s %s declares operands=%s, but %s rejects operands before dispatch",
					c.name, sc.name, sc.operands, c.name)
			}
		}
	}
}

// TestRunSubcommandTrustModeSplit pins the deliberate per-subcommand capability
// asymmetry the reference now publishes: run ls and run explain resolve the cache
// decision against the current state under the trust policy, so they honor
// --trust-mode; run log/show/rm read stored history and reject it. A change that
// "normalizes" the set in either direction fails here, and a new run subcommand
// must be classified rather than silently defaulting.
func TestRunSubcommandTrustModeSplit(t *testing.T) {
	wantTrust := map[string]bool{"ls": true, "explain": true, "log": false, "show": false, "rm": false}
	for _, sc := range runSubcommands {
		want, ok := wantTrust[sc.name]
		if !ok {
			t.Errorf("run subcommand %q is not classified by the trust-mode split test", sc.name)
			continue
		}
		if got := hasCap(sc.caps, capTrustMode); got != want {
			t.Errorf("run %s: honors --trust-mode = %v, want %v", sc.name, got, want)
		}
	}
}

// exitCodeTableRe matches one row of the exit-code table: a bare number at the
// start of a line followed by its meaning. Anchoring on the line start keeps a
// code mentioned in prose (for example "exit 5") from counting as a table row.
var exitCodeTableRe = regexp.MustCompile(`(?m)^(\d+)\s\s+\S`)

// exitCodeTableEntries returns the set of codes a rendered page lists as table
// rows. It is shared by the catalog cross-check below and the coverage matrix's
// "exit:" evidence branch in docscoverage_test.go.
func exitCodeTableEntries(out string) map[string]bool {
	listed := map[string]bool{}
	for _, m := range exitCodeTableRe.FindAllStringSubmatch(out, -1) {
		listed[m[1]] = true
	}
	return listed
}

// TestExitCodesPageMatchesCatalog cross-checks the exit-codes help page against the
// typed ExitCode catalog in both directions, so a code cannot ship
// documented-but-unproduced or produced-but-undocumented — the mechanical
// generalization of the old hard-coded "removed code 7 must not reappear" guard.
func TestExitCodesPageMatchesCatalog(t *testing.T) {
	topic, ok := help.Resolve("exit-codes")
	if !ok {
		t.Fatal("exit-codes topic does not resolve")
	}
	body := topic.Body

	listed := exitCodeTableEntries(body)

	// enum -> page: every catalog code appears as a code-table row.
	for _, code := range exitCodes() {
		if !listed[strconv.Itoa(int(code))] {
			t.Errorf("exit-codes page does not document code %d", int(code))
		}
	}

	// Every catalog code has a real machine name, so the generated reference can
	// never publish an entry with an empty or "unknown" refName (the refName switch
	// keeps its default for safety, but no shipped code may hit it).
	for _, code := range exitCodes() {
		if name := code.refName(); name == "" || name == "unknown" {
			t.Errorf("exit code %d has no reference name (got %q)", int(code), name)
		}
	}

	// page -> enum: every code-table row is a real catalog code.
	catalog := map[int]bool{}
	for _, code := range exitCodes() {
		catalog[int(code)] = true
	}
	for raw := range listed {
		n, _ := strconv.Atoi(raw)
		if !catalog[n] {
			t.Errorf("exit-codes page documents code %d that no command produces", n)
		}
	}
}

// TestAgentsHelpWithinBudget bounds the agent golden-path page so it stays a
// compact, loadable context for coding agents. Bytes are the real agent-context
// constraint, and this is the only claim the test makes: it is a growth guardrail,
// not a statement about how much the page should say. Current size is ~5.5 KB, so
// the ceiling leaves generous headroom and only fails on a large regression.
//
// It is tighter than the corpus-wide topic ceiling on purpose — the two protect
// different accepted budgets, one for a whole page and one for the page an agent is
// expected to load in full.
func TestAgentsHelpWithinBudget(t *testing.T) {
	topic, ok := help.Resolve("agents")
	if !ok {
		t.Fatal("agents topic does not resolve")
	}
	const maxBytes = 8000
	if bytes := len(topic.Body); bytes > maxBytes {
		t.Errorf("agents page = %d bytes, over the %d-byte agent-context budget", bytes, maxBytes)
	}
}

// TestReferenceMatchesGolden proves the deterministic CLI reference is byte-for-byte
// the committed golden, so any change to the public surface (a command, flag,
// alias, topic, default, JSON capability, or exit code) fails until the golden is
// regenerated and reviewed. This is the fast in-process complement to
// `refgen -check`, and the same reference input the published docs consume.
func TestReferenceMatchesGolden(t *testing.T) {
	var got strings.Builder
	if err := WriteReferenceJSON(&got); err != nil {
		t.Fatalf("WriteReferenceJSON: %v", err)
	}
	if got.String() != referenceGolden {
		t.Errorf("CLI reference drifted from testdata/reference.json.\nRegenerate with: just reference-update")
	}
}
