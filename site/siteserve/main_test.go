package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// servedSite is a handler over a directory shaped like a generated site.
func servedSite(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"index.html":        "<h1>home</h1>\n",
		"docs/a/index.html": "<h1>document a</h1>\n",
		"404.html":          "<h1>no such page</h1>\n",
		"robots.txt":        "User-agent: *\nAllow: /\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("opening the site: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return handler(root)
}

// TestHandlerAnswersLikeTheStaticHostTheSiteTargets covers what a file:// URL
// cannot show and what the browser review depends on: directory routes resolving
// to their index, and an unknown address answering with the generated 404 page
// under a 404 status rather than a plain-text error.
func TestHandlerAnswersLikeTheStaticHostTheSiteTargets(t *testing.T) {
	h := servedSite(t)

	tests := []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"the site root", "/", http.StatusOK, "<h1>home</h1>"},
		{"a directory route", "/docs/a/", http.StatusOK, "<h1>document a</h1>"},
		// A file served under its own name, which is the other half of the tree:
		// robots.txt, the sitemap, llms.txt, the machine reference, and every asset
		// take this path rather than the index-appending one above.
		{"a named file", "/robots.txt", http.StatusOK, "Allow: /"},
		// The body proves the generated page was served: http.Error would answer
		// with unmarked text.
		{"an unknown address", "/nope/", http.StatusNotFound, "<h1>no such page</h1>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("reading the response: %v", err)
			}
			if res.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body = %q, want it to contain %q", body, tc.want)
			}
		})
	}
}
