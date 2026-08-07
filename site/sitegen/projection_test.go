package main

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// One representative projection, asserted over the bytes the site would be
// written from. What is checked is what a visitor or an automated reader
// receives: the published addresses, the content at them, the navigation, the
// discovery artifacts, and which release the site claims to document.
//
// It is deliberately not a second model of the construction. The generator
// builds every route from one manifest traversal, so a test that rebuilt the
// expected route set from the same manifest would compare the design with a copy
// of itself; these assertions name the public shape instead.

// buildFrom loads a synthetic export and builds the whole site from it.
func buildFrom(t *testing.T, docs []fakeDoc, opts buildOptions) ([]outputFile, error) {
	t.Helper()
	return buildSite(context.Background(), loadFixture(t, docs), opts)
}

// mustBuild builds a site the test expects to succeed.
func mustBuild(t *testing.T, docs []fakeDoc, opts buildOptions) map[string]string {
	t.Helper()
	files, err := buildFrom(t, docs, opts)
	if err != nil {
		t.Fatalf("buildSite: %v", err)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[f.route] = string(f.data)
	}
	return out
}

// currentPageMarker is the attribute a page uses to say which navigation link is
// itself. It is written out here rather than derived from the template, so the
// assertion fails if the template stops emitting it.
const currentPageMarker = `aria-current="page"`

// sidebarOf returns the corpus-navigation element of a rendered document page.
// The scope matters: the masthead owns a separate exact-page marker for /docs/,
// and this assertion is about the sidebar contract alone.
func sidebarOf(page string) (string, bool) {
	start := strings.Index(page, `<nav class="doc-nav"`)
	if start < 0 {
		return "", false
	}
	rest := page[start:]
	end := strings.Index(rest, "</nav>")
	if end < 0 {
		return "", false
	}
	return rest[:end+len("</nav>")], true
}

// markedLinks returns the opening tags of the navigation links claiming to be the
// current page. A failure reports these rather than the navigation itself: the
// corpus runs to dozens of links, and the defect is always among the few marked.
func markedLinks(nav string) []string {
	var out []string
	for _, after := range strings.Split(nav, "<a ")[1:] {
		end := strings.Index(after, ">")
		if end < 0 {
			continue
		}
		if strings.Contains(after[:end], currentPageMarker) {
			out = append(out, "<a "+after[:end+1])
		}
	}
	return out
}

func TestSiteProjectsTheExportIntoItsPublicRoutes(t *testing.T) {
	docs := landingDocs(t)
	got := mustBuild(t, docs, buildOptions{baseURL: testBaseURL(t)})

	for _, route := range []string{
		routeHome, routeDocs, routeNotFound, routeMachineRef,
		routeRobots, routeSitemap, routeLLMs, routeLLMsFull,
		"/docs/alpha/", "/docs/beta/", "/docs/command-gamma/",
	} {
		if _, ok := got[route]; !ok {
			t.Errorf("route %q was not published", route)
		}
	}

	// The machine reference is republished verbatim: a consumer that hashes it
	// must get the digest the export manifest declares.
	if got[routeMachineRef] != machineRefBody {
		t.Errorf("machine reference = %q, want the exported bytes %q", got[routeMachineRef], machineRefBody)
	}
	if !strings.Contains(got[routeLLMsFull], "# Alpha topic") {
		t.Errorf("llms-full.txt does not carry the exported bodies")
	}

	// Every exported document is reachable from each discovery artifact exactly
	// once. Exactly once is the property: a document listed twice is as wrong as
	// one missing, because both mean the projection stopped being a faithful map
	// of the export.
	for _, d := range docs {
		abs := "https://example.test/docs/" + d.slug + "/"
		if _, ok := got[docRouteOf(d.slug)]; !ok {
			t.Errorf("document %q has no page", d.slug)
		}
		if n := strings.Count(got[routeSitemap], "<loc>"+abs+"</loc>"); n != 1 {
			t.Errorf("document %q appears %d times in the sitemap, want exactly once", d.slug, n)
		}
		if n := strings.Count(got[routeLLMs], "("+abs+")"); n != 1 {
			t.Errorf("document %q appears %d times in llms.txt, want exactly once", d.slug, n)
		}
	}

	// The crawl policy leaves the published site retrievable and announces where
	// the sitemap is, which is the one indexing-adjacent job robots.txt is the
	// right place for. The sitemap URL is spelled out rather than composed from
	// the route constants, so moving it has to be decided in two places.
	robots := got[routeRobots]
	if strings.Contains(robots, "Disallow: /") {
		t.Errorf("robots.txt blocks crawling; it is a crawl policy, not the indexing control:\n%s", robots)
	}
	if !strings.Contains(robots, "Allow: /") {
		t.Errorf("robots.txt does not allow retrieval:\n%s", robots)
	}
	if want := "Sitemap: https://example.test/sitemap.xml"; !strings.Contains(robots, want) {
		t.Errorf("robots.txt is missing %q:\n%s", want, robots)
	}

	// The two destinations that are not this site: a reader who wants the code or
	// an archive has nowhere else to go, and the landing is where they look. Both
	// are asserted as the exact hrefs the page must carry, not searched for among
	// whatever else it publishes — the site owns which links it writes, and a
	// second reading of its own output would be a rule about that, maintained here.
	for _, want := range []string{`href="` + sourceURL + `"`, `href="` + releasesURL + `"`} {
		if !strings.Contains(got[routeHome], want) {
			t.Errorf("the landing page does not carry %s", want)
		}
	}
}

