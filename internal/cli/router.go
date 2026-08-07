// Package cli parses argv and routes invocations to application command
// handlers. It owns the CLI contract — command names, global flags, usage text,
// and exit codes — and nothing about product behavior.
package cli

import (
	"context"
	"errors"
	"io"
	"strings"

	"awarer/internal/app"
	"awarer/internal/output"
)

// handler runs a command after argv has been parsed. Every registered command
// and subcommand has one: the registry names the shipped contract, so a missing
// handler is a construction defect a registry test rejects, not a command state.
type handler func(w *output.Writer, inv invocation) error

// operandKind is what a command does with the tokens after "--". It is a closed
// vocabulary rather than a bool because the published CLI contract has to say
// more than "operands are accepted": tokens after "--" are path filters for
// changes/diff and a whole wrapped command line for run, and documentation that
// conflates the two states syntax the parser does not implement.
//
// The zero value is operandNone, so a command that declares nothing rejects
// operands — the safe default for a new entry whose author has not thought about
// it yet.
type operandKind int

const (
	// operandNone rejects any token after "--" as a usage error.
	operandNone operandKind = iota
	// operandPathFilter consumes them as path filters that narrow the output.
	operandPathFilter
	// operandWrappedCommand consumes them as the wrapped command and its arguments.
	operandWrappedCommand
)

// accepts reports whether tokens after "--" are consumed rather than rejected.
func (k operandKind) accepts() bool { return k != operandNone }

// String is the stable token the CLI reference publishes. Like ExitCode.refName it
// keeps a default for safety, but no shipped command may reach it — a test pins
// that every registry entry names a real kind.
func (k operandKind) String() string {
	switch k {
	case operandNone:
		return "none"
	case operandPathFilter:
		return "path-filter"
	case operandWrappedCommand:
		return "wrapped-command"
	default:
		return "unknown"
	}
}

// command is the single definition of a top-level command: its name, its help
// summary, and its handler. Keeping all of these together is the one source of
// truth — adding a command is one entry, not edits spread across several tables.
type command struct {
	name    string
	summary string
	run     handler
	// operands says what this command does with tokens after "--". route enforces
	// the rejecting case centrally, and the CLI reference publishes the kind, so
	// the terminator's contract is stated once and documented exactly.
	operands operandKind
	// capabilities are the global options this command acts on. route validates
	// against them centrally, and writeUsage renders help from them, so the set
	// is the single source for both — a command cannot accept a flag in help
	// that the validator rejects, or vice versa.
	capabilities []capability
	// help is the authored command-specific "--help" body. Every implemented
	// command has one; it is rendered by writeCommandHelp.
	help commandHelp
	// ownsHelp marks a command whose handler renders its own "--help" because it
	// owns subcommands (config) or a wrapped-child boundary (run). For these,
	// route delegates to the handler instead of rendering writeCommandHelp.
	ownsHelp bool
	// subcommands are the nested "<command> <name>" verbs this command owns, in
	// display order. Dispatch, "--help", and the CLI reference all read this one
	// slice, so a subcommand cannot be dispatched, advertised, and documented from
	// three tables that drift. Empty for a command with no subcommands.
	subcommands []subcommand
}

// subcommand is a nested "<command> <name>" verb: its metadata and handler.
// name/summary/caps drive dispatch and validation; help is the authored "--help"
// body (a zero value falls back to thin summary-derived help). Every subcommand
// has a real handler.
type subcommand struct {
	name    string
	summary string
	// caps is the set of global options this subcommand honors; the owning command
	// validates it per subcommand (requireAccepted), so a global the subcommand
	// ignores is a loud usage error rather than a silent no-op, and "supports
	// --json" in help derives from it.
	caps []capability
	// operands says what this subcommand does with tokens after "--". A
	// subcommand does NOT inherit its parent's kind: "awa run" takes a wrapped
	// command line, but "awa run show" takes none, and route can only validate the
	// parent's union — so without this an operand aimed at run show would be
	// silently discarded. The zero value rejects, which is the safe default.
	operands operandKind
	help     commandHelp
	run      handler
}

