package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"awarer/internal/app"
	"awarer/internal/app/effectobserve"
	apprun "awarer/internal/app/run"
	"awarer/internal/app/scanner"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/process"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/output"
	"awarer/internal/output/inspect"
)

// runCmdHelp is the command-specific help for "awa run", shared by the registry
// entry and the wrapper-help path (awa run --help) so they cannot drift.
var runCmdHelp = commandHelp{
	usage: []string{"run [run flags] -- <command> [args...]", "run <subcommand> [flags]   (ls, log, show, rm, explain)"},
	long:  "Run a command through the deterministic run cache: a cache hit replays stored stdout/stderr/exit; a miss executes, observes, and (policy permitting) records a reusable entry. Prefer the explicit -- form so awa flags never mix with the wrapped command's flags.",
	flags: []flagHelp{
		{"--display <mode>", "terminal display: full|summary|tail:<n>|none", "full"},
		{"--record", "execute and record evidence, never a reusable hit", ""},
		{"--refresh", "ignore any cache hit and write a fresh entry", ""},
		{"--no-cache", "do not read or write the cache", ""},
		{"--no-cache-failures", "do not cache a failing command", ""},
		{"--scope <path>", "replace the run input scope", ""},
		{"--include <path>", "add an input path to the scope", ""},
		{"--exclude <path>", "remove an input path from the scope", ""},
		{"--cwd <path>", "run the command in this directory (inside the root)", ""},
		{"--allow-skipped-inputs", "allow a cache hit when some inputs were skipped", ""},
		{"--allow-tty", "attach the terminal to the child; forces non-cacheable", ""},
	},
	exitNote: "exit code: awa run returns the wrapped command's own exit code after a hit or a miss. Because that can overlap awa's 1-6 codes, use --json (run.exit_origin) or the run log to tell an awa-origin failure from a child result.",
	examples: []string{"awa run --display tail:200 -- go test ./...", "awa run -- pytest", "awa run ls --near"},
	topic:    "run",
}

// runSubcommands are the reserved "awa run <name>" verbs (ls, log, show, rm,
// explain). It is the single source for dispatch and help, attached to the run
// command's registry entry (see newRouter) so help and the CLI reference read the
// same slice runRun dispatches from.
//
// Only ls and explain honor --trust-mode/--strict: both resolve the run cache
// decision against the current state (ls the reusable-now set, explain the modeled
// hit/miss) through loadProjectConfig, which applies the trust override — exactly
// as "awa run" does. log/show/rm read stored history and never consult the trust
// policy, so they reject it rather than accept-and-ignore it.
var runSubcommands = []subcommand{
	{name: "ls", run: runRunLs, summary: "list reusable runs for the current state", caps: []capability{capRoot, capConfig, capJSON, capTrustMode}, help: commandHelp{
		usage: []string{"run ls [--all] [--near] [-n <count>] [--command <substr>] [--time <mode>]"},
		long:  "List runs reusable for the current project state (a cache-hit candidate now). --all also lists non-reusable runs with reasons; --near adds the nearest near-miss for each.",
		flags: []flagHelp{
			{"--all", "also list non-reusable runs, each with a reason", ""},
			{"--near", "add the nearest near-miss section", ""},
			{"-n, --limit <count>", "show at most <count> entries", ""},
			{"--command <substr>", "filter to runs whose command contains <substr>", ""},
			{"--time <mode>", "time display: relative|utc|local (human only)", "[ui].time"},
		},
		examples: []string{"awa run ls", "awa run ls --near --json"},
		topic:    "run",
	}},
	{name: "log", run: runRunLog, summary: "list recorded runs, newest first", caps: []capability{capRoot, capConfig, capJSON}, help: commandHelp{
		usage: []string{"run log [-n <count>] [--command <substr>] [--time <mode>]"},
		long:  "List recorded runs in chronological order, newest first, including non-reusable and record-only history.",
		flags: []flagHelp{
			{"-n, --limit <count>", "show at most <count> entries", ""},
			{"--command <substr>", "filter to runs whose command contains <substr>", ""},
			{"--time <mode>", "time display: relative|utc|local (human only)", "[ui].time"},
		},
		examples: []string{"awa run log -n 10"},
		topic:    "inspect",
	}},
	{name: "show", run: runRunShow, summary: "show one run's metadata and stored output", caps: []capability{capRoot, capConfig, capJSON}, help: commandHelp{
		usage: []string{"run show <id>|--last [--meta|--stdout|--stderr] [--tail <n>] [--grep <re>]"},
		long:  "Show one run's metadata and, on request, its stored output. --stdout/--stderr write one raw stream (pipe-safe); --tail/--grep filter it. Exactly one of an id or --last selects the run.",
		flags: []flagHelp{
			{"--last", "select the most recent readable run", ""},
			{"--meta", "metadata only (no output)", ""},
			{"--stdout", "write the stored stdout stream", ""},
			{"--stderr", "write the stored stderr stream", ""},
			{"--tail <n>", "only the last <n> lines of the selected output", ""},
			{"--grep <re>", "only output lines matching <re>", ""},
			// run show is stored-only and deliberately does not consult [ui].time, so a
			// malformed config layer cannot block evidence inspection. Its default is
			// therefore the built-in one, not the configured preference — naming
			// [ui].time here would promise a configurability this surface does not have.
			{"--time <mode>", "time display: relative|utc|local (human only; [ui].time is not read here)", "relative"},
		},
		examples: []string{"awa run show --last --tail 500", "awa run show <id> --stdout"},
		topic:    "inspect",
	}},
	{name: "rm", run: runRunRm, summary: "delete stored runs by id or filter", caps: []capability{capRoot, capConfig, capJSON}, help: commandHelp{
		usage: []string{"run rm <id>... | (--command <substr> | --older-than <dur>) [--dry-run]"},
		long:  "Delete stored runs, by explicit id(s) or by filter. Ids and filters are mutually exclusive. --dry-run reports what would be removed.",
		flags: []flagHelp{
			{"--command <substr>", "delete runs whose command contains <substr>", ""},
			{"--older-than <dur>", "delete runs older than <dur> (e.g. 720h)", ""},
			{"--dry-run", "report what would be removed without deleting", ""},
		},
		examples: []string{"awa run rm --older-than 720h --dry-run"},
		topic:    "inspect",
	}},
	{name: "explain", run: runRunExplain, summary: "explain how the run cache would behave", caps: []capability{capRoot, capConfig, capJSON, capTrustMode}, operands: operandWrappedCommand, help: commandHelp{
		usage: []string{"run explain -- <command> [args...]", "run explain --last", "run explain --from-run <id> --to-now"},
		long:  "Compute the same key and cacheability decision awa run would, without executing or recording. Reports the outcome (hit/miss/uncached/disabled), the reason, and the nearest candidates. It is a diagnostic and always exits 0. In command mode (-- <command>) it accepts the same input/policy flags as run; the stored-run modes (--last/--from-run) take no other flags.",
		flags: []flagHelp{
			{"--last", "explain the most recent recorded run", ""},
			{"--from-run <id>", "explain a stored run's inputs against now (needs --to-now)", ""},
			{"--to-now", "compare the selected run's inputs to the current state", ""},
			{"--scope <path>", "replace the run input scope (command mode)", ""},
			{"--include <path>", "add an input path to the scope (command mode)", ""},
			{"--exclude <path>", "remove an input path from the scope (command mode)", ""},
			{"--cwd <path>", "resolve the key as if run in this directory (command mode)", ""},
			{"--refresh", "model run's --refresh policy (command mode)", ""},
			{"--no-cache", "model run's --no-cache policy (command mode)", ""},
			{"--no-cache-failures", "model run's --no-cache-failures policy (command mode)", ""},
			{"--allow-skipped-inputs", "model run's --allow-skipped-inputs policy (command mode)", ""},
			{"--allow-tty", "model run's --allow-tty stdin (command mode)", ""},
		},
		examples: []string{"awa run explain -- go test ./...", "awa run explain --last"},
		topic:    "run",
	}},
}

