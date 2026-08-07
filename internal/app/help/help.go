// Package help holds awa's built-in operational documentation: the
// workflow-oriented topic pages served by "awa help <topic>" and published by
// "awa docs export".
//
// This is distinct from the terse parser/usage help owned by internal/cli.
// Parser help lists syntax; these pages explain why and when to use a workflow,
// which flags matter, and give copyable examples — aimed especially at coding
// agents that have no prior knowledge of awa and may not read the repository
// docs first.
//
// Authored bodies are Markdown files embedded with go:embed under topics/. The
// Markdown is canonical: "awa help <topic>" prints it verbatim and the exporter
// publishes the same bytes, so a terminal reader and a documentation consumer
// can never be shown two different manuals. A small typed catalog owns the
// stable slug, title, summary, display order, aliases, embedded filename, and
// bundle-relative output path; metadata is not duplicated into the Markdown.
//
// Document identity — what a slug is, what an output path may look like, and
// which kinds exist — belongs to internal/domain/docbundle, and this catalog
// parses into those value objects rather than restating their rules. The
// exporter therefore receives already-parsed values and never re-validates them.
//
// The config page has no authored file: its body is rendered from the pure
// internal/domain/configref reference model so the schema, defaults, and
// effect-vs-exclude guidance cannot drift from the decoder. Two other topics
// splice the same shared configref blocks in place through the checked
// awa:include directive (see expandIncludes).
//
// The package is deliberately pure: it imports nothing from internal/cli or
// internal/output and never touches the filesystem, project root, or live
// config. That keeps the content and resolution unit-testable in isolation and
// lets "awa help" and "awa docs export" run outside an initialized project.
package help

import (
	"embed"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"awarer/internal/domain/configref"
	"awarer/internal/domain/docbundle"
)

// topicFS carries the authored Markdown bodies. Embedding them means the
// released binary is the authority for its own documentation: no source tree,
// website, or README access is required to read a topic.
//
//go:embed topics/*.md
var topicFS embed.FS

// embedRoot is the only directory bodies may be read from. A catalog filename is
// a single component joined onto it, so no entry can address content outside the
// embedded root.
const embedRoot = "topics"

// topicsDir is the bundle-relative directory authored topics are published into.
const topicsDir = "topics"

// reservedSlug is the pseudo-topic "awa help topics" handles itself. It is not a
// page, so neither a canonical slug nor an alias may claim it.
const reservedSlug = "topics"

// Topic is one documentation page. Body is the canonical Markdown, already
// expanded and validated: the CLI prints it verbatim and the exporter publishes
// exactly these bytes at Path. Summary is the short label the topic index and
// "awa help topics" listing show next to Slug.
//
// Slug, Path, and Kind are the domain value objects, so a consumer receives
// identity that is already parsed and cannot be malformed.
type Topic struct {
	Slug    docbundle.Slug
	Title   string
	Summary string
	Kind    docbundle.Kind
	Path    docbundle.DocPath
	Aliases []string
	Body    string
}

// entry is the authored catalog declaration. It is deliberately separate from
// Topic: an entry is what a human writes in plain strings, a Topic is the parsed
// and validated product of buildCatalog, so an unvalidated entry can never
// escape this package.
type entry struct {
	slug    string
	title   string
	summary string
	kind    string        // parsed into the closed docbundle.Kind vocabulary
	file    string        // embedded filename; empty when render is set
	outPath string        // bundle-relative output path
	aliases []string      // ergonomic spellings that resolve to this entry
	render  func() string // rendered body; empty file entries must set it
}