// lookupSubcommand returns the subcommand named name within subs, or nil. It is
// the one place a "<command> <name>" token is resolved, for both dispatch and help.
func lookupSubcommand(subs []subcommand, name string) *subcommand {
	for i := range subs {
		if subs[i].name == name {
			return &subs[i]
		}
	}
	return nil
}

// dispatchSubcommand validates a resolved subcommand's global options against its
// own capability set and runs it. Sharing this keeps per-subcommand validation from
// being a rule each subcommand-owning command must remember to repeat: a global the
// subcommand does not honor (e.g. --trust-mode on "run ls", where the coarse
// route-level check only sees the parent's union capabilities) is a loud usage error
// rather than a silent no-op. Semantic conflicts a capability cannot model (e.g.
// --time vs --json) stay checked inside the handler.
func dispatchSubcommand(w *output.Writer, cmdName string, sc *subcommand, inv invocation) error {
	if err := requireAccepted(cmdName+" "+sc.name, inv.options, sc.caps...); err != nil {
		return err
	}
	// The parent's kind only gates whether operands reach the command at all; a
	// subcommand that does not read them must say so rather than ignore them.
	if !sc.operands.accepts() && len(inv.operands) > 0 {
		return usageErrorf("unexpected argument %q for %s %s", inv.operands[0], cmdName, sc.name)
	}
	return sc.run(w, inv)
}

// router holds the command registry for one invocation. It is built per call
// (see newRouter) so the CLI contract is never package-level mutable state.
type router struct {
	commands []command
}

