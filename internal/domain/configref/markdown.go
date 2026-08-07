package configref

import (
	"fmt"
	"strings"
)

// This file holds the canonical Markdown rendering of the configuration
// reference. It is the single long-form config document awa carries: `awa help
// config` prints it verbatim and `awa docs export` publishes the same bytes as
// reference/configuration.md, so there is exactly one configuration page rather
// than a terminal copy and a documentation copy that can disagree.
//
// The shared blocks below (precedence, effect-roots-vs-excludes, pattern
// semantics) are also spliced into the run and ignores topics through the
// checked awa:include directive in internal/app/help. They therefore return a
// self-contained Markdown fragment starting at an H2 heading and ending WITHOUT
// a trailing newline, so an include site can substitute one directive line for
// one block with no whitespace bookkeeping at the call site.

// markdownWidth is the wrap column for rendered prose. Wrapping is done here
// rather than left to the renderer because the same bytes are printed verbatim
// to a terminal by `awa help config`.
const markdownWidth = 78

// MarkdownReference renders the complete configuration document from the
// reference model: layer precedence, the discovery commands, the section/key
// reference with types, defaults, and when-to-change guidance, the
// effect-roots-vs-excludes decision, and the pattern semantics. Defaults come
// from config.Defaults() through Sections(), so the document cannot drift from
// the decoder. It is self-contained: an agent needs no source or website access.
func MarkdownReference() string {
	var b strings.Builder
	b.WriteString("# awa config — configuration schema, precedence, and when to change it\n")
	b.WriteString("\n")
	b.WriteString(PrecedenceMarkdown())
	b.WriteString("\n\n")

	b.WriteString("## Discover and manage config from the binary\n")
	b.WriteString("\n")
	b.WriteString("```text\n")
	b.WriteString("awa config template          annotated template (redirect to a file)\n")
	b.WriteString("awa config init --shared     write a committable awa.toml\n")
	b.WriteString("awa config init --local      write a private .awa/config.toml\n")
	b.WriteString("awa config path              show the shared and local paths and which exist\n")
	b.WriteString("awa config show              print a layer's raw contents\n")
	b.WriteString("awa config effective         the composed config plus each value's origin layer\n")
	b.WriteString("```\n")
	b.WriteString("\n")

	b.WriteString("## Reference\n")
	b.WriteString("\n")
	b.WriteString("Defaults are shown; an absent key keeps its default.\n")
	for _, s := range Sections() {
		b.WriteString("\n### [")
		b.WriteString(s.Name)
		b.WriteString("] — ")
		b.WriteString(s.Title)
		b.WriteString("\n\n")
		if s.Intro != "" {
			writeWrapped(&b, s.Intro, "", "")
			b.WriteString("\n")
		}
		for _, f := range s.Fields {
			fmt.Fprintf(&b, "- **`%s`** (%s, default: `%s`)\n", f.Key, f.Type, f.Default)
			writeWrapped(&b, f.Summary, "  - ", "    ")
			writeWrapped(&b, "When to change: "+f.WhenToChange, "  - ", "    ")
		}
	}

	b.WriteString("\n")
	b.WriteString(EffectVsExcludeMarkdown())
	b.WriteString("\n\n")
	b.WriteString(PatternSemanticsMarkdown())
	b.WriteString("\n")
	return b.String()
}

// PrecedenceMarkdown describes the config layer stack and the shared-vs-local
// split. It is included by the config reference and by the topics that need the
// precedence stated in place, so the stack is written once.
func PrecedenceMarkdown() string {
	return "## Layer precedence\n" +
		"\n" +
		"Configuration is layered, lowest precedence first:\n" +
		"\n" +
		"```text\n" +
		"product defaults\n" +
		"awa.toml            optional shared project config, committable\n" +
		".awa/config.toml    optional local override, ignored/private\n" +
		"--config <path>     optional explicit invocation override (above shared/local)\n" +
		"CLI flags           highest\n" +
		"```\n" +
		"\n" +
		"A later layer REPLACES a key, and for a list-valued key that means the whole\n" +
		"list, never a merge. A local `.awa/config.toml` that sets\n" +
		"`[scope].extra_excludes` discards the shared `awa.toml` list rather than\n" +
		"adding to it, and the same is true of `include`, `env_allowlist`,\n" +
		"`default_scope`, and `extra_effect_roots`. To keep a shared list and add to\n" +
		"it, restate the shared entries in the overriding layer. `awa config\n" +
		"effective` shows the resolved list and the layer it came from, which is the\n" +
		"reliable way to catch a list you did not mean to drop.\n" +
		"\n" +
		"Each active layer that changes observation, hashing, diff, run cache\n" +
		"identity, storage, or output policy is folded into the relevant config\n" +
		"identity, so a reused run or checkpoint reflects the config it actually ran\n" +
		"under. Invalid config in any active layer fails loudly and names the layer\n" +
		"and path.\n" +
		"\n" +
		"Shared vs local:\n" +
		"\n" +
		"- `awa.toml` — share scan/run policy with all contributors and agents.\n" +
		"- `.awa/config.toml` — personal overrides that must not be committed.\n" +
		"- `.awaignore` — committable native ignore patterns.\n" +
		"- `--config <path>` — one-off or CI invocation config."
}

