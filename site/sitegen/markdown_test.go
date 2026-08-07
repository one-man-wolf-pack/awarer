package main

import (
	"strings"
	"testing"
)

// renderOne loads a bundle whose first topic carries the given body and renders
// it, so a case can state exactly the Markdown it is about.
func renderOne(t *testing.T, body string) (renderedDoc, error) {
	t.Helper()
	docs := defaultDocs()
	docs[0].body = body
	b := loadFixture(t, docs)
	e := b.Documents()[0]
	return renderDocument(b, e, b.Body(e))
}

func TestRenderDocumentRewritesLinksToRoutes(t *testing.T) {
	r, err := renderOne(t, "# Alpha topic\n\nSee [the reference](../reference/beta.md) and "+
		"[a section](../reference/beta.md#options).\n\n## Section one\n\nWords.\n")
	if err != nil {
		t.Fatalf("renderDocument: %v", err)
	}

	html := string(r.html)
	for _, want := range []string{`href="/docs/beta/"`, `href="/docs/beta/#options"`} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML is missing %s:\n%s", want, html)
		}
	}
	if strings.Contains(html, ".md") {
		t.Errorf("a Markdown path survived into the published HTML:\n%s", html)
	}
	if len(r.sections) != 1 || r.sections[0].Text != "Section one" {
		t.Errorf("sections = %+v, want one entry titled %q", r.sections, "Section one")
	}
	if !r.anchors["section-one"] {
		t.Errorf("anchors = %v, want it to carry section-one", r.anchors)
	}
}

func TestRenderDocumentKeepsPlaceholdersInCode(t *testing.T) {
	r, err := renderOne(t, "# Alpha topic\n\nRun `awa run explain -- <command>` first.\n\n"+
		"```text\nawa changes <id>..now\n```\n")
	if err != nil {
		t.Fatalf("renderDocument: %v", err)
	}
	html := string(r.html)
	for _, want := range []string{"&lt;command&gt;", "&lt;id&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("placeholder %s was not published as text:\n%s", want, html)
		}
	}
}