// runRun handles "awa run". It either dispatches a reserved subcommand (ls, log,
// show, rm, explain) or runs a wrapped command through the cache. The command is the
// operands after "--", or — in the short form — the first non-flag token of the
// arguments and everything after it.
func runRun(w *output.Writer, inv invocation) error {
	// --help is owned by run (route delegates here) because run has subcommands and a
	// wrapped-child boundary. Handle it before any project or lock work.
	if inv.options.Help {
		return runWrapperHelp(w, inv)
	}
	// A reserved subcommand is named by the first argument, before any "--". This
	// holds even when operands follow (e.g. "run explain -- <cmd>"), since explain
	// takes a command after "--". Running an executable that shares a subcommand's
	// name still works through "run -- <name>", where the name lands in operands and
	// inv.args is empty.
	if len(inv.args) > 0 {
		if sc := lookupSubcommand(runSubcommands, inv.args[0]); sc != nil {
			return dispatchSubcommand(w, "run", sc, inv)
		}
	}

	flags, stop, err := parseRunFlags(inv.args)
	if err != nil {
		return err
	}

	var argv []string
	if len(inv.operands) > 0 {
		// The command is literal after "--"; everything before it must be a
		// run-local flag, never a stray token.
		if stop != len(inv.args) {
			return usageErrorf("unexpected argument %q before --; run-local flags must precede --", inv.args[stop])
		}
		argv = inv.operands
	} else {
		// Short form: the child command begins at inv.args[stop]. An awa-global flag
		// consumed after that boundary is ambiguous — it could be meant for awa or for
		// the child — so fail loudly and point at "--" rather than silently steal it.
		if stop < len(inv.args) {
			if err := ambiguousTailGlobal(inv.tail, stop); err != nil {
				return err
			}
		}
		argv = inv.args[stop:]
	}
	if len(argv) == 0 {
		return usageErrorf("run requires a command\nusage: awa run -- <command> [args...]")
	}

	return executeRun(w, inv, flags, argv)
}

// runWrapperHelp renders "--help" for the run wrapper. A reserved subcommand token
// shows that subcommand's help; a wrapped short-form command with --help after the
// child boundary is the same ambiguity as any other stolen global (fix with --);
// otherwise it shows run's own help. It does no project or lock work.
func runWrapperHelp(w *output.Writer, inv invocation) error {
	if len(inv.args) > 0 {
		if sc := lookupSubcommand(runSubcommands, inv.args[0]); sc != nil {
			// "explain" also wraps a child command in its short form, so a --help stolen
			// from after that child ("run explain mytool --help") is ambiguous exactly
			// like "run mytool --help" — apply the same boundary check before showing
			// explain's own help. The non-wrapping subcommands (ls/log/show/rm) take no
			// child, so a trailing --help is unambiguously theirs. Flag-parse errors stay
			// lenient here: --help wins, and the execute path surfaces them otherwise.
			if sc.name == "explain" {
				rest := inv.args[1:]
				if _, stop, err := parseExplainFlags(rest); err == nil {
					if aerr := explainChildAmbiguity(inv, rest, stop); aerr != nil {
						return aerr
					}
				}
			}
			writeHelpBody(w, "awa run "+sc.name, subcommandHelp("run", *sc), hasCap(sc.caps, capJSON))
			return nil
		}
		// A non-reserved first token is a wrapped short-form command. --help was
		// consumed as an awa global; if it landed after the child boundary it is
		// ambiguous, exactly like "awa run mytool --json".
		_, stop, err := parseRunFlags(inv.args)
		if err != nil {
			return err
		}
		if stop < len(inv.args) {
			if err := ambiguousTailGlobal(inv.tail, stop); err != nil {
				return err
			}
		}
	}
	writeHelpBody(w, "awa run", runCmdHelp, true)
	return nil
}

// ambiguousTailGlobal reports a usage error when an awa-global flag appears at or after the
// wrapped child's first token in a short-form run. It reads the parser's typed tail
// classification rather than re-parsing argv: it counts the command-arg tokens (which map
// one-to-one to inv.args) so its running count lines up with childArgsPos — the index in
// inv.args of the child's first token — and flags any global whose position lands past it.
// Such a global sits inside the child's region, where awa cannot tell whether it was meant
// for awa or the command; the fix is always to move awa flags before the command or use
// "--", so the message names both.
func ambiguousTailGlobal(tail []tailToken, childArgsPos int) error {
	args := 0
	for _, tok := range tail {
		if tok.class == tailGlobal {
			if args > childArgsPos {
				return usageErrorf(
					"ambiguous flag %q after the command: awa cannot tell whether it is meant for awa or for the command\n"+
						"put awa flags before the command, or use -- to pass everything after it to the command:\n"+
						"  awa run -- <command> [args...]", tok.name)
			}
			continue
		}
		args++
	}
	return nil
}

// runFlags are the parsed run-local options.
type runFlags struct {
	scope           []string
	scopeSet        bool
	include         []string
	exclude         []string
	cwd             string
	cwdSet          bool
	refresh         bool
	noCache         bool
	noCacheFailures bool
	allowSkipped    bool
	allowTTY        bool
	record          bool
	// display is the per-invocation terminal display mode (--display). It defaults
	// to full and never affects capture, the cache key, or the exit code.
	display inspect.DisplayMode
}

// parseRunFlags consumes the leading run-local flags of args and returns them
// along with the index of the first token that is not a run-local flag — the
// start of the command in the short form, or len(args) when all tokens were
// flags. An unknown flag before the command begins is a usage error pointing at
// "--".
func parseRunFlags(args []string) (runFlags, int, error) {
	f := runFlags{display: inspect.DefaultDisplayMode()}
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		// --display and --record are run-only: they shape this invocation's terminal
		// output and history recording, which have no meaning for the dry-run
		// "run explain" (it never executes or records). They are handled here rather
		// than in the applyRunFlag set shared with explain, so explain rejects them
		// loudly instead of accepting them as a silent no-op.
		if name == "--display" {
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return runFlags{}, 0, err
			}
			mode, err := inspect.ParseDisplayMode(v)
			if err != nil {
				return runFlags{}, 0, usageErrorf("%v", err)
			}
			f.display = mode
			continue
		}
		if name == "--record" {
			if err := noValue(name, hasValue); err != nil {
				return runFlags{}, 0, err
			}
			f.record = true
			continue
		}
		handled, err := applyRunFlag(&f, name, value, hasValue, args, &i)
		if err != nil {
			return runFlags{}, 0, err
		}
		if handled {
			continue
		}
		// Not a run-local flag. An unknown flag has no owner here — the command
		// must start with an executable, not a dash — so point the user at "--".
		if strings.HasPrefix(tok, "-") && tok != "-" {
			return runFlags{}, 0, usageErrorf("unknown flag %q for run; use -- to pass it to the command", tok)
		}
		// A bare token begins the command (short form). Back up to include it.
		return f, i - 1, nil
	}
	return f, i, nil
}