// newRouter builds the command registry. Every command below is implemented and
// routes to a real handler; the registry is the single source of truth for command
// names, summaries, and the capabilities (root discovery, config loading,
// --json, --trust-mode) each one accepts.
func newRouter() *router {
	return &router{commands: []command{
		{name: "init", summary: "create a new .awa project in the current directory", run: runInit, capabilities: []capability{capRoot, capJSON},
			help: commandHelp{
				usage: []string{"init [--profile <name>] [--root <path>]"},
				long:  "Create a .awa project in the current directory (or --root), always with the awa-owned .awa/.gitignore guard so local evidence stays untracked. It never creates or edits the repository-root .gitignore. Refuses to clobber an existing project.",
				flags: []flagHelp{
					{"--profile <name>", "starter profile: default|strict", "default"},
				},
				examples: []string{"awa init", "awa init --profile strict"},
				topic:    "quickstart",
			}},
		{name: "status", summary: "show project status (default command)", run: runStatus, capabilities: []capability{capRoot, capJSON},
			help: commandHelp{
				usage:    []string{"status"},
				long:     "Show the bounded project dashboard: the latest checkpoint baseline, uncommitted drift, and reusable-run and store health. This is the default command when awa is run with no arguments.",
				examples: []string{"awa status", "awa status --json"},
				topic:    "status",
			}},
		{name: "checkpoint", summary: "record a checkpoint of the worktree", run: runCheckpoint, capabilities: []capability{capRoot, capConfig, capJSON, capTrustMode},
			help: commandHelp{
				usage: []string{"checkpoint [-m <message>]"},
				long:  "Record a checkpoint of the current worktree so later changes/diff can compare against it. Give a specific message for humans; the printed id is what addresses the checkpoint later.",
				flags: []flagHelp{
					{"-m, --message <msg>", "checkpoint message", ""},
				},
				examples: []string{`awa checkpoint -m "before refactor"`, `awa checkpoint -m "review start" --json`},
				topic:    "checkpoints",
			}},
		{name: "log", summary: "list recorded checkpoints", run: runLog, capabilities: []capability{capRoot, capJSON},
			help: commandHelp{
				usage: []string{"log [-n <count>] [--all] [--oneline] [--time <mode>]"},
				long:  "List recorded checkpoints, newest first. --all includes the run timeline; --oneline is a compact one-line-per-entry form.",
				flags: []flagHelp{
					{"-n, --limit <count>", "show at most <count> entries", ""},
					{"-1", "shorthand for --limit 1", ""},
					{"--all", "include the run timeline, not just checkpoints", ""},
					{"--oneline", "compact one-line-per-entry output (human only)", ""},
					{"--time <mode>", "time display: relative|utc|local (human only)", "[ui].time"},
				},
				examples: []string{"awa log -n 5", "awa log --all --oneline"},
				topic:    "checkpoints",
			}},
		{name: "changes", summary: "summarize changes between two states", run: runChanges, operands: operandPathFilter, capabilities: []capability{capRoot, capConfig, capJSON, capTrustMode},
			help: commandHelp{
				usage: []string{"changes [range] [-- path...] [--stat|--name-only] [--no-renames]"},
				long:  "Summarize the change set between two states (default latest..now). --stat and --name-only are human output modes and cannot be combined with --json (the JSON already carries the summary and paths).",
				flags: []flagHelp{
					{"--stat", "one-line change-count summary (human only)", ""},
					{"--name-only", "print only changed paths (human only)", ""},
					{"--no-renames", "disable rename detection", ""},
				},
				examples: []string{"awa changes", "awa changes <checkpoint-id>..now", "awa changes -- src/"},
				topic:    "diff",
			}},
		{name: "diff", summary: "show a diff between two states", run: runDiff, operands: operandPathFilter, capabilities: []capability{capRoot, capConfig, capJSON, capTrustMode},
			help: commandHelp{
				usage: []string{"diff [range] [-- path...] [--context <n>] [--stat] [--no-renames]"},
				long:  "Show a content diff between two states (default latest..now), with clear notices for binary, hash-only, and unavailable content. --stat is a human output mode and cannot be combined with --json.",
				flags: []flagHelp{
					// The key is [checkpoint].diff_context, not [diff].context: [diff] holds
					// only algorithm. Naming a key that does not exist sends a reader to
					// write an invalid layer, which then fails as a config error.
					{"--context <n>", "lines of context around each change", "[checkpoint].diff_context"},
					{"--stat", "one-line change-count summary (human only)", ""},
					{"--no-renames", "disable rename detection", ""},
					{"--algorithm <name>", "select the diff engine (histogram|myers)", "[diff].algorithm"},
				},
				examples: []string{"awa diff", "awa diff <checkpoint-id>..now -- src/"},
				topic:    "diff",
			}},
		{name: "restore", summary: "restore selected paths from an immutable state", run: runRestore, operands: operandPathFilter, capabilities: []capability{capRoot, capConfig, capJSON, capTrustMode},
			help: commandHelp{
				usage: []string{
					"restore <state-ref> -- <path...>",
					"restore [--dry-run|--apply] <state-ref> -- <path...>",
					"restore [--dry-run|--apply] --all <state-ref>",
				},
				long: "Restore selected worktree paths from one immutable stored state: a checkpoint, a recorded run observation (run:<id>:before|after), or a previous restore's recovery observation (restore:<id>:before). Preview is the default and changes nothing; --dry-run is the explicit spelling of that same mode. Mutation requires --apply plus an explicit selection — one or more paths, or --all. When the evidence is incomplete restore refuses rather than overwriting; there is no force option. Before its first write an apply records an immutable recovery observation you can restore the previous state from.",
				flags: []flagHelp{
					{"--apply", "perform the restore (the only mutating mode)", ""},
					{"--dry-run", "preview explicitly; identical to the default", ""},
					{"--all", "select all proven restorable scope instead of paths", ""},
				},
				examples: []string{
					"awa restore <checkpoint-id> -- generated/client",
					"awa restore --apply <checkpoint-id> -- generated/client",
					"awa restore --apply --all <checkpoint-id>",
					"awa restore --apply restore:<operation-id>:before -- generated/client",
				},
				topic: "restore",
			}},
		{name: "run", summary: "run a command with caching and supervised history", run: runRun, operands: operandWrappedCommand, capabilities: []capability{capRoot, capConfig, capJSON, capTrustMode}, ownsHelp: true, help: runCmdHelp, subcommands: runSubcommands},
		{name: "gc", summary: "garbage-collect unreferenced state", run: runGc, capabilities: []capability{capRoot, capConfig, capJSON},
			help: commandHelp{
				usage: []string{"gc [--dry-run] [--committed] [--keep-last <n>] [--older-than <dur>]", "gc [--runs-only|--checkpoints-only|--blobs-only]"},
				long:  "Reclaim unreferenced checkpoints, runs, and blobs conservatively. --dry-run shows the plan without deleting. Deletion is all-or-nothing: any blocked candidate suppresses the whole destructive pass. An active or unknown writer lock blocks it and still exits 0 (try again later); storage corruption blocks it and exits 5; a second gc holding the collector lease exits 6 after the lock timeout.",
				flags: []flagHelp{
					{"--dry-run", "show the plan without deleting anything", ""},
					{"--committed", "also reclaim state made redundant by git commits", ""},
					{"--keep-last <n>", "retain the most recent <n> checkpoints", "policy default"},
					{"--older-than <dur>", "only reclaim entries older than <dur> (e.g. 720h)", ""},
					{"--runs-only", "restrict collection to the run cache", ""},
					{"--checkpoints-only", "restrict collection to checkpoints", ""},
					{"--blobs-only", "restrict collection to the blob store", ""},
				},
				examples: []string{"awa gc --dry-run", "awa gc --keep-last 20"},
				topic:    "gc",
			}},
		{name: "doctor", summary: "diagnose project and store health", run: runDoctor, capabilities: []capability{capRoot, capConfig, capJSON, capTrustMode},
			help: commandHelp{
				usage: []string{"doctor [--repair] [--strict]"},
				long:  "Inspect durable .awa/ state and print a health verdict plus one line per finding, including local evidence/privacy findings. --repair restores safe, awa-owned state (e.g. the .awa/.gitignore guard).",
				flags: []flagHelp{
					{"--repair", "restore safe awa-owned state where possible", ""},
				},
				exitNote: "exit code: the diagnosis, not an awa failure. Healthy or warnings-only exits 0; unrepaired errors or unrepaired repairable findings exit 5.",
				examples: []string{"awa doctor", "awa doctor --repair"},
				topic:    "doctor",
			}},
		{name: "config", summary: "inspect or edit configuration", run: runConfig, capabilities: configCapabilities(), ownsHelp: true, help: configCmdHelp, subcommands: configSubcommands},
		{name: "state", summary: "resolve and compare state for the external provider", run: runState, capabilities: stateCapabilities(), ownsHelp: true, help: stateCmdHelp, subcommands: stateSubcommands},
		{name: "docs", summary: "export the documentation this binary carries", run: runDocs, capabilities: docsCapabilities(), ownsHelp: true, help: docsCmdHelp, subcommands: docsSubcommands()},
		{name: "help", summary: "show operational help for a topic", run: runHelp,
			help: commandHelp{
				usage:    []string{"help [topic]"},
				long:     "Show operational, workflow-oriented help. 'awa help topics' lists every topic. For terse per-command syntax, use 'awa <command> --help'.",
				examples: []string{"awa help topics", "awa help agents", "awa help platform"},
			}},
		{name: "version", summary: "print version and build information", run: runVersion, capabilities: []capability{capJSON},
			help: commandHelp{
				usage:    []string{"version [--json]"},
				long:     "Print the awa version and build information.",
				examples: []string{"awa version"},
				topic:    "install",
			}},
	}}
}

