package help

import (
	"strings"
	"testing"

	"awarer/internal/domain/configref"
	"awarer/internal/domain/docbundle"
)

// wantCanonical is the closed set of topic slugs the CLI and specs promise, in
// display and export order. Adding, renaming, moving, or removing a page means
// editing this list deliberately; the tests below hold Topics(), Resolve(), and
// the alias targets to it.
var wantCanonical = []string{
	"agents", "install", "quickstart", "workflows", "status",
	"run", "record", "inspect", "checkpoints", "diff", "restore", "refs",
	"config", "ignores", "doctor", "gc", "troubleshooting",
	"privacy", "integrations", "platform", "json", "exit-codes",
}

// TestTopicsAreTheCanonicalSetInOrder pins membership AND position in one check:
// order is the "awa help" listing order and the exported manifest's document
// order, so a reshuffle is a contract change and must be visible as one.
//
// Duplicate slugs are not re-checked here — buildCatalog rejects them at init, and
// TestBuildCatalogRejections proves it does.
func TestTopicsAreTheCanonicalSetInOrder(t *testing.T) {
	topics := Topics()
	for i, want := range wantCanonical {
		if i >= len(topics) {
			t.Errorf("Topics() is missing %q and everything after it", want)
			break
		}
		if got := topics[i].Slug.String(); got != want {
			t.Errorf("Topics()[%d] = %q, want %q", i, got, want)
		}
	}
	if len(topics) > len(wantCanonical) {
		for _, extra := range topics[len(wantCanonical):] {
			t.Errorf("Topics() has the undeclared topic %q; add it to wantCanonical deliberately", extra.Slug)
		}
	}
}

func TestEveryCanonicalTopicResolvesAndHasContent(t *testing.T) {
	for _, slug := range wantCanonical {
		topic, ok := Resolve(slug)
		if !ok {
			t.Errorf("Resolve(%q) = not found, want the canonical topic", slug)
			continue
		}
		if topic.Slug.String() != slug {
			t.Errorf("Resolve(%q).Slug = %q, want %q", slug, topic.Slug, slug)
		}
		if strings.TrimSpace(topic.Summary) == "" {
			t.Errorf("topic %q has an empty Summary", slug)
		}
		if strings.TrimSpace(topic.Body) == "" {
			t.Errorf("topic %q has an empty Body", slug)
		}
		if strings.TrimSpace(topic.Title) == "" {
			t.Errorf("topic %q has an empty Title", slug)
		}
		if topic.Path.String() == "" {
			t.Errorf("topic %q has an empty Path", slug)
		}
	}
}

func TestAliasesResolveToCanonicalTopic(t *testing.T) {
	wantTargets := map[string]string{
		"agent":           "agents",
		"installation":    "install",
		"upgrade":         "install",
		"version":         "install",
		"start":           "quickstart",
		"getting-started": "quickstart",
		"init":            "quickstart",
		"workflow":        "workflows",
		"review":          "workflows",
		"dashboard":       "status",
		"history":         "inspect",
		"troubleshoot":    "troubleshooting",
		"performance":     "troubleshooting",
		"slow":            "troubleshooting",
		"machine":         "json",
		"scripting":       "json",
		"checkpoint":      "checkpoints",
		"cp":              "checkpoints",
		"changes":         "diff",
		"cache":           "run",
		"run-cache":       "run",
		"cfg":             "config",
		"configuration":   "config",
		"integration":     "integrations",
		"state":           "integrations",
		"provider":        "integrations",
		"rezonator":       "integrations",
		"recover":         "restore",
		"undo":            "restore",
	}
	if len(Aliases()) != len(wantTargets) {
		t.Errorf("Aliases() = %v, want %d entries", Aliases(), len(wantTargets))
	}
	for alias, wantSlug := range wantTargets {
		topic, ok := Resolve(alias)
		if !ok {
			t.Errorf("Resolve(alias %q) = not found", alias)
			continue
		}
		if topic.Slug.String() != wantSlug {
			t.Errorf("Resolve(alias %q).Slug = %q, want %q", alias, topic.Slug, wantSlug)
		}
	}
}

// TestAliasTargetsExist guards the invariant that no alias points at a topic
// that was removed or renamed. It checks against the public Topics() set (not
// alias resolution) so a self-referential alias cannot mask a dangling target.
func TestAliasTargetsExist(t *testing.T) {
	canonical := map[string]bool{}
	for _, topic := range Topics() {
		canonical[topic.Slug.String()] = true
	}
	for alias, target := range Aliases() {
		if !canonical[target] {
			t.Errorf("alias %q targets %q, which is not a canonical topic", alias, target)
		}
	}
}