// applyRunFlag consumes one shared run-local flag into f, reading a separate value
// token (advancing i) for flags that take one. It returns handled=false when name is
// not one of these flags, so the caller can fall back to its own handling. This is
// the single definition of the input/policy flags shared by "run" and command-mode
// "run explain"; the execution-only flags (--display, --record) are handled by
// parseRunFlags directly so explain does not accept them.
func applyRunFlag(f *runFlags, name, value string, hasValue bool, args []string, i *int) (bool, error) {
	switch name {
	case "--scope":
		v, err := requireValue(name, value, hasValue, args, i)
		if err != nil {
			return false, err
		}
		f.scope = append(f.scope, v)
		f.scopeSet = true
	case "--include":
		v, err := requireValue(name, value, hasValue, args, i)
		if err != nil {
			return false, err
		}
		f.include = append(f.include, v)
	case "--exclude":
		v, err := requireValue(name, value, hasValue, args, i)
		if err != nil {
			return false, err
		}
		f.exclude = append(f.exclude, v)
	case "--cwd":
		v, err := requireValue(name, value, hasValue, args, i)
		if err != nil {
			return false, err
		}
		f.cwd = v
		f.cwdSet = true
	case "--refresh":
		if err := noValue(name, hasValue); err != nil {
			return false, err
		}
		f.refresh = true
	case "--no-cache":
		if err := noValue(name, hasValue); err != nil {
			return false, err
		}
		f.noCache = true
	case "--no-cache-failures":
		if err := noValue(name, hasValue); err != nil {
			return false, err
		}
		f.noCacheFailures = true
	case "--allow-skipped-inputs":
		if err := noValue(name, hasValue); err != nil {
			return false, err
		}
		f.allowSkipped = true
	case "--allow-tty":
		if err := noValue(name, hasValue); err != nil {
			return false, err
		}
		f.allowTTY = true
	default:
		return false, nil
	}
	return true, nil
}

// hasRunLocalOverrides reports whether any shared input/policy flag that only makes
// sense for a command-mode explain was set. The stored-run modes (--last/--from-run)
// reconstruct their facts from the entry and the current config, so these flags would
// be silently ignored; rejecting them keeps the contract honest. (--record/--display
// are run-only and explain never parses them, so they cannot reach here.)
func hasRunLocalOverrides(f runFlags) bool {
	return f.scopeSet || f.cwdSet || len(f.include) > 0 || len(f.exclude) > 0 ||
		f.refresh || f.noCache || f.noCacheFailures || f.allowSkipped || f.allowTTY
}

// executeRun resolves the project, builds the run request, runs it through the
// cache, and renders the outcome. It returns the wrapped command's exit code via
// commandExit so awa's process exit matches the command's.
func executeRun(w *output.Writer, inv invocation, flags runFlags, argv []string) error {
	if flags.noCache && flags.refresh {
		return usageErrorf("--no-cache and --refresh cannot be combined")
	}
	// --refresh means "ignore any hit but publish a fresh reusable result", which is
	// the opposite of --record's "never publish a reusable result". Combining them is
	// contradictory, so reject it rather than silently letting record-only win.
	if flags.record && flags.refresh {
		return usageErrorf("--record and --refresh cannot be combined")
	}

	proj, layout, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}
	// A run writes captured output and metadata into .awa/; keep the git guard intact
	// before growing that state. Restoration is silent when the guard is only missing.
	if err := guardPreflight(w, proj, true); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return genericErrorf("determining current directory: %v", err)
	}

	execCWD, absDir, err := resolveExecCWD(layout.Root(), cwd, flags)
	if err != nil {
		return err
	}

	overrides, err := resolveScopeOverrides(layout.Root(), cwd, flags)
	if err != nil {
		return err
	}

	stdinMode := detectStdinMode(flags.allowTTY)

	svc, cleanup, err := buildRunService(inv.invCtx(), layout, cfg, indexMutable)
	if err != nil {
		return err
	}
	defer cleanup()

	// Display is a per-invocation presentation concern, decided here at the CLI
	// boundary; capture and caching are unaffected. On a miss the child streams are
	// teed to these sinks while the store captures the full payload, so choosing a
	// bounded sink (a line ring) or none (io.Discard) never changes what is stored or
	// keyed. Command output is discarded from stdout in JSON mode (it is reported via
	// metadata), so a JSON envelope is never polluted by command bytes. Using the
	// Writer's streams — not the process globals — honors the streams passed to cli.Run.
	mode := flags.display
	outW, errW := w.Stdout(), w.Stderr()
	var ringOut, ringErr *inspect.LineRing
	switch {
	case inv.options.JSON:
		outW, errW = io.Discard, io.Discard
	case mode.Kind() == inspect.DisplayFull:
		// Keep the real terminal streams — traditional behavior.
	case mode.Kind() == inspect.DisplayTail:
		ringOut = inspect.NewLineRing(mode.Tail().Lines())
		ringErr = inspect.NewLineRing(mode.Tail().Lines())
		outW, errW = ringOut, ringErr
	case mode.Kind() == inspect.DisplaySummary:
		// Retain a bounded window to build the summary excerpt from on a miss.
		ringOut = inspect.NewLineRing(summaryRetainLines)
		ringErr = inspect.NewLineRing(summaryRetainLines)
		outW, errW = ringOut, ringErr
	default: // none
		outW, errW = io.Discard, io.Discard
	}

	res, err := svc.Run(inv.invCtx(), apprun.Request{
		Project:    proj,
		Config:     cfg,
		Argv:       argv,
		CWD:        execCWD,
		AbsDir:     absDir,
		Scope:      overrides,
		StdinMode:  stdinMode,
		TTYAllowed: flags.allowTTY,
		Policy: apprun.Policy{
			Refresh:            flags.refresh,
			NoCache:            flags.noCache,
			NoCacheFailures:    flags.noCacheFailures,
			AllowSkippedInputs: flags.allowSkipped,
			Record:             flags.record,
		},
		Stdout: outW,
		Stderr: errW,
	})
	if err != nil {
		return classifyRunError(err)
	}

	// The inline cache diagnosis is presentation, not correctness: a failure to build
	// it must never change what the run stored or its exit code, so a non-fatal error
	// degrades to "no diagnosis" with a note rather than failing the command.
	diag, derr := svc.DiagnoseRun(inv.invCtx(), res, cfg.Run.TTL)
	if derr != nil {
		diag = apprun.RunInlineDiagnosis{}
		if !inv.options.JSON {
			w.Diagnostic("note: could not diagnose cache miss: " + derr.Error())
		}
	}

	if inv.options.JSON {
		if err := w.JSON("run", runView(res, mode, diag)); err != nil {
			return genericErrorf("%v", err)
		}
		return &commandExit{code: res.ExitCode}
	}

	switch res.Status {
	case runcache.CacheHit:
		if err := renderHitDisplay(w, svc, mode, res); err != nil {
			return classifyRunError(err)
		}
	default:
		renderMissDisplay(w, mode, res, ringOut, ringErr)
	}
	emitRunFooter(w, res, diag)
	return &commandExit{code: res.ExitCode}
}

