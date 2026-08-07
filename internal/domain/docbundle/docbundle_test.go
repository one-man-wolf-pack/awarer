package docbundle

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustSlug(t *testing.T, s string) Slug {
	t.Helper()
	v, err := NewSlug(s)
	if err != nil {
		t.Fatalf("NewSlug(%q): %v", s, err)
	}
	return v
}

func mustPath(t *testing.T, p string) DocPath {
	t.Helper()
	v, err := NewDocPath(p)
	if err != nil {
		t.Fatalf("NewDocPath(%q): %v", p, err)
	}
	return v
}

func mustDoc(t *testing.T, slug, p string, kind Kind, aliases []string, body string) Document {
	t.Helper()
	d, err := NewDocument(mustSlug(t, slug), "T "+slug, "s "+slug, kind, mustPath(t, p), aliases, []byte(body))
	if err != nil {
		t.Fatalf("NewDocument(%q): %v", slug, err)
	}
	return d
}

func mustMachine(t *testing.T) MachineRef {
	t.Helper()
	m, err := NewMachineRef(mustPath(t, "reference/cli.json"), 1, []byte("{}\n"))
	if err != nil {
		t.Fatalf("NewMachineRef: %v", err)
	}
	return m
}

func TestNewSlugRejections(t *testing.T) {
	for _, raw := range []string{"", "Alpha", "a_b", "-a", "a-", "a--b", "a/b", "a b", "a.b"} {
		if _, err := NewSlug(raw); err == nil {
			t.Errorf("NewSlug(%q) = nil error, want a rejection", raw)
		}
	}
	for _, raw := range []string{"a", "exit-codes", "a1-b2"} {
		if _, err := NewSlug(raw); err != nil {
			t.Errorf("NewSlug(%q) = %v, want acceptance", raw, err)
		}
	}
}

// TestNewDocPathRejections proves traversal, absolute targets, platform
// separators, and unclean spellings cannot become a DocPath — the property that
// makes "publish every document path" safe without a second check at the writer.
func TestNewDocPathRejections(t *testing.T) {
	for _, raw := range []string{
		"",
		"/topics/a.md",
		"../a.md",
		"topics/../../a.md",
		"topics//a.md",
		"./a.md",
		`topics\a.md`,
		"topics/",
		"topics/a",
		"..",
	} {
		if _, err := NewDocPath(raw); err == nil {
			t.Errorf("NewDocPath(%q) = nil error, want a rejection", raw)
		}
	}
	for _, raw := range []string{"manifest.json", "topics/a.md", "reference/cli.json", "a/b/c.md"} {
		if _, err := NewDocPath(raw); err != nil {
			t.Errorf("NewDocPath(%q) = %v, want acceptance", raw, err)
		}
	}
}

func TestDocPathDirAndBase(t *testing.T) {
	p := mustPath(t, "topics/run.md")
	if p.Dir() != "topics" || p.Base() != "run.md" {
		t.Errorf("Dir/Base = %q/%q, want topics/run.md", p.Dir(), p.Base())
	}
	root := mustPath(t, "manifest.json")
	if root.Dir() != "" || root.Base() != "manifest.json" {
		t.Errorf("root Dir/Base = %q/%q", root.Dir(), root.Base())
	}
}

