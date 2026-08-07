package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The site is a projection of a published export, and the only thing that makes
// that provable rather than aspirational is that the generator cannot reach the
// producer's own model. It reads a directory and a manifest; it does not import
// the help catalog, the CLI, or the configuration reference.
//
// One product package is allowed on purpose: docbundle owns the export's wire
// contract — writing it, rendering it, and parsing it. Re-declaring that schema
// here would be a second copy of the slug spelling, the path rules, and the
// uniqueness rules, which is the duplication the contract exists to prevent.

// allowedProductImport is the one package under awarer/internal the site may use.
const allowedProductImport = "awarer/internal/domain/docbundle"

// TestSiteImportsOnlyTheExportContract walks every Go file under site/, test
// files included: a test that reached the help catalog to build its expectations
// would make the suite prove the site against the producer's own model rather
// than against a published export.
func TestSiteImportsOnlyTheExportContract(t *testing.T) {
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		checked++
		for _, spec := range f.Imports {
			imported, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				return uerr
			}
			if !strings.HasPrefix(imported, "awarer/") || imported == allowedProductImport {
				continue
			}
			t.Errorf("%s imports %s; the site generator may use only %s from the product tree",
				path, imported, allowedProductImport)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the site sources: %v", err)
	}
	if checked < 5 {
		t.Fatalf("only %d site source files were checked; the guard is not looking at the tree", checked)
	}
}

// TestTerminalFixtureIsAReviewedExampleNotACapture is the guard on the one
// recorded artifact the site publishes. It is a file a human may re-record, and
// a recording made on a real machine would publish the path of whoever ran it —
// so both halves are here: the checked-in fixture is clean, and the rule that
// keeps it clean refuses a leak.
func TestTerminalFixtureIsAReviewedExampleNotACapture(t *testing.T) {
	view, err := buildTerminal()
	if err != nil {
		t.Fatalf("buildTerminal: %v", err)
	}
	if len(view.Lines) < 5 {
		t.Fatalf("the fixture has only %d lines; it is not the recording it claims to be", len(view.Lines))
	}
	if !strings.Contains(strings.ToLower(view.Caption), "example") {
		t.Errorf("caption %q does not say the block is a recording", view.Caption)
	}

	raw, err := os.ReadFile(filepath.Join("fixtures", "terminal-status.txt"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	// Poison markers, deliberately different from the reviewed example root.
	for _, poison := range []string{"/Users/", "/root/", "/private/", `C:\`, "$HOME"} {
		if strings.Contains(string(raw), poison) {
			t.Errorf("the fixture carries %q", poison)
		}
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if j := strings.Index(line, "/home/"); j >= 0 && !strings.HasPrefix(line[j:], exampleRoot) {
			t.Errorf("line %d carries a path outside the reviewed example root: %q", i+1, line)
		}
	}

	saved := terminalFixture
	defer func() { terminalFixture = saved }()

	terminalFixture = "$ awa status\nroot:  /Users/someone/project\n"
	if _, err := buildTerminal(); err == nil || !strings.Contains(err.Error(), "publish a real path") {
		t.Fatalf("error = %v, want a leaked path to be refused", err)
	}

	terminalFixture = "$ awa status\nroot:  /home/someoneelse/project\n"
	if _, err := buildTerminal(); err == nil || !strings.Contains(err.Error(), "reviewed example root") {
		t.Fatalf("error = %v, want a path outside the example root to be refused", err)
	}

	terminalFixture = ""
	if _, err := buildTerminal(); err == nil {
		t.Fatalf("an empty fixture was accepted")
	}
}

// TestAssetsAreContentAddressed pins the property that makes the asset names
// safe to cache behind an immutable header: the name is a function of the bytes,
// so a deployed page can never be served with a stale stylesheet. The rule that
// keeps the naming acyclic — a stylesheet may not name another asset, whose own
// name is not known while it is being hashed — is the same subject and is here.
func TestAssetsAreContentAddressed(t *testing.T) {
	as, err := buildAssets()
	if err != nil {
		t.Fatalf("buildAssets: %v", err)
	}
	again, err := buildAssets()
	if err != nil {
		t.Fatalf("buildAssets: %v", err)
	}
	if as.style.route != again.style.route || as.mark.route != again.mark.route {
		t.Fatalf("asset routes are not stable across calls")
	}
	if as.style.route == assetRoute("style", "css", digest12([]byte("something else"))) {
		t.Fatalf("the stylesheet route does not depend on its bytes")
	}
	for _, a := range as.all() {
		if len(a.data) == 0 {
			t.Errorf("asset %s is empty", a.route)
		}
		if !strings.HasPrefix(a.route, "/assets/") {
			t.Errorf("asset %s is not published under /assets/", a.route)
		}
	}

	saved := styleCSS
	defer func() { styleCSS = saved }()

	styleCSS = []byte("body { background: url(/assets/mark.png); }")
	if _, err := buildAssets(); err == nil || !strings.Contains(err.Error(), "content-addressed") {
		t.Fatalf("error = %v, want an asset reference in the stylesheet to be refused", err)
	}
}

func TestTemplatesParse(t *testing.T) {
	// ParseFS fails at run time, not at build time, and `go vet` does not read the
	// templates at all — so parsing every one of them is the only thing that turns
	// a template typo into a red test. That they render what they should is the
	// projection test's subject.
	tmpl, err := loadTemplates()
	if err != nil {
		t.Fatalf("loadTemplates: %v", err)
	}
	for _, k := range pageKinds {
		if _, ok := tmpl[k.template()]; !ok {
			t.Errorf("no template was loaded for %T", k)
		}
	}

	// Every page kind has a template file of its own, and the directory holds
	// nothing beyond them and the shared layout: a file nobody parses is a page
	// somebody forgot to wire, or dead presentation.
	files, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("reading the template directory: %v", err)
	}
	if got, want := len(files), len(pageKinds)+1; got != want {
		t.Errorf("%d template files for %d page kinds plus the layout, want %d", got, len(pageKinds), want)
	}
}