// catalogSpec is the ordered, closed set of documentation pages. Order is the
// display order in "awa help" and "awa help topics" and the document order in
// the exported manifest; agents is first because it is the golden path.
//
// The catalog is the single source of truth: the topic index, alias resolution,
// "awa help <topic>", and the export manifest are all derived from it, so a
// listing can never advertise a page that does not exist and the exported bundle
// can never disagree with what the binary serves.
var catalogSpec = []entry{
	{
		slug: "agents", title: "awa for coding agents",
		summary: "golden-path workflow for LLM/coding agents",
		kind:    string(docbundle.KindTopic), file: "agents.md", outPath: topicsDir + "/agents.md",
		aliases: []string{"agent"},
	},
	{
		slug: "install", title: "installing, verifying, and upgrading awa",
		summary: "download, verify, upgrade, and identify the binary",
		kind:    string(docbundle.KindTopic), file: "install.md", outPath: topicsDir + "/install.md",
		// "version" belongs here because this is the page `awa version --help`
		// points at: identifying the installed binary is the same question as
		// installing and upgrading it.
		aliases: []string{"installation", "upgrade", "version"},
	},
	{
		slug: "quickstart", title: "awa quick start — first project and first loop",
		summary: "initialize a project and run the first review loop",
		kind:    string(docbundle.KindTopic), file: "quickstart.md", outPath: topicsDir + "/quickstart.md",
		aliases: []string{"start", "getting-started", "init"},
	},
	{
		slug: "workflows", title: "coding, review, specification, and fix loops",
		summary: "structure a coding, review, spec, or fix loop",
		kind:    string(docbundle.KindTopic), file: "workflows.md", outPath: topicsDir + "/workflows.md",
		aliases: []string{"workflow", "review"},
	},
	{
		slug: "status", title: "awa status — the review dashboard",
		summary: "read the dashboard: baseline, drift, git, reuse, next",
		kind:    string(docbundle.KindTopic), file: "status.md", outPath: topicsDir + "/status.md",
		aliases: []string{"dashboard"},
	},
	{
		slug: "run", title: "awa run — deterministic run cache",
		summary: "cache deterministic checks; hit/miss/replay model",
		kind:    string(docbundle.KindTopic), file: "run.md", outPath: topicsDir + "/run.md",
		aliases: []string{"cache", "run-cache"},
	},
	{
		slug: "record", title: "awa run --record — supervised runs",
		summary: "always-execute history for non-cacheable commands",
		kind:    string(docbundle.KindTopic), file: "record.md", outPath: topicsDir + "/record.md",
	},
	{
		slug: "inspect", title: "finding stored runs and their output",
		summary: "locate, read, verify, and delete stored run output",
		kind:    string(docbundle.KindTopic), file: "inspect.md", outPath: topicsDir + "/inspect.md",
		aliases: []string{"history"},
	},
	{
		slug: "checkpoints", title: "awa checkpoint — explicit checkpoints",
		summary: "user-authored worktree checkpoints",
		kind:    string(docbundle.KindTopic), file: "checkpoints.md", outPath: topicsDir + "/checkpoints.md",
		aliases: []string{"checkpoint", "cp"},
	},
	{
		slug: "diff", title: "awa changes / awa diff",
		summary: "compare two states; default range latest..now",
		kind:    string(docbundle.KindTopic), file: "diff.md", outPath: topicsDir + "/diff.md",
		aliases: []string{"changes"},
	},
	{
		slug: "restore", title: "awa restore — repair selected paths from an immutable state",
		summary: "preview-first repair of selected paths from a stored state",
		kind:    string(docbundle.KindTopic), file: "restore.md", outPath: topicsDir + "/restore.md",
		aliases: []string{"recover", "undo"},
	},
	{
		slug: "refs", title: "state reference syntax",
		summary: "now, latest, @-N, run:<id>:before/after, restore:<id>:before, A..B",
		kind:    string(docbundle.KindTopic), file: "refs.md", outPath: topicsDir + "/refs.md",
	},
	{
		slug:    "config",
		title:   "awa config — configuration schema, precedence, and when to change it",
		summary: "config schema, layers/precedence, and when to change it",
		kind:    string(docbundle.KindReference), outPath: "reference/configuration.md",
		aliases: []string{"cfg", "configuration"},
		render:  configref.MarkdownReference,
	},
	{
		slug: "ignores", title: "scan inputs and ignore rules",
		summary: "protected paths, .awaignore on, .gitignore off",
		kind:    string(docbundle.KindTopic), file: "ignores.md", outPath: topicsDir + "/ignores.md",
	},
	{
		slug: "doctor", title: "awa doctor — diagnose and repair local state",
		summary: "diagnose and (with --repair) repair local state",
		kind:    string(docbundle.KindTopic), file: "doctor.md", outPath: topicsDir + "/doctor.md",
	},
	{
		slug: "gc", title: "awa gc — reclaim unreferenced local state",
		summary: "reclaim unreferenced checkpoints, runs, and blobs",
		kind:    string(docbundle.KindTopic), file: "gc.md", outPath: topicsDir + "/gc.md",
	},
	{
		slug: "troubleshooting", title: "diagnosing awa — slow, stale, blocked, or broken",
		summary: "symptom-to-command decision paths and recovery",
		kind:    string(docbundle.KindTopic), file: "troubleshooting.md", outPath: topicsDir + "/troubleshooting.md",
		aliases: []string{"troubleshoot", "performance", "slow"},
	},
	{
		slug: "privacy", title: "awa local evidence and privacy",
		summary: "what .awa/ stores; keep it private and untracked",
		kind:    string(docbundle.KindTopic), file: "privacy.md", outPath: topicsDir + "/privacy.md",
	},
	{
		slug: "integrations", title: "awa as a provider for other local tools",
		summary: "use awa as a state/evidence provider for other local tools",
		kind:    string(docbundle.KindTopic), file: "integrations.md", outPath: topicsDir + "/integrations.md",
		// "state" belongs here, not on privacy: this is the topic that documents the
		// awa-state/v1 provider contract, and it is what `awa state --help` points at.
		// An alias that shares a command's name must land on that command's topic.
		aliases: []string{"integration", "provider", "rezonator", "state"},
	},
	{
		slug: "platform", title: "awa platform and filesystem support",
		summary: "supported platforms, filesystems, paths, and links",
		kind:    string(docbundle.KindTopic), file: "platform.md", outPath: topicsDir + "/platform.md",
	},
	{
		slug: "json", title: "JSON output, exit origin, and script rules",
		summary: "envelope shape, partial-stream failure, exit origin",
		kind:    string(docbundle.KindTopic), file: "json.md", outPath: topicsDir + "/json.md",
		aliases: []string{"machine", "scripting"},
	},
	{
		slug: "exit-codes", title: "awa exit codes",
		summary: "stable process exit statuses 0–6 (plus 130 on interrupt)",
		kind:    string(docbundle.KindTopic), file: "exit-codes.md", outPath: topicsDir + "/exit-codes.md",
	},
}

