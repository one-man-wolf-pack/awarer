package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"awarer/internal/domain/docbundle"
)

// Tests run against a synthetic export this file writes. It is assembled through
// docbundle's own constructors and ManifestJSON, not through a second wire
// struct declared here: the export's schema has one owner, and a copy of it in
// this package would be exactly the duplicated authority the site generator
// exists without.
//
// What the fixture supplies is the input encoding. Nothing asserted anywhere in
// this package comes from docbundle — the subjects are routes, rendered HTML,
// navigation, discovery artifacts, and release labelling, none of which the
// bundle model produces. A test that needs an export the producer would refuse
// makes that one mutation on the written bytes, at the point that needs it.

// fakeDoc is one document in a synthetic export.
type fakeDoc struct {
	slug    string
	title   string
	summary string
	kind    string
	path    string
	aliases []string
	body    string
}

const machineRefPath = "reference/cli.json"
const machineRefBody = "{\"schema_version\": 1}\n"

// defaultDocs is a corpus small enough to reason about and shaped like the real
// one: three kinds, cross-directory links, an alias, an anchor reference, and a
// document whose basename repeats in another directory.
func defaultDocs() []fakeDoc {
	return []fakeDoc{
		{
			slug: "alpha", title: "Alpha topic", summary: "the first topic", kind: "topic",
			path: "topics/alpha.md", aliases: []string{"a-first"},
			body: "# Alpha topic\n\nSee [the reference](../reference/beta.md) and [gamma](../commands/gamma.md).\n\n" +
				"## Section one\n\nWords with `<placeholder>` inline code.\n\n```text\nawa status\n```\n",
		},
		{
			slug: "beta", title: "Beta reference", summary: "the reference page", kind: "reference",
			path: "reference/beta.md",
			body: "# Beta reference\n\nBack to [alpha](../topics/alpha.md#section-one).\n\n## Options\n\nMore words.\n",
		},
		{
			slug: "command-gamma", title: "awa gamma", summary: "the gamma command", kind: "command",
			path: "commands/gamma.md",
			body: "# awa gamma\n\nRuns gamma. See [alpha](../topics/alpha.md).\n",
		},
	}
}

// landingDocs is defaultDocs extended with the documents the landing page links
// to, so a build test exercises the real landing rather than a stub.
func landingDocs(t *testing.T) []fakeDoc {
	t.Helper()
	docs := defaultDocs()
	for _, c := range landingCopy.cards {
		docs = append(docs, fakeDoc{
			slug: c.slug, title: "Title " + c.slug, summary: "summary " + c.slug, kind: "topic",
			path: "topics/" + c.slug + ".md",
			body: "# Title " + c.slug + "\n\nWords.\n",
		})
	}
	return docs
}

// writeExport writes a complete, valid export directory and returns its path.
func writeExport(t *testing.T, docs []fakeDoc) string {
	t.Helper()
	dir := t.TempDir()

	built := make([]docbundle.Document, 0, len(docs))
	for _, d := range docs {
		slug, err := docbundle.NewSlug(d.slug)
		if err != nil {
			t.Fatalf("slug %q: %v", d.slug, err)
		}
		p, err := docbundle.NewDocPath(d.path)
		if err != nil {
			t.Fatalf("path %q: %v", d.path, err)
		}
		kind, err := docbundle.ParseKind(d.kind)
		if err != nil {
			t.Fatalf("kind %q: %v", d.kind, err)
		}
		doc, err := docbundle.NewDocument(slug, d.title, d.summary, kind, p, d.aliases, []byte(d.body))
		if err != nil {
			t.Fatalf("document %q: %v", d.slug, err)
		}
		built = append(built, doc)
	}

	refPath, err := docbundle.NewDocPath(machineRefPath)
	if err != nil {
		t.Fatalf("machine reference path: %v", err)
	}
	ref, err := docbundle.NewMachineRef(refPath, 2, []byte(machineRefBody))
	if err != nil {
		t.Fatalf("machine reference: %v", err)
	}

	// A full commit sha, because that is what a real export carries: the exporting
	// binary reports the one it was built from. A short stand-in would make every
	// fixture here disagree in shape with the thing it stands for.
	bundle, err := docbundle.NewBundle(built, ref, docbundle.Provenance{
		Version: "1.2.3",
		Commit:  "3f2a1c9d8e7b6a5f4e3d2c1b0a9f8e7d6c5b4a39",
	})
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	manifest, err := bundle.ManifestJSON()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}

	writeExportFile(t, dir, machineRefPath, machineRefBody)
	for _, d := range docs {
		writeExportFile(t, dir, d.path, d.body)
	}
	writeExportFile(t, dir, docbundle.ManifestName, string(manifest))
	return dir
}

// writeExportFile writes one file into the export, creating parents.
func writeExportFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	dest := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(dest), err)
	}
	if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", dest, err)
	}
}

// loadFixture writes an export and reads it back, which is what every build test
// needs before it can build anything.
func loadFixture(t *testing.T, docs []fakeDoc) *Bundle {
	t.Helper()
	b, err := loadBundle(context.Background(), writeExport(t, docs))
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	return b
}

// testBaseURL is the origin the tests build against.
func testBaseURL(t *testing.T) BaseURL {
	t.Helper()
	b, err := ParseBaseURL("https://example.test")
	if err != nil {
		t.Fatalf("ParseBaseURL: %v", err)
	}
	return b
}