// emitRunFooter prints the single compact review footer after a real execution or a
// cache hit. It leads with the storage state (hit/stored/record-only/uncached) and
// the run's identity — id, cwd, exit, duration, and whether the result is reusable —
// then the inspection command, and, when the run mutated or is otherwise
// non-reusable history, the exact before/after compare command. All footer lines go
// to stderr as diagnostics so a piped stdout receives only the command's own bytes.
//
// The footer is presentation: it never changes the process exit code. If the summary
// cannot be built — an impossible-state guard tripping, which means a domain bug —
// it reports that as a diagnostic and prints nothing further, so the user still gets
// the command's real exit code rather than an awa error swallowing it.
func emitRunFooter(w *output.Writer, res apprun.Result, diag apprun.RunInlineDiagnosis) {
	sum, err := apprun.NewRunExecutionSummary(res)
	if err != nil {
		w.Diagnostic("warning: could not summarize run (internal): " + err.Error())
		return
	}

	reuse := "reusable"
	if !sum.Reusable {
		reuse = "not reusable"
		if sum.Reason != "" {
			reuse = fmt.Sprintf("not reusable (%s)", sum.Reason)
		}
	}
	// Header names the storage state and the reuse verdict; the aligned block below
	// carries identity and facts, so the footer reads like the run show meta and the
	// status dashboard rather than a fourth footer grammar.
	w.Diagnostic(fmt.Sprintf("awa run: %s, %s", sum.State, reuse))

	if !sum.RunID.IsZero() {
		w.Diagnostic(fmt.Sprintf("  run:      %s", sum.RunID.Short()))
		w.Diagnostic(fmt.Sprintf("  cwd:      %s", footerCWD(sum.CWD)))
	}
	w.Diagnostic(fmt.Sprintf("  exit:     %s", exitLabel(sum.Exit)))
	w.Diagnostic("  duration: " + durationLabel(sum.Duration))
	emitTruncationBanner(w, res.Execution.Output)
	if sum.Recorded && !sum.RunID.IsZero() {
		w.Diagnostic(fmt.Sprintf("  inspect:  awa run show %s (add --tail N for more)", sum.RunID.Short()))
	}
	if !sum.Recorded {
		w.Diagnostic("  note:     full output not stored (run not recorded); nothing to inspect later")
	}
	// A mutating or otherwise non-reusable recorded run is where a reviewer most needs
	// the exact pre/post compare command, so surface it explicitly. A clean reusable
	// run's before and after are identical, so the compare would be empty noise — skip
	// it there.
	if !sum.Reusable && sum.PrePostCompareAvailable() {
		if sum.Mutated {
			w.Diagnostic("  warning:  this run changed observed state; its result is not replayable")
		}
		w.Diagnostic("  compare:  awa changes " + prePostCompareRef(sum.RunID.Short()))
	}

	emitEffectDiagnosis(w, res.EffectDiagnosis)
	emitCacheDiagnosis(w, res, sum, diag)

	// Latency diagnostics come last: a slow-but-known cause (a huge watched effect root
	// that dominated state observation) with a safe next step, bounded to one or two
	// notes and already selected by the app layer. They stay quiet when nothing crossed
	// the interactive threshold.
	emitPerformanceNotes(w, res.Performance)
}

// emitTruncationBanner writes a per-stream notice whenever a run's stored output is
// incomplete because the command exceeded the capture limit. This is capture-limit
// truncation — the stored evidence itself is partial — and is deliberately distinct
// from display overflow ("earlier lines omitted"), which only means the current view
// is a window onto a complete payload. The banner names the stream and the exact
// omitted byte count so a full replay, a tail, or a grep of the stored payload can
// never be mistaken for the complete command output. All lines go to stderr so a
// piped stdout still receives only the command's bytes. It is a no-op when capture
// was disabled (out == nil) or nothing was truncated.
func emitTruncationBanner(w *output.Writer, out *apprun.Output) {
	if out == nil {
		return
	}
	// Two-space indent to sit inside the aligned footer block.
	if out.Stdout.Truncated {
		w.Diagnostic("  " + truncationNotice("stdout", out.Stdout.OmittedBytes))
	}
	if out.Stderr.Truncated {
		w.Diagnostic("  " + truncationNotice("stderr", out.Stderr.OmittedBytes))
	}
}

// truncationNotice is the single wording for a capture-limit truncation warning,
// shared by every surface that reports partial stored output (the run footer and
// run show) so the phrasing and omitted-byte count can never drift between them.
func truncationNotice(stream string, omitted int64) string {
	return fmt.Sprintf("truncated: stored %s is incomplete evidence — %d bytes omitted at the capture limit", stream, omitted)
}

// prePostCompareRef renders the `awa changes` range that diffs a run's recorded
// before/after observations from an already-chosen id token, so the ref grammar has a
// single source of truth. The caller picks the id representation its surface needs:
// the footer passes the short id for a scannable human line, while the machine-readable
// JSON suggestion passes the full id so its argv is an unambiguous invocation rather
// than a display-only prefix.
func prePostCompareRef(idToken string) string {
	return fmt.Sprintf("run:%s:before..run:%s:after", idToken, idToken)
}

// emitCacheDiagnosis appends the bounded inline cache diagnosis to the footer: for a
// clean miss (one that became reusable or was left uncached) with a nearest previous
// run, it names that run, why it is not reusable, a bounded changed-input sample, and
// at most one advisory hint. It always points at the deeper `run explain` command for
// any non-hit, and adds `run ls --near` when a nearest candidate is shown.
//
// It stays quiet where it would only repeat what the footer already said: a hit needs
// no diagnosis, and a mutating or record-only run already leads with its reason and
// (for a mutating run) the pre/post compare command, so the nearest-candidate block is
// suppressed there to avoid noise — only the deeper-command pointer remains.
func emitCacheDiagnosis(w *output.Writer, res apprun.Result, sum apprun.RunExecutionSummary, diag apprun.RunInlineDiagnosis) {
	if res.Status == runcache.CacheHit {
		return
	}

	// The nearest-candidate block is shown only for a plain miss — one that stored a
	// reusable entry or ran uncached — not for a mutating/record-only run whose own
	// reason and compare command already explain it.
	showBlock := diag.Nearest != nil &&
		(sum.State == apprun.RunStateStored || sum.State == apprun.RunStateUncached)

	if showBlock {
		n := diag.Nearest
		w.Diagnostic("  cache diagnosis:")
		reason := "not-reusable"
		if n.Reason != "" {
			reason = fmt.Sprintf("not-reusable(%s)", n.Reason)
		}
		w.Diagnostic(fmt.Sprintf("    nearest previous: %s %s", n.Entry.ID.Short(), reason))
		// One compact detail from the shared renderer, so this preview names the same
		// watched-state fact status and the deeper surfaces do — and stays silent on the
		// same rootless candidates they do, rather than paraphrasing the reason above.
		if detail := effectDetail(n.Effect); detail != "" {
			w.Diagnostic("    effect: " + detail)
		}
		if n.Changed != nil && len(n.Changed.Paths()) > 0 {
			w.Diagnostic("    changed inputs: " + changedInputsLabel(n.Changed))
		}
		// Advisory only: a hint suggests where a [run] exclude might belong; it never
		// implies the path is safe to ignore and never removes it from the sample above.
		if len(n.Hints) > 0 {
			w.Diagnostic("    hint: review [run] excludes near " + n.Hints[0].Suggestion)
		}
	}

	argv := res.Execution.KeyInput.Command.Argv
	if len(argv) > 0 {
		w.Diagnostic("  explain:  awa run explain -- " + strings.Join(argv, " "))
	}
	if showBlock {
		w.Diagnostic("  near:     awa run ls --near")
	}
}