// catalog and aliasIndex are the validated product of catalogSpec, built once at
// init. Building eagerly makes an invalid catalog impossible to ship: the
// failure is a property of the compiled-in content, so it trips on the first
// process start and on every test run rather than at some later export.
var (
	catalog    []Topic
	aliasIndex map[string]string
)

func init() {
	built, index, err := buildCatalog(catalogSpec)
	if err != nil {
		panic("help: invalid documentation catalog: " + err.Error())
	}
	catalog = built
	aliasIndex = index
}

// Topics returns the canonical topics in display order.
func Topics() []Topic {
	out := make([]Topic, len(catalog))
	for i, t := range catalog {
		out[i] = t
		out[i].Aliases = append([]string(nil), t.Aliases...)
	}
	return out
}

// Aliases returns a copy of the alias → canonical-slug map.
func Aliases() map[string]string {
	out := make(map[string]string, len(aliasIndex))
	for k, v := range aliasIndex {
		out[k] = v
	}
	return out
}

// Resolve maps a raw topic token (a canonical slug or an alias) to its Topic.
// The second result is false for an unknown topic; callers turn that into a
// usage error rather than falling back to generic help.
func Resolve(raw string) (Topic, bool) {
	name := raw
	if canonical, ok := aliasIndex[raw]; ok {
		name = canonical
	}
	for _, t := range catalog {
		if t.Slug.String() == name {
			t.Aliases = append([]string(nil), t.Aliases...)
			return t, true
		}
	}
	return Topic{}, false
}

