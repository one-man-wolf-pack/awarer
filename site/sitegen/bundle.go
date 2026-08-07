package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"awarer/internal/domain/docbundle"
)

// This file turns an export directory on disk into the bytes the renderer needs,
// and does no more than that.
//
// The bundle's integrity belongs to the producer. docbundle.ParseManifest owns
// the wire contract — schema, slugs, paths, digest spelling, uniqueness, corpus
// totals — and the exporting binary's own tests own the rest: that every declared
// file is present with the declared digest, that the directory holds nothing
// else, and that each body is canonical Markdown whose H1 is the manifest title.
// The export is produced immediately before this runs, by a binary the operator
// selected. Re-deriving those facts here would be a second validator without a
// second trust boundary, and coordination between the two is the only thing it
// could ever produce.
//
// What remains is confinement of the named input: the directory is opened once
// as an *os.Root and every read goes through it, so no path in the manifest can
// resolve outside the tree the caller pointed at.

// Bundle is a parsed export: the manifest plus the bytes of the files it
// declares.
type Bundle struct {
	manifest docbundle.Manifest
	byPath   map[string]docbundle.ManifestEntry
	bodies   map[string][]byte
	machine  []byte
}

// Manifest returns the parsed manifest.
func (b *Bundle) Manifest() docbundle.Manifest { return b.manifest }

// Documents returns the declared documents in publication order.
func (b *Bundle) Documents() []docbundle.ManifestEntry { return b.manifest.Documents() }

// Body returns the bytes of a declared document.
func (b *Bundle) Body(e docbundle.ManifestEntry) []byte { return b.bodies[e.Path().String()] }

// MachineReference returns the bytes of the generated machine artifact.
func (b *Bundle) MachineReference() []byte { return b.machine }

// Lookup resolves a bundle-relative path to the document published at it.
func (b *Bundle) Lookup(p string) (docbundle.ManifestEntry, bool) {
	e, ok := b.byPath[p]
	return e, ok
}

// loadBundle opens an export directory and reads everything its manifest
// declares.
func loadBundle(ctx context.Context, dir string) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("opening the export directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	// The manifest is the one file read before anything has declared a length, so
	// the bound has to be the constant docbundle exports for exactly this reader.
	manifestBytes, err := readBounded(root, docbundle.ManifestName, docbundle.MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", docbundle.ManifestName, err)
	}
	m, err := docbundle.ParseManifest(manifestBytes)
	if err != nil {
		return nil, err
	}

	b := &Bundle{
		manifest: m,
		byPath:   make(map[string]docbundle.ManifestEntry, len(m.Documents())),
		bodies:   make(map[string][]byte, len(m.Documents())),
	}

	ref := m.MachineReference()
	b.machine, err = declared(ctx, root, ref.Path().String())
	if err != nil {
		return nil, err
	}
	for _, e := range m.Documents() {
		body, err := declared(ctx, root, e.Path().String())
		if err != nil {
			return nil, err
		}
		b.byPath[e.Path().String()] = e
		b.bodies[e.Path().String()] = body
	}
	return b, nil
}

// declared reads one file the manifest names. A missing or unreadable one fails
// the build naming the path, because a site missing a document it advertises is
// not a site worth deploying.
func declared(ctx context.Context, root *os.Root, rel string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil, fmt.Errorf("bundle file %q: %w", rel, err)
	}
	return data, nil
}

// readBounded reads a file whose length nothing has declared yet, capped by
// limit. The read goes one byte past the cap so a file at the boundary is
// distinguished from one over it.
func readBounded(root *os.Root, rel string, limit int) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if n > int64(limit) {
		return nil, fmt.Errorf("file is larger than the %d-byte limit", limit)
	}
	return buf.Bytes(), nil
}