// changedInputsLabel renders a bounded changed-path sample as a compact comma list
// with a "(+N more)" suffix when the sample is a truncated prefix of a larger set, so
// the footer names the drift without ever printing an unbounded path list.
func changedInputsLabel(s *runcache.ChangedPathSample) string {
	paths := s.Paths()
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, p.Path)
	}
	label := strings.Join(names, ", ")
	if s.Truncated() {
		label += fmt.Sprintf(" (+%d more)", s.Omitted())
	}
	return label
}

// footerCWD renders the run's execution cwd for the footer, showing the project root
// as "." rather than the empty string so the line always names a directory.
func footerCWD(cwd string) string {
	if cwd == "" {
		return "."
	}
	return cwd
}

// durationLabel renders a run duration to one decimal second, shared by the run
// footer and the summary view so they never drift.
func durationLabel(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// reuseToken returns the recorded run's reuse classification as a stable machine
// token for JSON, or the empty string when nothing was recorded (so the field is
// omitted rather than reporting a misleading default).
func reuseToken(res apprun.Result) string {
	if !res.Recorded {
		return ""
	}
	return res.Reuse.String()
}

// replayRun streams a hit entry's stored output to the user. Both payloads are
// opened — and thereby verified against their recorded hashes — before either is
// copied, so a corrupt or missing stderr fails the hit loudly instead of leaving
// the user with a half-replayed result.
//
// Each stream's bytes are replayed exactly, but the whole stdout is written before
// the whole stderr. A miss streams the two concurrently, so cross-stream
// interleaving is not preserved on a hit; preserving it would need a multiplexed,
// timestamped capture.
func replayRun(svc *apprun.Service, id runcache.RunID, stdout, stderr io.Writer) error {
	so, err := svc.OpenStdout(id)
	if err != nil {
		return err
	}
	defer func() { _ = so.Close() }()
	se, err := svc.OpenStderr(id)
	if err != nil {
		return err
	}
	defer func() { _ = se.Close() }()
	if _, err := io.Copy(stdout, so); err != nil {
		return err
	}
	if _, err := io.Copy(stderr, se); err != nil {
		return err
	}
	return nil
}

// Display rendering bounds. summaryRetainLines is the window kept while a miss
// streams, from which the summary excerpt is drawn; summaryExcerptMax caps how
// many lines a summary actually prints per stream.
const (
	summaryRetainLines = 200
	summaryExcerptMax  = 10
)

// builtinInterestingPattern selects the lines a summary highlights. It is a
// heuristic for human convenience only — a match (or the absence of one) is never
// proof of success or failure, which is why the wrapped command's exit code
// remains the authority.
var builtinInterestingPattern = regexp.MustCompile(`(?i)(passed|failed|\berror\b|\bwarning\b|panic|fatal|build succeeded)`)

// renderHitDisplay renders a cache hit's stored output according to the display
// mode. Full replays both payloads verbatim; none prints nothing; tail and
// summary read bounded views from the store so a large log is never loaded whole.
func renderHitDisplay(w *output.Writer, svc *apprun.Service, mode inspect.DisplayMode, res apprun.Result) error {
	switch mode.Kind() {
	case inspect.DisplayFull:
		if err := replayRun(svc, res.RunID, w.Stdout(), w.Stderr()); err != nil {
			return err
		}
	case inspect.DisplayNone:
		// Suppress child output; diagnostics and footer still print.
	case inspect.DisplayTail:
		outLines, outOv, err := tailStored(svc.OpenStdout, res.RunID, mode.Tail())
		if err != nil {
			return err
		}
		errLines, errOv, err := tailStored(svc.OpenStderr, res.RunID, mode.Tail())
		if err != nil {
			return err
		}
		emitTail(w, "stdout", w.Stdout(), outLines, outOv)
		emitTail(w, "stderr", w.Stderr(), errLines, errOv)
	case inspect.DisplaySummary:
		limit, err := inspect.NewTailLimit(summaryRetainLines)
		if err != nil {
			return err
		}
		outPool, _, err := tailStored(svc.OpenStdout, res.RunID, limit)
		if err != nil {
			return err
		}
		errPool, _, err := tailStored(svc.OpenStderr, res.RunID, limit)
		if err != nil {
			return err
		}
		renderSummary(w, outPool, errPool)
	}
	return nil
}

// renderMissDisplay renders a freshly executed run's output. Full output already
// streamed live, so full/none print nothing more here; tail and summary draw from
// the in-memory rings the live stream filled.
func renderMissDisplay(w *output.Writer, mode inspect.DisplayMode, res apprun.Result, ringOut, ringErr *inspect.LineRing) {
	switch mode.Kind() {
	case inspect.DisplayFull, inspect.DisplayNone:
		// Full already streamed; none stayed silent.
	case inspect.DisplayTail:
		emitTail(w, "stdout", w.Stdout(), ringOut.Lines(), ringOut.Overflowed())
		emitTail(w, "stderr", w.Stderr(), ringErr.Lines(), ringErr.Overflowed())
	case inspect.DisplaySummary:
		renderSummary(w, ringOut.Lines(), ringErr.Lines())
	}
}

// tailStored reads the last limit lines of one stored stream, opening and closing
// the verified reader. It never loads the whole payload into memory.
func tailStored(open streamOpener, id runcache.RunID, limit inspect.TailLimit) ([]string, bool, error) {
	rc, err := open(id)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rc.Close() }()
	return inspect.TailLines(rc, limit)
}

// emitTail prints a labeled tail: the header (and overflow note) goes to stderr as
// a diagnostic so a piped stdout receives only the tail bytes themselves.
func emitTail(w *output.Writer, label string, dst io.Writer, lines []string, overflow bool) {
	if len(lines) == 0 {
		w.Diagnostic(fmt.Sprintf("%s: (no output)", label))
		return
	}
	note := ""
	if overflow {
		note = " — earlier lines omitted"
	}
	w.Diagnostic(fmt.Sprintf("last %d line(s) of %s%s:", len(lines), label, note))
	for _, l := range lines {
		_, _ = io.WriteString(dst, l+"\n")
	}
}

// renderSummary prints the bounded, non-authoritative summary display to stderr: a
// header plus a short excerpt of each stream. It is the display view for
// --display summary; the run's identity, exit, duration, inspect command, and any
// capture-limit truncation are carried once by the footer (emitRunFooter), so this
// view does not repeat them. It is a terminal convenience: machine consumers should
// use --json or "run show".
func renderSummary(w *output.Writer, outPool, errPool []string) {
	// A display header, distinct from the footer's "awa run:" state line: it explains
	// why the terminal output is bounded. Each excerpt below carries its own "not a
	// complete log" caveat, so this line does not repeat it. Capture-limit truncation
	// is reported authoritatively (per stream, with byte counts) by the footer's
	// truncation banner, so it is not repeated here either.
	w.Diagnostic("summary view: full output hidden; excerpt below")
	emitExcerpt(w, "stdout", outPool)
	emitExcerpt(w, "stderr", errPool)
}

// emitExcerpt prints a small bounded excerpt of one stream for the summary view,
// preferring lines that match the built-in interesting patterns and otherwise
// falling back to the last lines. It is always labeled as an excerpt.
func emitExcerpt(w *output.Writer, label string, pool []string) {
	if len(pool) == 0 {
		return
	}
	var hits []string
	for _, l := range pool {
		if builtinInterestingPattern.MatchString(l) {
			hits = append(hits, l)
		}
	}
	excerpt, kind := hits, "matched"
	if len(excerpt) == 0 {
		excerpt, kind = pool, "last"
	}
	if len(excerpt) > summaryExcerptMax {
		excerpt = excerpt[len(excerpt)-summaryExcerptMax:]
	}
	w.Diagnostic(fmt.Sprintf("  %s (%s lines, excerpt — not a complete log):", label, kind))
	for _, l := range excerpt {
		w.Diagnostic("    " + l)
	}
}

