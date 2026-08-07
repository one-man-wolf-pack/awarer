package main

import (
	"fmt"
	"net/url"
)

// BaseURL is the canonical origin the generated site is published under. It is a
// value object rather than a string because every canonical link, Open Graph tag,
// sitemap entry, and llms.txt line is built from it: a stray trailing slash, a
// query, or an http scheme would be copied into hundreds of published URLs before
// anyone noticed.
//
// Only a root-hosted origin is accepted. Serving the site from a sub-path is not
// a shape this product has, and rejecting it here keeps route construction plain
// concatenation instead of a prefix-joining rule every caller must remember.
type BaseURL struct {
	origin string // scheme://host[:port], never with a trailing slash
}

// ParseBaseURL validates and constructs a BaseURL.
func ParseBaseURL(raw string) (BaseURL, error) {
	if raw == "" {
		return BaseURL{}, fmt.Errorf("base url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return BaseURL{}, fmt.Errorf("base url %q is not a url: %w", raw, err)
	}
	switch {
	case u.Scheme != "https":
		// The site is served over TLS. An http base url would publish canonical
		// links that downgrade every visitor who follows one.
		return BaseURL{}, fmt.Errorf("base url %q must use https", raw)
	case u.Opaque != "":
		// Checked before the host, because an opaque url ("https:host") has no host
		// by construction and "no host" would be the less useful diagnosis.
		return BaseURL{}, fmt.Errorf("base url %q must be a hierarchical url", raw)
	case u.Host == "":
		return BaseURL{}, fmt.Errorf("base url %q has no host", raw)
	case u.User != nil:
		return BaseURL{}, fmt.Errorf("base url %q must not carry credentials", raw)
	case u.RawQuery != "" || u.ForceQuery:
		return BaseURL{}, fmt.Errorf("base url %q must not carry a query", raw)
	case u.Fragment != "":
		return BaseURL{}, fmt.Errorf("base url %q must not carry a fragment", raw)
	case u.Path != "" && u.Path != "/":
		return BaseURL{}, fmt.Errorf("base url %q must address the site root, not a sub-path", raw)
	}
	return BaseURL{origin: u.Scheme + "://" + u.Host}, nil
}

// Absolute returns the canonical absolute URL of a site route. Routes are this
// generator's own rooted values, so this is deliberately concatenation and not a
// resolution step that could reinterpret one. Nothing repairs an unrooted
// argument: the result would be a visibly broken origin, which the published-link
// check refuses, rather than a plausible URL pointing at the wrong page.
func (b BaseURL) Absolute(route string) string {
	return b.origin + route
}

// String returns the origin without a trailing slash.
func (b BaseURL) String() string { return b.origin }
