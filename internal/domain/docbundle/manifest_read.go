package docbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// The reader is this file; the writer is manifest.go beside it, and both use the
// same wire struct so they cannot drift apart field by field. The manifest is
// this package's contract with its consumers, and one that re-declared the shape
// elsewhere would own a second copy of the schema, the slug spelling, the path
// rules, and the uniqueness rules — the duplication this package exists to
// prevent. ParseManifest is therefore the only supported way to read an export
// manifest.
//
// It reads hostile input. Everything a manifest asserts about itself is checked
// before any value is returned, so a caller that gets a Manifest back can act on
// it without re-validating: the schema version is the one this build speaks, no
// unknown or trailing bytes were accepted, every slug and path is a constructed
// value object, every digest is well formed, every declared size is bounded and
// positive, no two documents share a slug, a path, or an alias, nothing collides
// with the manifest's own filename or the machine reference, and the corpus
// totals are the ones the document list actually adds up to.
//
// Only the current schema is supported. There is no legacy reader: a bundle from
// an incompatible export is refused, not adapted.

// MaxManifestBytes bounds the manifest a reader will decode. A manifest describes
// a bounded corpus and the real one is tens of kilobytes; the cap exists so a
// corrupt or hostile length cannot be turned into an allocation before a single
// field has been checked.
const MaxManifestBytes = 4 << 20

// maxDocumentBytes bounds one declared document or machine artifact. Same reason:
// a consumer sizes its read from the manifest, so the manifest must not be able
// to ask for an unbounded one. It stays unexported because a consumer never needs
// the constant — it reads against the size this parser already accepted.
const maxDocumentBytes = 16 << 20

// ErrUnsupportedSchema marks a manifest whose export schema this build does not
// speak. It is separated from ordinary malformed input because the two mean
// different things to an operator: one is a version mismatch to resolve by using
// the matching tooling, the other is a broken or tampered bundle.
var ErrUnsupportedSchema = errors.New("docbundle: unsupported export schema")

// digestRe is the spelling of a content hash in the manifest: lowercase hex
// SHA-256. Uppercase is rejected rather than folded, so a digest compares as
// bytes without a normalization step every consumer would have to remember.
var digestRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Corpus is the bounded set of totals a manifest states about itself. A reader
// recomputes each one from the document list, so these are cross-checked facts
// rather than claims.
type Corpus struct {
	DocumentCount  int
	TopicCount     int
	CommandCount   int
	ReferenceCount int
	TotalBytes     int
}

// ManifestEntry is one document as the manifest describes it: identity and
// location, plus the size and digest a consumer verifies the published file
// against. It carries no body — the bytes live in the export directory, and
// reading them is the consumer's I/O, not this package's.
type ManifestEntry struct {
	slug    Slug
	title   Line
	summary Line
	kind    Kind
	path    DocPath
	aliases []string
	size    int
	sha     string
}

// Slug returns the document's stable identifier.
func (e ManifestEntry) Slug() Slug { return e.slug }

// Title returns the document's H1 title.
func (e ManifestEntry) Title() string { return e.title.String() }

// Summary returns the one-line label listings show.
func (e ManifestEntry) Summary() string { return e.summary.String() }

// Kind returns the document kind.
func (e ManifestEntry) Kind() Kind { return e.kind }

// Path returns the bundle-relative path of the published file.
func (e ManifestEntry) Path() DocPath { return e.path }

// Aliases returns a copy of the alternate identifiers resolving to this document.
func (e ManifestEntry) Aliases() []string { return append([]string(nil), e.aliases...) }

// Size returns the declared file length in bytes.
func (e ManifestEntry) Size() int { return e.size }

// SHA256 returns the declared lowercase hex content hash.
func (e ManifestEntry) SHA256() string { return e.sha }

// ManifestRef is the generated machine artifact as the manifest describes it.
type ManifestRef struct {
	path          DocPath
	schemaVersion int
	size          int
	sha           string
}

// Path returns the bundle-relative path of the machine artifact.
func (r ManifestRef) Path() DocPath { return r.path }

// SchemaVersion returns the artifact's own schema version, which is versioned
// independently of the export schema.
func (r ManifestRef) SchemaVersion() int { return r.schemaVersion }

// Size returns the declared artifact length in bytes.
func (r ManifestRef) Size() int { return r.size }

// SHA256 returns the declared lowercase hex content hash.
func (r ManifestRef) SHA256() string { return r.sha }

// Manifest is a decoded, fully validated export manifest.
type Manifest struct {
	prov    Provenance
	corpus  Corpus
	machine ManifestRef
	docs    []ManifestEntry
}

// Provenance returns the identity of the binary that produced the export.
func (m Manifest) Provenance() Provenance { return m.prov }

