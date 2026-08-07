package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"awarer/internal/app/help"
)

// The CLI reference is a deterministic, machine-readable projection of awa's
// public surface — commands, subcommands, their usage synopses (positional/range/
// path and mutually-exclusive syntax), operand acceptance, accepted global-option
// capabilities, local flags, global options, the awa-owned exit codes plus the
// "awa run" child-passthrough contract, and help topics. It exists so the public docs
// have a single checked input to derive or verify a command reference against,
// instead of copying the surface by hand.
//
// Everything here is DERIVED from the live registries the parser and help
// already use (newRouter and each command's subcommands, capabilityInfo,
// exitCodes, help.Topics/Aliases). It is not a second, hand-maintained catalog:
// the only per-flag data it reads are the authored flagHelp rows, and a drift
// test proves those agree with the real parser. Output is stable — registry
// slice order is preserved and JSON map keys (topic aliases) are emitted sorted
// by encoding/json — so two runs are byte-identical; it reads no project, env,
// git, clock, or filesystem state.

// referenceSchemaVersion is the schema version of the emitted document. Bump it
// only on an incompatible shape change, so a consumer can pin what it reads.
const referenceSchemaVersion = 1

// flagTokenRe matches a flag spelling (short or long) at a word boundary.
var flagTokenRe = regexp.MustCompile(`(?:^|[\s,])(-{1,2}[A-Za-z0-9][A-Za-z0-9-]*)`)

// flagHelpTokens returns the flag spellings a flagHelp.display row documents,
// read from the authored label (never from rendered --help output):
//
//	"-n, --limit <count>" -> ["-n","--limit"]
//	"--stat"              -> ["--stat"]
//	"-1"                  -> ["-1"]
//
// A value placeholder (<...>) ends the flag spellings on a row; everything after
// it is a value name or prose, so the label is truncated there before matching.
func flagHelpTokens(display string) []string {
	if i := strings.IndexByte(display, '<'); i >= 0 {
		display = display[:i]
	}
	var out []string
	for _, m := range flagTokenRe.FindAllStringSubmatch(display, -1) {
		out = append(out, m[1])
	}
	return out
}

// commandFlagTokens returns the union of every flag spelling a command's help
// documents, in help-row order. It is the machine view of a command's local
// flags that the drift test probes against the parser.
func commandFlagTokens(h commandHelp) []string {
	var out []string
	for _, f := range h.flags {
		out = append(out, flagHelpTokens(f.display)...)
	}
	return out
}

// reference is the root of the emitted document.
type reference struct {
	SchemaVersion int                `json:"schema_version"`
	Commands      []referenceCommand `json:"commands"`
	GlobalOptions []referenceGlobal  `json:"global_options"`
	ExitCodes     referenceExitCodes `json:"exit_codes"`
	HelpTopics    referenceTopics    `json:"help_topics"`
}

// referenceCommand is one command or subcommand. Usage carries the authored
// synopsis lines — the positional/range/path and mutually-exclusive-mode syntax
// the parser accepts, which no flag list captures. Operands names what the command
// does with tokens after "--" — rejects them, reads them as path filters, or takes
// them as a wrapped command line. It is published for subcommands too, which do
// NOT inherit their parent's: "awa run" takes a wrapped command line while
// "awa run show" takes none. Capabilities are the global-option
// ids this command/subcommand honors, in canonical order — so a consumer sees, for
// example, that run ls honors --trust-mode while run log does not, instead of only
// a --json bool.
// Topic names the operational help page that explains the command's workflow
// ("awa help <topic>"), omitted when the command has no matching page. It is the
// same authored pointer the rendered --help footer uses, published so generated
// documentation can link syntax to workflow guidance instead of restating it.
type referenceCommand struct {
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Usage   []string `json:"usage,omitempty"`
	Topic   string   `json:"topic,omitempty"`
	// ExitNote is the command's authored exit-status contract, present only for the
	// commands whose exit code means something beyond the shared catalog: doctor's
	// verdict and run's pass-through of the child's code. It is projected here
	// because it is part of the terse help, and a published reference that omitted
	// it would be less complete than `awa <command> --help`.
	ExitNote     string             `json:"exit_note,omitempty"`
	Operands     string             `json:"operands,omitempty"`
	Capabilities []string           `json:"capabilities"`
	Flags        []referenceFlag    `json:"flags,omitempty"`
	Subcommands  []referenceCommand `json:"subcommands,omitempty"`
}

