package cli

import (
	"bytes"
	"fmt"

	"awarer/internal/app"
	"awarer/internal/app/help"
	"awarer/internal/domain/docbundle"
)

// buildDocsBundle assembles the complete documentation bundle a released binary
// carries. This is the one place in the codebase that can see both halves of the
// documentation: the authored catalog in internal/app/help and the parser-owned
// registries in this package. Assembling here keeps the layering intact — the
// pure catalog never learns about the CLI, and the domain bundle model never
// learns about either.
//
// The whole bundle is built and validated in memory. The exporter therefore
// knows the export is complete and internally consistent before it creates any
// destination output, which is what makes "publish everything or publish
// nothing authoritative" achievable rather than aspirational.
//
// Ordering is deterministic and meaningful: the authored topics in catalog
// display order first (an agent reading top to bottom gets the golden path
// first), then the generated command pages in registry order, then the
// remaining generated reference pages. No map is iterated.
func buildDocsBundle() (docbundle.Bundle, error) {
	ref := buildReference()

	topics := help.Topics()
	topicPaths := make(map[string]string, len(topics))
	for _, t := range topics {
		topicPaths[t.Slug.String()] = t.Path.String()
	}

	docs := make([]docbundle.Document, 0, len(topics)+len(ref.Commands)+3)

	// Catalog topics arrive already parsed, so they are handed to the bundle
	// as-is: their identity was established once, in the catalog.
	for _, t := range topics {
		doc, err := docbundle.NewDocument(t.Slug, t.Title, t.Summary, t.Kind, t.Path, t.Aliases, []byte(t.Body))
		if err != nil {
			return docbundle.Bundle{}, err
		}
		docs = append(docs, doc)
	}

	index, err := newGeneratedDocument(
		"commands",
		"awa command reference",
		"every command and subcommand of the installed version",
		docbundle.KindCommand,
		docsCommandIndexPath,
		renderCommandIndex(ref),
	)
	if err != nil {
		return docbundle.Bundle{}, err
	}
	docs = append(docs, index)

	for _, c := range ref.Commands {
		doc, err := newGeneratedDocument(
			commandDocSlug(c.Name),
			"awa "+c.Name,
			c.Summary,
			docbundle.KindCommand,
			commandDocPath(c.Name),
			renderCommandPage(ref, c, topicPaths),
		)
		if err != nil {
			return docbundle.Bundle{}, err
		}
		docs = append(docs, doc)
	}

	globals, err := newGeneratedDocument(
		"global-options",
		"awa global options",
		"the global options every command is validated against",
		docbundle.KindReference,
		docsGlobalOptionsPath,
		renderGlobalOptions(ref),
	)
	if err != nil {
		return docbundle.Bundle{}, err
	}
	docs = append(docs, globals)

	exits, err := newGeneratedDocument(
		"exit-code-reference",
		"awa exit codes",
		"the exact process-exit catalog and wrapped-child contract",
		docbundle.KindReference,
		docsExitCodesPath,
		renderExitCodes(ref, topicPaths),
	)
	if err != nil {
		return docbundle.Bundle{}, err
	}
	docs = append(docs, exits)

	machine, err := buildMachineReference()
	if err != nil {
		return docbundle.Bundle{}, err
	}

	return docbundle.NewBundle(docs, machine, docsProvenance())
}

// newGeneratedDocument constructs one generated document, parsing its slug and
// path literals into value objects. Only generated pages need this: their
// identity is declared here in plain strings, whereas catalog topics arrive
// already parsed.
func newGeneratedDocument(slug, title, summary string, kind docbundle.Kind, outPath string, body string) (docbundle.Document, error) {
	s, err := docbundle.NewSlug(slug)
	if err != nil {
		return docbundle.Document{}, fmt.Errorf("document %q: %w", slug, err)
	}
	p, err := docbundle.NewDocPath(outPath)
	if err != nil {
		return docbundle.Document{}, fmt.Errorf("document %q: %w", slug, err)
	}
	return docbundle.NewDocument(s, title, summary, kind, p, nil, []byte(body))
}

// buildMachineReference carries the generated CLI reference JSON into the bundle.
// It is the machine contract of the export: an automated consumer reads it
// instead of parsing the Markdown, and it is the same document refgen emits and
// `refgen -check` pins to a committed golden.
func buildMachineReference() (docbundle.MachineRef, error) {
	var buf bytes.Buffer
	if err := WriteReferenceJSON(&buf); err != nil {
		return docbundle.MachineRef{}, fmt.Errorf("rendering the machine reference: %w", err)
	}
	p, err := docbundle.NewDocPath(docsMachineRefPath)
	if err != nil {
		return docbundle.MachineRef{}, err
	}
	return docbundle.NewMachineRef(p, referenceSchemaVersion, buf.Bytes())
}

// docsProvenance reports which binary produced the bundle. Only stable build
// identity is carried: the release version and, when the binary was built from an
// unmodified checkout, the source commit that describes it (see app.SourceCommit
// for why a dirty build claims none). Build time, dirtiness, toolchain version,
// host, and paths are deliberately excluded — they would either differ between
// two exports of the same binary or leak the machine that built it.
func docsProvenance() docbundle.Provenance {
	return docbundle.Provenance{
		Version: app.VersionString(),
		Commit:  app.SourceCommit(),
	}
}