// Corpus returns the bounded totals, already reconciled with the document list.
func (m Manifest) Corpus() Corpus { return m.corpus }

// MachineReference returns the generated machine artifact entry.
func (m Manifest) MachineReference() ManifestRef { return m.machine }

// Documents returns the declared documents in publication order. Order is the
// manifest's only statement of sequence — there is no ordinal field — so it is
// preserved exactly and a consumer that needs the exported order reads it here.
func (m Manifest) Documents() []ManifestEntry { return append([]ManifestEntry(nil), m.docs...) }

// ParseManifest decodes and validates an export manifest.
func ParseManifest(data []byte) (Manifest, error) {
	if len(data) == 0 {
		return Manifest{}, fmt.Errorf("docbundle: manifest is empty")
	}
	if len(data) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("docbundle: manifest is %d bytes, over the %d-byte limit", len(data), MaxManifestBytes)
	}

	// The wire struct is the one the writer marshals, so reader and writer cannot
	// drift apart field by field. Unknown fields are refused rather than ignored:
	// a field this build does not understand is either a schema this build does not
	// speak or an injection, and neither should be silently dropped.
	var raw manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Manifest{}, fmt.Errorf("docbundle: manifest is not readable: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("docbundle: manifest has trailing data after the document")
	}

	if raw.ExportSchemaVersion != ExportSchemaVersion {
		return Manifest{}, fmt.Errorf("%w: manifest declares %d, this build reads %d",
			ErrUnsupportedSchema, raw.ExportSchemaVersion, ExportSchemaVersion)
	}

	prov := Provenance{Version: raw.Product.Version, Commit: raw.Product.Commit}
	if err := prov.validate(); err != nil {
		return Manifest{}, err
	}

	machine, err := parseManifestRef(raw.MachineReference)
	if err != nil {
		return Manifest{}, err
	}

	docs, corpus, err := parseManifestDocuments(raw.Documents, machine)
	if err != nil {
		return Manifest{}, err
	}
	if err := reconcileCorpus(raw.Corpus, corpus); err != nil {
		return Manifest{}, err
	}

	return Manifest{prov: prov, corpus: corpus, machine: machine, docs: docs}, nil
}

// parseManifestRef validates the machine artifact row. Its path is checked
// against the manifest's own filename here rather than in the shared uniqueness
// pass below, because a map of reserved paths would silently collapse the two
// identical keys and report nothing — the same reason NewBundle decides it
// explicitly on the writing side.
func parseManifestRef(raw manifestMachineRef) (ManifestRef, error) {
	p, err := NewDocPath(raw.Path)
	if err != nil {
		return ManifestRef{}, fmt.Errorf("docbundle: machine reference path: %w", err)
	}
	if p.String() == ManifestName {
		return ManifestRef{}, fmt.Errorf("docbundle: machine reference is published at %s, which the manifest owns", ManifestName)
	}
	if raw.SchemaVersion <= 0 {
		return ManifestRef{}, fmt.Errorf("docbundle: machine reference schema version must be positive, got %d", raw.SchemaVersion)
	}
	if err := checkDeclaredSize("machine reference", raw.Bytes); err != nil {
		return ManifestRef{}, err
	}
	if !digestRe.MatchString(raw.SHA256) {
		return ManifestRef{}, fmt.Errorf("docbundle: machine reference digest %q is not a lowercase hex SHA-256", raw.SHA256)
	}
	return ManifestRef{path: p, schemaVersion: raw.SchemaVersion, size: raw.Bytes, sha: raw.SHA256}, nil
}