// buildCatalog parses catalogSpec into topics and enforces the catalog-wide
// invariants. It is a pure function of its input so tests can feed it poisoned
// specs and prove each rule rejects the case it exists for, instead of only
// observing the healthy catalog.
//
// Slug and path SHAPE is not checked here: docbundle owns what those look like,
// and buildTopic parses through its constructors. What is checked here is what
// only the catalog knows — uniqueness across entries, the reserved pseudo-topic,
// and that every intra-topic link names a real page.
func buildCatalog(spec []entry) ([]Topic, map[string]string, error) {
	if len(spec) == 0 {
		return nil, nil, fmt.Errorf("catalog is empty")
	}

	slugs := make(map[string]bool, len(spec))
	files := make(map[string]bool, len(spec))
	outPaths := make(map[string]bool, len(spec))
	index := make(map[string]string)
	out := make([]Topic, 0, len(spec))

	for _, e := range spec {
		topic, err := buildTopic(e)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case slugs[e.slug]:
			return nil, nil, fmt.Errorf("topic %q: duplicate slug", e.slug)
		case e.file != "" && files[e.file]:
			return nil, nil, fmt.Errorf("topic %q: duplicate embedded file %q", e.slug, e.file)
		case outPaths[e.outPath]:
			return nil, nil, fmt.Errorf("topic %q: duplicate output path %q", e.slug, e.outPath)
		}
		slugs[e.slug] = true
		if e.file != "" {
			files[e.file] = true
		}
		outPaths[e.outPath] = true
		out = append(out, topic)
	}

	// Aliases are indexed after every slug is known so an alias may appear before
	// its target in declaration order without the check depending on that order.
	for _, e := range spec {
		for _, a := range e.aliases {
			if _, err := docbundle.NewSlug(a); err != nil {
				return nil, nil, fmt.Errorf("topic %q alias: %w", e.slug, err)
			}
			switch {
			case a == reservedSlug:
				return nil, nil, fmt.Errorf("topic %q alias %q shadows the reserved pseudo-topic", e.slug, a)
			case slugs[a]:
				return nil, nil, fmt.Errorf("topic %q alias %q shadows canonical topic %q", e.slug, a, a)
			}
			if prev, dup := index[a]; dup {
				return nil, nil, fmt.Errorf("alias %q is claimed by both %q and %q", a, prev, e.slug)
			}
			index[a] = e.slug
		}
	}

	// Link targets that stay inside the topics directory must name a real page.
	// Cross-directory targets (a generated reference page) are form-checked here
	// and fully resolved against the whole bundle by the exporter's link test.
	for _, t := range out {
		if err := checkLinks(t, slugs); err != nil {
			return nil, nil, err
		}
	}

	return out, index, nil
}

