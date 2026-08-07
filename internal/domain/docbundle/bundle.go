package docbundle

import "fmt"

// Bundle is one complete, validated documentation export. Construction is the
// completeness check: after NewBundle returns, every document has a unique slug
// and a unique output path, nothing collides with the manifest, and the
// provenance is publishable. An exporter therefore validates once, before it
// creates any destination output, and can then publish without further checks.
//
// Document order is the caller's order and is preserved exactly. Nothing here
// iterates a map, so the manifest and the publish order are stable across runs.
type Bundle struct {
	docs    []Document
	machine MachineRef
	prov    Provenance
}

// NewBundle validates and constructs a bundle.
func NewBundle(docs []Document, machine MachineRef, prov Provenance) (Bundle, error) {
	if len(docs) == 0 {
		return Bundle{}, fmt.Errorf("docbundle: a bundle must carry at least one document")
	}
	if machine.path.p == "" {
		return Bundle{}, fmt.Errorf("docbundle: a bundle must carry a machine reference")
	}
	if err := prov.validate(); err != nil {
		return Bundle{}, err
	}
	// Checked explicitly rather than left to the reserved-path map below: a map
	// literal silently collapses two identical keys, so a machine reference published
	// at the manifest's own path would build a VALID-looking bundle and only fail once
	// the exporter had already created output. The uniqueness invariant is promised
	// here, so it is decided here.
	if machine.path.String() == ManifestName {
		return Bundle{}, fmt.Errorf("docbundle: the machine reference cannot be published at %s, which the manifest owns", ManifestName)
	}

	slugs := make(map[string]bool, len(docs))
	paths := make(map[string]bool, len(docs)+2)
	paths[ManifestName] = true
	paths[machine.path.String()] = true
	aliases := make(map[string]string)

	for _, d := range docs {
		if d.slug.s == "" || d.path.p == "" {
			return Bundle{}, fmt.Errorf("docbundle: bundle holds an unconstructed document")
		}
		if slugs[d.slug.String()] {
			return Bundle{}, fmt.Errorf("docbundle: duplicate document slug %q", d.slug)
		}
		slugs[d.slug.String()] = true
		if paths[d.path.String()] {
			return Bundle{}, fmt.Errorf("docbundle: duplicate output path %q", d.path)
		}
		paths[d.path.String()] = true
		for _, a := range d.aliases {
			if prev, dup := aliases[a]; dup {
				return Bundle{}, fmt.Errorf("docbundle: alias %q is claimed by both %q and %q", a, prev, d.slug)
			}
			aliases[a] = d.slug.String()
		}
	}

	// An alias that is also a canonical slug would make resolution ambiguous for
	// any consumer that indexes both namespaces together. This runs as a second
	// pass because an alias may legitimately precede the document it shadows, and
	// it walks docs rather than the alias map so the reported collision is the
	// first one in publication order rather than whichever the runtime hands back.
	for _, d := range docs {
		for _, a := range d.aliases {
			if slugs[a] {
				return Bundle{}, fmt.Errorf("docbundle: alias %q on %q shadows a canonical slug", a, d.slug)
			}
		}
	}

	owned := make([]Document, len(docs))
	copy(owned, docs)
	return Bundle{docs: owned, machine: machine, prov: prov}, nil
}

// Documents returns the bundle's documents in publication order.
func (b Bundle) Documents() []Document { return append([]Document(nil), b.docs...) }

// MachineReference returns the generated machine artifact entry.
func (b Bundle) MachineReference() MachineRef { return b.machine }

// Provenance returns the identity of the binary that produced the bundle.
func (b Bundle) Provenance() Provenance { return b.prov }

// DocumentCount returns the number of Markdown documents, excluding the machine
// reference and the manifest.
func (b Bundle) DocumentCount() int { return len(b.docs) }
