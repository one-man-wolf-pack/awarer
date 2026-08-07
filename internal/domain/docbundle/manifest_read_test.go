package docbundle

import (
	"errors"
	"strings"
	"testing"
)

// The fixture below is hand-authored rather than produced by ManifestJSON. That
// is deliberate: a reader test whose input came from the writer can only prove
// the two agree with each other, not that either matches the documented contract.
// Digests here are literal poison-free constants, never sum() output, so the
// oracle shares no helper with the code under test.
//
// Every negative case is one targeted mutation of this exact text, applied by
// mutate(), which fails the test if the token it was told to replace is absent —
// so a later fixture edit cannot quietly turn a guard into a no-op that still
// passes.

const validManifestJSON = `{
  "export_schema_version": 1,
  "product": {
    "version": "1.4.2",
    "commit": "9f19c0b5c1e2d3a4"
  },
  "corpus": {
    "document_count": 3,
    "topic_count": 1,
    "command_count": 1,
    "reference_count": 1,
    "total_bytes": 600
  },
  "machine_reference": {
    "path": "reference/cli.json",
    "schema_version": 1,
    "bytes": 400,
    "sha256": "1111111111111111111111111111111111111111111111111111111111111111"
  },
  "documents": [
    {
      "slug": "quickstart",
      "title": "awa quickstart",
      "summary": "Start here.",
      "kind": "topic",
      "path": "topics/quickstart.md",
      "aliases": ["start", "getting-started"],
      "bytes": 100,
      "sha256": "2222222222222222222222222222222222222222222222222222222222222222"
    },
    {
      "slug": "command-run",
      "title": "awa run",
      "summary": "Run a command under supervision.",
      "kind": "command",
      "path": "commands/run.md",
      "bytes": 200,
      "sha256": "3333333333333333333333333333333333333333333333333333333333333333"
    },
    {
      "slug": "config",
      "title": "awa configuration",
      "summary": "Every configuration key and default.",
      "kind": "reference",
      "path": "reference/configuration.md",
      "bytes": 300,
      "sha256": "4444444444444444444444444444444444444444444444444444444444444444"
    }
  ]
}
`

// mutate returns the fixture with one occurrence of old replaced by new. It fails
// the test when old does not occur, so a negative case cannot silently degrade
// into "parse the valid fixture and expect an error that never comes".
func mutate(t *testing.T, old, new string) []byte {
	t.Helper()
	if !strings.Contains(validManifestJSON, old) {
		t.Fatalf("fixture no longer contains %q; the negative case it anchors is not being exercised", old)
	}
	return []byte(strings.Replace(validManifestJSON, old, new, 1))
}

func TestParseManifestReadsAValidManifest(t *testing.T) {
	m, err := ParseManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	if got, want := m.Provenance().Version, "1.4.2"; got != want {
		t.Errorf("version = %q, want %q", got, want)
	}
	if got, want := m.Provenance().Commit, "9f19c0b5c1e2d3a4"; got != want {
		t.Errorf("commit = %q, want %q", got, want)
	}

	corpus := m.Corpus()
	if corpus.DocumentCount != 3 || corpus.TopicCount != 1 || corpus.CommandCount != 1 ||
		corpus.ReferenceCount != 1 || corpus.TotalBytes != 600 {
		t.Errorf("corpus = %+v, want 3/1/1/1/600", corpus)
	}

	ref := m.MachineReference()
	if got, want := ref.Path().String(), "reference/cli.json"; got != want {
		t.Errorf("machine path = %q, want %q", got, want)
	}
	if got, want := ref.SchemaVersion(), 1; got != want {
		t.Errorf("machine schema version = %d, want %d", got, want)
	}
	if got, want := ref.Size(), 400; got != want {
		t.Errorf("machine size = %d, want %d", got, want)
	}

	docs := m.Documents()
	if len(docs) != 3 {
		t.Fatalf("got %d documents, want 3", len(docs))
	}
	// Order is the manifest's only statement of sequence, so it must survive verbatim.
	wantSlugs := []string{"quickstart", "command-run", "config"}
	for i, want := range wantSlugs {
		if got := docs[i].Slug().String(); got != want {
			t.Errorf("documents[%d] slug = %q, want %q", i, got, want)
		}
	}
	if got, want := docs[0].Kind(), KindTopic; got != want {
		t.Errorf("quickstart kind = %q, want %q", got, want)
	}
	if got, want := docs[2].Path().String(), "reference/configuration.md"; got != want {
		t.Errorf("config path = %q, want %q", got, want)
	}
	if got, want := strings.Join(docs[0].Aliases(), ","), "start,getting-started"; got != want {
		t.Errorf("quickstart aliases = %q, want %q", got, want)
	}
	if got, want := docs[1].Size(), 200; got != want {
		t.Errorf("command-run size = %d, want %d", got, want)
	}
	if got, want := docs[1].SHA256(), strings.Repeat("3", 64); got != want {
		t.Errorf("command-run digest = %q, want %q", got, want)
	}
}