// EffectVsExcludeMarkdown is the effect-roots-vs-excludes decision plus the rules
// that make it correct. It is the one shared source included by the config
// reference, the run topic, and the ignores topic, so the guidance cannot drift
// between the three pages an agent is most likely to read.
func EffectVsExcludeMarkdown() string {
	return "## Effect roots vs excludes\n" +
		"\n" +
		"The central run-cache decision:\n" +
		"\n" +
		"- The command **writes or refreshes** a generated directory during the run,\n" +
		"  and that output is disposable — it may safely be absent after a replay\n" +
		"  → `[run].extra_excludes` or `.awaignore`. Otherwise the self-generated\n" +
		"  output makes every run non-reusable.\n" +
		"- The command **writes** output you actually need on disk afterwards\n" +
		"  → `awa run --record`. Excluding it would let a replay report success with\n" +
		"  the output missing.\n" +
		"- The command only **reads** an already-produced generated directory\n" +
		"  → `[run].extra_effect_roots`. Later changes to that generated state should\n" +
		"  invalidate reuse.\n" +
		"- The command is a deploy, migration, formatter, live probe, or otherwise\n" +
		"  non-reusable → `awa run --record`. Keep durable evidence without\n" +
		"  publishing a reusable hit.\n" +
		"\n" +
		"Rules:\n" +
		"\n" +
		"- Writing to a watched effect root during the run makes the result\n" +
		"  non-reusable.\n" +
		"- Effect roots are for generated state a command depends on but does not\n" +
		"  produce during that command.\n" +
		"- The two lists are separate: the watched set is the built-in effect roots\n" +
		"  plus `extra_effect_roots`. An exclude you add yourself is NOT watched, so\n" +
		"  an excluded, unwatched directory is invisible to the cache in both\n" +
		"  directions.\n" +
		"- Excluding a path therefore weakens what `awa run` can observe, so do it\n" +
		"  intentionally.\n" +
		"- awa will not auto-edit config to improve the cache hit rate."
}

// PatternSemanticsMarkdown documents how ignore and config patterns match. It is
// included by the config reference and the ignores topic.
func PatternSemanticsMarkdown() string {
	return "## Pattern semantics\n" +
		"\n" +
		"- `.awaignore` — gitignore-like: globs, a trailing slash matches a\n" +
		"  directory, a leading slash anchors to the project root, and later rules\n" +
		"  override earlier ones. It is on by default; `.gitignore` is off by\n" +
		"  default.\n" +
		"- `[scope].extra_excludes` and `[run].extra_excludes` — gitignore-style\n" +
		"  patterns, additive on top of the built-in baseline excludes.\n" +
		"- `[run].extra_effect_roots` — directory NAMES (single path segments),\n" +
		"  matched by basename wherever they appear under the observed scope — not\n" +
		"  path globs. `\"target\"` watches every `target/` directory; `\"build/out\"`\n" +
		"  is rejected as a path.\n" +
		"\n" +
		"Use `awa config effective` to see the resolved effective lists and the layer\n" +
		"each value came from."

}

// writeWrapped writes text wrapped to markdownWidth, prefixing the first line
// with first and every continuation line with cont. It keeps the rendered
// Markdown readable when printed verbatim to a terminal without embedding hard
// newlines in the reference summaries. Wrapping is deterministic: the same text
// always breaks at the same words.
func writeWrapped(b *strings.Builder, text, first, cont string) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return
	}
	line := first
	started := false
	for _, w := range words {
		if started && len(line)+1+len(w) > markdownWidth {
			b.WriteString(strings.TrimRight(line, " "))
			b.WriteString("\n")
			line = cont
			started = false
		}
		if started {
			line += " "
		}
		line += w
		started = true
	}
	b.WriteString(strings.TrimRight(line, " "))
	b.WriteString("\n")
}
