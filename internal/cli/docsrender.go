package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file renders the generated half of the exported documentation bundle:
// the command pages, the global-option page, and the exit-code page. Every fact
// here is projected from the same live reference model the CLI reference JSON is
// built from (buildReference, which walks the router registry, the capability
// catalog, and the exit-code catalog), so a generated page cannot state a flag,
// default, capability, or exit status the binary does not actually implement.
//
// Nothing is parsed from rendered --help text and no flag list is restated by
// hand. Authored topic pages explain workflows; these pages own the exhaustive
// syntax tables and link back to the matching topic instead of repeating it.
//
// Rendering is deterministic: it walks ordered slices only, and reads no clock,
// environment, filesystem, or terminal width.

// The bundle-relative locations of the generated documents. Every renderer that
// links to one and the assembler that publishes it read these, so a page cannot
// be published at one path and linked at another. Authored topics carry their own
// paths from the help catalog, so no topics directory is named here.
const (
	docsCommandsDir  = "commands"
	docsReferenceDir = "reference"

	docsCommandIndexPath  = docsCommandsDir + "/index.md"
	docsGlobalOptionsPath = docsReferenceDir + "/global-options.md"
	docsExitCodesPath     = docsReferenceDir + "/exit-codes.md"
	docsMachineRefPath    = docsReferenceDir + "/cli.json"
)

// docsBuilder accumulates a Markdown document. It exists so every generated page
// applies the same block spacing rules — exactly one blank line between blocks
// and exactly one trailing newline — instead of each renderer tracking newlines.
type docsBuilder struct {
	b strings.Builder
}

// heading writes an ATX heading at the given level.
func (d *docsBuilder) heading(level int, text string) {
	d.gap()
	d.b.WriteString(strings.Repeat("#", level) + " " + text + "\n")
}

// para writes one paragraph.
func (d *docsBuilder) para(text string) {
	d.gap()
	d.b.WriteString(text + "\n")
}

// bullet writes one list item. Consecutive bullets form one list; a bullet that
// follows any other block starts a new one after a blank line.
func (d *docsBuilder) bullet(text string) {
	if !d.lastLineIsListItem() {
		d.gap()
	}
	d.b.WriteString("- " + text + "\n")
}

// subBullet writes a nested list item under the preceding bullet.
func (d *docsBuilder) subBullet(text string) {
	d.b.WriteString("  - " + text + "\n")
}

// code writes a fenced block, one line per entry.
func (d *docsBuilder) code(lines []string) {
	d.gap()
	d.b.WriteString("```text\n")
	for _, l := range lines {
		d.b.WriteString(l + "\n")
	}
	d.b.WriteString("```\n")
}

// gap separates the next block from the previous one with exactly one blank line.
func (d *docsBuilder) gap() {
	if d.b.Len() == 0 {
		return
	}
	if !strings.HasSuffix(d.b.String(), "\n\n") {
		d.b.WriteString("\n")
	}
}

// lastLineIsListItem reports whether the document currently ends with a list item,
// so a following bullet joins the same list.
func (d *docsBuilder) lastLineIsListItem() bool {
	s := d.b.String()
	if s == "" || !strings.HasSuffix(s, "\n") || strings.HasSuffix(s, "\n\n") {
		return false
	}
	s = strings.TrimSuffix(s, "\n")
	last := s[strings.LastIndex(s, "\n")+1:]
	return strings.HasPrefix(last, "- ") || strings.HasPrefix(last, "  - ")
}

// String returns the document, guaranteed to end in exactly one newline.
func (d *docsBuilder) String() string {
	return strings.TrimRight(d.b.String(), "\n") + "\n"
}

// htmlOpener matches the one thing that changes meaning when terminal help text
// is embedded in Markdown prose: a "<" that begins an HTML tag, an autolink, a
// comment, or a processing instruction. Registry help is written for a terminal,
// where "<count>" is an ordinary placeholder; dropped into Markdown unescaped it
// parses as inline HTML and every renderer silently swallows it, so a published
// page reads "show at most  entries".
var htmlOpener = regexp.MustCompile(`<([A-Za-z/!?])`)