func TestNewDocumentRejections(t *testing.T) {
	slug := mustSlug(t, "alpha")
	p := mustPath(t, "topics/alpha.md")
	cases := []struct {
		name string
		call func() (Document, error)
	}{
		{"zero slug", func() (Document, error) {
			return NewDocument(Slug{}, "T", "s", KindTopic, p, nil, []byte("x"))
		}},
		{"empty title", func() (Document, error) {
			return NewDocument(slug, "", "s", KindTopic, p, nil, []byte("x"))
		}},
		{"empty summary", func() (Document, error) {
			return NewDocument(slug, "T", "", KindTopic, p, nil, []byte("x"))
		}},
		// The label rules are the parser's rules: a producer must not be able to build
		// a bundle whose manifest the reader would then refuse.
		{"summary carrying a newline", func() (Document, error) {
			return NewDocument(slug, "T", "s\n- [x](https://evil.example): y", KindTopic, p, nil, []byte("x"))
		}},
		{"title carrying a tab", func() (Document, error) {
			return NewDocument(slug, "T\tU", "s", KindTopic, p, nil, []byte("x"))
		}},
		{"summary padded with whitespace", func() (Document, error) {
			return NewDocument(slug, "T", " s ", KindTopic, p, nil, []byte("x"))
		}},
		{"unknown kind", func() (Document, error) {
			return NewDocument(slug, "T", "s", Kind("page"), p, nil, []byte("x"))
		}},
		{"zero path", func() (Document, error) {
			return NewDocument(slug, "T", "s", KindTopic, DocPath{}, nil, []byte("x"))
		}},
		{"empty body", func() (Document, error) {
			return NewDocument(slug, "T", "s", KindTopic, p, nil, nil)
		}},
		{"invalid UTF-8 body", func() (Document, error) {
			return NewDocument(slug, "T", "s", KindTopic, p, nil, []byte{0xff, 0xfe})
		}},
		{"invalid alias", func() (Document, error) {
			return NewDocument(slug, "T", "s", KindTopic, p, []string{"Bad Alias"}, []byte("x"))
		}},
		{"duplicate alias", func() (Document, error) {
			return NewDocument(slug, "T", "s", KindTopic, p, []string{"a", "a"}, []byte("x"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.call(); err == nil {
				t.Error("NewDocument = nil error, want a rejection")
			}
		})
	}
}

// TestDocumentOwnsItsBytes proves the defensive copy in both directions: a caller
// cannot change published content after the hash was taken, and cannot corrupt
// the document by writing into the slice it got back.
func TestDocumentOwnsItsBytes(t *testing.T) {
	body := []byte("# T alpha\n")
	d := mustDoc(t, "alpha", "topics/alpha.md", KindTopic, []string{"a"}, string(body))
	before := d.SHA256()

	body[0] = 'X'
	if d.SHA256() != before || string(d.Body()) != "# T alpha\n" {
		t.Error("mutating the caller's buffer changed the document")
	}

	got := d.Body()
	got[0] = 'X'
	if string(d.Body()) != "# T alpha\n" {
		t.Error("Body() shares its backing array with the document")
	}

	aliases := d.Aliases()
	aliases[0] = "mutated"
	if d.Aliases()[0] != "a" {
		t.Error("Aliases() shares its backing array with the document")
	}
}

func TestNewBundleRejections(t *testing.T) {
	machine := mustMachine(t)
	prov := Provenance{Version: "1.2.3"}
	alpha := mustDoc(t, "alpha", "topics/alpha.md", KindTopic, nil, "a")

	cases := []struct {
		name string
		call func() (Bundle, error)
	}{
		{"no documents", func() (Bundle, error) { return NewBundle(nil, machine, prov) }},
		{"no machine reference", func() (Bundle, error) {
			return NewBundle([]Document{alpha}, MachineRef{}, prov)
		}},
		{"no version", func() (Bundle, error) {
			return NewBundle([]Document{alpha}, machine, Provenance{})
		}},
		{"unparseable version", func() (Bundle, error) {
			return NewBundle([]Document{alpha}, machine, Provenance{Version: "1.2.3 dirty"})
		}},
		{"unparseable commit", func() (Bundle, error) {
			return NewBundle([]Document{alpha}, machine, Provenance{Version: "1.2.3", Commit: "/home/user/repo"})
		}},
		{"duplicate slug", func() (Bundle, error) {
			return NewBundle([]Document{alpha, mustDoc(t, "alpha", "topics/beta.md", KindTopic, nil, "b")}, machine, prov)
		}},
		{"duplicate path", func() (Bundle, error) {
			return NewBundle([]Document{alpha, mustDoc(t, "beta", "topics/alpha.md", KindTopic, nil, "b")}, machine, prov)
		}},
		{"collides with the manifest", func() (Bundle, error) {
			return NewBundle([]Document{mustDoc(t, "beta", "manifest.json", KindTopic, nil, "b")}, machine, prov)
		}},
		{"collides with the machine reference", func() (Bundle, error) {
			return NewBundle([]Document{mustDoc(t, "beta", "reference/cli.json", KindTopic, nil, "b")}, machine, prov)
		}},
		// The machine reference against the manifest specifically: the reserved-path
		// set is a map, and a map literal collapses two identical keys without
		// complaint, so this pair is the one collision the set cannot report on its
		// own. Left unchecked it builds a bundle that looks valid and fails only after
		// the exporter has created output.
		{"machine reference claims the manifest's path", func() (Bundle, error) {
			clash, err := NewMachineRef(mustPath(t, ManifestName), 1, []byte("{}\n"))
			if err != nil {
				t.Fatalf("NewMachineRef(%s): %v", ManifestName, err)
			}
			return NewBundle([]Document{alpha}, clash, prov)
		}},
		{"duplicate alias across documents", func() (Bundle, error) {
			return NewBundle([]Document{
				mustDoc(t, "alpha", "topics/alpha.md", KindTopic, []string{"x"}, "a"),
				mustDoc(t, "beta", "topics/beta.md", KindTopic, []string{"x"}, "b"),
			}, machine, prov)
		}},
		{"alias shadows a slug", func() (Bundle, error) {
			return NewBundle([]Document{
				alpha,
				mustDoc(t, "beta", "topics/beta.md", KindTopic, []string{"alpha"}, "b"),
			}, machine, prov)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.call(); err == nil {
				t.Error("NewBundle = nil error, want a rejection")
			}
		})
	}
}

func testBundle(t *testing.T) Bundle {
	t.Helper()
	b, err := NewBundle([]Document{
		mustDoc(t, "alpha", "topics/alpha.md", KindTopic, []string{"z", "a"}, "# T alpha\n"),
		mustDoc(t, "index", "commands/index.md", KindCommand, nil, "# T index\n"),
		mustDoc(t, "config", "reference/configuration.md", KindReference, nil, "# T config\n"),
	}, mustMachine(t), Provenance{Version: "1.2.3", Commit: "abc1234"})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	return b
}

// TestManifestIsDeterministic is the property the whole export rests on: the
// same bundle renders the same bytes every time, so two exports of one binary
// can be compared byte-for-byte.
func TestManifestIsDeterministic(t *testing.T) {
	b := testBundle(t)
	first, err := b.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := b.ManifestJSON()
		if err != nil {
			t.Fatalf("ManifestJSON: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("ManifestJSON is not deterministic")
		}
	}
}

func TestManifestContents(t *testing.T) {
	b := testBundle(t)
	raw, err := b.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v\n%s", err, raw)
	}

	if m.ExportSchemaVersion != ExportSchemaVersion {
		t.Errorf("export_schema_version = %d, want %d", m.ExportSchemaVersion, ExportSchemaVersion)
	}
	if m.Product.Version != "1.2.3" || m.Product.Commit != "abc1234" {
		t.Errorf("product = %+v", m.Product)
	}
	if m.Corpus.DocumentCount != 3 || m.Corpus.TopicCount != 1 || m.Corpus.CommandCount != 1 || m.Corpus.ReferenceCount != 1 {
		t.Errorf("corpus = %+v", m.Corpus)
	}
	wantBytes := 0
	for _, d := range b.Documents() {
		wantBytes += d.Size()
	}
	if m.Corpus.TotalBytes != wantBytes {
		t.Errorf("total_bytes = %d, want %d", m.Corpus.TotalBytes, wantBytes)
	}
	if m.MachineReference.Path != "reference/cli.json" || m.MachineReference.SchemaVersion != 1 {
		t.Errorf("machine_reference = %+v", m.MachineReference)
	}
	if len(m.Documents) != 3 {
		t.Fatalf("documents = %d, want 3", len(m.Documents))
	}
	// Document order is the bundle's order, and aliases are canonically sorted so
	// the manifest does not depend on the order a catalog declared them in.
	if m.Documents[0].Slug != "alpha" || m.Documents[1].Slug != "index" || m.Documents[2].Slug != "config" {
		t.Errorf("document order = %q/%q/%q", m.Documents[0].Slug, m.Documents[1].Slug, m.Documents[2].Slug)
	}
	if got := strings.Join(m.Documents[0].Aliases, ","); got != "a,z" {
		t.Errorf("aliases = %q, want sorted a,z", got)
	}
	for _, d := range m.Documents {
		if len(d.SHA256) != 64 {
			t.Errorf("document %q sha256 = %q, want 64 hex chars", d.Slug, d.SHA256)
		}
	}
}

// TestManifestOmitsAbsentCommit proves an unstamped development build publishes
// no commit field at all rather than an empty or invented one.
func TestManifestOmitsAbsentCommit(t *testing.T) {
	b, err := NewBundle(
		[]Document{mustDoc(t, "alpha", "topics/alpha.md", KindTopic, nil, "a")},
		mustMachine(t),
		Provenance{Version: "0.0.0-dev"},
	)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	raw, err := b.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}
	if strings.Contains(string(raw), `"commit"`) {
		t.Errorf("manifest carries a commit field for an unstamped build:\n%s", raw)
	}
}

