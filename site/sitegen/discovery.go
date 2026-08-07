package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"awarer/internal/domain/docbundle"
)

// Discovery artifacts are projections of the same manifest the pages are built
// from, never a second list to maintain. Each is produced by one traversal of
// that manifest, so a document appears in every one of them exactly once by
// construction.
//
// # Crawling is not indexing
//
// robots.txt is a crawl policy and cannot express an indexing decision either way.
// This site is public documentation and wants to be read, so retrieval is open to
// everything published: a blanket Disallow would stop a well-behaved crawler from
// ever fetching the page whose canonical metadata describes it, and it would block
// llms.txt, which exists to be fetched. What each page is — product, release,
// canonical address, purpose — is stated in the page's own metadata, where a reader
// that fetched it can act on it.
//
// The one thing this file is the right place for is sitemap discovery, so it
// announces the sitemap's absolute URL — absolute because that is the only form
// a crawler can resolve from a file it may have fetched from any host serving
// this tree.

// maxLLMsFullBytes bounds the concatenated corpus. The real one is a few hundred
// kilobytes; the cap exists so growth is a decision someone makes rather than a
// file that quietly becomes unusable. Exceeding it fails the build — truncating
// would publish a corpus that silently stops mid-document.
const maxLLMsFullBytes = 2 << 20

// renderRobots builds the crawl policy and announces sitemap discovery.
func renderRobots(base BaseURL) []byte {
	return []byte(strings.Join([]string{
		"# awarer documentation site.",
		"#",
		"# This file governs crawling, not indexing. Crawling every published route is",
		"# intended: the pages, the machine-discovery artifacts, and llms.txt all exist to",
		"# be fetched. Blocking retrieval here would only keep a crawler from reading the",
		"# canonical metadata that describes each page.",
		"User-agent: *",
		"Allow: /",
		"",
		"Sitemap: " + base.Absolute(routeSitemap),
		"",
	}, "\n"))
}

// urlEntry is one sitemap URL. Marshalled rather than concatenated so a URL that
// ever needs escaping is escaped by the encoder, not by a rule this file
// remembers to apply.
type urlEntry struct {
	Loc string `xml:"loc"`
}

// urlSet is the sitemap document.
type urlSet struct {
	XMLName xml.Name   `xml:"urlset"`
	NS      string     `xml:"xmlns,attr"`
	URLs    []urlEntry `xml:"url"`
}

// renderSitemap lists the canonical pages, in publication order.
//
// There is no lastmod: the export carries no timestamp by design, so any date
// here would have to be invented from the clock — which would make two builds of
// the same input differ.
func renderSitemap(base BaseURL, routes []string) ([]byte, error) {
	set := urlSet{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, r := range routes {
		set.URLs = append(set.URLs, urlEntry{Loc: base.Absolute(r)})
	}
	body, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("rendering the sitemap: %w", err)
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.Write(body)
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// renderLLMs builds the concise map an automated reader starts from: which
// release this is, where the authority for it lives, and one line per document
// with the title and summary the manifest carries.
//
// It says nothing about the product in its own words. Every claim here would be a
// second place where awa is described, and this file is a projection of an export
// — a reader that wants to know what the tool does follows the first link. The
// landing page is the only surface allowed to speak for itself.
//
// A preview build says so here, not only in the page banner. An automated reader
// that starts at llms.txt would otherwise be told "Release: awa 0.0.0-dev" — a
// version the installation document defines as "built from source, not a
// release" — and would have no way to see the contradiction.
func renderLLMs(base BaseURL, m docbundle.Manifest, groups []navGroup, preview bool) []byte {
	var b strings.Builder
	b.WriteString("# awarer\n\n")
	if preview {
		fmt.Fprintf(&b, "Development preview: built from an unreleased binary reporting version %s. "+
			"This is not the published documentation of any release.\n", m.Provenance().Version)
	} else {
		fmt.Fprintf(&b, "Release: awa %s\n", m.Provenance().Version)
	}
	if c := m.Provenance().Commit; c != "" {
		fmt.Fprintf(&b, "Source commit: %s\n", c)
	}
	b.WriteString("Authority: the released binary. This site renders that binary's own documentation export; ")
	b.WriteString("`awa help <topic>` and `awa docs export` produce the same content offline.\n\n")

	for _, g := range groups {
		fmt.Fprintf(&b, "## %s\n\n", g.Name)
		for _, l := range g.Links {
			fmt.Fprintf(&b, "- [%s](%s): %s\n", l.Title, base.Absolute(l.Route), l.Summary)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Machine reference\n\n")
	fmt.Fprintf(&b, "- [Generated CLI reference](%s): every command, flag, capability, and exit code as JSON (schema %d).\n",
		base.Absolute(routeMachineRef), m.MachineReference().SchemaVersion())
	fmt.Fprintf(&b, "- [Full corpus](%s): every document above, concatenated as Markdown.\n", base.Absolute(routeLLMsFull))
	return []byte(b.String())
}

// renderLLMsFull concatenates the canonical Markdown corpus. Bodies are the
// exported bytes, unmodified: an automated reader that fetches this file gets
// exactly what the binary carries, not a re-rendering of it. Each document is
// preceded by its canonical URL so a fragment retrieved from the middle can still
// be attributed.
//
// The bodies keep their own relative links, which point at paths inside an
// export rather than at site routes. Rewriting them would mean editing Markdown
// with string surgery on text this file does not parse — including inside code
// blocks, where an example may legitimately look like a link. The header below
// states the convention instead, and the per-document canonical URL gives a
// reader the resolvable address of anything it wants to follow.
func renderLLMsFull(base BaseURL, b *Bundle) ([]byte, error) {
	var out bytes.Buffer
	m := b.Manifest()

	fmt.Fprintf(&out, "# awarer documentation corpus (awa %s)\n\n", m.Provenance().Version)
	out.WriteString("Every document the release carries, in publication order, as exported Markdown.\n")
	out.WriteString("Bodies are reproduced byte for byte from `awa docs export`; the relative `.md` links\n")
	out.WriteString("inside them address that export's own layout, not this site. Each document below is\n")
	fmt.Fprintf(&out, "preceded by its canonical URL, and %s maps every document to one.\n", base.Absolute(routeLLMs))

	for _, e := range b.Documents() {
		fmt.Fprintf(&out, "\n---\nsource: %s\n---\n\n", base.Absolute(docRoute(e.Slug())))
		out.Write(b.Body(e))
		if out.Len() > maxLLMsFullBytes {
			return nil, fmt.Errorf("the concatenated corpus passed the %d-byte limit at document %q; raise the limit deliberately rather than publishing a truncated corpus",
				maxLLMsFullBytes, e.Slug())
		}
	}
	return out.Bytes(), nil
}
