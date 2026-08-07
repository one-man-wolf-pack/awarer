package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"strings"
)

// The presentation layer is three checked-in files: one stylesheet, one product
// mark, and the few lines of script that pick a theme. There is no build step,
// no bundler, and no external request — a page renders completely from bytes
// this repository carries.
//
// # Why the theme script is inline
//
// It must run before the first paint, or a reader who chose dark sees a white
// page flash first. An external script cannot do that without blocking the
// render on a second request, so the bootstrap is inlined in <head> and kept
// small enough to read. It is the only script the site has.

//go:embed static/style.css
var styleCSS []byte

//go:embed static/mark.png
var markPNG []byte

// themeBootstrap resolves the theme before the document body exists and wires the
// toggle through one delegated listener, so it does not depend on the button
// having been parsed yet. "system" is the absence of a stored choice, not a third
// stored value, so clearing the preference genuinely returns to the OS setting.
const themeBootstrap = `
(function () {
  var root = document.documentElement;
  var KEY = "awarer-theme";
  function stored() {
    try { return localStorage.getItem(KEY); } catch (e) { return null; }
  }
  function apply(v) {
    if (v === "light" || v === "dark") { root.setAttribute("data-theme", v); }
    else { root.removeAttribute("data-theme"); }
  }
  function label() {
    var b = document.querySelector("[data-theme-toggle]");
    if (!b) { return; }
    var v = stored() || "system";
    b.textContent = "theme: " + v;
    b.setAttribute("aria-label", "Theme: " + v + ". Click to change.");
  }
  apply(stored());
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest("[data-theme-toggle]") : null;
    if (!b) { return; }
    var order = ["system", "light", "dark"];
    var next = order[(order.indexOf(stored() || "system") + 1) % order.length];
    try { next === "system" ? localStorage.removeItem(KEY) : localStorage.setItem(KEY, next); } catch (e) {}
    apply(next === "system" ? null : next);
    label();
  });
  document.addEventListener("DOMContentLoaded", label);
})();
`

// asset is one static file published under a content-addressed name.
type asset struct {
	route string
	data  []byte
}

// assets are the site's static files, and the routes the templates reference.
type assets struct {
	style asset
	mark  asset
}

// buildAssets hashes the checked-in static files into content-addressed routes.
//
// Nothing here forms a dependency graph: the stylesheet is forbidden to reference
// another asset, so no hash is ever embedded in a file that is itself hashed.
// That is a real constraint, not an observation — a url() in the stylesheet would
// make the publish order significant, so it is refused rather than ordered.
func buildAssets() (assets, error) {
	if strings.Contains(string(styleCSS), "url(") {
		return assets{}, fmt.Errorf("the stylesheet references another file with url(...); assets are content-addressed and may not depend on each other")
	}
	return assets{
		style: asset{route: assetRoute("style", "css", digest12(styleCSS)), data: styleCSS},
		mark:  asset{route: assetRoute("mark", "png", digest12(markPNG)), data: markPNG},
	}, nil
}

// all returns the assets in publication order.
func (a assets) all() []asset { return []asset{a.style, a.mark} }

// digest12 is the short content digest used in an asset filename. Twelve hex
// characters of SHA-256 is far past any accidental collision for a handful of
// files, and keeps the published name readable.
func digest12(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// themeScript returns the bootstrap for the layout. It is marked as script
// content because it is a checked-in constant in this repository, not input.
func themeScript() template.JS {
	return template.JS(themeBootstrap) //nolint:gosec // a checked-in constant, never derived from input
}