// TestProvenanceChangeAffectsOnlyProductFields is the provenance mutation probe:
// building the same documents from a differently stamped binary must change the
// declared version and commit and nothing else. If a build identity leaked into a
// document body or a content hash, two releases would republish every page and no
// consumer could tell a real content change from a rebuild.
func TestProvenanceChangeAffectsOnlyProductFields(t *testing.T) {
	docs := []Document{
		mustDoc(t, "alpha", "topics/alpha.md", KindTopic, []string{"a"}, "# T alpha\n"),
		mustDoc(t, "config", "reference/configuration.md", KindReference, nil, "# T config\n"),
	}
	build := func(p Provenance) map[string]any {
		b, err := NewBundle(docs, mustMachine(t), p)
		if err != nil {
			t.Fatalf("NewBundle: %v", err)
		}
		raw, err := b.ManifestJSON()
		if err != nil {
			t.Fatalf("ManifestJSON: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	first := build(Provenance{Version: "1.2.3", Commit: "abc1234"})
	second := build(Provenance{Version: "9.9.9", Commit: "def5678"})

	if len(first) != len(second) {
		t.Fatalf("manifest key sets differ: %v vs %v", first, second)
	}
	for key, want := range first {
		got := second[key]
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if key == "product" {
			if string(wantJSON) == string(gotJSON) {
				t.Error("changing the build identity did not change the declared provenance")
			}
			continue
		}
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("changing the build identity also changed %q:\n%s\n%s", key, wantJSON, gotJSON)
		}
	}
}

// TestBundleDocumentsAreCopySafe proves a caller cannot reorder or replace the
// bundle's documents after validation by writing into the returned slice.
func TestBundleDocumentsAreCopySafe(t *testing.T) {
	b := testBundle(t)
	docs := b.Documents()
	docs[0] = docs[2]
	if b.Documents()[0].Slug().String() != "alpha" {
		t.Error("Documents() shares its backing array with the bundle")
	}
}