// buildTopic parses one entry into a Topic: identity through the domain
// constructors, body through the embedded file or the renderer, then the
// document-shape checks the catalog adds on top.
func buildTopic(e entry) (Topic, error) {
	slug, err := docbundle.NewSlug(e.slug)
	if err != nil {
		return Topic{}, err
	}
	if e.slug == reservedSlug {
		return Topic{}, fmt.Errorf("topic %q: slug shadows the reserved pseudo-topic", e.slug)
	}
	switch {
	case e.title == "":
		return Topic{}, fmt.Errorf("topic %q: empty title", e.slug)
	case e.summary == "":
		return Topic{}, fmt.Errorf("topic %q: empty summary", e.slug)
	case e.file == "" && e.render == nil:
		return Topic{}, fmt.Errorf("topic %q: neither an embedded file nor a renderer", e.slug)
	case e.file != "" && e.render != nil:
		return Topic{}, fmt.Errorf("topic %q: has both an embedded file and a renderer", e.slug)
	}
	if e.file != "" {
		if err := checkEmbedName(e.file); err != nil {
			return Topic{}, fmt.Errorf("topic %q: %w", e.slug, err)
		}
	}
	outPath, err := docbundle.NewDocPath(e.outPath)
	if err != nil {
		return Topic{}, fmt.Errorf("topic %q: %w", e.slug, err)
	}
	// Every documentation page is Markdown; the domain only requires an extension.
	if !strings.HasSuffix(e.outPath, ".md") {
		return Topic{}, fmt.Errorf("topic %q: output path %q must end in .md", e.slug, e.outPath)
	}
	kind, err := docbundle.ParseKind(e.kind)
	if err != nil {
		return Topic{}, fmt.Errorf("topic %q: %w", e.slug, err)
	}
	if kind == docbundle.KindCommand {
		return Topic{}, fmt.Errorf("topic %q: the authored catalog holds no generated command pages", e.slug)
	}

	body, err := loadBody(e)
	if err != nil {
		return Topic{}, fmt.Errorf("topic %q: %w", e.slug, err)
	}
	if err := checkBody(e, body); err != nil {
		return Topic{}, fmt.Errorf("topic %q: %w", e.slug, err)
	}

	return Topic{
		Slug:    slug,
		Title:   e.title,
		Summary: e.summary,
		Kind:    kind,
		Path:    outPath,
		Aliases: append([]string(nil), e.aliases...),
		Body:    body,
	}, nil
}

// checkEmbedName enforces that a body filename is a single, clean component under
// the embedded root, so joining it onto embedRoot cannot reach other content.
func checkEmbedName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("embedded filename %q is not a single component", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("embedded filename %q must not contain a separator", name)
	}
	if !strings.HasSuffix(name, ".md") {
		return fmt.Errorf("embedded filename %q must end in .md", name)
	}
	return nil
}

// loadBody produces the raw body for an entry: the embedded file for an authored
// topic, or the model rendering for a generated one.
func loadBody(e entry) (string, error) {
	if e.render != nil {
		return e.render(), nil
	}
	raw, err := topicFS.ReadFile(embedRoot + "/" + e.file)
	if err != nil {
		return "", fmt.Errorf("reading embedded body: %w", err)
	}
	body, err := expandIncludes(string(raw))
	if err != nil {
		return "", err
	}
	return body, nil
}

// includeMarker is the directive token. It is searched for verbatim after
// expansion so a malformed or unexpanded directive cannot reach a reader.
const includeMarker = "awa:include"

// includeDirectiveRe matches a whole line that is exactly one include directive.
// Nothing else is recognised: a directive with trailing prose, or one embedded
// mid-line, is left alone and then caught by the post-expansion marker check.
var includeDirectiveRe = regexp.MustCompile(`^[ \t]*<!--[ \t]+` + includeMarker + `[ \t]+([a-z0-9-]+)[ \t]+-->[ \t]*$`)

// includeBlocks is the closed registry of blocks an authored topic may splice in.
// Each renders one canonical fragment owned by a live model, so guidance that
// must be identical on several pages is written once and cannot drift. The map
// is closed on purpose: an unknown key is a build failure, not a silent no-op.
var includeBlocks = map[string]func() string{
	"config-precedence": configref.PrecedenceMarkdown,
	"effect-vs-exclude": configref.EffectVsExcludeMarkdown,
	"pattern-semantics": configref.PatternSemanticsMarkdown,
}