func TestRenderDocumentRefusesUnpublishableMarkup(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "inline HTML",
			body: "# Alpha topic\n\nPass <count> entries.\n",
			want: "inline HTML is not published",
		},
		{
			name: "an HTML block",
			body: "# Alpha topic\n\n<div class=\"x\">\ntext\n</div>\n",
			want: "an HTML block is not published",
		},
		{
			name: "an image",
			body: "# Alpha topic\n\n![diagram](../reference/beta.md)\n",
			want: "image",
		},
		{
			name: "an autolink",
			body: "# Alpha topic\n\nSee <https://example.com/> for more.\n",
			want: "autolink",
		},
		{
			name: "a javascript link",
			body: "# Alpha topic\n\n[click](javascript:alert(1))\n",
			want: "not a relative link to an exported document",
		},
		{
			name: "a protocol-relative link",
			body: "# Alpha topic\n\n[click](//evil.example/x.md)\n",
			want: "not a relative link to an exported document",
		},
		{
			name: "an absolute link",
			body: "# Alpha topic\n\n[click](/etc/passwd.md)\n",
			want: "not a relative link to an exported document",
		},
		{
			name: "an external https link",
			body: "# Alpha topic\n\n[home](https://example.com/page.md)\n",
			want: "not a relative link to an exported document",
		},
		{
			name: "a percent-encoded traversal",
			body: "# Alpha topic\n\n[up](%2e%2e/%2e%2e/secret.md)\n",
			want: "not a relative link to an exported document",
		},
		{
			name: "a link to a file the export does not publish",
			body: "# Alpha topic\n\n[missing](../reference/nothing.md)\n",
			want: "which the export does not publish",
		},
		{
			name: "a link that escapes the bundle",
			body: "# Alpha topic\n\n[out](../../outside.md)\n",
			want: "which the export does not publish",
		},
		{
			name: "a link to a non-Markdown file",
			body: "# Alpha topic\n\n[machine](../reference/cli.json)\n",
			want: "not a relative link to an exported document",
		},
		{
			name: "an anchor that is not an identifier",
			body: "# Alpha topic\n\n[bad](../reference/beta.md#a/b)\n",
			want: "anchor that is not a heading identifier",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderOne(t, tc.body)
			if err == nil {
				t.Fatalf("renderDocument accepted Markdown it must refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestRawHTMLDiagnosticNamesTheLine is what makes the raw-HTML guard usable: the
// corpus is full of placeholders, and an author who writes one outside backticks
// needs to be told where, not just that.
func TestRawHTMLDiagnosticNamesTheLine(t *testing.T) {
	_, err := renderOne(t, "# Alpha topic\n\nfirst line\n\nsecond line with <count> here\n")
	if err == nil {
		t.Fatalf("renderDocument accepted inline HTML")
	}
	if !strings.Contains(err.Error(), "topics/alpha.md:5:18") {
		t.Fatalf("error = %v, want it to name line 5, column 18", err)
	}
	if !strings.Contains(err.Error(), "backticks") {
		t.Fatalf("error = %v, want it to say how to fix the document", err)
	}
}

// TestRenderDocumentDropsOnlyALeadingTitleHeading states what the renderer does
// with a body's own H1 and what it does not do about the producer's contract.
//
// The published title comes from the manifest under every input, so the body's
// first block was never a correctness dependency — only a presentational one:
// left in place, the H1 would print twice. That a body opens with an H1 equal to
// the manifest title is the exporter's fact, proven where it is owned; a body
// shaped otherwise renders here rather than failing a release build over
// something this package does not own.
func TestRenderDocumentDropsOnlyALeadingTitleHeading(t *testing.T) {
	with, err := renderOne(t, "# Alpha topic\n\nWords.\n\n## Section one\n\nMore.\n")
	if err != nil {
		t.Fatalf("renderDocument: %v", err)
	}
	if strings.Contains(string(with.html), "<h1") {
		t.Errorf("the document's own H1 survived into the content:\n%s", with.html)
	}
	// Removed before the walk, so the identifier of a heading the page does not
	// publish never reaches the anchor set — a link to it would otherwise resolve
	// here and nowhere in a browser.
	if with.anchors["alpha-topic"] {
		t.Errorf("anchors = %v, want the dropped title to carry no anchor", with.anchors)
	}

	without, err := renderOne(t, "Words before any heading.\n\n## Section one\n\nMore.\n")
	if err != nil {
		t.Fatalf("renderDocument refused a body that does not open with its title: %v", err)
	}
	if !strings.Contains(string(without.html), "Words before any heading.") {
		t.Errorf("the opening paragraph was dropped:\n%s", without.html)
	}
	if len(without.sections) != 1 || without.sections[0].Text != "Section one" {
		t.Errorf("sections = %+v, want the document's own outline", without.sections)
	}
}

func TestRenderProseRefusesLinksAndMarkup(t *testing.T) {
	if _, err := renderProse("copy", "Plain words with `code`."); err != nil {
		t.Fatalf("renderProse rejected valid copy: %v", err)
	}

	tests := []struct {
		name string
		src  string
		want string
	}{
		{"a link", "See [the docs](https://example.com).", "must not contain links"},
		{"an autolink", "See <https://example.com/>.", "must not contain links"},
		{"raw HTML", `Words <b>bold</b>.`, "must not contain HTML"},
		{"two paragraphs", "One.\n\nTwo.", "exactly one paragraph"},
		{"a list", "- one\n- two", "exactly one paragraph"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renderProse("copy", tc.src)
			if err == nil {
				t.Fatalf("renderProse accepted copy it must refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestRenderProseEscapesItsInput(t *testing.T) {
	got, err := renderProse("copy", "A & B are `<placeholders>`.")
	if err != nil {
		t.Fatalf("renderProse: %v", err)
	}
	if !strings.Contains(string(got), "&amp;") || !strings.Contains(string(got), "&lt;placeholders&gt;") {
		t.Fatalf("prose was not escaped: %q", got)
	}
}
