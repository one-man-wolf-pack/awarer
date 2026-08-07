package cli

import (
	"embed"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file closes the reverse half of the flag contract: that the parser accepts NO
// flag the documentation omits.
//
// awa keeps per-command switch parsers by design, and a unified descriptor would be a
// parser rewrite. What is at stake without this check: the reference JSON and the
// exported Markdown are the single public source for awa's CLI, so a flag the parser
// quietly accepts is a flag missing from a published document, and a manual audit is
// not a contract a site can be built on.
//
// The way out does not require the rewrite. The parsers already name every spelling
// they accept, as a string literal in their own source; they simply were not readable
// as data. Collecting those literals gives a parser-owned vocabulary, and probing each
// against each command's real parser turns it into a per-command accepted set. Both
// halves are mechanical; neither asks anyone to maintain a list.
//
// The delicate part is deciding what "the parser accepts this" looks like from
// outside, and value-taking flags are where the naive answer fails: a bare --foo whose
// parser wants a value produces a usage error quoting --foo, which reads exactly like
// a refusal while meaning the opposite. See acceptsToken.

// packageSources embeds this package's Go sources so the scan works after TestMain
// chdirs to an isolated temp directory, and under -trimpath, neither of which a
// runtime directory read survives.
//
//go:embed *.go
var packageSources embed.FS

// parserFlagLiteralRe matches a token that could be a flag spelling. Requiring a
// letter after the dashes keeps the operand separator "--" and negative numbers out.
var parserFlagLiteralRe = regexp.MustCompile(`^-{1,2}[A-Za-z][A-Za-z0-9-]*$`)

// scan sanity bounds. They are deliberately far below the real numbers: their job is
// to fail loudly if the scan ever finds nothing — a silent empty vocabulary would make
// every check below pass vacuously — not to be a second inventory to maintain.
const (
	minScannedFiles      = 10
	minVocabularySpelled = 30
)

// parserFlagVocabulary returns every flag-shaped string literal in this package's
// non-test sources, sorted.
//
// It collects literals ANYWHERE rather than only switch cases, and that breadth is the
// point. awa's parsers accept flags in more than one syntactic shape — a `case
// "--stat":` in most, an `if name == "--display"` where a flag has to be handled
// outside a shared set — and nothing stops the next one taking a third. A scanner keyed
// to one shape reports a clean vocabulary while being blind to every flag written in
// another, which is the most dangerous way for this file to fail: silently, and looking
// healthy.
//
// Over-collection is harmless by construction: a literal that no parser accepts is
// probed and rejected. Under-collection is the direction that loses coverage, so the
// scan is deliberately indiscriminate and the probe decides.
func parserFlagVocabulary(t *testing.T) []string {
	t.Helper()
	entries, err := packageSources.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the embedded package sources: %v", err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := packageSources.ReadFile(name)
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, uerr := strconv.Unquote(lit.Value)
			if uerr == nil && parserFlagLiteralRe.MatchString(v) {
				seen[v] = true
			}
			return true
		})
	}
	if scanned < minScannedFiles {
		t.Fatalf("scanned only %d package sources; the embed pattern is not seeing the parsers", scanned)
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) < minVocabularySpelled {
		t.Fatalf("parser vocabulary is only %d spellings (%v); the scan is not finding the parsers", len(out), out)
	}
	return out
}

// parserTarget is one invocable command or subcommand together with the help whose
// flag rows are its documented surface. Unlike flagProbes it includes targets that
// document NO flags: a command whose help lists nothing is exactly where an
// undocumented parser flag would hide best.
type parserTarget struct {
	label    string
	prefix   []string
	help     commandHelp
	rootable bool // whether THIS target accepts --root, so the probe can stay out of the cwd
}

func parserTargets() []parserTarget {
	var out []parserTarget
	for _, c := range newRouter().commands {
		out = append(out, parserTarget{c.name, []string{c.name}, c.help, hasCap(c.capabilities, capRoot)})
		for _, sc := range c.subcommands {
			// Per subcommand, not inherited: a subcommand may accept a narrower set
			// than its parent's union, and passing it --root would be rejected as an
			// unaccepted global before its own parser ever ran.
			out = append(out, parserTarget{c.name + " " + sc.name, []string{c.name, sc.name}, sc.help, hasCap(sc.caps, capRoot)})
		}
	}
	return out
}

