package docbundle

import (
	"bytes"
	"encoding/json"
)

// The manifest is the machine contract of an export: it names the schema, the
// binary that produced the bundle, bounded corpus facts, the generated machine
// artifact, and every document with its output path and content hash. A consumer
// reads only this file to know what the bundle contains and to verify it.
//
// Field order below is the emitted order, and no map is marshalled, so the JSON
// is byte-stable for a given bundle.

type manifest struct {
	ExportSchemaVersion int                `json:"export_schema_version"`
	Product             manifestProduct    `json:"product"`
	Corpus              manifestCorpus     `json:"corpus"`
	MachineReference    manifestMachineRef `json:"machine_reference"`
	Documents           []manifestDocument `json:"documents"`
}

// manifestProduct is the build identity of the binary that exported the bundle.
// Commit is omitted when the binary carries no VCS stamp, rather than published
// as an empty or guessed value.
type manifestProduct struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// manifestCorpus states the bounded totals a consumer needs before reading any
// document: how many pages of each kind exist and how large the corpus is.
type manifestCorpus struct {
	DocumentCount  int `json:"document_count"`
	TopicCount     int `json:"topic_count"`
	CommandCount   int `json:"command_count"`
	ReferenceCount int `json:"reference_count"`
	TotalBytes     int `json:"total_bytes"`
}

// manifestMachineRef locates and versions the generated machine artifact.
type manifestMachineRef struct {
	Path          string `json:"path"`
	SchemaVersion int    `json:"schema_version"`
	Bytes         int    `json:"bytes"`
	SHA256        string `json:"sha256"`
}

// manifestDocument describes one published Markdown document.
type manifestDocument struct {
	Slug    string   `json:"slug"`
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Kind    Kind     `json:"kind"`
	Path    string   `json:"path"`
	Aliases []string `json:"aliases,omitempty"`
	Bytes   int      `json:"bytes"`
	SHA256  string   `json:"sha256"`
}

// ManifestJSON renders the bundle manifest as deterministic JSON. It marshals
// into a buffer first so a failure yields no bytes at all, disables HTML escaping
// so placeholders like <count> in a summary stay readable, and indents two
// spaces to match the CLI reference. Two calls on the same bundle return
// identical bytes.
func (b Bundle) ManifestJSON() ([]byte, error) {
	m := manifest{
		ExportSchemaVersion: ExportSchemaVersion,
		Product: manifestProduct{
			Version: b.prov.Version,
			Commit:  b.prov.Commit,
		},
		MachineReference: manifestMachineRef{
			Path:          b.machine.path.String(),
			SchemaVersion: b.machine.schemaVersion,
			Bytes:         b.machine.Size(),
			SHA256:        b.machine.sha,
		},
		Documents: make([]manifestDocument, 0, len(b.docs)),
	}

	for _, d := range b.docs {
		m.Documents = append(m.Documents, manifestDocument{
			Slug:    d.slug.String(),
			Title:   d.title.String(),
			Summary: d.summary.String(),
			Kind:    d.kind,
			Path:    d.path.String(),
			Aliases: d.Aliases(),
			Bytes:   d.Size(),
			SHA256:  d.sha,
		})
		m.Corpus.DocumentCount++
		m.Corpus.TotalBytes += d.Size()
		switch d.kind {
		case KindTopic:
			m.Corpus.TopicCount++
		case KindCommand:
			m.Corpus.CommandCount++
		case KindReference:
			m.Corpus.ReferenceCount++
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	// enc.Encode already appends a trailing newline.
	return buf.Bytes(), nil
}