func TestDocumentPageCarriesItsNavigationAndRewrittenLinks(t *testing.T) {
	docs := landingDocs(t)
	got := mustBuild(t, docs, buildOptions{baseURL: testBaseURL(t)})

	// Exactly one, and on the page's own link: a missing marker leaves a reader
	// with no announced position in the corpus, and a second one makes the
	// announcement a lie.
	for _, d := range docs {
		route := docRouteOf(d.slug)
		page, ok := got[route]
		if !ok {
			t.Fatalf("route %q was not published", route)
		}
		nav, ok := sidebarOf(page)
		if !ok {
			t.Fatalf("page %s has no corpus navigation", route)
		}
		// The count is taken over the whole navigation, not only over the links, so
		// a marker that lands on a list item or a heading is caught too.
		if n := strings.Count(nav, currentPageMarker); n != 1 {
			t.Errorf("page %s marks %d navigation elements with %s, want exactly 1; marked links: %v",
				route, n, currentPageMarker, markedLinks(nav))
		}
		self := `<a href="` + route + `" ` + currentPageMarker + `>`
		if !strings.Contains(nav, self) {
			t.Errorf("page %s does not mark its own navigation link; want %s, marked links: %v",
				route, self, markedLinks(nav))
		}
	}

	// A document link becomes a site route, an anchored one keeps its fragment,
	// and no export-relative spelling survives into the page.
	alpha := got["/docs/alpha/"]
	if !strings.Contains(alpha, `href="/docs/beta/"`) {
		t.Errorf("the link to beta was not rewritten to its route:\n%s", alpha)
	}
	beta := got["/docs/beta/"]
	if !strings.Contains(beta, `href="/docs/alpha/#section-one"`) {
		t.Errorf("the anchored link to alpha was not rewritten:\n%s", beta)
	}
	// Both spellings, because they are what an unrewritten destination looks like
	// with and without an anchor. The quote and the hash are part of the pattern:
	// a document page prints its own export path in `.SourceOf`, so a bare ".md"
	// would report every page.
	for route, page := range got {
		if !strings.HasSuffix(route, "/") {
			continue
		}
		for _, unrewritten := range []string{`.md"`, `.md#`} {
			if strings.Contains(page, unrewritten) {
				t.Errorf("%s publishes an export-relative link (%s)", route, unrewritten)
			}
		}
	}
}

func TestEveryPageStatesTheReleaseAndItsCanonicalURL(t *testing.T) {
	files, err := buildFrom(t, landingDocs(t), buildOptions{baseURL: testBaseURL(t)})
	if err != nil {
		t.Fatalf("buildSite: %v", err)
	}

	pages := 0
	for _, f := range files {
		if !strings.HasSuffix(f.route, "/") && f.route != routeNotFound {
			continue
		}
		pages++
		body := string(f.data)
		if !strings.Contains(body, "awa 1.2.3") {
			t.Errorf("%s does not state the release it documents", f.route)
		}
		want := `<link rel="canonical" href="https://example.test` + f.route + `">`
		if !strings.Contains(body, want) {
			t.Errorf("%s is missing %s", f.route, want)
		}
		// An unmarked page is the indexable default; a noindex reintroduced in the
		// layout would contradict robots.txt without failing anything else.
		if strings.Contains(body, `name="robots"`) {
			t.Errorf("%s carries a robots directive; the published site does not deny indexing", f.route)
		}
		// html/template substitutes ZgotmplZ for a URL it judged unsafe in context
		// rather than failing. Every URL that reaches an attribute here is a route
		// constant, a slug-derived route, a content-addressed asset, or one of the
		// two product URLs — so this is unreachable, and it is asserted rather than
		// re-implemented as a production scan because an unconstrained string
		// interpolated into a future template would ship a broken link silently.
		if strings.Contains(body, "ZgotmplZ") {
			t.Errorf("%s contains ZgotmplZ: a renderer refused to emit a URL rather than failing", f.route)
		}
	}
	if pages < 4 {
		t.Fatalf("only %d HTML pages were checked", pages)
	}
}

