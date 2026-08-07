package main

import (
	"fmt"

	"awarer/internal/domain/docbundle"
)

// Navigation groups the corpus by document kind, in the order a reader meets it:
// the authored operational topics first (they are the golden path), then the
// generated per-command syntax pages, then the generated reference tables.
//
// The kind comes from the manifest, never from the directory a file happens to
// live in. They do not agree: the configuration reference is a reference-kind
// document published at reference/configuration.md under the slug "config", and
// grouping by path prefix would file it wrong. Within a group, order is the
// manifest's order, which is the order the exporter published.

// navGroup is one section of the corpus.
type navGroup struct {
	Name  string
	Note  string
	Links []navLink
}

// navLink is one document in the navigation.
type navLink struct {
	Slug    string
	Title   string
	Summary string
	Route   string
}

// kindSections is the closed presentation order of the closed kind set. A kind
// with no section here is a build error rather than a silently unlisted group:
// every exported document must appear in navigation exactly once, so a new kind
// must be given a home deliberately.
type kindSection struct {
	kind docbundle.Kind
	name string
	note string
}

var kindSections = []kindSection{
	{docbundle.KindTopic, "Operational topics", "How to work with awa, written for direct retrieval."},
	{docbundle.KindCommand, "Command reference", "Exhaustive syntax for every command, generated from the binary's own registry."},
	{docbundle.KindReference, "Reference", "Configuration keys, global options, and the process-exit contract."},
}

// buildNav groups the manifest's documents for presentation.
func buildNav(docs []docbundle.ManifestEntry) ([]navGroup, error) {
	byKind := make(map[docbundle.Kind][]navLink, len(kindSections))
	for _, e := range docs {
		byKind[e.Kind()] = append(byKind[e.Kind()], navLink{
			Slug:    e.Slug().String(),
			Title:   e.Title(),
			Summary: e.Summary(),
			Route:   docRoute(e.Slug()),
		})
	}

	groups := make([]navGroup, 0, len(kindSections))
	placed := 0
	for _, s := range kindSections {
		links := byKind[s.kind]
		if len(links) == 0 {
			continue
		}
		placed += len(links)
		groups = append(groups, navGroup{Name: s.name, Note: s.note, Links: links})
	}
	if placed != len(docs) {
		return nil, fmt.Errorf("navigation covers %d of %d exported documents; a document kind has no section", placed, len(docs))
	}
	return groups, nil
}

// kindName returns the display name of a kind, for the label one document page
// shows.
func kindName(k docbundle.Kind) string {
	for _, s := range kindSections {
		if s.kind == k {
			return s.name
		}
	}
	return string(k)
}

// flatten returns every navigation link in presentation order, which is the
// order previous/next links follow.
func flatten(groups []navGroup) []navLink {
	var out []navLink
	for _, g := range groups {
		out = append(out, g.Links...)
	}
	return out
}
