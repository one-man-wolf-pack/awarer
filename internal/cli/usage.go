package cli

import (
	"awarer/internal/output"
)

// flagHelp is one command-local flag's help row: how it is written (as the user
// types it) and what it does. def names a default that matters, or "" to omit it.
// Global options (--json/--root/--config/--trust-mode) are never listed here — they
// come from the command's capabilities so command help can never drift from the
// validator.
type flagHelp struct {
	display string
	desc    string
	def     string
}

// commandHelp is the authored, per-command usage shown by "awa <command> --help".
// It is deliberately terse parser help: purpose, syntax, local flags, and one or
// two copyable examples. Longer workflow education lives in "awa help <topic>",
// named by topic. The summary (rendered as the title) and --json support are pulled
// from the command's existing registry metadata, not repeated here, so they cannot
// drift; usage/long/flags/examples are authored because the parser loops share no
// arity table to derive them from.
type commandHelp struct {
	usage    []string   // synopsis lines, without the leading "awa "
	long     string     // one or two sentences of purpose
	flags    []flagHelp // command-local flags only
	examples []string   // full "awa ..." example lines
	exitNote string     // unusual exit behavior; "" to omit
	topic    string     // matching "awa help <topic>" for the footer; "" to omit
}

// hasCap reports whether caps contains c. Used to derive "supports --json" for
// command help from the same capability set the validator enforces.
func hasCap(caps []capability, c capability) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}

// unionCapabilities is the coarse capability set a subcommand-owning command
// declares: the union of what its subcommands honor, in the canonical
// allCapabilities order. route validates against it, and each subcommand's own
// precise set is then applied by dispatchSubcommand. Deriving it keeps the
// parent's declaration from drifting when a subcommand gains or loses a global.
func unionCapabilities(subs []subcommand) []capability {
	var caps []capability
	for _, c := range allCapabilities {
		for _, sc := range subs {
			if hasCap(sc.caps, c) {
				caps = append(caps, c)
				break
			}
		}
	}
	return caps
}

// subcommandHelp returns the "--help" body for "<cmdName> <sc.name>": the authored
// help if the subcommand has any, else thin help derived from its summary. The
// fallback guarantees a subcommand always renders a usable page rather than an
// empty one, so a subcommand whose contract is fully described by its summary need
// not hand-author a page. Both the run and config help paths render through this.
func subcommandHelp(cmdName string, sc subcommand) commandHelp {
	if len(sc.help.usage) > 0 {
		return sc.help
	}
	return commandHelp{
		usage:    []string{cmdName + " " + sc.name},
		long:     sc.summary + ".",
		examples: []string{"awa " + cmdName + " " + sc.name},
	}
}

// writeSubcommandOwnerHelp renders the "--help" frame of a command that owns
// subcommands: its synopsis, purpose, and the subcommand list read from the one
// registry that also drives dispatch, so the listing cannot advertise a verb the
// command does not route. The column width is derived from the longest name, so
// adding a subcommand cannot leave the list misaligned.
func writeSubcommandOwnerHelp(w *output.Writer, name string, h commandHelp, subs []subcommand) {
	w.Line("awa " + name)
	w.Line("")
	w.Line("usage:")
	for _, u := range h.usage {
		w.Line("  awa " + u)
	}
	if h.long != "" {
		w.Line("")
		w.Line(h.long)
	}
	width := 0
	for _, sc := range subs {
		if len(sc.name) > width {
			width = len(sc.name)
		}
	}
	w.Line("")
	w.Line("subcommands:")
	for _, sc := range subs {
		w.Line("  " + pad(sc.name, width+1) + sc.summary)
	}
	w.Line("")
	w.Line("global options: run 'awa --help'")
}

// writeCommandHelp renders "awa <command> --help" from the command's registry
// metadata: the summary as the title and the authored help body. It derives --json
// support from the command's capabilities so it can never advertise JSON a command
// does not accept.
func writeCommandHelp(w *output.Writer, c *command) {
	writeHelpBody(w, "awa "+c.name, c.help, hasCap(c.capabilities, capJSON))
}

// writeHelpBody renders a commandHelp under a title, shared by the registry commands
// and the run subcommands (which are not in the registry) so both read identically.
// jsonSupported drives the single "supports --json" line.
func writeHelpBody(w *output.Writer, title string, h commandHelp, jsonSupported bool) {
	w.Line(title)
	w.Line("")
	w.Line("usage:")
	for _, u := range h.usage {
		w.Line("  awa " + u)
	}
	if h.long != "" {
		w.Line("")
		w.Line(h.long)
	}
	if len(h.flags) > 0 {
		w.Line("")
		w.Line("flags:")
		for _, f := range h.flags {
			line := "  " + pad(f.display, 26) + f.desc
			if f.def != "" {
				line += " (default: " + f.def + ")"
			}
			w.Line(line)
		}
	}
	if jsonSupported {
		w.Line("")
		w.Line("supports --json (schema-versioned output)")
	}
	if h.exitNote != "" {
		w.Line("")
		w.Line(h.exitNote)
	}
	w.Line("")
	w.Line("global options: run 'awa --help'")
	if len(h.examples) > 0 {
		w.Line("")
		w.Line("examples:")
		for _, e := range h.examples {
			w.Line("  " + e)
		}
	}
	if h.topic != "" {
		w.Line("")
		w.Linef("See 'awa help %s' for the workflow.", h.topic)
	}
}

// writeUsage prints concise help to stdout. Commands and global options are
// rendered from the same metadata the validator uses (the command registry and
// the capability catalog), so help can never advertise a command or flag that
// the validator rejects. The catalog holds shipped capabilities only — a test
// pins that every one of them is accepted by a real top-level command, the set
// route validates an invocation against — so each is rendered as usable. Stdout
// write failures are surfaced centrally by Run via w.Err(), so this writes
// freely without threading errors.
func (r *router) writeUsage(w *output.Writer) {
	w.Line("awa — worktree checkpoints, run cache, and supervised command history")
	w.Line("")
	w.Line("usage:")
	w.Line("  awa [global options] <command> [arguments]")
	w.Line("")
	w.Line("commands:")
	for _, c := range r.commands {
		w.Linef("  %-10s %s", c.name, c.summary)
	}
	w.Line("")
	w.Line("config subcommands:")
	for _, sc := range configSubcommands {
		w.Linef("  config %-9s %s", sc.name, sc.summary)
	}
	w.Line("")
	w.Line("global options:")
	for _, cap := range allCapabilities {
		info := capabilityInfo[cap]
		w.Line("  " + pad(info.display(), 32) + info.help)
	}
	w.Line("  " + pad("-h, --help", 32) + "show this help")
	w.Line("")
	w.Line("Run 'awa help agents' for agent workflows, or 'awa help topics' for all topics.")
}

// pad right-pads s with spaces to at least n columns (plus a trailing space),
// leaving a gap before the help text without truncating long option displays.
func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s + " "
}