// acceptsToken reports whether a command's parser RECOGNIZED the token it was given.
//
// Matching the wording of one refusal would not do, because commands legitimately
// refuse a stray token in whatever terms fit their own syntax — an unknown flag, an
// unknown subcommand, an unknown config layer, a malformed A..B range. What they all
// share is that they name the offending token, while a parser that accepted it goes on
// to do its work and never mentions it. That difference needs no per-command knowledge.
//
// One usage error inverts the rule: a demand for the flag's VALUE. Only a flag the
// parser already matched can be asked to supply one, so `flag "--foo" requires a
// value` is proof of recognition wearing the costume of a refusal. Reading it as a
// refusal is what left every value-taking flag outside this guard, which is why the
// exception is checked first and keyed to the probed token rather than to any flag.
func acceptsToken(code int, stderr, tok string) bool {
	if strings.Contains(stderr, fmt.Sprintf("flag %q requires a", tok)) {
		return true
	}
	return code != int(ExitUsageError) || !strings.Contains(stderr, tok)
}

// probeShapes returns the two forms a flag can be written in. Both are needed and
// neither suffices alone: a bare token is how a boolean flag is accepted and how a
// value-taking one reveals itself by demanding a value, while the inline form is how a
// value-taking flag parses cleanly even if its parser never reaches requireValue.
//
// The value is attached with "=" rather than passed as a second argument on purpose.
// A separate token would change the command's ARITY, and commands that count their
// operands would then refuse the probe for the wrong reason — a refusal this test
// would have to tell apart from a real one.
func probeShapes(flag string) []string { return []string{flag, flag + "=0"} }

// TestValueDemandReadsAsAcceptance pins the discriminator itself against a REAL
// value-taking flag, because the guard below is only as good as this one judgement.
// If a demand for a value ever reads as a refusal again, every undocumented
// value-taking flag silently leaves the guard's reach while the suite stays green —
// the exact false-green this test exists to make impossible to reintroduce.
func TestValueDemandReadsAsAcceptance(t *testing.T) {
	root := t.TempDir()
	if code, _, stderr := run("init", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("seeding a project: exit %d, stderr=%q", code, stderr)
	}
	const flag = "--message"
	code, _, stderr := run("--root", root, "checkpoint", flag)
	if !strings.Contains(stderr, "requires a") {
		t.Fatalf("checkpoint %s did not demand a value (code=%d, stderr=%q); "+
			"pick another documented value-taking flag for this pin", flag, code, stderr)
	}
	if !acceptsToken(code, stderr, flag) {
		t.Errorf("a value demand for %q reads as a refusal, so every undocumented "+
			"value-taking flag is invisible to TestParserAcceptsNoUndocumentedFlag\nstderr: %s", flag, stderr)
	}
}

// TestParserAcceptsNoUndocumentedFlag is the reverse direction of the flag contract:
// for every command, every spelling the package's parsers know is either rejected as
// unknown, documented in that command's own help, or a global option.
//
// Together with TestHelpFlagsAreAcceptedByParser the pair is closed in both
// directions, so a flag added to a parser switch and nowhere else fails here — and
// therefore never reaches cli.json, the exported Markdown, or a site built from them
// while the suite stays green.
func TestParserAcceptsNoUndocumentedFlag(t *testing.T) {
	vocabulary := parserFlagVocabulary(t)
	globals := globalFlagTokens()

	for _, target := range parserTargets() {
		t.Run(target.label, func(t *testing.T) {
			// A real project, so commands that resolve one before parsing flags reach
			// their flag parser instead of failing early and making the probe vacuous.
			root := t.TempDir()
			if code, _, stderr := run("init", "--root", root); code != int(ExitSuccess) {
				t.Fatalf("seeding a project: exit %d, stderr=%q", code, stderr)
			}
			argv := func(tok string) []string {
				var args []string
				if target.rootable {
					args = append(args, "--root", root)
				}
				args = append(args, target.prefix...)
				return append(args, tok)
			}

			// Sentinel first: if this command does not reject an obviously unknown
			// flag, it never reaches flag parsing under this probe and every result
			// below would be meaningless rather than reassuring.
			const sentinel = "--zzz-not-a-real-flag"
			for _, shape := range probeShapes(sentinel) {
				code, _, stderr := run(argv(shape)...)
				if acceptsToken(code, stderr, sentinel) {
					t.Fatalf("sentinel %s not refused (code=%d, stderr=%q); the scan below would be vacuous", shape, code, stderr)
				}
			}

			documented := map[string]bool{}
			for _, tok := range commandFlagTokens(target.help) {
				documented[tok] = true
			}
			for _, flag := range vocabulary {
				if globals[flag] || documented[flag] {
					continue
				}
				for _, shape := range probeShapes(flag) {
					code, _, stderr := run(argv(shape)...)
					if !acceptsToken(code, stderr, flag) {
						continue
					}
					t.Errorf("the parser for %q accepts %q (probed as %q), which its --help documents nowhere: "+
						"it would be absent from cli.json, the exported reference, and any site built from them\nstderr: %s",
						target.label, flag, shape, stderr)
					break
				}
			}
		})
	}
}