// TestAliasIndexMatchesTopicAliases proves the flattened alias map and the
// per-topic alias lists are two views of one catalog, so a listing built from
// either cannot advertise an alias the resolver does not honor.
func TestAliasIndexMatchesTopicAliases(t *testing.T) {
	fromTopics := map[string]string{}
	for _, topic := range Topics() {
		for _, a := range topic.Aliases {
			fromTopics[a] = topic.Slug.String()
		}
	}
	index := Aliases()
	if len(fromTopics) != len(index) {
		t.Fatalf("per-topic aliases = %v, flattened = %v", fromTopics, index)
	}
	for alias, slug := range index {
		if fromTopics[alias] != slug {
			t.Errorf("alias %q: flattened target %q, per-topic target %q", alias, slug, fromTopics[alias])
		}
	}
}

func TestResolveUnknownTopic(t *testing.T) {
	if topic, ok := Resolve("definitely-not-a-topic"); ok {
		t.Errorf("Resolve(unknown) = (%v, true), want not found", topic)
	}
	// "topics" is handled by the CLI listing, not a page; it must not resolve
	// to a topic here.
	if _, ok := Resolve("topics"); ok {
		t.Errorf("Resolve(%q) resolved, want not found", "topics")
	}
}

// TestAccessorsAreCopySafe proves a caller cannot mutate the shared catalog
// through a returned value. The catalog is package state built once at init, so
// a shared backing array would let one command's rendering corrupt another's.
func TestAccessorsAreCopySafe(t *testing.T) {
	topics := Topics()
	topics[0].Title = "mutated"
	topics[0].Aliases = append(topics[0].Aliases, "injected")
	if Topics()[0].Title == "mutated" {
		t.Error("Topics() shares its backing array with the catalog")
	}
	for _, a := range Topics()[0].Aliases {
		if a == "injected" {
			t.Error("Topics() shares its alias slice with the catalog")
		}
	}

	aliases := Aliases()
	aliases["injected"] = "agents"
	if _, ok := Aliases()["injected"]; ok {
		t.Error("Aliases() shares its map with the catalog")
	}

	resolved, ok := Resolve("agents")
	if !ok {
		t.Fatal("Resolve(agents) = not found")
	}
	resolved.Aliases = append(resolved.Aliases, "injected")
	again, _ := Resolve("agents")
	for _, a := range again.Aliases {
		if a == "injected" {
			t.Error("Resolve() shares its alias slice with the catalog")
		}
	}
}

// TestBodiesAreCanonicalMarkdown pins the properties that let one byte sequence
// serve both the terminal and the exported bundle: LF newlines, exactly one
// final newline, one H1 equal to the catalog title, and no unexpanded directive.
func TestBodiesAreCanonicalMarkdown(t *testing.T) {
	for _, topic := range Topics() {
		if strings.ContainsRune(topic.Body, '\r') {
			t.Errorf("topic %q body contains CR", topic.Slug)
		}
		if !strings.HasSuffix(topic.Body, "\n") || strings.HasSuffix(topic.Body, "\n\n") {
			t.Errorf("topic %q body must end with exactly one newline", topic.Slug)
		}
		lines := strings.Split(topic.Body, "\n")
		if want := "# " + topic.Title; lines[0] != want {
			t.Errorf("topic %q body starts with %q, want %q", topic.Slug, lines[0], want)
		}
		for _, line := range lines[1:] {
			if strings.HasPrefix(line, "# ") {
				t.Errorf("topic %q body has a second H1: %q", topic.Slug, line)
			}
		}
		if strings.Contains(topic.Body, includeMarker) {
			t.Errorf("topic %q body still contains an %s directive", topic.Slug, includeMarker)
		}
	}
}

// TestSharedBlocksAreSplicedVerbatim proves the include directive is a real
// anti-drift mechanism: the block that reaches the run and ignores pages is the
// same byte sequence configref renders for the configuration reference, not a
// paraphrase that can be edited on one page only.
func TestSharedBlocksAreSplicedVerbatim(t *testing.T) {
	cases := []struct {
		slug  string
		block string
	}{
		{"run", configref.EffectVsExcludeMarkdown()},
		{"ignores", configref.EffectVsExcludeMarkdown()},
		{"ignores", configref.PatternSemanticsMarkdown()},
		{"config", configref.EffectVsExcludeMarkdown()},
		{"config", configref.PatternSemanticsMarkdown()},
	}
	for _, c := range cases {
		topic, ok := Resolve(c.slug)
		if !ok {
			t.Fatalf("Resolve(%q) = not found", c.slug)
		}
		if !strings.Contains(topic.Body, c.block) {
			t.Errorf("topic %q does not carry the canonical block verbatim:\n%s", c.slug, c.block)
		}
	}
}

