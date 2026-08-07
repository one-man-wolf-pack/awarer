package docbundle

import (
	"fmt"
	"regexp"
	"sort"
	"unicode/utf8"
)

// Document is one publishable file in a bundle: its identity, where it is
// written, and its exact bytes. Every field is set once at construction and read
// through accessors, so a document cannot be mutated after the bundle validated
// it. Body is copied in and out, so a caller's buffer and the published bytes
// can never alias.
type Document struct {
	slug    Slug
	title   Line
	summary Line
	kind    Kind
	path    DocPath
	aliases []string
	body    []byte
	sha     string
}

// provenanceTokenRe is the permitted spelling of a build-identity token (the
// product version and the source commit): bounded, with no whitespace or
// path-like characters, so provenance cannot smuggle a local path or a
// multi-line value into the manifest.
var provenanceTokenRe = regexp.MustCompile(`^[0-9A-Za-z.+_-]{1,64}$`)

// NewDocument validates and constructs a Document. Aliases are copied and sorted
// so the manifest is independent of the order a catalog happened to declare them
// in; body is copied so later mutation of the caller's slice cannot change what
// gets published or invalidate the recorded hash.
func NewDocument(slug Slug, title, summary string, kind Kind, p DocPath, aliases []string, body []byte) (Document, error) {
	if slug.s == "" {
		return Document{}, fmt.Errorf("docbundle: document has no slug")
	}
	// Both labels go through the same constructor the reader uses, so a bundle
	// cannot be built carrying a title or summary the parser would later refuse.
	titleLine, err := NewLine(fmt.Sprintf("document %q title", slug), title)
	if err != nil {
		return Document{}, err
	}
	summaryLine, err := NewLine(fmt.Sprintf("document %q summary", slug), summary)
	if err != nil {
		return Document{}, err
	}

	switch {
	case !kind.valid():
		return Document{}, fmt.Errorf("docbundle: document %q has unknown kind %q", slug, kind)
	case p.p == "":
		return Document{}, fmt.Errorf("docbundle: document %q has no output path", slug)
	case len(body) == 0:
		return Document{}, fmt.Errorf("docbundle: document %q has an empty body", slug)
	case !utf8.Valid(body):
		return Document{}, fmt.Errorf("docbundle: document %q body is not valid UTF-8", slug)
	}

	seen := make(map[string]bool, len(aliases))
	copied := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if _, err := NewSlug(a); err != nil {
			return Document{}, fmt.Errorf("docbundle: document %q alias: %w", slug, err)
		}
		if seen[a] {
			return Document{}, fmt.Errorf("docbundle: document %q lists alias %q twice", slug, a)
		}
		seen[a] = true
		copied = append(copied, a)
	}
	sort.Strings(copied)

	owned := make([]byte, len(body))
	copy(owned, body)

	return Document{
		slug:    slug,
		title:   titleLine,
		summary: summaryLine,
		kind:    kind,
		path:    p,
		aliases: copied,
		body:    owned,
		sha:     sum(owned),
	}, nil
}

// Slug returns the document's stable identifier.
func (d Document) Slug() Slug { return d.slug }

// Title returns the document's H1 title.
func (d Document) Title() string { return d.title.String() }

// Summary returns the one-line label listings show.
func (d Document) Summary() string { return d.summary.String() }

// Kind returns the document kind.
func (d Document) Kind() Kind { return d.kind }

// Path returns the bundle-relative output path.
func (d Document) Path() DocPath { return d.path }

// Aliases returns a copy of the sorted alias list.
func (d Document) Aliases() []string { return append([]string(nil), d.aliases...) }

// Body returns a copy of the document bytes.
func (d Document) Body() []byte { return append([]byte(nil), d.body...) }

// Size returns the document length in bytes.
func (d Document) Size() int { return len(d.body) }

// SHA256 returns the lowercase hex content hash.
func (d Document) SHA256() string { return d.sha }

// MachineRef is the generated machine-readable reference published beside the
// Markdown: the artifact an automated consumer reads instead of parsing prose.
// It carries its own schema version because it is versioned independently of the
// export schema.
type MachineRef struct {
	path          DocPath
	schemaVersion int
	body          []byte
	sha           string
}

// NewMachineRef validates and constructs the machine reference entry.
func NewMachineRef(p DocPath, schemaVersion int, body []byte) (MachineRef, error) {
	switch {
	case p.p == "":
		return MachineRef{}, fmt.Errorf("docbundle: machine reference has no output path")
	case schemaVersion <= 0:
		return MachineRef{}, fmt.Errorf("docbundle: machine reference schema version must be positive, got %d", schemaVersion)
	case len(body) == 0:
		return MachineRef{}, fmt.Errorf("docbundle: machine reference body is empty")
	}
	owned := make([]byte, len(body))
	copy(owned, body)
	return MachineRef{path: p, schemaVersion: schemaVersion, body: owned, sha: sum(owned)}, nil
}

// Path returns the bundle-relative output path.
func (m MachineRef) Path() DocPath { return m.path }

// SchemaVersion returns the machine reference's own schema version.
func (m MachineRef) SchemaVersion() int { return m.schemaVersion }

// Body returns a copy of the artifact bytes.
func (m MachineRef) Body() []byte { return append([]byte(nil), m.body...) }

// Size returns the artifact length in bytes.
func (m MachineRef) Size() int { return len(m.body) }

// SHA256 returns the lowercase hex content hash.
func (m MachineRef) SHA256() string { return m.sha }

// Provenance identifies the binary that produced a bundle. It carries only
// stable build identity: the released version and, when the binary was built
// from a VCS checkout, the full source commit. Build time, dirtiness, toolchain,
// host, and paths are deliberately excluded — they would make two exports of the
// same binary differ or leak the machine that built it.
type Provenance struct {
	Version string
	Commit  string
}

// validate enforces that provenance is publishable: a version is always present
// and a commit, when carried, is a bounded opaque token.
func (p Provenance) validate() error {
	if p.Version == "" {
		return fmt.Errorf("docbundle: provenance has no product version")
	}
	if !provenanceTokenRe.MatchString(p.Version) {
		return fmt.Errorf("docbundle: product version %q is not a plain version token", p.Version)
	}
	if p.Commit != "" && !provenanceTokenRe.MatchString(p.Commit) {
		return fmt.Errorf("docbundle: source commit %q is not a plain commit token", p.Commit)
	}
	return nil
}