// parseManifestDocuments validates every document row and the relationships
// between them, and returns the corpus totals recomputed from what it read.
func parseManifestDocuments(raws []manifestDocument, machine ManifestRef) ([]ManifestEntry, Corpus, error) {
	if len(raws) == 0 {
		return nil, Corpus{}, fmt.Errorf("docbundle: manifest declares no documents")
	}

	docs := make([]ManifestEntry, 0, len(raws))
	slugs := make(map[string]bool, len(raws))
	paths := make(map[string]bool, len(raws)+2)
	paths[ManifestName] = true
	paths[machine.path.String()] = true
	aliases := make(map[string]string)
	var corpus Corpus

	for _, raw := range raws {
		entry, err := parseManifestEntry(raw)
		if err != nil {
			return nil, Corpus{}, err
		}
		if slugs[entry.slug.String()] {
			return nil, Corpus{}, fmt.Errorf("docbundle: duplicate document slug %q", entry.slug)
		}
		slugs[entry.slug.String()] = true
		if paths[entry.path.String()] {
			return nil, Corpus{}, fmt.Errorf("docbundle: duplicate output path %q", entry.path)
		}
		paths[entry.path.String()] = true
		for _, a := range entry.aliases {
			if prev, dup := aliases[a]; dup {
				return nil, Corpus{}, fmt.Errorf("docbundle: alias %q is claimed by both %q and %q", a, prev, entry.slug)
			}
			aliases[a] = entry.slug.String()
		}

		docs = append(docs, entry)
		corpus.DocumentCount++
		corpus.TotalBytes += entry.size
		switch entry.kind {
		case KindTopic:
			corpus.TopicCount++
		case KindCommand:
			corpus.CommandCount++
		case KindReference:
			corpus.ReferenceCount++
		}
	}

	// Second pass, for the reason NewBundle runs one: an alias may legitimately
	// appear before the document whose slug it would shadow, so shadowing can only
	// be decided once every canonical slug is known.
	for _, d := range docs {
		for _, a := range d.aliases {
			if slugs[a] {
				return nil, Corpus{}, fmt.Errorf("docbundle: alias %q on %q shadows a canonical slug", a, d.slug)
			}
		}
	}

	return docs, corpus, nil
}

// parseManifestEntry validates one document row in isolation.
func parseManifestEntry(raw manifestDocument) (ManifestEntry, error) {
	slug, err := NewSlug(raw.Slug)
	if err != nil {
		return ManifestEntry{}, err
	}
	// The same constructor the writer uses, so the two sides cannot disagree about
	// what a publishable label is. Both are published into line-structured formats
	// where a stray newline is not cosmetic: it forges a second entry.
	title, err := NewLine(fmt.Sprintf("document %q title", slug), raw.Title)
	if err != nil {
		return ManifestEntry{}, err
	}
	summary, err := NewLine(fmt.Sprintf("document %q summary", slug), raw.Summary)
	if err != nil {
		return ManifestEntry{}, err
	}
	kind, err := ParseKind(string(raw.Kind))
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("docbundle: document %q: %w", slug, err)
	}
	p, err := NewDocPath(raw.Path)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("docbundle: document %q path: %w", slug, err)
	}
	if err := checkDeclaredSize(fmt.Sprintf("document %q", slug), raw.Bytes); err != nil {
		return ManifestEntry{}, err
	}
	if !digestRe.MatchString(raw.SHA256) {
		return ManifestEntry{}, fmt.Errorf("docbundle: document %q digest %q is not a lowercase hex SHA-256", slug, raw.SHA256)
	}

	seen := make(map[string]bool, len(raw.Aliases))
	owned := make([]string, 0, len(raw.Aliases))
	for _, a := range raw.Aliases {
		if _, err := NewSlug(a); err != nil {
			return ManifestEntry{}, fmt.Errorf("docbundle: document %q alias: %w", slug, err)
		}
		if seen[a] {
			return ManifestEntry{}, fmt.Errorf("docbundle: document %q lists alias %q twice", slug, a)
		}
		seen[a] = true
		owned = append(owned, a)
	}

	return ManifestEntry{
		slug:    slug,
		title:   title,
		summary: summary,
		kind:    kind,
		path:    p,
		aliases: owned,
		size:    raw.Bytes,
		sha:     raw.SHA256,
	}, nil
}

// checkDeclaredSize bounds a length that came from the manifest. A consumer sizes
// a read or an allocation from it, so zero, negative, and absurd are all refused
// before the number is handed on.
func checkDeclaredSize(what string, n int) error {
	switch {
	case n <= 0:
		return fmt.Errorf("docbundle: %s declares %d bytes", what, n)
	case n > maxDocumentBytes:
		return fmt.Errorf("docbundle: %s declares %d bytes, over the %d-byte limit", what, n, maxDocumentBytes)
	}
	return nil
}

// reconcileCorpus compares the totals a manifest states with the ones its own
// document list produces. They are redundant by design: a bundle that lost or
// gained a row while keeping its summary intact is caught here rather than
// downstream, where a consumer would only notice by counting.
func reconcileCorpus(stated manifestCorpus, got Corpus) error {
	mismatches := []struct {
		field  string
		stated int
		got    int
	}{
		{"document_count", stated.DocumentCount, got.DocumentCount},
		{"topic_count", stated.TopicCount, got.TopicCount},
		{"command_count", stated.CommandCount, got.CommandCount},
		{"reference_count", stated.ReferenceCount, got.ReferenceCount},
		{"total_bytes", stated.TotalBytes, got.TotalBytes},
	}
	for _, m := range mismatches {
		if m.stated != m.got {
			return fmt.Errorf("docbundle: corpus %s states %d, documents give %d", m.field, m.stated, m.got)
		}
	}
	return nil
}