// Run parses argv, routes to a command, and returns the process exit code.
// stdout carries payload; stderr carries usage and diagnostics. It never
// panics on bad input.
// ctx is the root cancellable context (cancelled on SIGINT); when it is
// cancelled mid-command Run reports the interrupt exit code.
func Run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	w := output.New(stdout, stderr)

	err := newRouter().route(ctx, argv, w)

	// A wrapped command result: the command ran (or a cached result was replayed)
	// and its exit code is authoritative — unless writing awa's own payload (e.g.
	// the JSON envelope) failed, which is an awa error, not the command's result.
	if ce, ok := err.(*commandExit); ok {
		if werr := w.Err(); werr != nil {
			w.Error(werr.Error())
			return int(ExitGenericError)
		}
		return ce.code
	}

	if err == nil {
		// A command can succeed yet fail to flush its payload to a broken
		// stdout. The Writer is the single channel every command writes
		// through, so surface that failure once here rather than making each
		// handler remember to return w.Err().
		err = w.Err()
	}
	if err == nil {
		return int(ExitSuccess)
	}

	// Cancellation through the root context (Ctrl+C / SIGINT) is not a product
	// failure: report it once with the interrupt code rather than dressing it up
	// as a generic or storage error. Checked before codedError so a cancelled
	// scan/store read that surfaces the raw context error maps here.
	if errors.Is(err, context.Canceled) {
		w.Error("interrupted")
		return int(ExitInterrupted)
	}

	if ce, ok := err.(*codedError); ok {
		w.Error(ce.Error())
		return int(ce.Code())
	}
	// Defensive: an unclassified error is a programmer error here, but still
	// fail loud with a non-zero code rather than swallow it.
	w.Error(err.Error())
	return int(ExitGenericError)
}