func TestExpandIncludesSubstitutesRegisteredBlock(t *testing.T) {
	got, err := expandIncludes("# t\n\n<!-- awa:include pattern-semantics -->\n\ntail\n")
	if err != nil {
		t.Fatalf("expandIncludes: %v", err)
	}
	want := "# t\n\n" + configref.PatternSemanticsMarkdown() + "\n\ntail\n"
	if got != want {
		t.Errorf("expandIncludes =\n%q\nwant\n%q", got, want)
	}
}

func TestExpandIncludesIsDeterministic(t *testing.T) {
	const raw = "# t\n\n<!-- awa:include effect-vs-exclude -->\n"
	first, err := expandIncludes(raw)
	if err != nil {
		t.Fatalf("expandIncludes: %v", err)
	}
	second, err := expandIncludes(raw)
	if err != nil {
		t.Fatalf("expandIncludes: %v", err)
	}
	if first != second {
		t.Error("expandIncludes is not deterministic for the same input")
	}
}

func TestExpandIncludesRejections(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"unknown key", "# t\n\n<!-- awa:include not-a-block -->\n"},
		{"repeated key", "# t\n\n<!-- awa:include pattern-semantics -->\n\n<!-- awa:include pattern-semantics -->\n"},
		{"malformed directive survives", "# t\n\n<!-- awa:include pattern-semantics --> trailing prose\n"},
		{"marker without comment", "# t\n\nawa:include pattern-semantics\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := expandIncludes(c.raw); err == nil {
				t.Error("expandIncludes = nil error, want a rejection")
			}
		})
	}
}

// TestEveryIncludeBlockIsSpliceSafe proves the registry itself cannot introduce
// recursion or whitespace drift: a block that carried a directive, a CR, or a
// trailing newline would corrupt every page that includes it.
func TestEveryIncludeBlockIsSpliceSafe(t *testing.T) {
	for key, render := range includeBlocks {
		block := render()
		if block == "" {
			t.Errorf("include block %q is empty", key)
		}
		if strings.Contains(block, includeMarker) {
			t.Errorf("include block %q contains an %s directive (recursion)", key, includeMarker)
		}
		if strings.ContainsRune(block, '\r') {
			t.Errorf("include block %q contains CR", key)
		}
		if strings.HasSuffix(block, "\n") {
			t.Errorf("include block %q must not end with a newline", key)
		}
		if !strings.HasPrefix(block, "## ") {
			t.Errorf("include block %q must start at an H2 heading", key)
		}
	}
}

