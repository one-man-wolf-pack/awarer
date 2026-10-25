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
		if s.Example != "" {
			b.WriteString("\nExample:\n\n```toml\n")
			b.WriteString(s.Example)
			b.WriteString("```\n")
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
		"`default_scope`, `extra_effect_roots`, and `hash_only_patterns`. To keep a\n" +
		"shared list and add to it, restate the shared entries in the overriding\n" +
		"layer. `awa config effective` shows the resolved list and the layer it came\n" +
		"from, which is the reliable way to catch a list you did not mean to drop.\n" +
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
		"The central run-cache decision for a generated or scratch directory:\n" +
		"\n" +
		"- The command **writes** output you need afterwards, or has side effects\n" +
		"  (deploy, migration, formatter, live probe) → `awa run --record`. A replay\n" +
		"  recreates no files, so it could report success with the output missing.\n" +
		"- One exact check **rewrites** a directory whose prior contents cannot change\n" +
		"  its result and whose files are not needed after a replay → a\n" +
		"  `[[run.validation]]` policy for result-only reuse (`awa help validation`).\n" +
		"  Both assertions must hold; awa checks neither.\n" +
		"- The command only **reads** a generated directory the run input scan no longer\n" +
		"  sees → `[run].extra_effect_roots`, so later changes to it invalidate reuse.\n" +
		"- The input scan still **sees** that directory → configure nothing; its\n" +
		"  contents already key the run.\n" +
		"- No command's result depends on the path, even if one writes it (scratch,\n" +
		"  logs) → `[run].extra_excludes` or `.awaignore`.\n" +
		"\n" +
		"An effect-root selector is literal, never a glob: a name such as\n" +
		"`extra_effect_roots = [\"bin\"]` matches at **any** depth, for example across\n" +
		"monorepo packages; a path such as `[\"artifacts/bin\"]` matches only there, not\n" +
		"`other/bin` or `other/artifacts/bin`.\n" +
		"\n" +
		"Rules:\n" +
		"\n" +
		"- Writing to a watched root during the run makes the result non-reusable,\n" +
		"  unless a matched validation policy waives that exact directory.\n" +
		"- The watched set is the built-in effect roots plus `extra_effect_roots`. An\n" +
		"  exclude neither silences a watched root nor is watched itself, so an\n" +
		"  excluded, unwatched directory is invisible to the cache for every command.\n" +
		"- Never exclude a real dependency to get hits: a miss is cheaper than a false\n" +
		"  hit.\n" +
		"- awa will not auto-edit config to improve the cache hit rate."
}

// PatternSemanticsMarkdown documents how ignore and config patterns match. It is
// included by the config reference and the ignores topic.
func PatternSemanticsMarkdown() string {
	return "## Pattern semantics\n" +
		"\n" +
		"`.awaignore`, an enabled `.gitignore`, the `extra_excludes` lists, and\n" +
		"`[checkpoint].hash_only_patterns` share one case-sensitive pattern language,\n" +
		"Git's own ignore pattern format:\n" +
		"\n" +
		"- A name without a slash (`*.log`, `cache`) matches at any depth. A leading\n" +
		"  or inner slash anchors the pattern to the directory that declares it:\n" +
		"  `foo/bar` and `/foo/bar` both match only there, never `deep/foo/bar`.\n" +
		"  Config lists are declared at the project root; a nested ignore file\n" +
		"  declares at its own directory.\n" +
		"- `*` and `?` never match a slash and `?` matches exactly one character.\n" +
		"  `**`, character classes, and backslash escapes work as in Git. A trailing\n" +
		"  slash matches directories only. Blank lines and lines starting with `#`\n" +
		"  are ignored, as are trailing spaces not escaped with a backslash.\n" +
		"- The last matching rule decides, and a leading `!` reverses it.\n" +
		"- Scanning prunes an excluded directory, so a `!` rule for something inside\n" +
		"  it cannot bring that back; re-include the directory itself.\n" +
		"  `hash_only_patterns` never prunes: it selects among files already scanned,\n" +
		"  a matching directory selects every file below it, and a later `!` rule\n" +
		"  un-selects one file or a whole directory (`*.env` then `!fixtures/`). It is\n" +
		"  matched against the project-relative path a followed symlink is reached\n" +
		"  by, and never changes what is scanned.\n" +
		"- A pattern Git would never match (an unclosed `[`, a lone trailing\n" +
		"  backslash, a bare `!` or `/`) matches nothing. In an ignore file or an\n" +
		"  `extra_excludes` list awa keeps running and warns on stderr with the file\n" +
		"  and line or the key and element index, from every command that scans.\n" +
		"  `awa config validate` warns about the config lists; `awa doctor` reports\n" +
		"  those and the root ignore files (see `awa help doctor`). In\n" +
		"  `hash_only_patterns` it is a configuration error: a rule that silently\n" +
		"  selected nothing would store the bytes it was written to keep out.\n" +
		"- Each config list element is one pattern. An element containing a line\n" +
		"  feed is a configuration error in every list; write it as separate\n" +
		"  elements. Trailing carriage returns of an `extra_excludes` element are\n" +
		"  dropped and one inside the pattern is literal; `hash_only_patterns`\n" +
		"  refuses carriage returns too. Line endings of the TOML file do not matter.\n" +
		"- `awa run --exclude` takes such a pattern too, not a literal path. From the\n" +
		"  project root, `--exclude name` matches `name` at any depth and\n" +
		"  `--exclude dir/name` only at the root; `*`, `?`, `[`, and a leading `#` or\n" +
		"  `!` keep their meaning. Quote the pattern so the shell does not expand it.\n" +
		"\n" +
		"Other lists are not patterns:\n" +
		"\n" +
		"- `[run].extra_effect_roots` — literal selectors, never globs, in one of two\n" +
		"  forms. A single segment is a directory NAME matched by basename wherever it\n" +
		"  appears: `\"target\"` watches every `target/`. A slash-separated\n" +
		"  project-relative PATH matches that one location only:\n" +
		"  `\"artifacts/bin\"` watches `artifacts/bin` and neither `other/bin` nor\n" +
		"  `other/artifacts/bin`. Matching is case-sensitive and `/` is the separator\n" +
		"  on every platform. A backslash, a volume spelling, and a `.`/`..` component\n" +
		"  are rejected in either form; a path is also rejected when it begins or ends\n" +
		"  with a slash, holds an empty component, or names `.git` or `.awa`.\n" +
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