// resolveExecCWD resolves the command's execution directory: --cwd relative to
// the caller's cwd, or the caller's cwd itself. It must stay within the project
// root, and is returned both as a root-relative ExecutionCWD (for the key) and as
// an absolute directory (for the process).
func resolveExecCWD(rootAbs, cwd string, flags runFlags) (runcache.ExecutionCWD, string, error) {
	target := cwd
	if flags.cwdSet {
		target = flags.cwd
		if !filepath.IsAbs(target) {
			target = filepath.Join(cwd, target)
		}
	}
	rel, err := projectRelPath(rootAbs, cwd, target)
	if err != nil {
		if flags.cwdSet {
			return runcache.ExecutionCWD{}, "", usageErrorf("--cwd %q is outside the project root", flags.cwd)
		}
		return runcache.ExecutionCWD{}, "", usageErrorf("current directory %s is outside the project root %s\nuse --cwd to choose a directory inside it", cwd, rootAbs)
	}
	execCWD, err := runcache.NewExecutionCWD(rel)
	if err != nil {
		return runcache.ExecutionCWD{}, "", usageErrorf("%v", err)
	}
	absDir := filepath.Join(rootAbs, filepath.FromSlash(rel))
	// The boundary check above is lexical, but the process runs in absDir with the OS
	// following any symlink component. A cwd reached through a link that points
	// outside the project would run the command outside the scanned tree — reading
	// unscanned data and caching a result that goes stale silently. Resolve both
	// sides and require the real cwd to stay within the real root.
	inside, err := dirWithinRoot(rootAbs, absDir)
	if err != nil {
		if flags.cwdSet {
			return runcache.ExecutionCWD{}, "", usageErrorf("--cwd %q is not a usable directory inside the project root: %v", flags.cwd, err)
		}
		return runcache.ExecutionCWD{}, "", usageErrorf("current directory %s cannot be resolved: %v", cwd, err)
	}
	if !inside {
		if flags.cwdSet {
			return runcache.ExecutionCWD{}, "", usageErrorf("--cwd %q resolves through a symlink to outside the project root", flags.cwd)
		}
		return runcache.ExecutionCWD{}, "", usageErrorf("current directory %s resolves through a symlink to outside the project root %s", cwd, rootAbs)
	}
	return execCWD, absDir, nil
}

// dirWithinRoot reports whether dir, with every symlink component resolved, stays
// within root once root's own symlinks are resolved too. Comparing real path to
// real path (rather than real to lexical) keeps a legitimately symlinked root —
// e.g. macOS /tmp -> /private/tmp — from reading as an escape. Both paths must
// exist; a missing directory is returned as an error.
func dirWithinRoot(rootAbs, dir string) (bool, error) {
	realRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return false, err
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(realRoot, realDir)
	if err != nil {
		return false, err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false, nil
	}
	return true, nil
}

// resolveScopeOverrides resolves the --scope/--include/--exclude paths to clean
// project-root-relative form, rejecting any that escape the root.
func resolveScopeOverrides(rootAbs, cwd string, flags runFlags) (apprun.ScopeOverrides, error) {
	var ov apprun.ScopeOverrides
	if flags.scopeSet {
		ov.ScopeReplace = make([]string, 0, len(flags.scope))
		for _, p := range flags.scope {
			rel, err := projectRelPath(rootAbs, cwd, p)
			if err != nil {
				return apprun.ScopeOverrides{}, usageErrorf("--scope %q is outside the project root", p)
			}
			ov.ScopeReplace = append(ov.ScopeReplace, rel)
		}
	}
	for _, p := range flags.include {
		rel, err := projectRelPath(rootAbs, cwd, p)
		if err != nil {
			return apprun.ScopeOverrides{}, usageErrorf("--include %q is outside the project root", p)
		}
		ov.Include = append(ov.Include, rel)
	}
	for _, p := range flags.exclude {
		rel, err := projectRelPath(rootAbs, cwd, p)
		if err != nil {
			return apprun.ScopeOverrides{}, usageErrorf("--exclude %q is outside the project root", p)
		}
		ov.Exclude = append(ov.Exclude, rel)
	}
	return ov, nil
}

// projectRelPath resolves arg (relative to cwd, or absolute) to a clean
// slash-separated path relative to the project root. Unlike parsePathFilters it
// keeps "." (the root), which is a valid run scope. A path that escapes the root
// is an error.
func projectRelPath(rootAbs, cwd, arg string) (string, error) {
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, arg)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(filepath.Clean(rootAbs), abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path %q escapes root", arg)
	}
	return rel, nil
}

// detectStdinMode decides how the child's stdin is wired. A character device
// (a terminal, or /dev/null) carries no replayable data: it is connected to the
// null device by default, or inherited under --allow-tty. Inherited stdin (TTY,
// pipe, file, socket) may carry data that is not part of the cache key, so it is
// forced uncached by the application layer.
func detectStdinMode(allowTTY bool) runcache.StdinMode {
	info, err := os.Stdin.Stat()
	if err != nil {
		return runcache.StdinNull
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		if allowTTY {
			return runcache.StdinTTY
		}
		return runcache.StdinNull
	}
	return runcache.StdinPipe
}

// indexMode selects which worktree index the run service's scanner is wired with.
// Making it explicit (rather than a bare bool) keeps the two distinct postures —
// authoritative maintenance and no index at all — legible at every call site.
//
// There is deliberately no read-only acceleration mode here. Every scan the run
// service performs is a cache decision — publishing a result, serving one, or telling
// a user which stored runs would replay — and those all set scanner Options.ForceRehash,
// which declines to consult the index at all. Wiring one would open a database whose
// every lookup is skipped. The read-only index remains the right tool for the callers
// that do reuse hashes (changes, diff, restore, state comparison), which is why
// lazyReadOnlyIndex still exists; it is simply not a posture a run service can be in.
type indexMode int

const (
	// indexNone wires no index: the scanner always re-hashes. Used by the non-scanning
	// subcommands (log/show/rm), which never look a path up, and by every scanning
	// read-only caller (status run-reuse, run explain, run ls), whose scans force a
	// rehash and would never consult one.
	indexNone indexMode = iota
	// indexMutable wires the lazy mutable index that opens (and mutates) on first
	// use inside the run presence lock. Used only by the run write path, which
	// persists the scan it just performed so the other commands' reads stay accurate.
	indexMutable
)

