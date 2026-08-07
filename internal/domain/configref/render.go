package configref

import (
	"fmt"
	"strings"
)

// Template renders the annotated config template printed by `awa config template`
// and written by `awa config init`. Every key is shown COMMENTED OUT at its
// default, so uncommenting a line is how a user overrides it and the file never
// becomes a stale dump of every default. The header explains that an absent or
// empty config means product defaults.
//
// The long-form reference is rendered separately by MarkdownReference; this is
// the terse, machine-writable artifact.
func Template() string {
	var b strings.Builder
	b.WriteString("# awa configuration template.\n")
	b.WriteString("#\n")
	b.WriteString("# An absent or empty config means product defaults; a config file carries only\n")
	b.WriteString("# overrides. Every key below is shown commented out at its default value —\n")
	b.WriteString("# uncomment and change only what you want to override.\n")
	b.WriteString("#\n")
	b.WriteString("# Shared vs local: commit this as awa.toml at the project root to share policy\n")
	b.WriteString("# with everyone; keep private overrides in .awa/config.toml (ignored). See\n")
	b.WriteString("# `awa help config` for the full precedence and reference.\n")
	for _, s := range Sections() {
		b.WriteString("\n[")
		b.WriteString(s.Name)
		b.WriteString("]\n")
		if s.Intro != "" {
			writeWrappedComment(&b, s.Intro, "# ")
		}
		for _, f := range s.Fields {
			b.WriteString("# ")
			b.WriteString(f.Key)
			b.WriteString(" (")
			b.WriteString(f.Type)
			b.WriteString(") — ")
			b.WriteString(f.Summary)
			b.WriteString("\n")
			fmt.Fprintf(&b, "# %s = %s\n", f.Key, f.Default)
		}
	}
	return b.String()
}

// writeWrappedComment writes text wrapped to a comfortable width, prefixing every
// line with prefix. It keeps template output readable without embedding hard
// newlines in the reference summaries.
func writeWrappedComment(b *strings.Builder, text, prefix string) {
	const width = 74
	words := strings.Fields(text)
	line := prefix
	for _, w := range words {
		if len(line) > len(prefix) && len(line)+1+len(w) > width {
			b.WriteString(strings.TrimRight(line, " "))
			b.WriteString("\n")
			line = prefix
		}
		if len(line) > len(prefix) {
			line += " "
		}
		line += w
	}
	if strings.TrimSpace(line) != "" {
		b.WriteString(strings.TrimRight(line, " "))
		b.WriteString("\n")
	}
}
