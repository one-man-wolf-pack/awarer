package main

import (
	"context"
	"fmt"
)

// This file assembles the site in memory: pages from the export, the landing
// from its typed content, the discovery artifacts from the same manifest. It
// writes nothing, and it is one pass — every route below is produced once, from
// the manifest, by the construction that publishes it.
//
// Everything here answers a URL, and the collected set is the whole deployable
// tree: there is no file the hosting platform reads instead of serving, so
// nothing is composed outside the route rules.

// outputFile is one published file: the URL it answers and the bytes it holds.
// The file that serves it is derived when the tree is written, from the route.
type outputFile struct {
	route string
	data  []byte
}

// buildOptions are the caller's decisions.
type buildOptions struct {
	baseURL BaseURL
	// release clears the development-preview banner. It defaults to false and is
	// set only by an explicit flag: keying it off the exported version string
	// would make the banner depend on how the *exporting* binary was built, so a
	// development generator fed a released export would publish an unmarked,
	// production-looking site.
	release bool
}

// buildSite assembles the whole site.
func buildSite(ctx context.Context, b *Bundle, opts buildOptions) ([]outputFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tmpl, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	as, err := buildAssets()
	if err != nil {
		return nil, err
	}
	m := b.Manifest()
	nav, err := buildNav(b.Documents())
	if err != nil {
		return nil, err
	}

	site := &siteView{
		Version:     m.Provenance().Version,
		Commit:      m.Provenance().Commit,
		Preview:     !opts.release,
		BaseURL:     opts.baseURL,
		StyleURL:    as.style.route,
		MarkURL:     as.mark.route,
		MarkAbsURL:  opts.baseURL.Absolute(as.mark.route),
		SourceURL:   sourceURL,
		ReleasesURL: releasesURL,
		HomeURL:     routeHome,
		DocsURL:     routeDocs,
		LLMsURL:     routeLLMs,
		LLMsFullURL: routeLLMsFull,
		MachineURL:  routeMachineRef,
		Nav:         nav,
		ThemeScript: themeScript(),
	}

	rendered := make(map[string]renderedDoc, len(b.Documents()))
	for _, e := range b.Documents() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, err := renderDocument(b, e, b.Body(e))
		if err != nil {
			return nil, err
		}
		rendered[e.Slug().String()] = r
	}
	// Only now is every document's heading set known, which is what an anchored
	// link across documents needs.
	if err := resolveAnchors(b.Documents(), rendered); err != nil {
		return nil, err
	}

	var files []outputFile
	add := func(route string, data []byte) { files = append(files, outputFile{route: route, data: data}) }

	landing, err := buildLanding(b)
	if err != nil {
		return nil, err
	}
	home, err := tmpl.render(landingPage{
		pageMeta: pageMeta{
			Site:        site,
			Title:       "awarer — worktree checkpoints and reusable checks for coding agents",
			Description: "awa is a local CLI that gives coding agents explicit worktree checkpoints, reusable deterministic checks, and durable execution history.",
			Route:       routeHome,
			Canonical:   opts.baseURL.Absolute(routeHome),
		},
		Landing: landing,
	})
	if err != nil {
		return nil, err
	}
	add(routeHome, home)

	index, err := tmpl.render(indexPage{
		pageMeta: pageMeta{
			Site:        site,
			Title:       "Documentation",
			Description: fmt.Sprintf("Every document the awa %s binary carries: operational topics, the generated command reference, and the configuration, global-option, and exit-code reference.", site.Version),
			Route:       routeDocs,
			Canonical:   opts.baseURL.Absolute(routeDocs),
		},
		Index: &indexView{
			Groups:         nav,
			MachineRefURL:  routeMachineRef,
			MachineRefSize: m.MachineReference().Size(),
			DocumentCount:  m.Corpus().DocumentCount,
		},
	})
	if err != nil {
		return nil, err
	}
	add(routeDocs, index)

	ordered := flatten(nav)
	position := make(map[string]int, len(ordered))
	for i, l := range ordered {
		position[l.Slug] = i
	}
	for _, e := range b.Documents() {
		r := rendered[e.Slug().String()]
		i := position[e.Slug().String()]
		view := &documentView{
			Slug:     e.Slug().String(),
			KindName: kindName(e.Kind()),
			Content:  r.html,
			Sections: r.sections,
			Aliases:  e.Aliases(),
			SourceOf: e.Path().String(),
		}
		if i > 0 {
			prev := ordered[i-1]
			view.Prev = &prev
		}
		if i < len(ordered)-1 {
			next := ordered[i+1]
			view.Next = &next
		}
		route := docRoute(e.Slug())
		page, err := tmpl.render(documentPage{
			pageMeta: pageMeta{
				Site:        site,
				Title:       e.Title(),
				Description: e.Summary(),
				Route:       route,
				Canonical:   opts.baseURL.Absolute(route),
			},
			Document: view,
		})
		if err != nil {
			return nil, err
		}
		add(route, page)
	}

	notFound, err := tmpl.render(notFoundPage{pageMeta{
		Site:        site,
		Title:       "Not found",
		Description: "This address is not part of the current awarer documentation site.",
		Route:       routeNotFound,
		Canonical:   opts.baseURL.Absolute(routeNotFound),
	}})
	if err != nil {
		return nil, err
	}
	add(routeNotFound, notFound)

	// The machine reference is republished byte for byte from the bundle. It is a
	// copy of an exported artifact, not a re-rendering: an automated consumer that
	// hashes it gets the digest the manifest declares.
	add(routeMachineRef, b.MachineReference())

	for _, a := range as.all() {
		add(a.route, a.data)
	}

	sitemapRoutes := []string{routeHome, routeDocs}
	for _, l := range ordered {
		sitemapRoutes = append(sitemapRoutes, l.Route)
	}
	sitemap, err := renderSitemap(opts.baseURL, sitemapRoutes)
	if err != nil {
		return nil, err
	}
	llmsFull, err := renderLLMsFull(opts.baseURL, b)
	if err != nil {
		return nil, err
	}
	add(routeRobots, renderRobots(opts.baseURL))
	add(routeSitemap, sitemap)
	add(routeLLMs, renderLLMs(opts.baseURL, m, nav, site.Preview))
	add(routeLLMsFull, llmsFull)

	return files, nil
}