// buildRunService wires the run service's composition root: the scanner (with its
// worktree index for hash reuse), the run store, the process runner and
// executable resolver, and the clock and environment reader. It returns a cleanup
// that closes the index, which the caller must defer.
func buildRunService(ctx context.Context, layout paths.Layout, cfg domainconfig.Config, mode indexMode) (*apprun.Service, func(), error) {
	hasher := blake3hash.New()
	// Opening the mutable worktree index mutates it (creates the database, switches
	// journal mode, may rebuild the schema, clears incomplete-scan markers), so it must
	// happen under the run presence lock — never at wiring time, where it would race a
	// collector rebuilding the index. The write path (run) is therefore wired with a
	// lazy mutable index that opens on first use, inside the locked scan, and writes
	// back what that scan read. Every other caller gets no index: the read-only ones
	// force a rehash and would never look a path up, and the non-scanning subcommands
	// (log/show/rm) never scan at all.
	var scn *scanner.Service
	cleanup := func() {}
	switch mode {
	case indexMutable:
		li := &lazyIndex{dir: layout.IndexesDir()}
		scn = scanner.New(worktreefs.New(), hasher, li)
		cleanup = func() { _ = li.Close() }
	case indexNone:
		// No index: the scanner always re-hashes and never persists.
		scn = scanner.New(worktreefs.New(), hasher, nil)
	default:
		panic(fmt.Sprintf("buildRunService: unhandled index mode %d", mode))
	}
	proc := process.New()
	svc := apprun.New(apprun.Deps{
		Scanner:        scn,
		Store:          runstore.New(layout, hasher),
		Runner:         proc,
		Resolver:       proc,
		EffectObserver: effectobserve.New(effectfs.New(hasher), hasher),
		Env:            osEnv{},
		Clock:          realClock{},
		Hasher:         hasher,
		AwaVersion:     app.VersionString(),
		Locks:          newPresenceLocker(ctx, layout, lockfile.OpRun, cfg.Locks.Timeout.Duration()),
		RmLocks:        newPresenceLocker(ctx, layout, lockfile.OpRunRm, cfg.Locks.Timeout.Duration()),
	})
	return svc, cleanup, nil
}

// lazyIndex defers opening the SQLite worktree index until its first use, so the
// mutating open lands inside the caller's locked section instead of at wiring time. It
// is for the run write path, which holds a presence lock before its first scan. A
// read-only caller must not use it — its first Lookup would trigger a mutating open
// outside any lock — so such callers are wired with a nil index and re-hash instead.
type lazyIndex struct {
	dir  string
	once sync.Once
	idx  *sqliteindex.Index
	err  error
}

func (l *lazyIndex) open() (*sqliteindex.Index, error) {
	l.once.Do(func() {
		idx, err := sqliteindex.Open(l.dir)
		if err != nil {
			// Tag the failure so the run classifier maps it to the storage exit code, as
			// the eager open did before the open was deferred into the locked scan.
			l.err = fmt.Errorf("%w: %v", errWorktreeIndexOpen, err)
			return
		}
		l.idx = idx
	})
	return l.idx, l.err
}

// errWorktreeIndexOpen marks a failure to open the mutable worktree index on the run
// write path, so classifyRunError can map it to the storage exit code.
var errWorktreeIndexOpen = errors.New("opening worktree index")

func (l *lazyIndex) BeginScan(ctx context.Context, meta worktree.ScanMetadata) (worktree.ScanTx, error) {
	idx, err := l.open()
	if err != nil {
		return nil, err
	}
	return idx.BeginScan(ctx, meta)
}

func (l *lazyIndex) Lookup(ctx context.Context, path worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	idx, err := l.open()
	if err != nil {
		return worktree.IndexedEntry{}, false, err
	}
	return idx.Lookup(ctx, path)
}

// Close closes the underlying index if it was ever opened; an unused lazyIndex closes
// to a no-op.
func (l *lazyIndex) Close() error {
	if l.idx != nil {
		return l.idx.Close()
	}
	return nil
}

// lazyReadOnlyIndex defers a strictly read-only index open until its first Lookup,
// for scanning callers that hold no presence lock and do reuse indexed hashes: the
// changes, diff, restore and state-comparison paths. A mode=ro open never mutates the
// store, so it is safe outside the lock. The run service is not among its callers —
// its scans force a rehash — so this is the accelerator for reading the worktree, not
// for deciding what may be replayed.
// It is acceleration only and fail-soft: an open failure (absent, locked, stale,
// too-new, corrupt, or WAL/read-unsupported) is swallowed into a permanent-miss
// mode where every Lookup re-hashes, so a degraded index never fails the command.
// It opens on the first Lookup using that Lookup's context, so the open inherits the
// user command's cancellation without the struct having to retain a context.
type lazyReadOnlyIndex struct {
	once sync.Once
	dir  string
	idx  *sqliteindex.Index
}

// open opens the read-only index at most once, using ctx for the open's bounded
// metadata reads. On failure it leaves l.idx nil (permanent-miss mode); the error is
// intentionally not retained because a lost acceleration has no user action.
func (l *lazyReadOnlyIndex) open(ctx context.Context) *sqliteindex.Index {
	l.once.Do(func() {
		idx, err := sqliteindex.OpenReadOnly(ctx, l.dir)
		if err != nil {
			return
		}
		l.idx = idx
	})
	return l.idx
}

// Lookup accelerates a path through the read-only index when one opened, and returns
// a plain miss otherwise so the scanner re-hashes. The index's own Lookup is already
// fail-soft, so a read error here also degrades to a miss rather than an error.
func (l *lazyReadOnlyIndex) Lookup(ctx context.Context, path worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	idx := l.open(ctx)
	if idx == nil {
		// The open may have failed because ctx was cancelled/expired mid-scan; that is a
		// stop signal, not a missing index, so surface it rather than degrade to a miss
		// that would re-hash this path from disk. A genuine open failure is still a miss.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return worktree.IndexedEntry{}, false, ctxErr
		}
		return worktree.IndexedEntry{}, false, nil
	}
	return idx.Lookup(ctx, path)
}

// Close closes the underlying index if one opened; an unused lazyReadOnlyIndex closes
// to a no-op.
func (l *lazyReadOnlyIndex) Close() error {
	if l.idx != nil {
		return l.idx.Close()
	}
	return nil
}

// osEnv reads the awa process environment. Only Lookup is needed: the child's
// environment is assembled from the effective allowlist, never inherited wholesale.
type osEnv struct{}

func (osEnv) Lookup(name string) (string, bool) { return os.LookupEnv(name) }

// realClock is the production clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// hitMessage renders the cache-hit notice for human output.
// classifyRunError maps a run failure to the right exit code: store corruption is
// exit 5; an ambiguous prefix is a usage error; an unknown id or key is not-found;
// an invalid config is a config error; a start failure or anything else is
// generic.
func classifyRunError(err error) error {
	if c := interruptError(err); c != nil {
		return c
	}
	if coded := lockErrorCode(err); coded != nil {
		return coded
	}
	switch {
	case errors.Is(err, runcache.ErrCorruptStore), errors.Is(err, errWorktreeIndexOpen):
		return stateActionErrorf("%v", err)
	case errors.Is(err, runcache.ErrAmbiguousPrefix):
		return usageErrorf("%v\nuse a longer id prefix", err)
	case errors.Is(err, runcache.ErrInvalidPrefix):
		return usageErrorf("%v", err)
	case errors.Is(err, runcache.ErrNotFound):
		return notFoundErrorf("%v", err)
	case errors.Is(err, runcache.ErrStartFailed):
		return genericErrorf("%v", err)
	}
	var ve *domainconfig.ValidationError
	if errors.As(err, &ve) {
		return configErrorf("%v", err)
	}
	return genericErrorf("%v", err)
}

// --- JSON views ---