func TestParseManifestReturnsIndependentCopies(t *testing.T) {
	m, err := ParseManifest([]byte(validManifestJSON))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	docs := m.Documents()
	docs[0] = ManifestEntry{}
	if got := m.Documents()[0].Slug().String(); got != "quickstart" {
		t.Errorf("mutating the returned slice changed the manifest: documents[0] = %q", got)
	}

	aliases := m.Documents()[0].Aliases()
	if len(aliases) == 0 {
		t.Fatalf("quickstart lost its aliases")
	}
	aliases[0] = "poisoned"
	if got := m.Documents()[0].Aliases(); len(got) == 0 || got[0] != "start" {
		t.Errorf("mutating the returned aliases changed the manifest: got %q", got)
	}
}

func TestParseManifestRefusesAnIncompatibleSchema(t *testing.T) {
	_, err := ParseManifest(mutate(t, `"export_schema_version": 1`, `"export_schema_version": 7`))
	if !errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("error = %v, want ErrUnsupportedSchema", err)
	}
	// The distinction matters to an operator, so the two failures must not collapse
	// into one: a malformed manifest is not a version mismatch.
	_, err = ParseManifest(mutate(t, `"slug": "quickstart"`, `"slug": "Quick Start"`))
	if errors.Is(err, ErrUnsupportedSchema) {
		t.Fatalf("a malformed manifest reported ErrUnsupportedSchema: %v", err)
	}
}