// referenceFlag is one command-local flag, projected from its authored help row.
// Value is the placeholder a value-taking flag expects, brackets included (e.g.
// "<mode>"), so a consumer can restate the flag's real syntax instead of only
// knowing that some value is required.
type referenceFlag struct {
	Spellings  []string `json:"spellings"`
	Desc       string   `json:"desc"`
	Default    string   `json:"default,omitempty"`
	TakesValue bool     `json:"takes_value"`
	Value      string   `json:"value,omitempty"`
}

// flagValueName returns the value placeholder in an authored flag label, with its
// angle brackets, or "" for a boolean flag. It reads the authored label, never
// rendered --help output, so it stays a projection of the same metadata the
// parser drift test probes.
func flagValueName(display string) string {
	start := strings.IndexByte(display, '<')
	if start < 0 {
		return ""
	}
	end := strings.IndexByte(display[start:], '>')
	if end < 0 {
		return ""
	}
	return display[start : start+end+1]
}

// referenceGlobal is one global option: its capability id (empty for -h/--help,
// which the parser handles specially rather than as a capability), each spelling
// and whether it takes a value, and its help.
type referenceGlobal struct {
	Capability string              `json:"capability,omitempty"`
	Spellings  []referenceSpelling `json:"spellings"`
	Help       string              `json:"help"`
}

// referenceSpelling is one way to write a global option.
type referenceSpelling struct {
	Flag  string `json:"flag"`
	Value string `json:"value,omitempty"`
}

// referenceExitCodes models the whole process-exit contract: the codes awa itself
// returns, and the separate "awa run" passthrough of a wrapped child's exit — which
// is why an exit in 1-6 is not necessarily awa-origin.
type referenceExitCodes struct {
	AwaOwned     []referenceExitCode `json:"awa_owned"`
	RunChildExit referenceRunExit    `json:"run_child_exit"`
}

// referenceExitCode is one awa-owned process exit status.
type referenceExitCode struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

// referenceRunExit describes how "awa run" reports a wrapped command's exit: it
// passes the child's own code through (so it can overlap awa's 1-6 range), and the
// origin is disambiguated by a machine field in the run envelope.
type referenceRunExit struct {
	Passthrough      bool                  `json:"passthrough"`
	OverlapsAwaRange bool                  `json:"overlaps_awa_range"`
	OriginField      string                `json:"origin_field"`
	Interruption     referenceInterruption `json:"interruption"`
}

// referenceInterruption records the two interruption exits: a normal command maps a
// SIGINT to awa's own 130, while "awa run" passes through the killed child's own
// signal-derived code instead.
type referenceInterruption struct {
	NormalCommand int    `json:"normal_command"`
	WrappedChild  string `json:"wrapped_child"`
}

// referenceTopics is the operational help surface.
type referenceTopics struct {
	Canonical []referenceTopic  `json:"canonical"`
	Aliases   map[string]string `json:"aliases"`
}

// referenceTopic is one canonical operational help topic.
type referenceTopic struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

