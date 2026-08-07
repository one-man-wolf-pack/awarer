// Command sitegen renders the public awarer site from one `awa docs export`
// bundle.
//
// It is a build tool, not a shipped binary, and it is deliberately a consumer:
// its only input is an export directory plus this package's own presentation
// sources. It does not import the help catalog, the CLI, or any model the
// exporting binary builds its documentation from — going through the published
// export is what makes the site provably a projection of what a binary actually
// carries, rather than a second rendering of the same source tree.
//
// Usage:
//
//	sitegen --docs <export-dir> --output <absent-dir> --base-url <https-url> [--release]
//
// One build into a directory that does not exist. The site is assembled whole in
// memory and only then written, so a failure before the first write leaves
// nothing behind and a failure during it leaves a partial tree that carries no
// meaning — remove it and run again. This tool removes, replaces, and recovers
// nothing.
//
// The output is a pure function of the export, the base url, and --release: no
// clock, no randomness, no environment, no network, and no local path reaches a
// published byte.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt) //nolint:forbidigo // the process entrypoint of a build tool; there is no caller context to inherit
	defer stop()

	if err := run(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			// "Any", because an interrupt during the read or the render wrote nothing
			// at all: the run must not send someone looking for a directory it never
			// created.
			fmt.Fprintln(os.Stderr, "sitegen: interrupted; any partial output carries no meaning — remove it and rebuild")
		} else {
			fmt.Fprintf(os.Stderr, "sitegen: %v\n", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	docs := flag.String("docs", "", "directory holding an `awa docs export` bundle")
	output := flag.String("output", "", "directory to write the site into; it must not exist")
	baseURLFlag := flag.String("base-url", "", "canonical https origin the site is served from")
	release := flag.Bool("release", false, "drop the development-preview banner; only for a build from a released binary's export")
	flag.Parse()

	if flag.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", flag.Arg(0))
	}
	if *docs == "" {
		return fmt.Errorf("--docs is required")
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}
	base, err := ParseBaseURL(*baseURLFlag)
	if err != nil {
		return err
	}

	docsAbs, err := filepath.Abs(*docs)
	if err != nil {
		return err
	}
	outAbs, err := filepath.Abs(*output)
	if err != nil {
		return err
	}

	bundle, err := loadBundle(ctx, docsAbs)
	if err != nil {
		return err
	}
	files, err := buildSite(ctx, bundle, buildOptions{baseURL: base, release: *release})
	if err != nil {
		return err
	}
	if err := writeSite(ctx, outAbs, files); err != nil {
		return err
	}

	// The error is returned rather than ignored: an unreported success is
	// indistinguishable from a failure to whoever is reading the log.
	m := bundle.Manifest()
	_, err = fmt.Fprintf(os.Stdout, "wrote %d files for awa %s (%d documents) to %s\n",
		len(files), m.Provenance().Version, m.Corpus().DocumentCount, outAbs)
	return err
}