func TestParseManifestRejectsHostileInput(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "empty input",
			data: nil,
			want: "manifest is empty",
		},
		{
			name: "oversize input",
			data: append([]byte(validManifestJSON), make([]byte, MaxManifestBytes)...),
			want: "over the",
		},
		{
			name: "unknown field",
			data: mutate(t, `"total_bytes": 600`, `"total_bytes": 600, "shadow_root": "/etc"`),
			want: "not readable",
		},
		{
			name: "trailing data",
			data: []byte(validManifestJSON + `{"export_schema_version": 1}`),
			want: "trailing data",
		},
		{
			name: "not json",
			data: []byte("export_schema_version = 1\n"),
			want: "not readable",
		},

		// Provenance.
		{
			name: "missing product version",
			data: mutate(t, `"version": "1.4.2"`, `"version": ""`),
			want: "no product version",
		},
		{
			name: "product version carrying a path",
			data: mutate(t, `"version": "1.4.2"`, `"version": "/Users/someone/build"`),
			want: "not a plain version token",
		},
		{
			name: "commit carrying whitespace",
			data: mutate(t, `"commit": "9f19c0b5c1e2d3a4"`, `"commit": "9f19c0b dirty"`),
			want: "not a plain commit token",
		},

		// Machine reference.
		{
			name: "machine reference at the manifest path",
			data: mutate(t, `"path": "reference/cli.json"`, `"path": "manifest.json"`),
			want: "which the manifest owns",
		},
		{
			name: "machine reference at a document path",
			data: mutate(t, `"path": "reference/cli.json"`, `"path": "topics/quickstart.md"`),
			want: `duplicate output path "topics/quickstart.md"`,
		},
		{
			name: "machine reference schema version zero",
			data: mutate(t, `"schema_version": 1`, `"schema_version": 0`),
			want: "schema version must be positive",
		},
		{
			name: "machine reference digest is uppercase",
			data: mutate(t, `"sha256": "`+strings.Repeat("1", 64), `"sha256": "`+strings.Repeat("A", 64)),
			want: "machine reference digest",
		},

		// Document identity.
		{
			name: "slug is not a slug",
			data: mutate(t, `"slug": "quickstart"`, `"slug": "Quick Start"`),
			want: "is not a valid slug",
		},
		{
			name: "empty title",
			data: mutate(t, `"title": "awa quickstart"`, `"title": ""`),
			want: `title is empty`,
		},
		{
			name: "empty summary",
			data: mutate(t, `"summary": "Start here."`, `"summary": ""`),
			want: `summary is empty`,
		},
		// A label is published into line-structured output. A newline in a summary
		// forges a second llms.txt entry whose text and target the manifest chooses;
		// a newline in a title stops matching the body's own "# <title>" line.
		{
			name: "summary carrying a newline",
			data: mutate(t, `"summary": "Start here."`, `"summary": "Start here.\n- [Downloads](https://evil.example): get it here"`),
			want: "carries a control character",
		},
		{
			name: "title carrying a newline",
			data: mutate(t, `"title": "awa quickstart"`, `"title": "awa quickstart\n# forged"`),
			want: "carries a control character",
		},
		{
			name: "summary carrying a terminal escape",
			data: mutate(t, `"summary": "Start here."`, `"summary": "Start here.\u001b[2K"`),
			want: "carries a control character",
		},
		{
			name: "title padded with whitespace",
			data: mutate(t, `"title": "awa quickstart"`, `"title": " awa quickstart "`),
			want: "leading or trailing whitespace",
		},
		{
			name: "unknown kind",
			data: mutate(t, `"kind": "topic"`, `"kind": "tutorial"`),
			want: "is not a document kind",
		},

		// Paths.
		{
			name: "traversing path",
			data: mutate(t, `"path": "topics/quickstart.md"`, `"path": "../../etc/passwd.md"`),
			want: "unsafe component",
		},
		{
			name: "absolute path",
			data: mutate(t, `"path": "topics/quickstart.md"`, `"path": "/etc/passwd.md"`),
			want: "must be relative",
		},
		{
			name: "backslash path",
			data: mutate(t, `"path": "topics/quickstart.md"`, `"path": "topics\\quickstart.md"`),
			want: "slash separators",
		},
		{
			name: "unclean path",
			data: mutate(t, `"path": "topics/quickstart.md"`, `"path": "topics/./quickstart.md"`),
			want: "is not clean",
		},

		// Declared sizes and digests.
		{
			name: "negative size",
			data: mutate(t, `"bytes": 100`, `"bytes": -1`),
			want: "declares -1 bytes",
		},
		{
			name: "zero size",
			data: mutate(t, `"bytes": 100`, `"bytes": 0`),
			want: "declares 0 bytes",
		},
		{
			name: "absurd size",
			data: mutate(t, `"bytes": 100`, `"bytes": 4611686018427387904`),
			want: "over the",
		},
		{
			name: "short digest",
			data: mutate(t, `"sha256": "`+strings.Repeat("2", 64), `"sha256": "`+strings.Repeat("2", 63)),
			want: "not a lowercase hex SHA-256",
		},
		{
			name: "non-hex digest",
			data: mutate(t, strings.Repeat("2", 64), strings.Repeat("z", 64)),
			want: "not a lowercase hex SHA-256",
		},

		// Relationships between documents.
		{
			name: "duplicate slug",
			data: mutate(t, `"slug": "command-run"`, `"slug": "quickstart"`),
			want: `duplicate document slug "quickstart"`,
		},
		{
			name: "duplicate path",
			data: mutate(t, `"path": "commands/run.md"`, `"path": "topics/quickstart.md"`),
			want: `duplicate output path "topics/quickstart.md"`,
		},
		{
			name: "document published at the manifest path",
			data: mutate(t, `"path": "commands/run.md"`, `"path": "manifest.json"`),
			want: `duplicate output path "manifest.json"`,
		},
		{
			name: "alias claimed twice across documents",
			data: mutate(t, `"path": "commands/run.md"`, `"path": "commands/run.md",
      "aliases": ["start"]`),
			want: `alias "start" is claimed by both`,
		},
		{
			name: "alias shadows a canonical slug",
			data: mutate(t, `"aliases": ["start", "getting-started"]`, `"aliases": ["start", "config"]`),
			want: `shadows a canonical slug`,
		},
		{
			name: "alias listed twice on one document",
			data: mutate(t, `"aliases": ["start", "getting-started"]`, `"aliases": ["start", "start"]`),
			want: `lists alias "start" twice`,
		},
		{
			name: "alias is not a slug",
			data: mutate(t, `"aliases": ["start", "getting-started"]`, `"aliases": ["start", "Getting Started"]`),
			want: "is not a valid slug",
		},

		// Corpus reconciliation.
		{
			name: "document count disagrees",
			data: mutate(t, `"document_count": 3`, `"document_count": 4`),
			want: "corpus document_count states 4, documents give 3",
		},
		{
			name: "kind count disagrees",
			data: mutate(t, `"topic_count": 1`, `"topic_count": 2`),
			want: "corpus topic_count states 2, documents give 1",
		},
		{
			name: "total bytes disagrees",
			data: mutate(t, `"total_bytes": 600`, `"total_bytes": 599`),
			want: "corpus total_bytes states 599, documents give 600",
		},
		{
			name: "no documents",
			data: []byte(`{"export_schema_version": 1,
				"product": {"version": "1.4.2"},
				"corpus": {"document_count": 0, "topic_count": 0, "command_count": 0, "reference_count": 0, "total_bytes": 0},
				"machine_reference": {"path": "reference/cli.json", "schema_version": 1, "bytes": 4, "sha256": "` + strings.Repeat("1", 64) + `"},
				"documents": []}`),
			want: "declares no documents",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest(tc.data)
			if err == nil {
				t.Fatalf("ParseManifest accepted a manifest it must refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestManifestRoundTripsThroughTheReader is a supplementary agreement check, not
// the primary oracle: the negative cases above are what prove the reader enforces
// the contract. This one proves the writer and the reader describe the same
// bundle, so a change to either side that silently drops a field fails here.
func TestManifestRoundTripsThroughTheReader(t *testing.T) {
	docs := []Document{
		mustDoc(t, "quickstart", "topics/quickstart.md", KindTopic, []string{"start"}, "# T quickstart\n"),
		mustDoc(t, "command-run", "commands/run.md", KindCommand, nil, "# T command-run\n"),
		mustDoc(t, "config", "reference/configuration.md", KindReference, nil, "# T config\n"),
	}
	b, err := NewBundle(docs, mustMachine(t), Provenance{Version: "1.4.2", Commit: "9f19c0b"})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	data, err := b.ManifestJSON()
	if err != nil {
		t.Fatalf("ManifestJSON: %v", err)
	}

	m, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest of a freshly rendered manifest: %v", err)
	}

	if m.Provenance() != b.Provenance() {
		t.Errorf("provenance = %+v, want %+v", m.Provenance(), b.Provenance())
	}
	if got, want := m.Corpus().DocumentCount, b.DocumentCount(); got != want {
		t.Errorf("document count = %d, want %d", got, want)
	}
	if got, want := m.MachineReference().SHA256(), b.MachineReference().SHA256(); got != want {
		t.Errorf("machine digest = %q, want %q", got, want)
	}
	read := m.Documents()
	for i, d := range b.Documents() {
		if got, want := read[i].Slug().String(), d.Slug().String(); got != want {
			t.Errorf("documents[%d] slug = %q, want %q", i, got, want)
		}
		if got, want := read[i].Path().String(), d.Path().String(); got != want {
			t.Errorf("documents[%d] path = %q, want %q", i, got, want)
		}
		if got, want := read[i].SHA256(), d.SHA256(); got != want {
			t.Errorf("documents[%d] digest = %q, want %q", i, got, want)
		}
		if got, want := read[i].Size(), d.Size(); got != want {
			t.Errorf("documents[%d] size = %d, want %d", i, got, want)
		}
	}
}