// mdProse prepares a registry-sourced string for use as Markdown prose. The
// escape is deliberately narrow: only "<" is ambiguous in these strings, and
// escaping every punctuation character the CommonMark spec permits would fill the
// exported corpus with backslashes for an audience that reads it as plain text.
// A "\<" renders as a literal "<", so the published page says exactly what the
// terminal says.
//
// It is applied where help text becomes prose, never to the syntax spellings —
// those are already wrapped in backticks, where nothing is interpreted.
func mdProse(s string) string {
	return htmlOpener.ReplaceAllString(s, `\<$1`)
}

// commandDocSlug is the stable slug of a generated command page. Command names
// overlap topic slugs (run, diff, config, gc, doctor), so the prefix keeps the
// two namespaces disjoint inside one manifest.
func commandDocSlug(name string) string { return "command-" + name }

// commandDocPath is the bundle-relative path of a generated command page.
func commandDocPath(name string) string { return docsCommandsDir + "/" + name + ".md" }

// topicLink renders a link from a generated page in dir to an operational topic
// page, using the topic's own bundle path so the link is correct in the published
// tree rather than assumed.
func topicLink(fromDir, topicSlug, topicPath string) string {
	return fmt.Sprintf("[awa help %s](%s)", topicSlug, relativeDocLink(fromDir, topicPath))
}

// relativeDocLink expresses the bundle-relative target as a link from a document
// in fromDir. It is written for any nesting depth rather than assuming the
// current one-level layout, so moving a generated page into a deeper directory
// cannot silently produce links that resolve one level off.
func relativeDocLink(fromDir, target string) string {
	from := splitPathSegments(fromDir)
	to := splitPathSegments(target)
	shared := 0
	// The last segment of target is the file, so only its directories can be
	// shared with fromDir.
	for shared < len(from) && shared < len(to)-1 && from[shared] == to[shared] {
		shared++
	}
	segments := make([]string, 0, len(from)-shared+len(to)-shared)
	for i := shared; i < len(from); i++ {
		segments = append(segments, "..")
	}
	segments = append(segments, to[shared:]...)
	return strings.Join(segments, "/")
}

