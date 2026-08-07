// Command awa is the awa CLI entry point.
//
// main stays boring on purpose: it wires the process streams to the CLI router
// and exits with the returned code. All parsing, routing, and behavior live in
// internal packages so they remain testable without a real process.
package main

import (
	"context"
	"os"
	"os/signal"

	"awarer/internal/cli"
)

func main() {
	// One root context for the whole process, cancelled on the first OS
	// interrupt (Ctrl+C / SIGINT). It threads through routing into every
	// long-running operation — scans, git, GC, store cursors, and child
	// processes — so cancellation is a structural property, not a best-effort
	// side effect of whichever subsystem happens to notice the signal first.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