// buildCatalog rejection fixtures. Every rule the catalog enforces gets a probe
// that would ship a broken page if the rule were removed, so the guards are
// proven rather than assumed.
func TestBuildCatalogRejections(t *testing.T) {
	ok := func(body string) func() string { return func() string { return body } }
	base := entry{
		slug: "alpha", title: "Alpha", summary: "s", kind: string(docbundle.KindReference),
		outPath: "topics/alpha.md", render: ok("# Alpha\n"),
	}
	with := func(mut func(*entry)) entry {
		e := base
		mut(&e)
		return e
	}

	cases := []struct {
		name string
		spec []entry
	}{
		{"empty catalog", nil},
		{"empty slug", []entry{with(func(e *entry) { e.slug = "" })}},
		{"invalid slug", []entry{with(func(e *entry) { e.slug = "Alpha_One" })}},
		{"reserved slug", []entry{with(func(e *entry) { e.slug = "topics"; e.render = ok("# Alpha\n") })}},
		{"duplicate slug", []entry{base, with(func(e *entry) { e.outPath = "topics/beta.md" })}},
		{"duplicate output path", []entry{base, with(func(e *entry) { e.slug = "beta" })}},
		{"duplicate embedded file", []entry{
			{slug: "a", title: "A", summary: "s", kind: string(docbundle.KindTopic), file: "run.md", outPath: "topics/a.md"},
			{slug: "b", title: "B", summary: "s", kind: string(docbundle.KindTopic), file: "run.md", outPath: "topics/b.md"},
		}},
		{"empty title", []entry{with(func(e *entry) { e.title = "" })}},
		{"empty summary", []entry{with(func(e *entry) { e.summary = "" })}},
		{"unknown kind", []entry{with(func(e *entry) { e.kind = "page" })}},
		{"no body source", []entry{with(func(e *entry) { e.render = nil })}},
		{"two body sources", []entry{with(func(e *entry) { e.file = "run.md" })}},
		{"missing embedded file", []entry{
			{slug: "a", title: "A", summary: "s", kind: string(docbundle.KindTopic), file: "not-a-page.md", outPath: "topics/a.md"},
		}},
		{"escaping embedded file", []entry{
			{slug: "a", title: "A", summary: "s", kind: string(docbundle.KindTopic), file: "../help.go", outPath: "topics/a.md"},
		}},
		{"absolute output path", []entry{with(func(e *entry) { e.outPath = "/topics/alpha.md" })}},
		{"traversing output path", []entry{with(func(e *entry) { e.outPath = "topics/../../alpha.md" })}},
		{"backslash output path", []entry{with(func(e *entry) { e.outPath = `topics\alpha.md` })}},
		{"non-markdown output path", []entry{with(func(e *entry) { e.outPath = "topics/alpha.txt" })}},
		{"empty body", []entry{with(func(e *entry) { e.render = ok("") })}},
		{"no trailing newline", []entry{with(func(e *entry) { e.render = ok("# Alpha") })}},
		{"two trailing newlines", []entry{with(func(e *entry) { e.render = ok("# Alpha\n\n") })}},
		{"CRLF body", []entry{with(func(e *entry) { e.render = ok("# Alpha\r\n") })}},
		{"title mismatch", []entry{with(func(e *entry) { e.render = ok("# Beta\n") })}},
		{"second H1", []entry{with(func(e *entry) { e.render = ok("# Alpha\n\n# Again\n") })}},
		{"empty alias", []entry{with(func(e *entry) { e.aliases = []string{""} })}},
		{"reserved alias", []entry{with(func(e *entry) { e.aliases = []string{"topics"} })}},
		{"alias shadows a slug", []entry{
			base,
			{slug: "beta", title: "Beta", summary: "s", kind: string(docbundle.KindReference), outPath: "topics/beta.md",
				render: ok("# Beta\n"), aliases: []string{"alpha"}},
		}},
		{"duplicate alias", []entry{
			with(func(e *entry) { e.aliases = []string{"a1"} }),
			{slug: "beta", title: "Beta", summary: "s", kind: string(docbundle.KindReference), outPath: "topics/beta.md",
				render: ok("# Beta\n"), aliases: []string{"a1"}},
		}},
		{"link to a missing topic", []entry{with(func(e *entry) {
			e.render = ok("# Alpha\n\n[x](nowhere.md)\n")
		})}},
		{"absolute link", []entry{with(func(e *entry) {
			e.render = ok("# Alpha\n\n[x](/topics/alpha.md)\n")
		})}},
		{"external link", []entry{with(func(e *entry) {
			e.render = ok("# Alpha\n\n[x](https://example.com/a.md)\n")
		})}},
		{"escaping link", []entry{with(func(e *entry) {
			e.render = ok("# Alpha\n\n[x](../../secrets.md)\n")
		})}},
		{"non-markdown link", []entry{with(func(e *entry) {
			e.render = ok("# Alpha\n\n[x](alpha.txt)\n")
		})}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := buildCatalog(c.spec); err == nil {
				t.Error("buildCatalog = nil error, want a rejection")
			}
		})
	}
}

// TestBuildCatalogAcceptsAValidSpec is the positive control for the rejection
// table: without it, a buildCatalog that rejected everything would look healthy.
func TestBuildCatalogAcceptsAValidSpec(t *testing.T) {
	spec := []entry{
		{slug: "alpha", title: "Alpha", summary: "s", kind: string(docbundle.KindReference), outPath: "topics/alpha.md",
			aliases: []string{"a"}, render: func() string { return "# Alpha\n\n[beta](beta.md)\n" }},
		{slug: "beta", title: "Beta", summary: "s", kind: string(docbundle.KindReference), outPath: "topics/beta.md",
			render: func() string { return "# Beta\n" }},
	}
	built, index, err := buildCatalog(spec)
	if err != nil {
		t.Fatalf("buildCatalog: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("buildCatalog returned %d topics, want 2", len(built))
	}
	if index["a"] != "alpha" {
		t.Errorf("alias index = %v, want a -> alpha", index)
	}
}