// splitPathSegments splits a bundle-relative slash path into its segments; the
// bundle root is the empty path and has none.
func splitPathSegments(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// globalOptionSpellings renders the ways one global option may be written. It is
// the only place a global option's syntax is turned into prose, and it reads the
// reference projection rather than the live capability catalog, so every page in
// the bundle states the option identically and states what the reference
// publishes.
func globalOptionSpellings(g referenceGlobal) string {
	parts := make([]string, 0, len(g.Spellings))
	for _, s := range g.Spellings {
		text := s.Flag
		if s.Value != "" {
			text += " " + s.Value
		}
		parts = append(parts, "`"+text+"`")
	}
	return strings.Join(parts, " / ")
}

// renderCommandIndex renders the command index: every top-level command with its
// summary and a link to its page.
func renderCommandIndex(ref reference) string {
	var d docsBuilder
	d.heading(1, "awa command reference")
	d.para("Every command of the installed version. Each page owns the exhaustive syntax for its command; the workflow guidance lives in the operational topics.")
	d.heading(2, "Commands")
	for _, c := range ref.Commands {
		d.bullet(fmt.Sprintf("[`awa %s`](%s) — %s", c.Name,
			relativeDocLink(docsCommandsDir, commandDocPath(c.Name)), mdProse(c.Summary)))
	}
	d.heading(2, "See also")
	d.bullet("[global options](" + relativeDocLink(docsCommandsDir, docsGlobalOptionsPath) + ")")
	d.bullet("[exit codes](" + relativeDocLink(docsCommandsDir, docsExitCodesPath) + ")")
	return d.String()
}

// renderCommandPage renders one command's exhaustive syntax page: synopsis,
// operand semantics, accepted global options, local flags with their defaults,
// and every subcommand with its own capability and flag set.
func renderCommandPage(ref reference, c referenceCommand, topicPaths map[string]string) string {
	var d docsBuilder
	d.heading(1, "awa "+c.Name)
	d.para(mdProse(c.Summary) + ".")
	writeUsageBlock(&d, c.Usage)

	writeOperandSection(&d, 2, c.Operands)

	writeGlobalOptionsSection(&d, 2, ref, c.Capabilities)
	writeFlagsSection(&d, 2, c.Flags)
	writeExitNoteSection(&d, 2, c.ExitNote)

	if len(c.Subcommands) > 0 {
		d.heading(2, "Subcommands")
		for _, sc := range c.Subcommands {
			d.heading(3, "awa "+c.Name+" "+sc.Name)
			d.para(mdProse(sc.Summary) + ".")
			writeUsageBlock(&d, sc.Usage)
			writeOperandSection(&d, 4, sc.Operands)
			writeGlobalOptionsSection(&d, 4, ref, sc.Capabilities)
			writeFlagsSection(&d, 4, sc.Flags)
			writeExitNoteSection(&d, 4, sc.ExitNote)
			writeWorkflowSection(&d, 4, docsCommandsDir, sc.Topic, topicPaths)
		}
	}

	writeWorkflowSection(&d, 2, docsCommandsDir, c.Topic, topicPaths)
	return d.String()
}

// writeGlobalOptionsSection states which global options the command or subcommand
// honors, derived from its capability set rather than authored, so it can never
// advertise a global the validator rejects. It walks the published option list and
// keeps the accepted ones, so the order matches the global-options page and no
// capability id can fail to resolve.
func writeGlobalOptionsSection(d *docsBuilder, level int, ref reference, caps []string) {
	d.heading(level, "Global options")
	if len(caps) == 0 {
		d.para("Accepts no global options other than `-h` / `--help`.")
		return
	}
	for _, g := range ref.GlobalOptions {
		if g.Capability == "" || !containsString(caps, g.Capability) {
			continue
		}
		d.bullet(globalOptionSpellings(g) + " — " + mdProse(g.Help))
	}
	if containsString(caps, string(capJSON)) {
		d.para("Supports `--json` (schema-versioned output).")
	}
}

// operandProse states what a command does with tokens after "--", one sentence per
// published kind. The heading names the terminator rather than "operands" because
// a reader is looking up what "-- something" does, and the sentences distinguish
// path filters from a wrapped command line — the distinction a boolean lost.
//
// An unrecognized kind renders nothing rather than guessing; a drift test proves
// every command page states its registry kind, so a new kind cannot ship silent.
func operandProse(kind string) string {
	switch kind {
	case operandNone.String():
		return "A token after `--` is a usage error: this command takes no operands."
	case operandPathFilter.String():
		return "Tokens after `--` are path filters. They are resolved relative to your current directory and normalized to the project root, and they narrow the output only — they never create a partial state."
	case operandWrappedCommand.String():
		return "Tokens after `--` are the wrapped command and its arguments; awa passes them through unchanged."
	default:
		return ""
	}
}

// writeOperandSection renders the terminator contract for a command or one of its
// subcommands. A subcommand states its own: "awa run" takes a wrapped command line
// while "awa run show" takes none, and a page that showed only the parent's would
// document syntax the parser rejects.
func writeOperandSection(d *docsBuilder, level int, kind string) {
	prose := operandProse(kind)
	if prose == "" {
		return
	}
	d.heading(level, "Tokens after `--`")
	d.para(prose)
}

// writeUsageBlock renders a synopsis as a fenced block, shared by commands and
// subcommands so both state their syntax the same way. Nothing is written when
// the registry carries no synopsis.
func writeUsageBlock(d *docsBuilder, usage []string) {
	if len(usage) == 0 {
		return
	}
	lines := make([]string, 0, len(usage))
	for _, u := range usage {
		lines = append(lines, "awa "+u)
	}
	d.code(lines)
}

// writeExitNoteSection renders a command's authored exit-status contract, for the
// commands whose exit code carries meaning the shared catalog does not: doctor's
// exit code is its verdict, and run's is the wrapped child's. The terse `--help`
// prints this, so the published page must too — the exported reference may never
// be less complete than the binary's own help.
//
// The note is printed verbatim, keeping its own "exit code:" lead under the heading.
// That is a deliberate accepted redundancy, not an oversight: the heading names the
// section while the lead marks the specific CLI fact, so a fragment retrieved on its
// own still says what it is about. Moving the label into each renderer would reshape
// the terse help — a stable pinned surface — for no gain in clarity.
func writeExitNoteSection(d *docsBuilder, level int, note string) {
	if note == "" {
		return
	}
	d.heading(level, "Exit status")
	d.para(mdProse(note))
}

// writeFlagsSection renders the command-local flags with their defaults. Rows come
// from the authored flagHelp metadata that the parser drift test probes, so a flag
// documented here is a flag the parser accepts.
func writeFlagsSection(d *docsBuilder, level int, flags []referenceFlag) {
	if len(flags) == 0 {
		return
	}
	d.heading(level, "Flags")
	for _, f := range flags {
		syntax := strings.Join(f.Spellings, ", ")
		if f.Value != "" {
			syntax += " " + f.Value
		}
		line := "`" + syntax + "`"
		if f.TakesValue && f.Value == "" {
			line += " (takes a value)"
		}
		line += " — " + mdProse(f.Desc)
		if f.Default != "" {
			line += " (default: `" + f.Default + "`)"
		}
		d.bullet(line)
	}
}

// writeWorkflowSection links to the operational topic that explains the command,
// when the registry names one. fromDir is the directory of the page being
// rendered, because a relative link is only correct relative to its own document.
func writeWorkflowSection(d *docsBuilder, level int, fromDir, topic string, topicPaths map[string]string) {
	path, ok := topicPaths[topic]
	if !ok {
		return
	}
	d.heading(level, "Workflow")
	d.para("See " + topicLink(fromDir, topic, path) + ".")
}

// renderGlobalOptions renders the global-option page: each option's spellings,
// help, and the commands that accept it, all read from the live catalog and
// registry.
func renderGlobalOptions(ref reference) string {
	var d docsBuilder
	d.heading(1, "awa global options")
	d.para("Global options may appear anywhere on the command line. A command accepts only the options listed on its own page; passing an unaccepted global is a usage error rather than a silently ignored flag.")
	d.heading(2, "Options")
	for _, g := range ref.GlobalOptions {
		d.bullet(globalOptionSpellings(g) + " — " + mdProse(g.Help))
		if g.Capability == "" {
			d.subBullet("Handled by the parser for every command.")
			continue
		}
		d.subBullet("Accepted by: " + backtickList(commandsAccepting(ref, g.Capability)) + ".")
	}
	d.heading(2, "See also")
	d.bullet("[command reference](" + relativeDocLink(docsReferenceDir, docsCommandIndexPath) + ")")
	d.bullet("[exit codes](" + relativeDocLink(docsReferenceDir, docsExitCodesPath) + ")")
	return d.String()
}

// commandsAccepting lists the top-level commands honoring a capability, in
// registry order. A command whose subcommands honor it also counts, because the
// command's own capability set is the union its router validates against.
func commandsAccepting(ref reference, id string) []string {
	var out []string
	for _, c := range ref.Commands {
		if containsString(c.Capabilities, id) {
			out = append(out, c.Name)
		}
	}
	return out
}

// renderExitCodes renders the process-exit contract: the awa-owned catalog and
// the separate "awa run" child passthrough, both from the typed model.
func renderExitCodes(ref reference, topicPaths map[string]string) string {
	var d docsBuilder
	d.heading(1, "awa exit codes")
	d.para("The exact process-exit contract. Each code has a stable number and a stable machine name; the meanings and the diagnosis guidance live in the operational topic linked below.")

	d.heading(2, "awa-owned exit statuses")
	for _, c := range ref.ExitCodes.AwaOwned {
		d.bullet("`" + strconv.Itoa(c.Code) + "` — `" + c.Name + "`")
	}

	d.heading(2, "Wrapped child exits")
	run := ref.ExitCodes.RunChildExit
	if run.Passthrough {
		d.para("`awa run` returns the wrapped command's own exit code rather than an awa code.")
	}
	if run.OverlapsAwaRange {
		d.para("A child's code can overlap awa's own range, so the exit status alone does not identify the origin. The run envelope disambiguates it in `" + run.OriginField + "`.")
	}
	d.bullet("Interruption of a normal command: `" + strconv.Itoa(run.Interruption.NormalCommand) + "`.")
	d.bullet("Interruption of a wrapped child: " + run.Interruption.WrappedChild + ".")

	d.heading(2, "See also")
	if path, ok := topicPaths["exit-codes"]; ok {
		d.bullet(topicLink(docsReferenceDir, "exit-codes", path))
	}
	d.bullet("[global options](" + relativeDocLink(docsReferenceDir, docsGlobalOptionsPath) + ")")
	return d.String()
}

// backtickList renders names as a comma-separated inline-code list.
func backtickList(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "`"+n+"`")
	}
	return strings.Join(out, ", ")
}

// containsString reports whether list contains want.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