// route resolves the invocation and dispatches it. A nil result means the
// command succeeded and has already written its output; a non-nil result is a
// codedError carrying the exit code to report.
func (r *router) route(ctx context.Context, argv []string, w *output.Writer) error {
	inv, err := parse(argv)
	if err != nil {
		return err
	}
	// The root cancellable context rides on the invocation so every handler
	// reads inv.ctx instead of minting its own context.Background().
	inv.ctx = ctx

	// Bare "--help" (no command named) shows top-level usage on stdout. A command
	// named alongside --help gets command-specific help below instead.
	if inv.options.Help && inv.command == "" && len(inv.operands) == 0 {
		r.writeUsage(w)
		return nil
	}

	if inv.command == "" {
		// Tokens with no command to own them — e.g. "awa -- version", where
		// "--" terminates options before any command was named — are malformed,
		// not a bare invocation.
		if len(inv.args) > 0 {
			return usageErrorf("expected a command before %q\nrun 'awa --help' to see available commands", inv.args[0])
		}
		if len(inv.operands) > 0 {
			return usageErrorf("expected a command before %q\nrun 'awa --help' to see available commands", inv.operands[0])
		}
		// Bare invocation defaults to status ("awa" with no arguments shows
		// project status). Route it as status so it gets the
		// same validation as an explicit "awa status", rather than a bypass.
		inv.command = "status"
	}

	cmd := r.lookup(inv.command)
	if cmd == nil {
		// An unknown command combined with --help still teaches the available
		// commands rather than erroring, so "awa bogus --help" shows top-level usage.
		if inv.options.Help {
			r.writeUsage(w)
			return nil
		}
		return usageErrorf("unknown command %q\nrun 'awa --help' to see available commands", inv.command)
	}
	// Command-specific --help. A command that owns subcommands or a wrapped-child
	// boundary (config, run) renders its own help from the handler; every other
	// command gets the registry-driven page. Help does no project or lock work, so
	// it runs outside an initialized project like the operational help does.
	if inv.options.Help {
		if cmd.ownsHelp {
			return cmd.run(w, inv)
		}
		writeCommandHelp(w, cmd)
		return nil
	}
	// Enforce the "--" contract centrally: a command that does not consume
	// operands must not silently ignore tokens given after the terminator.
	if !cmd.operands.accepts() && len(inv.operands) > 0 {
		return usageErrorf("unexpected argument %q for %s", inv.operands[0], cmd.name)
	}
	// Reject global options the command does not act on from the one place that
	// knows its capabilities, so no handler has to remember to check.
	if err := requireAccepted(cmd.name, inv.options, cmd.capabilities...); err != nil {
		return err
	}
	return cmd.run(w, inv)
}

// lookup resolves a registered command name to its command, or nil if unknown.
// A command name is the only spelling the parser accepts.
func (r *router) lookup(name string) *command {
	for i := range r.commands {
		c := &r.commands[i]
		if c.name == name {
			return c
		}
	}
	return nil
}

// runVersion handles "awa version". It acts only on --json (enforced centrally
// from the command's capabilities) and takes no positional arguments.
func runVersion(w *output.Writer, inv invocation) error {
	if len(inv.args) > 0 {
		arg := inv.args[0]
		if strings.HasPrefix(arg, "-") {
			return usageErrorf("unknown flag %q for version", arg)
		}
		return usageErrorf("unexpected argument %q for version", arg)
	}
	if err := app.Version(w, inv.options.JSON); err != nil {
		return genericErrorf("%v", err)
	}
	return nil
}