// buildReference assembles the document by walking the live registries. Ordering
// is inherited from the registry slices; nothing here reads external state.
func buildReference() reference {
	r := newRouter()
	ref := reference{
		SchemaVersion: referenceSchemaVersion,
		HelpTopics: referenceTopics{
			Aliases: help.Aliases(),
		},
	}

	for _, c := range r.commands {
		ref.Commands = append(ref.Commands, referenceCommand{
			Name:         c.name,
			Summary:      c.summary,
			Usage:        append([]string(nil), c.help.usage...),
			Topic:        c.help.topic,
			ExitNote:     c.help.exitNote,
			Operands:     c.operands.String(),
			Capabilities: capabilityIDs(c.capabilities),
			Flags:        referenceFlags(c.help.flags),
			Subcommands:  referenceSubcommands(c.name, c.subcommands),
		})
	}

	for _, cap := range allCapabilities {
		info := capabilityInfo[cap]
		ref.GlobalOptions = append(ref.GlobalOptions, referenceGlobal{
			Capability: string(cap),
			Spellings:  referenceSpellings(info.spellings),
			Help:       info.help,
		})
	}
	// -h/--help is a real global the binary advertises but is not a capability (the
	// parser handles it specially), so publish it explicitly with no capability id.
	ref.GlobalOptions = append(ref.GlobalOptions, referenceGlobal{
		Spellings: []referenceSpelling{{Flag: "-h"}, {Flag: "--help"}},
		Help:      "show help",
	})

	ref.ExitCodes = referenceExitCodes{
		RunChildExit: referenceRunExit{
			Passthrough:      true,
			OverlapsAwaRange: true,
			OriginField:      "data.run.exit_origin",
			Interruption: referenceInterruption{
				NormalCommand: int(ExitInterrupted),
				WrappedChild:  "child signal-derived (e.g. 137 for SIGKILL)",
			},
		},
	}
	for _, code := range exitCodes() {
		ref.ExitCodes.AwaOwned = append(ref.ExitCodes.AwaOwned, referenceExitCode{
			Code: int(code),
			Name: code.refName(),
		})
	}

	for _, t := range help.Topics() {
		ref.HelpTopics.Canonical = append(ref.HelpTopics.Canonical, referenceTopic{
			Name:    t.Slug.String(),
			Summary: t.Summary,
		})
	}

	return ref
}

// referenceSubcommands projects a command's owned subcommands, in registry order.
// Usage comes from the same authored-or-thin help the binary renders (so even a
// summary-only subcommand carries its synopsis); a subcommand with authored help
// also contributes its flag rows (e.g. run ls's flags, config init's
// --shared/--local).
func referenceSubcommands(cmdName string, subs []subcommand) []referenceCommand {
	if len(subs) == 0 {
		return nil
	}
	out := make([]referenceCommand, 0, len(subs))
	for _, sc := range subs {
		out = append(out, referenceCommand{
			Name:         sc.name,
			Summary:      sc.summary,
			Usage:        subcommandHelp(cmdName, sc).usage,
			Topic:        sc.help.topic,
			ExitNote:     sc.help.exitNote,
			Operands:     sc.operands.String(),
			Capabilities: capabilityIDs(sc.caps),
			Flags:        referenceFlags(sc.help.flags),
		})
	}
	return out
}

// capabilityIDs lists the capability ids in caps, in the canonical allCapabilities
// order (not declaration order), so two commands honoring the same set render it
// identically.
func capabilityIDs(caps []capability) []string {
	var out []string
	for _, c := range allCapabilities {
		if hasCap(caps, c) {
			out = append(out, string(c))
		}
	}
	return out
}

// referenceSpellings projects a capability's structured spellings for the reference.
func referenceSpellings(spellings []capSpelling) []referenceSpelling {
	out := make([]referenceSpelling, 0, len(spellings))
	for _, s := range spellings {
		out = append(out, referenceSpelling{Flag: s.flag, Value: s.value})
	}
	return out
}

// referenceFlags projects authored help rows into machine flag entries.
func referenceFlags(flags []flagHelp) []referenceFlag {
	if len(flags) == 0 {
		return nil
	}
	out := make([]referenceFlag, 0, len(flags))
	for _, f := range flags {
		out = append(out, referenceFlag{
			Spellings:  flagHelpTokens(f.display),
			Desc:       f.desc,
			Default:    f.def,
			TakesValue: strings.ContainsRune(f.display, '<'),
			Value:      flagValueName(f.display),
		})
	}
	return out
}

// WriteReferenceJSON renders the complete CLI reference as deterministic JSON to
// w. It marshals fully into a buffer first, so a marshal failure writes nothing;
// but this is a plain streaming write, not an atomic publish — a short write to w
// (a full disk, a broken pipe) can still leave a truncated stream. Publishing the
// canonical golden atomically is the caller's job (see `just reference-update`).
// HTML escaping is disabled so value placeholders like <count> stay readable; the
// output is byte-identical across runs and requires no project.
func WriteReferenceJSON(w io.Writer) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(buildReference()); err != nil {
		return err
	}
	// enc.Encode already appends a trailing newline.
	_, err := w.Write(buf.Bytes())
	return err
}
