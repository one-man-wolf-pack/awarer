package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What is proven here is what the generator itself decides: that it reads the
// export the manifest describes, and that a manifest or a document it cannot
// read stops the build instead of producing a site with a hole in it.
//
// Whether the export's contents are intact is not asked. The bundle's digests,
// declared sizes, absence of undeclared files, and canonical Markdown shape are
// the producer's facts, proven where they are owned — in
// internal/domain/docbundle, internal/app/docsexport, and internal/cli. A second
// opinion here would be a coordination cost with no second trust boundary.

func TestLoadBundleReadsTheDeclaredExport(t *testing.T) {
	b := loadFixture(t, defaultDocs())

	if got, want := len(b.Documents()), 3; got != want {
		t.Fatalf("got %d documents, want %d", got, want)
	}
	if got, want := b.Manifest().Provenance().Version, "1.2.3"; got != want {
		t.Errorf("version = %q, want %q", got, want)
	}
	if got, want := string(b.MachineReference()), machineRefBody; got != want {
		t.Errorf("machine reference = %q, want %q", got, want)
	}
	// Order is the manifest's, and the site depends on it for navigation and
	// previous/next links.
	for i, want := range []string{"alpha", "beta", "command-gamma"} {
		if got := b.Documents()[i].Slug().String(); got != want {
			t.Errorf("documents[%d] = %q, want %q", i, got, want)
		}
	}
	if got, want := string(b.Body(b.Documents()[1])), defaultDocs()[1].body; got != want {
		t.Errorf("body = %q, want the exported bytes %q", got, want)
	}
	if e, ok := b.Lookup("reference/beta.md"); !ok || e.Slug().String() != "beta" {
		t.Errorf("Lookup(reference/beta.md) = %v, %v", e.Slug(), ok)
	}
	if _, ok := b.Lookup("reference/nothing.md"); ok {
		t.Errorf("Lookup found a path the export does not publish")
	}
}

// TestLoadBundleFailsLoudlyOnAnUnreadableExport covers the three inputs that
// stop the build before a page is rendered. Each is a plausible partial or
// damaged export, and each must fail rather than produce a smaller site.
func TestLoadBundleFailsLoudlyOnAnUnreadableExport(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(t *testing.T, dir string)
		want   string
	}{
		{
			// The exporter writes the manifest last, so this is what an interrupted
			// export leaves behind.
			name: "the manifest is missing",
			break_: func(t *testing.T, dir string) {
				remove(t, filepath.Join(dir, "manifest.json"))
			},
			want: "manifest.json",
		},
		{
			name: "the manifest is not readable JSON",
			break_: func(t *testing.T, dir string) {
				writeExportFile(t, dir, "manifest.json", "{not json\n")
			},
			// The parser's own prefix, not the word "manifest": the read path above
			// also names the file, so a looser assertion could not tell a manifest
			// that was refused from one that was never opened.
			want: "docbundle:",
		},
		{
			name: "a declared document is missing",
			break_: func(t *testing.T, dir string) {
				remove(t, filepath.Join(dir, "reference", "beta.md"))
			},
			want: "reference/beta.md",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeExport(t, defaultDocs())
			tc.break_(t, dir)

			_, err := loadBundle(context.Background(), dir)
			if err == nil {
				t.Fatalf("loadBundle accepted an export it cannot read")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadBundleStopsOnCancellation(t *testing.T) {
	dir := writeExport(t, defaultDocs())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := loadBundle(ctx, dir); err == nil {
		t.Fatalf("loadBundle ignored a cancelled context")
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing %s: %v", path, err)
	}
}