type runCacheDoc struct {
	Status string `json:"status"`
	Key    string `json:"key"`
	// Written reports whether this run wrote a new cache entry. It is true only for a
	// freshly stored miss; a hit replays an existing entry without writing, so it is
	// false even though a resolvable entry exists. Whether an entry is resolvable is
	// conveyed instead by run.id / cache.hit_run_id being present.
	Written bool `json:"written"`
	// Reason is the machine-stable explanation for a non-cacheable run (e.g.
	// "stdin-not-keyed", "capture-disabled", "skipped-inputs", "failed-not-cached").
	// It is empty — and omitted — for a cacheable run.
	Reason   string `json:"reason,omitempty"`
	HitRunID string `json:"hit_run_id,omitempty"`
	// Diagnosis is the bounded inline cache diagnosis: whether a nearest previous run
	// was found (available), that near-miss candidate (with its reason, score,
	// changed-path sample, and advisory hints), and structured follow-up command
	// suggestions. It is always present. available reports only whether a nearest
	// candidate exists; suggestions are independent and may be non-empty even when
	// available is false (a non-reusable run with no nearest still suggests run-explain
	// and run-compare). Only a cache hit reports available=false with empty suggestions.
	Diagnosis runDiagnosisDoc `json:"diagnosis"`
}

type runRunDoc struct {
	// ID is present whenever the run has a durable record resolvable by "run show": a
	// cache hit, or any recorded miss (reusable, record-only, mutating, or
	// unknown-post-state). Only a capture-disabled run — which records nothing — has
	// no id.
	ID       string   `json:"id,omitempty"`
	Command  []string `json:"command"`
	CWD      string   `json:"cwd"`
	ExitCode int      `json:"exit_code"`
	Signal   string   `json:"signal"`
	// ExitOrigin names who produced ExitCode/Signal. It is always "child" in a run
	// JSON envelope, because the envelope is only emitted for a wrapped-command
	// execution or a cache-hit replay. An awa-origin failure (bad config, corrupt
	// store, lock timeout) is reported through the awa error writer with an awa exit
	// code and never as this envelope — so a consumer can classify wrapper-vs-child
	// origin here rather than guessing from the numeric $? alone.
	ExitOrigin string `json:"exit_origin"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMs int64  `json:"duration_ms"`
	Cacheable  bool   `json:"cacheable"`
	// Recorded reports whether this run has a durable history record. Reuse is the
	// stable machine enum for that record's reuse classification (reusable,
	// non-reusable, record-only, unknown-post-state); it is present only when Recorded.
	Recorded bool   `json:"recorded"`
	Reuse    string `json:"reuse,omitempty"`
	// Mutation is the stable machine enum for the run mutation guard's outcome
	// (unobserved, unchanged, changed, scan-failed): whether the command changed
	// observed project state between the pre-run and post-run scans, left it
	// unchanged, or could not be observed. A hit executes nothing, so it is
	// "unobserved". A changed run is never cacheable (cache.reason is "mutated-state").
	Mutation string `json:"mutation"`
}

type runInputsDoc struct {
	TreeHash       string   `json:"tree_hash"`
	TrustMode      string   `json:"trust_mode"`
	Scope          []string `json:"scope"`
	SkippedCount   int      `json:"skipped_count"`
	SkippedAllowed bool     `json:"skipped_allowed"`
}

type runStreamDoc struct {
	CapturedBytes int64 `json:"captured_bytes"`
	RetainedBytes int64 `json:"retained_bytes"`
	Truncated     bool  `json:"truncated"`
	OmittedBytes  int64 `json:"omitted_bytes"`
}

type runOutputsDoc struct {
	Stdout runStreamDoc `json:"stdout"`
	Stderr runStreamDoc `json:"stderr"`
}

// runDisplayDoc records the per-invocation display request. It describes only what
// was shown on the terminal; it never reflects what was captured or stored, which
// the cache and outputs fields convey. The full output remains inspectable via
// run.id with "run show".
type runDisplayDoc struct {
	Mode string `json:"mode"`
	// TailLines is the requested tail window, present only for mode "tail".
	TailLines int `json:"tail_lines,omitempty"`
	// Hidden is always true in this envelope: --json writes the machine document
	// and no child bytes, whatever human mode was requested. Mode carries the
	// requested mode as context, so it can read "full" while Hidden stays true.
	Hidden bool `json:"hidden"`
}

type runDoc struct {
	Cache   runCacheDoc   `json:"cache"`
	Run     runRunDoc     `json:"run"`
	Inputs  runInputsDoc  `json:"inputs"`
	Display runDisplayDoc `json:"display"`
	// Outputs is present only when the run's output was captured; a run with capture
	// disabled has no output metadata rather than a misleading zero-byte block.
	Outputs *runOutputsDoc `json:"outputs,omitempty"`
	// Diagnostics carries the run's latency diagnostics (diagnostics.performance[]),
	// present only when a known cause crossed the interactive threshold.
	Diagnostics *perfDiagnosticsDoc `json:"diagnostics,omitempty"`
	// Effect carries the effect-state non-reusable diagnosis (stable tokens + typed
	// actions), present only when the run was left non-reusable because its watched
	// generated-output state changed or could not be observed. It is the same object a
	// near-miss candidate carries; effectdiag.go owns the shape.
	Effect *effectDiagnosisDoc `json:"effect,omitempty"`
}

func runView(res apprun.Result, mode inspect.DisplayMode, diag apprun.RunInlineDiagnosis) runDoc {
	e := res.Execution
	// The run id is only meaningful — resolvable by "run show" — when an entry
	// exists: a hit reuses one, a stored miss just wrote one. Otherwise omit it.
	runID := ""
	hitID := ""
	if !res.RunID.IsZero() {
		runID = res.RunID.String()
		if res.Status == runcache.CacheHit {
			hitID = res.RunID.String()
		}
	}
	var outputs *runOutputsDoc
	if e.Output != nil {
		outputs = &runOutputsDoc{
			Stdout: streamView(e.Output.Stdout),
			Stderr: streamView(e.Output.Stderr),
		}
	}
	display := runDisplayDoc{Mode: mode.String(), Hidden: true}
	if mode.Kind() == inspect.DisplayTail {
		display.TailLines = mode.Tail().Lines()
	}
	return runDoc{
		Cache: runCacheDoc{Status: res.Status.String(), Key: res.Key.String(), Written: res.Written, Reason: e.Decision.Reason, HitRunID: hitID, Diagnosis: runDiagnosisView(res, diag)},
		Run: runRunDoc{
			ID:         runID,
			Command:    e.KeyInput.Command.Argv,
			CWD:        e.KeyInput.CWD.String(),
			ExitCode:   e.Exit.Code,
			Signal:     e.Exit.Signal,
			ExitOrigin: e.Exit.Origin().String(),
			StartedAt:  e.StartedAt.UTC().Format(time.RFC3339Nano),
			FinishedAt: e.FinishedAt.UTC().Format(time.RFC3339Nano),
			DurationMs: e.DurationMs(),
			Cacheable:  e.Decision.Cacheable,
			Recorded:   res.Recorded,
			Reuse:      reuseToken(res),
			Mutation:   e.Mutation.Outcome().String(),
		},
		Inputs: runInputsDoc{
			TreeHash:       e.KeyInput.InputTreeHash.String(),
			TrustMode:      e.KeyInput.TrustMode.String(),
			Scope:          e.KeyInput.IncludeScope,
			SkippedCount:   e.Skipped.Count,
			SkippedAllowed: e.Skipped.Allowed,
		},
		Display:     display,
		Outputs:     outputs,
		Diagnostics: perfDiagnosticsView(res.Performance),
		Effect:      effectDiagnosisView(res.EffectDiagnosis, res.Execution.KeyInput.Command.Argv),
	}
}

func streamView(s apprun.StreamSummary) runStreamDoc {
	return runStreamDoc{
		CapturedBytes: s.CapturedBytes,
		RetainedBytes: s.RetainedBytes,
		Truncated:     s.Truncated,
		OmittedBytes:  s.OmittedBytes,
	}
}
