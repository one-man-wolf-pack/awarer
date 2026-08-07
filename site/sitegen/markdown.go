package main

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"path"
	"regexp"
	"strings"

	"awarer/internal/domain/docbundle"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Markdown is parsed by goldmark, configured as narrowly as the corpus allows:
// CommonMark plus automatic heading identifiers, and nothing else. GFM, linkify,
// and typographer are deliberately absent — the exported corpus uses none of
// them, and each extension would add syntax the exporter never validates against.
// Raw HTML rendering stays off (goldmark's default), so even a node this file
// failed to reject could not reach the page as markup.
//
// # Why links are an allowlist
//
// Every link destination must match one shape: a relative path ending in .md,
// optionally with a fragment, that resolves to a document the manifest declares.
// A rejection list would be wrong here. goldmark keeps the destination as written
// and the HTML renderer passes it through util.URLEscape(dest, true), which
// deliberately leaves ':', '/', '?' and '#' intact — so "javascript:..." would
// render verbatim and live. Anything this file does not recognise fails the
// build instead of being published.
//
// Autolinks are checked separately because <https://x> and <a@b.c> are core
// CommonMark, not an extension, and produce ast.AutoLink rather than ast.Link.

// linkTargetRe is the accepted spelling of a document link: slash-separated
// components of ordinary path characters, ending in .md. Components are not
// restricted to non-".." here because the corpus legitimately links across
// directories ("../reference/configuration.md"); escaping the bundle is decided
// after resolution, by whether the result is a path the manifest declares.
var linkTargetRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*\.md$`)

// fragmentRe is the accepted spelling of an anchor: goldmark's generated heading
// identifiers, and nothing that would need escaping in a URL.
var fragmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// heading is one section of a rendered document, used for the on-page outline.
type heading struct {
	ID   string
	Text string
}

// docLink is one anchored link this document publishes, recorded so it can be
// resolved once every document's headings are known.
//
// Only anchored links are recorded. A link without a fragment is already fully
// decided while the document is walked: Lookup succeeding proves the manifest
// declares the target, and a declared document always gets a page. The heading
// is the one fact a single document's walk cannot see.
type docLink struct {
	// dest is the destination as the author wrote it, so a diagnostic names what
	// is in the file rather than the route it was rewritten to.
	dest string
	// target is the destination document's slug, which is how the rendered set is
	// keyed; route is the same document's address, which is what a reader chasing
	// the diagnostic would open.
	target   string
	route    string
	fragment string
}

// renderedDoc is one document turned into page content.
type renderedDoc struct {
	html     template.HTML
	sections []heading
	anchors  map[string]bool
	links    []docLink
}

// renderDocument converts one exported document to HTML, rewriting its links to
// site routes and refusing anything the site does not publish.
func renderDocument(b *Bundle, e docbundle.ManifestEntry, body []byte) (renderedDoc, error) {
	t := &docTransformer{doc: e, bundle: b, out: &renderedDoc{anchors: map[string]bool{}}}

	md := goldmark.New(
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(t, 100)),
		),
	)

	var buf bytes.Buffer
	if err := md.Convert(body, &buf); err != nil {
		return renderedDoc{}, fmt.Errorf("document %q: %w", e.Slug(), err)
	}
	if err := errors.Join(t.errs...); err != nil {
		return renderedDoc{}, err
	}

	// The bytes are trusted as markup only because they came out of a renderer
	// with raw HTML disabled and an AST that was proven to hold no HTML node.
	t.out.html = template.HTML(buf.String()) //nolint:gosec // see above
	return *t.out, nil
}

// docTransformer walks one document's AST: it rewrites link destinations,
// collects the heading outline, and records every reason the document cannot be
// published. Errors are accumulated rather than returned because goldmark's
// ASTTransformer interface has no error path; the caller checks them before it
// uses the rendered bytes.
type docTransformer struct {
	doc    docbundle.ManifestEntry
	bundle *Bundle
	out    *renderedDoc
	errs   []error
}

// Transform implements parser.ASTTransformer.
func (t *docTransformer) Transform(node *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()

	// Before the walk, so the H1's generated identifier never reaches the anchor
	// set. Collected first and removed afterwards, the set would name a heading
	// the page does not publish, and a link to that fragment would pass every
	// check and resolve to nothing in a browser.
	dropLeadingTitle(node)

	err := ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.RawHTML:
			t.reject(source, rawHTMLStart(n), "inline HTML is not published; wrap the text in backticks if it is a placeholder such as `<id>`")
		case *ast.HTMLBlock:
			t.reject(source, blockStart(n), "an HTML block is not published; exported documentation is Markdown only")
		case *ast.Image:
			t.errs = append(t.errs, t.errf("image %q is not published; the site carries no document images", string(n.Destination)))
		case *ast.AutoLink:
			t.errs = append(t.errs, t.errf("autolink %q is not published; link to an exported document instead", string(n.URL(source))))
		case *ast.Link:
			t.rewriteLink(n)
		case *ast.Heading:
			t.collectHeading(n, source)
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		t.errs = append(t.errs, t.errf("walking the document: %v", err))
	}
}

// dropLeadingTitle removes a document's own H1 when it has one. The page renders
// the title from the manifest, above the metadata and the outline, so a reader
// meets the title before the apparatus rather than after it, and leaving the
// body's H1 in place would print it twice.
//
// A body that does not open with an H1 is left alone rather than refused. The
// published title comes from the manifest under every input, so the body's first
// block was never a correctness dependency — only a presentational one, and it
// is discharged by making the removal conditional. Whether the exported bodies
// are canonical Markdown whose H1 is the manifest title is the producer's fact,
// proven where it is owned; a shadow rule here would fail a release build over
// something this package does not own. llms-full.txt republishes the body bytes
// untouched either way.
func dropLeadingTitle(node *ast.Document) {
	first := node.FirstChild()
	if h, ok := first.(*ast.Heading); ok && h.Level == 1 {
		node.RemoveChild(node, first)
	}
}

// rewriteLink replaces a document-relative Markdown link with the site route of
// the document it names.
func (t *docTransformer) rewriteLink(n *ast.Link) {
	dest := string(n.Destination)
	target, fragment, hasFragment := strings.Cut(dest, "#")

	if !linkTargetRe.MatchString(target) {
		t.errs = append(t.errs, t.errf("link %q is not a relative link to an exported document", dest))
		return
	}
	if hasFragment && !fragmentRe.MatchString(fragment) {
		t.errs = append(t.errs, t.errf("link %q has an anchor that is not a heading identifier", dest))
		return
	}

	resolved := path.Join(t.doc.Path().Dir(), target)
	entry, ok := t.bundle.Lookup(resolved)
	if !ok {
		t.errs = append(t.errs, t.errf("link %q resolves to %q, which the export does not publish", dest, resolved))
		return
	}

	route := docRoute(entry.Slug())
	if hasFragment {
		t.out.links = append(t.out.links, docLink{
			dest: dest, target: entry.Slug().String(), route: route, fragment: fragment,
		})
		route += "#" + fragment
	}
	n.Destination = []byte(route)
}

// resolveAnchors proves every anchored link lands on a heading the target
// document publishes.
//
// It runs over the rendered model, once, after every document has been walked —
// the earliest point at which the answer exists. The alternative, re-reading the
// emitted HTML and re-parsing its href attributes, would check the generator's
// construction against a second reading of its own output; the fact actually in
// question is whether one document's fragment names another's heading, and that
// is what this looks at.
//
// The documents are walked in manifest order rather than by ranging the map, so
// a corpus with more than one dangling anchor names the same one every run.
func resolveAnchors(docs []docbundle.ManifestEntry, rendered map[string]renderedDoc) error {
	for _, e := range docs {
		slug := e.Slug().String()
		for _, l := range rendered[slug].links {
			// A target the manifest declares is a document this loop rendered, so a
			// miss here can only be a missing heading. No separate "was it rendered"
			// branch: the zero renderedDoc has no anchors and reaches the same
			// refusal, with the diagnostic a reader can act on.
			if !rendered[l.target].anchors[l.fragment] {
				return fmt.Errorf("document %q links to %q, but %s publishes no heading %q",
					slug, l.dest, l.route, l.fragment)
			}
		}
	}
	return nil
}

// collectHeading records a heading's generated identifier so anchors can be
// verified, and keeps the second-level ones for the on-page outline.
func (t *docTransformer) collectHeading(n *ast.Heading, source []byte) {
	raw, ok := n.AttributeString("id")
	if !ok {
		return
	}
	id, ok := raw.([]byte)
	if !ok {
		return
	}
	t.out.anchors[string(id)] = true
	if n.Level == 2 {
		t.out.sections = append(t.out.sections, heading{ID: string(id), Text: nodeText(n, source)})
	}
}

// reject records a violation at a source offset, translated into a line and
// column. The position is what makes the diagnostic actionable: the corpus is
// full of placeholders like <checkpoint-id>, and the first one an author writes
// outside backticks becomes a parsed HTML node somewhere in a long document.
func (t *docTransformer) reject(source []byte, offset int, msg string) {
	line, col := position(source, offset)
	t.errs = append(t.errs, fmt.Errorf("document %q (%s:%d:%d): %s", t.doc.Slug(), t.doc.Path(), line, col, msg))
}

// errf builds a violation that has no useful source position.
func (t *docTransformer) errf(format string, args ...any) error {
	return fmt.Errorf("document %q (%s): %s", t.doc.Slug(), t.doc.Path(), fmt.Sprintf(format, args...))
}

// rawHTMLStart returns the source offset of an inline HTML node, or -1.
func rawHTMLStart(n *ast.RawHTML) int {
	if n.Segments == nil || n.Segments.Len() == 0 {
		return -1
	}
	return n.Segments.At(0).Start
}

// blockStart returns the source offset of a block node's first line, or -1.
func blockStart(n ast.Node) int {
	lines := n.Lines()
	if lines == nil || lines.Len() == 0 {
		return -1
	}
	return lines.At(0).Start
}

// position translates a byte offset into a 1-based line and column.
func position(source []byte, offset int) (line, col int) {
	if offset < 0 || offset > len(source) {
		return 0, 0
	}
	line = 1 + bytes.Count(source[:offset], []byte("\n"))
	lineStart := bytes.LastIndexByte(source[:offset], '\n') + 1
	return line, offset - lineStart + 1
}

// nodeText flattens a node's literal text, dropping inline markup. It is used for
// the on-page outline, where "`awa run` options" should read as plain words
// rather than as the backticks an author typed.
func nodeText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch c := c.(type) {
		case *ast.Text:
			b.Write(c.Segment.Value(source))
		case *ast.String:
			b.Write(c.Value)
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// renderProse converts one hand-authored sentence or paragraph of landing copy.
// The landing is the only place the site carries words that did not come out of
// the binary, so it goes through the same parser rather than a second one — but
// with every link form refused. Landing links are structural: they come from
// typed fields the template renders, never from prose an author edits.
func renderProse(where, src string) (template.HTML, error) {
	t := &proseTransformer{where: where}
	md := goldmark.New(goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(t, 100))))

	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	if err := errors.Join(t.errs...); err != nil {
		return "", err
	}

	// A single paragraph is unwrapped so the caller can place the words inside its
	// own element. Anything else means the copy grew into a block structure the
	// landing templates do not lay out, which is a content bug, not a rendering one.
	html := strings.TrimSuffix(buf.String(), "\n")
	if !strings.HasPrefix(html, "<p>") || !strings.HasSuffix(html, "</p>") ||
		strings.Count(html, "<p>") != 1 {
		return "", fmt.Errorf("%s: landing copy must be exactly one paragraph, got %q", where, html)
	}
	return template.HTML(strings.TrimSuffix(strings.TrimPrefix(html, "<p>"), "</p>")), nil //nolint:gosec // rendered with raw HTML disabled and every link form refused
}

// proseTransformer refuses every construct the landing may not carry.
type proseTransformer struct {
	where string
	errs  []error
}

// Transform implements parser.ASTTransformer.
func (t *proseTransformer) Transform(node *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindRawHTML, ast.KindHTMLBlock:
			t.errs = append(t.errs, fmt.Errorf("%s: landing copy must not contain HTML", t.where))
		case ast.KindLink, ast.KindAutoLink, ast.KindImage:
			t.errs = append(t.errs, fmt.Errorf("%s: landing copy must not contain links; use a typed link field", t.where))
		}
		return ast.WalkContinue, nil
	})
}
