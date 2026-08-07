package cli

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"awarer/internal/app/help"
	"awarer/internal/domain/configref"
	"awarer/internal/domain/docbundle"
)

// This file is the documentation drift audit. It proves that the exported documentation
// bundle is a projection of the live models rather than a second manual: one
// catalog behind the topic index and the manifest, one registry behind every
// documented command/flag/capability, one config model behind the configuration
// reference, one exit-code catalog behind the exit-code page — and that the whole
// thing is byte-deterministic and internally linked.

func mustBundle(t *testing.T) docbundle.Bundle {
	t.Helper()
	b, err := buildDocsBundle()
	if err != nil {
		t.Fatalf("buildDocsBundle: %v", err)
	}
	return b
}

// bundleBodies indexes the bundle's documents by output path.
func bundleBodies(t *testing.T, b docbundle.Bundle) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range b.Documents() {
		out[d.Path().String()] = string(d.Body())
	}
	return out
}

// docBySlug indexes the bundle's documents by slug.
func docBySlug(t *testing.T, b docbundle.Bundle) map[string]docbundle.Document {
	t.Helper()
	out := map[string]docbundle.Document{}
	for _, d := range b.Documents() {
		out[d.Slug().String()] = d
	}
	return out
}

// TestBundleTopicsComeFromTheOneCatalog proves requirement 1: the topic index,
// alias resolution, the manifest, and the exported Markdown all read the same
// catalog. Every field is compared, including the body bytes — so an exporter
// that re-rendered or reformatted a page instead of publishing what "awa help"
// serves would fail here.
func TestBundleTopicsComeFromTheOneCatalog(t *testing.T) {
	b := mustBundle(t)
	bySlug := docBySlug(t, b)

	for _, topic := range help.Topics() {
		doc, ok := bySlug[topic.Slug.String()]
		if !ok {
			t.Errorf("catalog topic %q is missing from the bundle", topic.Slug)
			continue
		}
		if doc.Title() != topic.Title {
			t.Errorf("topic %q: bundle title %q, catalog title %q", topic.Slug, doc.Title(), topic.Title)
		}
		if doc.Summary() != topic.Summary {
			t.Errorf("topic %q: bundle summary %q, catalog summary %q", topic.Slug, doc.Summary(), topic.Summary)
		}
		if doc.Path() != topic.Path {
			t.Errorf("topic %q: bundle path %q, catalog path %q", topic.Slug, doc.Path(), topic.Path)
		}
		if string(doc.Body()) != topic.Body {
			t.Errorf("topic %q: exported bytes differ from what 'awa help %s' serves", topic.Slug, topic.Slug)
		}
		wantAliases := strings.Join(sortedCopy(topic.Aliases), ",")
		if got := strings.Join(doc.Aliases(), ","); got != wantAliases {
			t.Errorf("topic %q: bundle aliases %q, catalog aliases %q", topic.Slug, got, wantAliases)
		}
	}

	// Reverse direction: no bundle document may claim a topic slug the catalog
	// does not own, and every alias in the manifest comes from the catalog.
	catalogSlugs := map[string]bool{}
	for _, topic := range help.Topics() {
		catalogSlugs[topic.Slug.String()] = true
	}
	catalogAliases := help.Aliases()
	for _, doc := range b.Documents() {
		for _, a := range doc.Aliases() {
			if catalogAliases[a] != doc.Slug().String() {
				t.Errorf("bundle document %q publishes alias %q, which the catalog does not map to it", doc.Slug(), a)
			}
		}
		if doc.Kind() != docbundle.KindTopic {
			continue
		}
		if !catalogSlugs[doc.Slug().String()] {
			t.Errorf("bundle publishes topic %q, which is not in the catalog", doc.Slug())
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestGeneratedReferenceCoversTheWholeSurface proves requirement 2: every
// command, subcommand, flag spelling, flag default, and accepted capability the
// registry defines is represented in the generated Markdown. Adding a flag row or
// a subcommand without regenerating the pages fails here.
func TestGeneratedReferenceCoversTheWholeSurface(t *testing.T) {
	b := mustBundle(t)
	bodies := bundleBodies(t, b)

	index, ok := bodies[docsCommandIndexPath]
	if !ok {
		t.Fatal("the bundle has no command index")
	}

	for _, c := range newRouter().commands {
		if !strings.Contains(index, "awa "+c.name) {
			t.Errorf("command index omits %q", c.name)
		}
		page, ok := bodies[commandDocPath(c.name)]
		if !ok {
			t.Errorf("no generated page for command %q", c.name)
			continue
		}
		assertFlagsDocumented(t, c.name, page, c.help.flags)
		for _, id := range capabilityIDs(c.capabilities) {
			for _, s := range capabilityInfo[capability(id)].spellings {
				// The full spelling, value placeholder included: a page that named
				// the flag but dropped what it takes would state syntax the parser
				// does not accept.
				want := s.flag
				if s.value != "" {
					want += " " + s.value
				}
				if !strings.Contains(page, want) {
					t.Errorf("command page %q omits accepted global %q", c.name, want)
				}
			}
		}
		for _, sc := range c.subcommands {
			heading := "### awa " + c.name + " " + sc.name
			if !strings.Contains(page, heading) {
				t.Errorf("command page %q omits subcommand %q", c.name, sc.name)
				continue
			}
			assertFlagsDocumented(t, c.name+" "+sc.name, page, sc.help.flags)
		}
	}
}

// TestEveryLiveOperandKindRendersProseOnItsPage is the assembly fuse over the two
// writeOperandSection call sites. It proves two things about every operand kind the
// router actually reaches: operandProse returns content for it, and that content
// reaches the command's generated page.
//
// Both halves are needed, and each fails on its own. operandProse renders nothing for a
// kind it does not recognize — a newly minted operandKind, or one whose String() falls
// through to "unknown" — and writeOperandSection then omits the section in silence. A
// call site that stops being reached empties the same section while every kind still
// renders. Neither shows up anywhere else: the machine reference publishes the kind for
// machines, and this section is what a human or an agent reads. The kind string is the
// one the renderer consumes, so the two cannot drift apart — reference.go builds
// referenceCommand.Operands from the same String().
//
// The page expectation is a COUNT, not mere presence. A page carries the command's own
// contract plus one per subcommand, and several of them legitimately share a kind, so a
// page that rendered the parent's sentence and dropped every subcommand's would still
// contain the right string.
//
// operandProse is the expected value because this proves wiring, not wording. Which
// sentence a kind renders is the renderer's to choose; asserting an independently
// written sentence is what this replaced.
func TestEveryLiveOperandKindRendersProseOnItsPage(t *testing.T) {
	bodies := bundleBodies(t, mustBundle(t))
	sites := 0

	for _, c := range newRouter().commands {
		want := map[operandKind]int{}
		users := map[operandKind][]string{}
		record := func(kind operandKind, label string) {
			want[kind]++
			users[kind] = append(users[kind], label)
			sites++
		}
		record(c.operands, "awa "+c.name)
		for _, sc := range c.subcommands {
			record(sc.operands, "awa "+c.name+" "+sc.name)
		}

		page, ok := bodies[commandDocPath(c.name)]
		if !ok {
			// TestGeneratedReferenceCoversTheWholeSurface owns the missing page itself;
			// say what went unchecked here rather than repeat its finding.
			t.Errorf("command %q has no generated page, so its terminator contract went unchecked", c.name)
			continue
		}
		for kind, n := range want {
			prose := operandProse(kind.String())
			if prose == "" {
				t.Errorf("operand kind %q is reachable from %v but renders no prose; "+
					"their generated pages would omit the terminator contract", kind, users[kind])
				continue
			}
			if got := strings.Count(page, prose); got < n {
				t.Errorf("command page %q states the %q terminator contract %d time(s), want %d, "+
					"one for each of %v; a writeOperandSection call site is not reaching the page",
					c.name, kind, got, n, users[kind])
			}
		}
	}
	if sites == 0 {
		t.Fatal("the router declares no operand sites; the walk looks vacuous")
	}
}

// assertFlagsDocumented checks that every spelling and default of every authored
// flag row appears on the rendered page.
func assertFlagsDocumented(t *testing.T, label, page string, flags []flagHelp) {
	t.Helper()
	for _, f := range flags {
		for _, tok := range flagHelpTokens(f.display) {
			if !strings.Contains(page, tok) {
				t.Errorf("%s: generated page omits flag %q", label, tok)
			}
		}
		if f.def != "" && !strings.Contains(page, f.def) {
			t.Errorf("%s: generated page omits the default %q for %q", label, f.def, f.display)
		}
		// A value-taking flag must publish the placeholder it expects, not merely
		// the fact that some value is needed.
		if v := flagValueName(f.display); v != "" && !strings.Contains(page, v) {
			t.Errorf("%s: generated page omits the value placeholder %q for %q", label, v, f.display)
		}
	}
}

// TestGeneratedExitCodeReferenceMatchesCatalog proves the exit-code page is the
// catalog, in both directions: every produced code is published with its stable
// machine name, and the page publishes nothing the binary cannot return.
func TestGeneratedExitCodeReferenceMatchesCatalog(t *testing.T) {
	b := mustBundle(t)
	page, ok := bundleBodies(t, b)[docsExitCodesPath]
	if !ok {
		t.Fatal("the bundle has no exit-code reference")
	}

	rowRe := regexp.MustCompile("(?m)^- `(\\d+)` — `([a-z-]+)`$")
	published := map[int]string{}
	for _, m := range rowRe.FindAllStringSubmatch(page, -1) {
		n, _ := strconv.Atoi(m[1])
		published[n] = m[2]
	}

	for _, code := range exitCodes() {
		name, ok := published[int(code)]
		if !ok {
			t.Errorf("exit-code reference omits code %d", int(code))
			continue
		}
		if name != code.refName() {
			t.Errorf("exit-code reference names %d %q, catalog says %q", int(code), name, code.refName())
		}
	}
	catalog := map[int]bool{}
	for _, code := range exitCodes() {
		catalog[int(code)] = true
	}
	for n := range published {
		if !catalog[n] {
			t.Errorf("exit-code reference publishes code %d that no command produces", n)
		}
	}
	// The wrapped-child contract is part of the same page, keyed on the machine
	// field a consumer must read.
	if !strings.Contains(page, "data.run.exit_origin") {
		t.Errorf("exit-code reference omits the run exit-origin field:\n%s", page)
	}
}

// TestGeneratedConfigurationReferenceMatchesModel proves requirement 3: the
// exported configuration page states every section, key, and default the decoder
// accepts, because it is rendered from that same model.
func TestGeneratedConfigurationReferenceMatchesModel(t *testing.T) {
	b := mustBundle(t)
	page, ok := bundleBodies(t, b)[docsReferenceDir+"/configuration.md"]
	if !ok {
		t.Fatal("the bundle has no configuration reference")
	}
	for _, s := range configref.Sections() {
		if !strings.Contains(page, "["+s.Name+"]") {
			t.Errorf("configuration reference omits section [%s]", s.Name)
		}
		for _, f := range s.Fields {
			if !strings.Contains(page, "`"+f.Key+"`") {
				t.Errorf("configuration reference omits key [%s].%s", s.Name, f.Key)
			}
			if !strings.Contains(page, f.Default) {
				t.Errorf("configuration reference omits the default %q of [%s].%s", f.Default, s.Name, f.Key)
			}
		}
	}
	// It is one document, not a copy: the config topic and the exported reference
	// are the same page at the same path.
	topic, found := help.Resolve("config")
	if !found {
		t.Fatal("the config topic does not resolve")
	}
	if topic.Path.String() != docsReferenceDir+"/configuration.md" || topic.Body != page {
		t.Errorf("awa help config and the exported configuration reference are two documents")
	}
}

// TestBundleIsDeterministic proves requirement 4 at the source: for one binary,
// two builds produce identical documents, paths, hashes, and manifest bytes.
func TestBundleIsDeterministic(t *testing.T) {
	first := mustBundle(t)
	firstManifest, err := first.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	for i := 0; i < 3; i++ {
		again := mustBundle(t)
		againManifest, err := again.ManifestJSON()
		if err != nil {
			t.Fatalf("ManifestJSON: %v", err)
		}
		if string(againManifest) != string(firstManifest) {
			t.Fatal("the manifest is not byte-deterministic across builds")
		}
		a, bDocs := first.Documents(), again.Documents()
		if len(a) != len(bDocs) {
			t.Fatalf("document count changed between builds: %d vs %d", len(a), len(bDocs))
		}
		for j := range a {
			if a[j].Path().String() != bDocs[j].Path().String() || a[j].SHA256() != bDocs[j].SHA256() {
				t.Fatalf("document %d differs between builds: %s/%s vs %s/%s",
					j, a[j].Path(), a[j].SHA256(), bDocs[j].Path(), bDocs[j].SHA256())
			}
		}
	}
}

// TestManifestDescribesEveryPublishedDocument proves the manifest is a complete
// and accurate index: one entry per document, with the path and content hash a
// consumer verifies the published files against.
func TestManifestDescribesEveryPublishedDocument(t *testing.T) {
	b := mustBundle(t)
	raw, err := b.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	var m struct {
		ExportSchemaVersion int `json:"export_schema_version"`
		Corpus              struct {
			DocumentCount int `json:"document_count"`
			TotalBytes    int `json:"total_bytes"`
		} `json:"corpus"`
		MachineReference struct {
			Path          string `json:"path"`
			SchemaVersion int    `json:"schema_version"`
		} `json:"machine_reference"`
		Documents []struct {
			Slug   string `json:"slug"`
			Path   string `json:"path"`
			Kind   string `json:"kind"`
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}

	docs := b.Documents()
	if len(m.Documents) != len(docs) || m.Corpus.DocumentCount != len(docs) {
		t.Fatalf("manifest lists %d documents (corpus says %d), bundle has %d", len(m.Documents), m.Corpus.DocumentCount, len(docs))
	}
	for i, entry := range m.Documents {
		if entry.Path != docs[i].Path().String() || entry.SHA256 != docs[i].SHA256() || entry.Bytes != docs[i].Size() {
			t.Errorf("manifest entry %d (%s) does not describe the document it publishes", i, entry.Slug)
		}
		if entry.Kind == "" {
			t.Errorf("manifest entry %s has no kind", entry.Slug)
		}
	}
	if m.MachineReference.Path != b.MachineReference().Path().String() ||
		m.MachineReference.SchemaVersion != referenceSchemaVersion {
		t.Errorf("manifest machine reference = %+v", m.MachineReference)
	}
	if m.ExportSchemaVersion != docbundle.ExportSchemaVersion {
		t.Errorf("manifest export schema = %d", m.ExportSchemaVersion)
	}
}

// TestBundleBodiesAreCanonicalMarkdown pins the shape every published document
// keeps, generated ones included: LF newlines, one final newline, and exactly one
// H1 that equals the manifest title. A site generator can rely on it, and a
// terminal reader sees the same bytes.
func TestBundleBodiesAreCanonicalMarkdown(t *testing.T) {
	for _, doc := range mustBundle(t).Documents() {
		body := string(doc.Body())
		if strings.ContainsRune(body, '\r') {
			t.Errorf("document %q contains CR", doc.Slug())
		}
		if !strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\n\n") {
			t.Errorf("document %q must end with exactly one newline", doc.Slug())
		}
		lines := strings.Split(body, "\n")
		if want := "# " + doc.Title(); lines[0] != want {
			t.Errorf("document %q starts with %q, want %q", doc.Slug(), lines[0], want)
		}
		for _, line := range lines[1:] {
			if strings.HasPrefix(line, "# ") {
				t.Errorf("document %q has a second H1: %q", doc.Slug(), line)
			}
		}
	}
}

// bundleCodeSpanRe matches a fenced block or an inline code span, including one
// that wraps across lines. Everything it matches is text no Markdown renderer
// interprets, so it is masked before the raw-HTML scan below.
var bundleCodeSpanRe = regexp.MustCompile("(?s)```.*?```|`[^`]*`")

// maskCode blanks the code a Markdown renderer does not interpret, keeping every
// line break and every column. Deleting it instead would shift the position the
// diagnostic reports, and a report that names the wrong line is exactly the
// unattributable failure this test exists to prevent.
func maskCode(body string) string {
	return bundleCodeSpanRe.ReplaceAllStringFunc(body, func(m string) string {
		masked := []byte(m)
		for i, c := range masked {
			if c != '\n' {
				masked[i] = ' '
			}
		}
		return string(masked)
	})
}

// bundleHTMLTagRe matches an unescaped "<" that opens an HTML tag, an autolink, a
// comment, or a processing instruction — the spellings CommonMark parses as raw
// HTML. A backslash-escaped "<" renders as a literal character and is fine, so
// the preceding byte is part of the match (Go's regexp has no lookbehind).
var bundleHTMLTagRe = regexp.MustCompile(`(^|[^\\])(<[A-Za-z/!?])`)

// TestExportedBodiesCarryNoRawHTML proves the published corpus is Markdown a
// renderer can show verbatim. Registry help is written for a terminal, where
// "<count>" is a placeholder; embedded in Markdown prose unescaped it parses as
// inline HTML and a renderer silently drops it, so a published page would read
// "show at most  entries" — a defect no reader could attribute and no exporter
// test would notice, because the bytes are still valid UTF-8 Markdown.
//
// The scan is deliberately independent of mdProse: it masks code, then looks for
// the opener, rather than asking the escaper whether it escaped. The masked copy
// is only what is searched — the source line is what a failure quotes.
func TestExportedBodiesCarryNoRawHTML(t *testing.T) {
	checked := 0
	for _, doc := range mustBundle(t).Documents() {
		checked++
		body := string(doc.Body())
		source := strings.Split(body, "\n")
		for i, line := range strings.Split(maskCode(body), "\n") {
			if loc := bundleHTMLTagRe.FindStringSubmatchIndex(line); loc != nil {
				t.Errorf("document %q line %d parses as raw HTML at column %d: %q\n"+
					"registry text embedded as Markdown prose must go through mdProse",
					doc.Slug(), i+1, loc[4]+1, source[i])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no documents were scanned")
	}
}

// TestMaskCodeKeepsEveryPositionItBlanks pins what the scan above relies on: the
// masked copy is byte-for-byte the same shape as the source, so a line index and
// a column found in one address the same character in the other.
func TestMaskCodeKeepsEveryPositionItBlanks(t *testing.T) {
	const source = "Prose with `<inline>` code.\n\n```text\nawa run -- go test ./...\n```\n\nMore <b> prose.\n"
	masked := maskCode(source)

	if len(masked) != len(source) {
		t.Fatalf("masked body is %d bytes, source is %d", len(masked), len(source))
	}
	if got, want := strings.Split(masked, "\n"), strings.Split(source, "\n"); len(got) != len(want) {
		t.Fatalf("masked body has %d lines, source has %d", len(got), len(want))
	}
	if strings.Contains(masked, "<inline>") || strings.Contains(masked, "<target>") {
		t.Errorf("code was not masked:\n%s", masked)
	}
	// Prose is untouched, or the scan would have nothing left to find.
	if !strings.Contains(masked, "More <b> prose.") {
		t.Errorf("prose was masked:\n%s", masked)
	}
}

// bundleLinkRe matches a Markdown inline link target.
var bundleLinkRe = regexp.MustCompile(`\]\(([^)]*)\)`)

// TestExportedLinksResolveWithinTheBundle proves requirement 8's link half:
// every cross-document link in the published corpus, authored or generated,
// resolves to a file the same export actually writes.
func TestExportedLinksResolveWithinTheBundle(t *testing.T) {
	b := mustBundle(t)
	published := map[string]bool{b.MachineReference().Path().String(): true}
	for _, d := range b.Documents() {
		published[d.Path().String()] = true
	}

	checked := 0
	for _, d := range b.Documents() {
		dir := d.Path().Dir()
		for _, m := range bundleLinkRe.FindAllStringSubmatch(string(d.Body()), -1) {
			target := m[1]
			if strings.Contains(target, "://") {
				t.Errorf("document %q links off-bundle to %q", d.Slug(), target)
				continue
			}
			checked++
			resolved := joinBundlePath(dir, target)
			if !published[resolved] {
				t.Errorf("document %q links to %q, which resolves to %q and is not published", d.Slug(), target, resolved)
			}
		}
	}
	// Guard against a vacuous pass structurally rather than against a corpus count.
	// This also covers an empty bundle, which cannot produce a link to check.
	if checked == 0 {
		t.Error("no links checked; the link scan looks vacuous")
	}
}

// joinBundlePath resolves a link target relative to the linking document's
// directory, both expressed as bundle-relative slash paths.
func joinBundlePath(dir, target string) string {
	parts := []string{}
	if dir != "" {
		parts = strings.Split(dir, "/")
	}
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			} else {
				parts = append(parts, "..")
			}
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "/")
}

// fencedCommandRe matches an "awa <token>" invocation at the start of a line.
var fencedCommandRe = regexp.MustCompile(`(?m)^awa ([^\s]+)(?:\s+([^\s]+))?`)

// TestExportedExamplesNameRealCommands proves requirement 8's example half: every
// "awa ..." line inside a fenced block names a command the registry implements,
// and where that command owns subcommands, a following word names a real one. A
// renamed or removed command leaves a stale example that fails here.
func TestExportedExamplesNameRealCommands(t *testing.T) {
	commands := map[string][]string{} // command name -> subcommand names
	for _, c := range newRouter().commands {
		var subs []string
		for _, sc := range c.subcommands {
			subs = append(subs, sc.name)
		}
		commands[c.name] = subs
	}

	checked := 0
	for _, d := range mustBundle(t).Documents() {
		for _, line := range fencedLines(string(d.Body())) {
			m := fencedCommandRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			checked++
			subs, ok := commands[m[1]]
			if !ok {
				t.Errorf("document %q shows %q, but %q is not a command", d.Slug(), line, m[1])
				continue
			}
			word := m[2]
			// Only a plain word can be a subcommand: a flag, a placeholder, or a
			// bracketed synopsis token is something else.
			if len(subs) == 0 || word == "" || !isPlainWord(word) {
				continue
			}
			if !containsString(subs, word) {
				t.Errorf("document %q shows %q, but %q is not an %q subcommand", d.Slug(), line, word, m[1])
			}
		}
	}
	if checked == 0 {
		t.Error("no example lines checked; the example scan looks vacuous")
	}
}

// TestExportedExampleFlagsAreDocumented is the other half of example checking: a
// command name can be right while the flags beside it are wrong, and a misspelled
// flag in an example is a command an agent will run and see rejected.
//
// Membership in a command's documented flag rows is equivalent to parser
// acceptance, and that equivalence is itself proven, in both directions:
// TestHelpFlagsAreAcceptedByParser shows every documented flag parses, and
// TestParserAcceptsNoUndocumentedFlag shows the parser accepts nothing that is not
// documented. So checking a token against the flag rows here is checking it against
// the parser, without executing 200 example lines.
func TestExportedExampleFlagsAreDocumented(t *testing.T) {
	globals := globalFlagTokens()
	commands := map[string]map[string]bool{} // command name -> its documented flag tokens
	subcommands := map[string]map[string]bool{}
	for _, c := range newRouter().commands {
		commands[c.name] = tokenSet(commandFlagTokens(c.help))
		for _, sc := range c.subcommands {
			subcommands[c.name+" "+sc.name] = tokenSet(commandFlagTokens(sc.help))
		}
	}

	checked := 0
	for _, d := range mustBundle(t).Documents() {
		for _, line := range fencedLines(string(d.Body())) {
			m := fencedCommandRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			accepted, ok := commands[m[1]]
			if !ok {
				continue // TestExportedExamplesNameRealCommands owns this failure
			}
			label := "awa " + m[1]
			if sub, ok := subcommands[m[1]+" "+m[2]]; ok && isPlainWord(m[2]) {
				accepted, label = sub, label+" "+m[2]
			}
			for _, tok := range exampleFlagTokens(line) {
				checked++
				if globals[tok] || accepted[tok] {
					continue
				}
				t.Errorf("document %q shows %q, but %q is not a documented flag of %q",
					d.Slug(), line, tok, label)
			}
		}
	}
	if checked == 0 {
		t.Error("no example flag tokens checked; the flag scan looks vacuous")
	}
}

// exampleFlagTokens returns the awa-owned flag spellings on one example line.
//
// Everything from a standalone "--" onward is dropped: it belongs to the wrapped
// child command, whose flags are not awa's to validate. A trailing "# comment" is
// dropped for the same reason — prose beside an example is not part of it.
func exampleFlagTokens(line string) []string {
	if i := strings.Index(line, " -- "); i >= 0 {
		line = line[:i]
	}
	if i := strings.Index(line, " #"); i >= 0 {
		line = line[:i]
	}
	var out []string
	for _, m := range helpFlagTokenRe.FindAllStringSubmatch(line, -1) {
		out = append(out, m[1])
	}
	return out
}

// tokenSet indexes a token slice for membership tests.
func tokenSet(toks []string) map[string]bool {
	set := make(map[string]bool, len(toks))
	for _, t := range toks {
		set[t] = true
	}
	return set
}

// isPlainWord reports whether tok is a bare lowercase word — the only shape a
// subcommand name can have, as opposed to a flag, a placeholder, or a range.
func isPlainWord(tok string) bool {
	for _, r := range tok {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return tok != ""
}

// fencedLines returns the lines inside fenced code blocks of a Markdown body.
func fencedLines(body string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, line)
		}
	}
	return out
}

// TestBundleCarriesTheCheckedMachineReference proves the machine artifact in the
// bundle is the same document `just check` pins to a golden, so the
// export cannot ship a reference the repository never reviewed.
func TestBundleCarriesTheCheckedMachineReference(t *testing.T) {
	ref := mustBundle(t).MachineReference()
	if got := string(ref.Body()); got != referenceGolden {
		t.Error("the exported machine reference differs from testdata/reference.json.\nRegenerate with: just reference-update")
	}
	if ref.SchemaVersion() != referenceSchemaVersion {
		t.Errorf("machine reference schema = %d, want %d", ref.SchemaVersion(), referenceSchemaVersion)
	}
}
