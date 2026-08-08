package cli

import (
	"sort"
	"strings"

	"awarer/internal/app/help"
	"awarer/internal/output"
)

// runHelp handles "awa help [topic]". It serves the operational help pages from
// internal/app/help — workflow guidance for humans and coding agents, distinct
// from the terse parser usage that --help prints.
//
// Help is pure discovery: it declares no capabilities, so it never loads config,
// discovers a project root, opens .awa/, or takes a lock. It works outside an
// initialized project. Topic content lives entirely in internal/app/help, so
// this handler needs no access to the command registry.
func runHelp(w *output.Writer, inv invocation) error {
	if len(inv.args) == 0 {
		writeHelpIndex(w)
		return nil
	}

	topic := inv.args[0]
	if rest := inv.args[1:]; len(rest) > 0 {
		return rejectExtra("help", rest[0])
	}
	// help takes no flags: a leading-dash token is a flag error, not a topic, so
	// it reports the same way as every other command (see runVersion).
	if strings.HasPrefix(topic, "-") {
		return rejectExtra("help", topic)
	}

	if topic == "topics" {
		writeHelpTopics(w)
		return nil
	}

	page, ok := help.Resolve(topic)
	if !ok {
		return usageErrorf("unknown help topic %q\nrun 'awa help topics' to see available topics", topic)
	}
	// The body is canonical Markdown ending in exactly one newline; trimming it
	// before splitting prints the page verbatim without a trailing blank line, so
	// what the terminal shows and what the exporter publishes are the same bytes.
	for _, line := range strings.Split(strings.TrimSuffix(page.Body, "\n"), "\n") {
		w.Line(line)
	}
	return nil
}

// writeHelpIndex prints the concise landing page: what awa is, the best starting
// points, and the list of topics. It never requires an initialized project.
func writeHelpIndex(w *output.Writer) {
	w.Line("awa — worktree checkpoints, run cache, and supervised command history")
	w.Line("")
	w.Line("Operational help explains why and when to use each workflow, with copyable")
	w.Line("examples. For terse command syntax, use 'awa --help'.")
	w.Line("")
	w.Line("Start here:")
	// The start-here set is the retrieval spine: what awa is for an agent, how to
	// get and identify it, how to begin in a fresh project, how to structure a
	// loop, and what to do when something is wrong.
	w.Line("  awa help agents           workflow for coding agents (the golden path)")
	w.Line("  awa help install          download, verify, upgrade, identify the binary")
	w.Line("  awa help quickstart       first project and first review loop")
	w.Line("  awa help workflows        coding, review, specification, and fix loops")
	w.Line("  awa help troubleshooting  symptom-to-command decision paths")
	w.Line("")
	w.Line("topics:")
	width := slugColumnWidth()
	for _, t := range help.Topics() {
		w.Line("  " + pad(t.Slug.String(), width) + t.Summary)
	}
	w.Line("")
	w.Line("Run 'awa help <topic>', for example:")
	w.Line("  awa help status")
	w.Line("  awa help checkpoints")
	w.Line("  awa help diff")
	w.Line("  awa help refs")
	w.Line("  awa help exit-codes")
	w.Line("")
	w.Line("The version you are running, and its documentation as files:")
	w.Line("  awa version                            the installed version")
	w.Line("  awa docs export --output <directory>   the complete documentation bundle")
}

// slugColumnWidth is the listing column width, derived from the catalog rather
// than fixed: a longer slug than the widest one today must widen the column, not
// silently ragged-edge the listing. The trailing space separates the columns.
func slugColumnWidth() int {
	width := 0
	for _, t := range help.Topics() {
		if n := len(t.Slug.String()); n > width {
			width = n
		}
	}
	return width + 1
}

// writeHelpTopics prints the canonical topic slugs and their aliases. It is
// rendered from the same catalog the pages come from, so the listing can never
// drift from what "awa help <topic>" actually serves. Aliases are sorted so the
// listing is stable regardless of declaration order.
func writeHelpTopics(w *output.Writer) {
	w.Line("topics:")
	width := slugColumnWidth()
	for _, t := range help.Topics() {
		line := "  " + pad(t.Slug.String(), width) + t.Summary
		if len(t.Aliases) > 0 {
			a := append([]string(nil), t.Aliases...)
			sort.Strings(a)
			line += " (aliases: " + strings.Join(a, ", ") + ")"
		}
		w.Line(line)
	}
}
