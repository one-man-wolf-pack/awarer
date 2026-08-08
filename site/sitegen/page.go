package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"

	"awarer/internal/domain/docbundle"
)

// Routes are the site's public shape and are current-only: one page per exported
// document, no versioned tree, no historical aliases. They are built with plain
// string composition over slugs the manifest already constrained to URL-safe
// spelling, so nothing here has to escape or normalise a path.
const (
	routeHome       = "/"
	routeDocs       = "/docs/"
	routeNotFound   = "/404.html"
	routeMachineRef = "/docs/cli.json"
	routeRobots     = "/robots.txt"
	routeSitemap    = "/sitemap.xml"
	routeLLMs       = "/llms.txt"
	routeLLMsFull   = "/llms-full.txt"
)

// docRoute is the canonical route of one exported document.
func docRoute(slug docbundle.Slug) string { return routeDocs + slug.String() + "/" }

// assetRoute is the route of a content-addressed static asset. The digest is in
// the filename so a deployed page can never be served with a stale stylesheet,
// and it comes from the bytes, so two builds of the same input name it the same.
func assetRoute(name, ext, digest string) string {
	return "/assets/" + name + "." + digest + "." + ext
}

//go:embed templates/*.gohtml
var templateFS embed.FS

// pageKinds is the closed set of pages the site has, and the only list of them:
// each template to parse is read off the page type that renders through it, so a
// page cannot exist without its layout being loaded.
var pageKinds = []page{landingPage{}, indexPage{}, documentPage{}, notFoundPage{}}

// templateSet holds one parsed layout per page kind. They are parsed separately
// rather than into one set because each page template defines "content", and a
// combined set would silently keep only the last definition.
type templateSet map[string]*template.Template

// loadTemplates parses the shared layout together with every page template.
func loadTemplates() (templateSet, error) {
	out := make(templateSet, len(pageKinds))
	for _, k := range pageKinds {
		name := k.template()
		t, err := template.New("base.gohtml").ParseFS(templateFS, "templates/base.gohtml", "templates/"+name+".gohtml")
		if err != nil {
			return nil, fmt.Errorf("parsing the %s template: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// siteView is the part of the model every page shares.
type siteView struct {
	// Version is the awa release this site documents. Every page states it: a
	// current-only site with no version in view would be unattributable.
	Version string
	Commit  string
	// Preview marks a site built from a development binary. It defaults to true
	// and is cleared only by an explicit flag, so an unmarked production-looking
	// page is never the accidental outcome.
	Preview     bool
	BaseURL     BaseURL
	StyleURL    string
	MarkURL     string
	MarkAbsURL  string
	SourceURL   string
	ReleasesURL string
	// The routes the shared layout and the index link to. They are here rather
	// than written into the templates so a route is spelled once, in the constant
	// block, and renaming one cannot leave a template pointing at an address the
	// site no longer publishes.
	HomeURL     string
	DocsURL     string
	LLMsURL     string
	LLMsFullURL string
	MachineURL  string
	Nav         []navGroup
	ThemeScript template.JS
}

// pageMeta is what every page carries: the site-wide model and the page's own
// identity. Each page kind embeds it and adds exactly the payload its template
// renders — one struct with a nullable field per kind would let a page be handed
// the wrong payload, or none, and the shared layout would still produce a
// perfectly valid document with an empty body.
type pageMeta struct {
	Site *siteView
	// Title is the document title; the layout composes the browser title from it.
	Title string
	// Description becomes the meta description and, for documents, the summary
	// the index and llms.txt publish, so all three say the same thing.
	Description string
	Route       string
	Canonical   string
}

// page is anything renderable. A page names its own template and carries its own
// address, so the pairing of payload and layout is a property of the type rather
// than two arguments a caller has to get right — a page handed the wrong layout
// is not a thing that can be written.
//
// Each kind adds exactly the payload its template renders. One struct with a
// nullable field per kind would let a page be handed the wrong payload, or none,
// and the shared layout would still produce a perfectly valid document with an
// empty body.
type page interface {
	meta() pageMeta
	template() string
}

func (m pageMeta) meta() pageMeta { return m }

// landingPage is the hand-authored front page.
type landingPage struct {
	pageMeta
	Landing *landingView
}

func (landingPage) template() string { return "landing" }

// indexPage is the document index at /docs/.
type indexPage struct {
	pageMeta
	Index *indexView
}

func (indexPage) template() string { return "docindex" }

// documentPage is one exported document.
type documentPage struct {
	pageMeta
	Document *documentView
}

func (documentPage) template() string { return "doc" }

// notFoundPage is the static page an unknown address lands on.
type notFoundPage struct {
	pageMeta
}

func (notFoundPage) template() string { return "notfound" }

// landingView is the hand-authored front page.
type landingView struct {
	Headline       template.HTML
	Subhead        template.HTML
	HeroActions    []actionView
	Workflow       workflowView
	Cards          []cardView
	Terminal       terminalView
	InstallNote    template.HTML
	InstallActions []actionView
}

// actionView is one button, with the route already resolved against the export.
type actionView struct {
	Label   string
	Route   string
	Primary bool
}

// workflowView is the single real command sequence the landing shows.
type workflowView struct {
	Caption template.HTML
	Steps   []workflowStep
}

// workflowStep is one command with the reason to run it.
type workflowStep struct {
	Command string
	Comment string
}

// cardView links a question a reader arrives with to the document that answers it.
type cardView struct {
	Question string
	Answer   template.HTML
	Route    string
	DocTitle string
}

// terminalView is the recorded terminal output shown as evidence. Caption states
// that it is a recording: a screenshot-like block that reads as live state would
// be a claim the site cannot make.
type terminalView struct {
	Caption string
	Lines   []string
}

// indexView is the document index at /docs/.
type indexView struct {
	Groups         []navGroup
	MachineRefURL  string
	MachineRefSize int
	DocumentCount  int
}

// documentView is one exported document.
type documentView struct {
	Slug     string
	KindName string
	Content  template.HTML
	Sections []heading
	Aliases  []string
	Prev     *navLink
	Next     *navLink
}

// render executes a page through the layout it names.
func (ts templateSet) render(view page) ([]byte, error) {
	t, ok := ts[view.template()]
	if !ok {
		return nil, fmt.Errorf("no %s template was loaded", view.template())
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, view); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", view.meta().Route, err)
	}
	return buf.Bytes(), nil
}