// TestPreviewAndReleaseBuildsLabelThemselves pins the one thing that separates a
// publishable site from a development one. The banner is keyed off an explicit
// flag, not off the exported version, so a development generator handed a
// released export cannot publish an unmarked production-looking site.
func TestPreviewAndReleaseBuildsLabelThemselves(t *testing.T) {
	docs := landingDocs(t)
	preview := mustBuild(t, docs, buildOptions{baseURL: testBaseURL(t)})

	if !strings.Contains(preview[routeHome], "Development preview") {
		t.Errorf("a default build is not marked as a preview")
	}
	// llms.txt must say it too. An automated reader that starts there would
	// otherwise be told "Release: awa 1.2.3" by a site built from an unreleased
	// binary, with nothing on the page to contradict it.
	if !strings.Contains(preview[routeLLMs], "Development preview") {
		t.Errorf("llms.txt does not mark a preview build:\n%s", preview[routeLLMs])
	}

	released := mustBuild(t, docs, buildOptions{baseURL: testBaseURL(t), release: true})
	if strings.Contains(released[routeHome], "Development preview") {
		t.Errorf("--release did not clear the preview banner")
	}
	if got := released[routeLLMs]; !strings.Contains(got, "Release: awa 1.2.3") ||
		strings.Contains(got, "Development preview") {
		t.Errorf("llms.txt does not state the release for a released build:\n%s", got)
	}
}

// TestBuildSiteRefusesAnUnresolvedInternalAnchor proves the anchor rule is
// decided across documents: here the destination document exists and only the
// heading is missing, which is the case a target-exists check alone would pass.
func TestBuildSiteRefusesAnUnresolvedInternalAnchor(t *testing.T) {
	docs := landingDocs(t)
	found := false
	for i := range docs {
		if docs[i].slug != "alpha" {
			continue
		}
		found = true
		// The document keeps its own "Section one" heading: another document links to
		// that anchor, and dropping it would fail this build for the wrong reason.
		docs[i].body = "# Alpha topic\n\nSee [beta](../reference/beta.md#no-such-heading).\n\n## Section one\n\nWords.\n"
	}
	if !found {
		t.Fatalf("the fixture no longer carries the document this case mutates")
	}

	_, err := buildFrom(t, docs, buildOptions{baseURL: testBaseURL(t)})
	if err == nil {
		t.Fatalf("buildSite published a link to a heading that does not exist")
	}
	for _, want := range []string{`"alpha"`, "no-such-heading", "/docs/beta/"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s", err, want)
		}
	}
}

// TestBuildSiteRefusesALandingPointerWithNoDocument covers both kinds of landing
// pointer, because they are resolved by different code: the buttons in the hero
// and install sections, and the question cards. A dead landing link is a public
// regression on the one page a first-time reader arrives at.
func TestBuildSiteRefusesALandingPointerWithNoDocument(t *testing.T) {
	without := func(slug string) []fakeDoc {
		var out []fakeDoc
		for _, d := range landingDocs(t) {
			if d.slug != slug {
				out = append(out, d)
			}
		}
		return out
	}

	t.Run("a button", func(t *testing.T) {
		_, err := buildFrom(t, without("install"), buildOptions{baseURL: testBaseURL(t)})
		if err == nil || !strings.Contains(err.Error(), `button "Install" points at document "install"`) {
			t.Fatalf("error = %v, want it to name the missing button target", err)
		}
	})

	t.Run("a card", func(t *testing.T) {
		card := landingCopy.cards[len(landingCopy.cards)-1]
		_, err := buildFrom(t, without(card.slug), buildOptions{baseURL: testBaseURL(t)})
		if err == nil || !strings.Contains(err.Error(), "landing card "+strconv.Quote(card.question)) {
			t.Fatalf("error = %v, want it to name the missing card target", err)
		}
	})
}

// TestNavigationCoversTheCorpusInReadingOrder pins what the sidebar owes a
// reader: every exported document is reachable from it, exactly once, and the
// groups arrive in the order the corpus is meant to be met — authored topics
// first, then the generated command pages, then the reference tables.
//
// The count is what carries the guard in buildNav: a document whose kind has no
// section is a build failure rather than one that silently disappears from the
// site.
func TestNavigationCoversTheCorpusInReadingOrder(t *testing.T) {
	docs := defaultDocs()
	nav, err := buildNav(loadFixture(t, docs).Documents())
	if err != nil {
		t.Fatalf("buildNav: %v", err)
	}

	seen := map[string]int{}
	for _, l := range flatten(nav) {
		seen[l.Slug]++
	}
	for _, d := range docs {
		if seen[d.slug] != 1 {
			t.Errorf("document %q appears %d times in navigation, want exactly once", d.slug, seen[d.slug])
		}
	}
	if got, want := len(flatten(nav)), len(docs); got != want {
		t.Errorf("navigation lists %d links for %d documents", got, want)
	}

	var names []string
	for _, g := range nav {
		names = append(names, g.Name)
	}
	if want := []string{"Operational topics", "Command reference", "Reference"}; !slices.Equal(names, want) {
		t.Errorf("navigation groups = %v, want %v", names, want)
	}
}

func TestBuildSiteStopsOnCancellation(t *testing.T) {
	b := loadFixture(t, landingDocs(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := buildSite(ctx, b, buildOptions{baseURL: testBaseURL(t)}); err == nil {
		t.Fatalf("buildSite ignored a cancelled context")
	}
}

// docRouteOf is the route of a document, spelled from the slug the fixture
// declares rather than through the production helper.
func docRouteOf(slug string) string { return "/docs/" + slug + "/" }
