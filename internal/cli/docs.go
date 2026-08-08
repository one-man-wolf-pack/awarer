package cli

import (
	"errors"

	"awarer/internal/app/docsexport"
	"awarer/internal/domain/docbundle"
	"awarer/internal/output"
)

// docsSubcommands lists the documentation surfaces. There is exactly one: export.
// No sibling is reserved for later — an advertised subcommand that returns "not
// implemented" is a promise the binary cannot keep.
//
// export declares no capabilities on purpose. The bundle is carried by the binary
// itself, so the command must not discover a project root, read a config layer, or
// take a lock; declaring none makes the router reject --root/--config/--json
// rather than accepting flags that would be inert. The manifest, not a JSON CLI
// envelope, is the machine contract of an export.
//
// This is a function rather than a package variable because the export handler
// reaches the command registry (the generated reference is a projection of it),
// and a variable holding that handler would make the registry and this list
// mutually dependent at package initialization.
func docsSubcommands() []subcommand {
	return []subcommand{{name: "export", summary: "write the complete documentation bundle to a directory", run: runDocsExport, help: commandHelp{
		usage: []string{"docs export --output <directory>"},
		long:  "Export every document of the installed version — the operational topics, the generated command reference, the configuration and exit-code reference, and a versioned JSON manifest — into one directory. The destination must not exist yet and its parent must, and there is no force mode: filling in or overwriting something named by mistake is not a recoverable error. Once the destination has been created, a failed or interrupted export leaves it in place and names it; awa never cleans it up. After a crash there is no message, so look at the destination itself: manifest.json is installed last and atomically, so a killed export left either nothing, an incomplete directory without manifest.json, or a complete and valid export. A directory without manifest.json is not an export — remove it yourself before retrying, because a retry refuses an existing path rather than resuming into it. Output is deterministic for a given binary and reads no project, config, git, network, or clock state, so two exports of the same binary are byte-for-byte identical.",
		flags: []flagHelp{
			{"--output <directory>", "destination directory to create; it must not exist yet, and its parent must exist", ""},
		},
		examples: []string{"awa docs export --output ./awa-docs"},
	}}}
}

// docsCapabilities is the coarse set route validates "awa docs" against. It is
// empty: documentation export is pure with respect to project state, so every
// global option is rejected rather than accepted as an inert no-op.
func docsCapabilities() []capability { return unionCapabilities(docsSubcommands()) }

// docsCmdHelp is the "awa docs --help" frame; docsHelp appends the subcommand list
// from docsSubcommands so the names live in exactly one place.
var docsCmdHelp = commandHelp{
	usage: []string{"docs <subcommand>"},
	long:  "Write the documentation of the installed version to files. The bundle collects the operational topics awa help shows, the generated command reference, and the configuration and exit-code reference as flat Markdown, plus a machine-readable reference and a manifest.",
}

// runDocs dispatches "awa docs <subcommand>".
func runDocs(w *output.Writer, inv invocation) error {
	if inv.options.Help {
		return docsHelp(w, inv)
	}
	if len(inv.args) == 0 {
		return usageErrorf("docs requires a subcommand: export")
	}
	sub := inv.args[0]
	spec := lookupSubcommand(docsSubcommands(), sub)
	if spec == nil {
		return usageErrorf("unknown docs subcommand %q: want export", sub)
	}
	return dispatchSubcommand(w, "docs", spec, inv)
}

// docsHelp renders "awa docs --help" and "awa docs <sub> --help". It does no
// project or lock work.
func docsHelp(w *output.Writer, inv invocation) error {
	if len(inv.args) > 0 {
		if spec := lookupSubcommand(docsSubcommands(), inv.args[0]); spec != nil {
			writeHelpBody(w, "awa docs "+spec.name, subcommandHelp("docs", *spec), hasCap(spec.caps, capJSON))
			return nil
		}
	}
	writeSubcommandOwnerHelp(w, "docs", docsCmdHelp, docsSubcommands())
	return nil
}

// runDocsExport handles "awa docs export --output <directory>". It builds and
// validates the complete bundle in memory before touching the destination, so a
// catalog or rendering fault fails with nothing written at all.
func runDocsExport(w *output.Writer, inv invocation) error {
	outputDir, err := parseDocsExportArgs(inv.args[1:])
	if err != nil {
		return err
	}

	bundle, err := buildDocsBundle()
	if err != nil {
		return genericErrorf("building the documentation bundle: %v", err)
	}

	result, err := docsexport.Publish(inv.invCtx(), bundle, outputDir)
	if err != nil {
		return docsExportFailure(err)
	}

	w.Linef("exported %d documents (export schema %d, awa %s) to %s",
		bundle.DocumentCount(), docbundle.ExportSchemaVersion, bundle.Provenance().Version, result.Output)
	w.Linef("manifest: %s", result.ManifestPath)
	return nil
}

// docsExportFailure maps a failed export to its exit. It is a named classifier
// rather than an if-chain inside the handler because the interesting decision is
// not which code — it is which failures may keep their message.
//
// A refused destination is the user's argument, so it is a usage error. An
// interruption keeps the interrupt code rather than a generic failure that would
// read like a broken destination, but only a cancellation that created nothing may
// also keep the standard terse wording: an export cancelled after it reserved the
// destination leaves that directory behind, and "interrupted" alone would send the
// operator away without the one thing they have to act on. Every question here is
// asked of the error's identity, never its wording.
func docsExportFailure(err error) *codedError {
	if errors.Is(err, docsexport.ErrUnsafeDestination) {
		return usageErrorf("docs export: %v", err)
	}
	if c := interruptError(err); c != nil {
		if errors.Is(err, docsexport.ErrIncompleteExport) {
			return interruptedErrorf("docs export: %v", err)
		}
		return c
	}
	return genericErrorf("docs export: %v", err)
}

// parseDocsExportArgs reads the one required flag. --output is mandatory rather
// than defaulted: a default destination would let a mistyped invocation write a
// tree the user never named.
func parseDocsExportArgs(args []string) (string, error) {
	out := ""
	i := 0
	for i < len(args) {
		tok := args[i]
		// requireValue reads args[i], so i must already point past the flag token.
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--output":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return "", err
			}
			out = v
		default:
			return "", rejectExtra("docs export", tok)
		}
	}
	if out == "" {
		return "", usageErrorf("docs export requires --output <directory>")
	}
	return out, nil
}
