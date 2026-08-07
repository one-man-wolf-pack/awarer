package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteToPathMapsDirectoryRoutesToIndexHTML(t *testing.T) {
	// The URL shape a static host serves: a route ending in "/" is a directory
	// page and needs an index.html beside it, or the address answers nothing.
	for route, want := range map[string]string{
		routeHome:       "index.html",
		routeDocs:       "docs/index.html",
		"/docs/alpha/":  "docs/alpha/index.html",
		routeNotFound:   "404.html",
		routeMachineRef: "docs/cli.json",
		routeRobots:     "robots.txt",
		routeLLMsFull:   "llms-full.txt",
	} {
		got, err := routeToPath(route)
		if err != nil {
			t.Errorf("routeToPath(%q): %v", route, err)
			continue
		}
		if got != want {
			t.Errorf("routeToPath(%q) = %q, want %q", route, got, want)
		}
	}
	if _, err := routeToPath("docs/"); err == nil {
		t.Errorf("routeToPath accepted a route that is not rooted")
	}
}

func TestWriteSiteWritesTheWholeTreeIntoAnAbsentDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dist")
	files := []outputFile{
		{route: routeHome, data: []byte("home")},
		{route: "/docs/alpha/", data: []byte("alpha")},
		{route: routeRobots, data: []byte("robots")},
	}
	if err := writeSite(context.Background(), root, files); err != nil {
		t.Fatalf("writeSite: %v", err)
	}
	for rel, want := range map[string]string{
		"index.html":            "home",
		"docs/alpha/index.html": "alpha",
		"robots.txt":            "robots",
	} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("reading %s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestWriteSiteRefusesAnExistingOutput is the whole of the tool's relationship
// with what is already on disk: it writes into a path that is not there, and it
// removes, replaces, and recovers nothing. Both cases are refused by the same
// rule, which is why neither needs a diagnosis of its own.
func TestWriteSiteRefusesAnExistingOutput(t *testing.T) {
	files := []outputFile{{route: routeHome, data: []byte("home")}}

	t.Run("a directory is already there", func(t *testing.T) {
		root := t.TempDir()
		err := writeSite(context.Background(), root, files)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("error = %v, want an existing output to be refused", err)
		}
	})

	t.Run("a file is already there", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "dist")
		if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing the obstruction: %v", err)
		}
		err := writeSite(context.Background(), root, files)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("error = %v, want an existing output to be refused", err)
		}
	})
}

func TestWriteSiteStopsOnCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dist")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := writeSite(ctx, root, []outputFile{{route: routeHome, data: []byte("home")}})
	if err == nil {
		t.Fatalf("writeSite ignored a cancelled context")
	}
}