// expandIncludes substitutes each include directive line with its registered
// block. Expansion is deterministic (a pure line-for-block substitution) and
// non-recursive: a block that itself contained a directive would leave the
// marker behind, which the final check rejects. A key used twice in one document
// is an error, so a page cannot accidentally state the same block twice.
func expandIncludes(raw string) (string, error) {
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, len(lines))
	used := make(map[string]bool)
	for _, line := range lines {
		m := includeDirectiveRe.FindStringSubmatch(line)
		if m == nil {
			out = append(out, line)
			continue
		}
		key := m[1]
		render, ok := includeBlocks[key]
		if !ok {
			return "", fmt.Errorf("unknown include %q", key)
		}
		if used[key] {
			return "", fmt.Errorf("include %q used more than once", key)
		}
		used[key] = true
		out = append(out, strings.Split(render(), "\n")...)
	}
	expanded := strings.Join(out, "\n")
	if strings.Contains(expanded, includeMarker) {
		return "", fmt.Errorf("an %s directive survived expansion", includeMarker)
	}
	return expanded, nil
}

// checkBody enforces the document invariants every body must satisfy, whether
// authored or rendered: valid UTF-8, LF newlines, exactly one final newline, and
// exactly one H1 that states the catalog title. Those are what make "print the
// body verbatim" and "publish the body verbatim" the same operation.
func checkBody(e entry, body string) error {
	switch {
	case body == "":
		return fmt.Errorf("empty body")
	case !utf8.ValidString(body):
		return fmt.Errorf("body is not valid UTF-8")
	case strings.ContainsRune(body, '\r'):
		return fmt.Errorf("body must use LF newlines")
	case !strings.HasSuffix(body, "\n"):
		return fmt.Errorf("body must end with a newline")
	case strings.HasSuffix(body, "\n\n"):
		return fmt.Errorf("body must end with exactly one newline")
	}
	lines := strings.Split(body, "\n")
	want := "# " + e.title
	if lines[0] != want {
		return fmt.Errorf("body must start with %q, got %q", want, lines[0])
	}
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "# ") {
			return fmt.Errorf("body has a second H1: %q", line)
		}
	}
	return nil
}

// linkRe matches a Markdown inline link target.
var linkRe = regexp.MustCompile(`\]\(([^)]*)\)`)

// checkLinks enforces the canonical link form: every link target is a relative
// Markdown path, and a target that resolves inside the topics directory must
// name a real catalog slug. Cross-directory targets are validated for form here
// and resolved against the complete bundle by the exporter, which is the only
// place that knows the generated documents too.
func checkLinks(t Topic, slugs map[string]bool) error {
	for _, m := range linkRe.FindAllStringSubmatch(t.Body, -1) {
		target := m[1]
		switch {
		case target == "":
			return fmt.Errorf("topic %q: empty link target", t.Slug)
		case strings.Contains(target, "://"):
			return fmt.Errorf("topic %q: link %q must be a relative document path", t.Slug, target)
		case strings.HasPrefix(target, "/"):
			return fmt.Errorf("topic %q: link %q must be relative", t.Slug, target)
		case strings.Contains(target, `\`):
			return fmt.Errorf("topic %q: link %q must use slash separators", t.Slug, target)
		case !strings.HasSuffix(target, ".md"):
			return fmt.Errorf("topic %q: link %q must target a .md document", t.Slug, target)
		}
		resolved := path.Join(t.Path.Dir(), target)
		if strings.HasPrefix(resolved, "../") || resolved == ".." {
			return fmt.Errorf("topic %q: link %q escapes the bundle", t.Slug, target)
		}
		dir, base := path.Split(resolved)
		if strings.TrimSuffix(dir, "/") != topicsDir {
			continue // a generated document; the exporter resolves it
		}
		if slug := strings.TrimSuffix(base, ".md"); !slugs[slug] {
			return fmt.Errorf("topic %q: link %q names no topic", t.Slug, target)
		}
	}
	return nil
}
