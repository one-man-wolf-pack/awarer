package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	apprun "awarer/internal/app/run"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

// explainFlags are the parsed flags of "awa run explain": the run-local flags it
// shares with run, plus the mode selectors --last, --from-run, and --to-now.
type explainFlags struct {
	run     runFlags
	last    bool
	fromRun string
	toNow   bool
}

// parseExplainFlags consumes the leading flags of args and returns them with the
// index of the first non-flag token (the start of a short-form command), or
// len(args) when all tokens were flags. It recognizes the explain mode selectors
// and otherwise defers to the shared run-local flag set.
func parseExplainFlags(args []string) (explainFlags, int, error) {
	var ef explainFlags
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--last":
			if err := noValue(name, hasValue); err != nil {
				return explainFlags{}, 0, err
			}
			ef.last = true
			continue
		case "--from-run":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return explainFlags{}, 0, err
			}
			ef.fromRun = v
			continue
		case "--to-now":
			if err := noValue(name, hasValue); err != nil {
				return explainFlags{}, 0, err
			}
			ef.toNow = true
			continue
		}
		handled, err := applyRunFlag(&ef.run, name, value, hasValue, args, &i)
		if err != nil {
			return explainFlags{}, 0, err
		}
		if handled {
			continue
		}
		if strings.HasPrefix(tok, "-") && tok != "-" {
			return explainFlags{}, 0, usageErrorf("unknown flag %q for run explain; use -- to pass it to the command", tok)
		}
		return ef, i - 1, nil
	}
	return ef, i, nil
}

// explainChildAmbiguity applies the run-explain child-boundary ambiguity check. args
// is inv.args[1:] (the tokens after the "explain" subcommand token) and stop is the
// child command's index within it, so the boundary in inv.args space is 1+stop (the
// dropped "explain" token occupies index 0). It returns the ambiguity error when an
// awa-global flag was stolen from after the child in a short-form "run explain <cmd>",
// or nil when no child token follows. Shared by the execute path and the wrapper-help
// path so the 1+stop convention lives in one place and the two can never disagree.
func explainChildAmbiguity(inv invocation, args []string, stop int) error {
	if stop < len(args) {
		return ambiguousTailGlobal(inv.tail, 1+stop)
	}
	return nil
}

// runRunExplain handles "awa run explain". It computes the same key and
// cacheability decision "awa run" would, but never executes the command or writes
// an entry, and reports how the run cache would behave.
func runRunExplain(w *output.Writer, inv invocation) error {
	args := inv.args[1:] // drop the "explain" token

	flags, stop, err := parseExplainFlags(args)
	if err != nil {
		return err
	}

	var argv []string
	if len(inv.operands) > 0 {
		if stop != len(args) {
			return usageErrorf("unexpected argument %q before --; run-local flags must precede --", args[stop])
		}
		argv = inv.operands
	} else {
		// Short form: reject an awa-global consumed after the wrapped child, matching
		// "awa run". The same check runs on the wrapper-help path, so both share the
		// boundary convention via explainChildAmbiguity.
		if err := explainChildAmbiguity(inv, args, stop); err != nil {
			return err
		}
		argv = args[stop:]
	}
	hasCommand := len(argv) > 0

	// Reject impossible request shapes before doing any work, so an ambiguous
	// invocation never silently picks a mode.
	switch {
	case flags.last && flags.fromRun != "":
		return usageErrorf("--last and --from-run cannot be combined")
	case flags.last && hasCommand:
		return usageErrorf("--last does not take a command")
	case flags.fromRun != "" && hasCommand:
		return usageErrorf("--from-run does not take a command")
	case flags.fromRun != "" && !flags.toNow:
		return usageErrorf("--from-run requires --to-now")
	case flags.toNow && flags.fromRun == "" && !flags.last:
		return usageErrorf("--to-now requires --from-run or --last")
	case flags.run.noCache && flags.run.refresh:
		return usageErrorf("--no-cache and --refresh cannot be combined")
	}

	var mode apprun.ExplainMode
	switch {
	case flags.last:
		mode = apprun.ModeLast
	case flags.fromRun != "":
		mode = apprun.ModeFromRunToNow
	case hasCommand:
		mode = apprun.ModeCommand
	default:
		return usageErrorf("run explain requires a command, --last, or --from-run <id> --to-now")
	}

	if mode != apprun.ModeCommand && hasRunLocalOverrides(flags.run) {
		return usageErrorf("run-local flags are not valid with --last or --from-run")
	}

	proj, layout, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}

	// run explain re-observes the command's input state and must reach the same verdict
	// a real run would, so it re-derives the key exactly as the hit path does: reading
	// file content, never standing an indexed hash in for it. No index is wired, because
	// its lookups would be skipped, and none is needed — nothing here writes, so no
	// presence lock is taken either.
	svc, cleanup, err := buildRunService(inv.invCtx(), layout, cfg, indexNone)
	if err != nil {
		return err
	}
	defer cleanup()

	explainReq := apprun.ExplainRequest{Mode: mode}
	switch mode {
	case apprun.ModeCommand:
		cwd, err := os.Getwd()
		if err != nil {
			return genericErrorf("determining current directory: %v", err)
		}
		execCWD, absDir, err := resolveExecCWD(layout.Root(), cwd, flags.run)
		if err != nil {
			return err
		}
		overrides, err := resolveScopeOverrides(layout.Root(), cwd, flags.run)
		if err != nil {
			return err
		}
		explainReq.Request = apprun.Request{
			Project:    proj,
			Config:     cfg,
			Argv:       argv,
			CWD:        execCWD,
			AbsDir:     absDir,
			Scope:      overrides,
			StdinMode:  detectStdinMode(flags.run.allowTTY),
			TTYAllowed: flags.run.allowTTY,
			Policy: apprun.Policy{
				Refresh:            flags.run.refresh,
				NoCache:            flags.run.noCache,
				NoCacheFailures:    flags.run.noCacheFailures,
				AllowSkippedInputs: flags.run.allowSkipped,
			},
		}
	default:
		explainReq.Request = apprun.Request{Project: proj, Config: cfg}
		explainReq.RunRef = flags.fromRun
	}

	res, err := svc.Explain(inv.invCtx(), explainReq)
	if err != nil {
		return classifyRunError(err)
	}

	if inv.options.JSON {
		if err := w.JSON("run.explain", explainView(res)); err != nil {
			return genericErrorf("%v", err)
		}
		return nil
	}
	// Human times follow the same [ui].time policy as run ls/log (explain has no
	// --time flag of its own), so the nearest-run line reads consistently with the
	// other review surfaces rather than a bare local RFC3339 stamp.
	td, err := timeDisplayFromFlag("", cfg)
	if err != nil {
		return usageErrorf("%v", err)
	}
	renderExplain(w, res, td, time.Now())
	emitPerformanceNotes(w, res.Performance)
	return nil
}

// --- JSON views ---

type explainSkippedSampleDoc struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type explainSkippedDoc struct {
	Count   int                       `json:"count"`
	Allowed bool                      `json:"allowed"`
	Samples []explainSkippedSampleDoc `json:"samples"`
}

type explainCurrentDoc struct {
	Key           string            `json:"key"`
	Cacheable     bool              `json:"cacheable"`
	Command       []string          `json:"command"`
	CWD           string            `json:"cwd"`
	InputTreeHash string            `json:"input_tree_hash"`
	RunConfigHash string            `json:"run_config_hash"`
	StdinMode     string            `json:"stdin_mode"`
	TTYAllowed    bool              `json:"tty_allowed"`
	Skipped       explainSkippedDoc `json:"skipped"`
}

type explainExactDoc struct {
	Exists  bool   `json:"exists"`
	RunID   string `json:"run_id"`
	Healthy bool   `json:"healthy"`
}

type explainDoc struct {
	Outcome     string            `json:"outcome"`
	Reason      string            `json:"reason"`
	Current     explainCurrentDoc `json:"current"`
	ExactMatch  explainExactDoc   `json:"exact_match"`
	Differences []keyDiffDoc      `json:"differences"`
	// Candidates are the nearest non-reusable runs, in the same shape run ls --near
	// emits, so both surfaces decode identically.
	Candidates []nearMissDoc `json:"candidates"`
	Warnings   []string      `json:"warnings"`
	// Diagnostics carries the latency notes for the observation explain performed. It
	// is omitted when nothing crossed the threshold, matching the run payload.
	Diagnostics *perfDiagnosticsDoc `json:"diagnostics,omitempty"`
}

func explainView(res apprun.ExplainResult) explainDoc {
	ki := res.Subject.KeyInput
	doc := explainDoc{
		Outcome: string(res.Outcome),
		Reason:  res.Reason,
		Current: explainCurrentDoc{
			Key:           res.Subject.Key.String(),
			Cacheable:     res.Subject.Cacheable,
			Command:       ki.Command.Argv,
			CWD:           ki.CWD.String(),
			InputTreeHash: ki.InputTreeHash.String(),
			RunConfigHash: ki.RunConfigHash.String(),
			StdinMode:     ki.StdinMode.String(),
			TTYAllowed:    ki.TTYAllowed,
			Skipped:       skippedView(res.Subject.Skipped),
		},
		Diagnostics: perfDiagnosticsView(res.Performance),
		ExactMatch:  exactView(res.Subject.ExactHit),
		Differences: diffDocs(res.Differences),
		Candidates:  nearMissViews(res.Candidates),
		Warnings:    res.Warnings,
	}
	if doc.Warnings == nil {
		doc.Warnings = []string{}
	}
	return doc
}

func skippedView(s runcache.SkippedSummary) explainSkippedDoc {
	samples := make([]explainSkippedSampleDoc, 0, len(s.Samples))
	for _, sm := range s.Samples {
		samples = append(samples, explainSkippedSampleDoc{Path: sm.Path, Reason: sm.Reason})
	}
	return explainSkippedDoc{Count: s.Count, Allowed: s.Allowed, Samples: samples}
}

func exactView(hit *apprun.ExactHit) explainExactDoc {
	if hit == nil {
		return explainExactDoc{Exists: false}
	}
	return explainExactDoc{Exists: true, RunID: hit.RunID.String(), Healthy: hit.Healthy}
}

// --- human output ---

// renderExplain prints a concise, scan-friendly explanation. It never prints raw
// environment values (the comparison already redacts them to markers) and points
// at "awa changes" for path-level detail when the input tree differs. Times use
// the shared FormatTime policy (td/now) so explain matches run ls/log and the
// footer rather than emitting its own local timestamp form.
func renderExplain(w *output.Writer, res apprun.ExplainResult, td domainconfig.TimeDisplay, now time.Time) {
	w.Linef("cache:  %s", res.Outcome)
	if res.Reason != "" {
		w.Linef("reason: %s", humanReason(res.Reason))
	}
	w.Linef("key:    %s", res.Subject.Key.String())

	if hit := res.Subject.ExactHit; hit != nil {
		health := "healthy"
		if !hit.Healthy {
			health = "unhealthy"
		}
		w.Linef("exact match: %s (%s)", hit.RunID.Short(), health)
	}

	if len(res.Candidates) > 0 {
		c := res.Candidates[0]
		w.Line("")
		w.Linef("nearest run: %s  %s  exit=%s", c.Entry.ID.Short(),
			FormatTime(td, c.Entry.StartedAt, now), exitLabel(c.Entry.Exit))
		if same := displayCategories(c.Score.Matched); same != "" {
			w.Linef("same:    %s", same)
		}
		if changed := displayCategories(c.Score.Different); changed != "" {
			w.Linef("changed: %s", changed)
		}
		// The effect detail, the changed-path sample, and the advisory hints are the same
		// review facts run ls --near shows; explain already carries them for the reasons
		// that can have them, so render them here too rather than discard the work.
		if detail := effectDetail(c.Effect); detail != "" {
			w.Linef("effect:  %s", detail)
		}
		if c.Changed != nil {
			for _, line := range changedSampleLines(*c.Changed) {
				w.Line(line)
			}
		}
		for _, h := range c.Hints {
			w.Line("    hint: " + h.Message)
		}
	}

	if len(res.Differences.Differences) > 0 {
		w.Line("")
		for _, line := range differenceLines(res.Differences) {
			w.Line(line)
		}
		if slices.Contains(res.Differences.Codes(), runcache.DiffInputTreeChanged) {
			w.Line("use `awa changes` for path-level workspace changes")
		}
	}

	for _, warn := range res.Warnings {
		w.Diagnostic("warning: " + warn)
	}
}

// maxHumanEnvDifferences bounds how many per-variable environment differences human
// output prints. One environment change is one line, and a single legitimate event —
// comparing against evidence recorded before the effective environment was corrected —
// can differ in a dozen variables at once. Printing all of them buries the reason the
// reader came for. The bound is presentation only: the comparison itself and the JSON
// surface keep every typed difference, so no cache decision depends on it.
const maxHumanEnvDifferences = 6

// differenceLines renders a key comparison for human output: the primary actionable
// reason first, then the remaining differences in their deterministic field order, with
// the per-variable environment detail bounded and its omission stated.
//
// Leading with the primary reason matters because the field order is not the order of
// usefulness. A comparison can differ in the input tree and in ten environment
// variables at once, and the reader needs the one that explains the decision before the
// detail that merely accompanies it.
func differenceLines(cmp runcache.KeyComparison) []string {
	primary := cmp.PrimaryReason()
	ordered := make([]runcache.Difference, 0, len(cmp.Differences))
	for _, d := range cmp.Differences {
		if d.Code == primary {
			ordered = append(ordered, d)
		}
	}
	for _, d := range cmp.Differences {
		if d.Code != primary {
			ordered = append(ordered, d)
		}
	}

	out := make([]string, 0, len(ordered)+1)
	shown, omitted, said := 0, 0, false
	// The notice belongs immediately after the environment lines it describes, not at
	// the end of the whole block: the comparison groups environment differences
	// together, so a difference of another kind following them would otherwise separate
	// the count from what was counted.
	flush := func() {
		if omitted > 0 && !said {
			out = append(out, fmt.Sprintf("env: showing %d of %d variable differences (+%d more) — use --json for the complete set",
				shown, shown+omitted, omitted))
			said = true
		}
	}
	for _, d := range ordered {
		if d.Code != runcache.DiffEnvChanged {
			flush()
			out = append(out, diffLine(d))
			continue
		}
		if shown >= maxHumanEnvDifferences {
			omitted++
			continue
		}
		shown++
		out = append(out, diffLine(d))
	}
	flush()
	return out
}

// diffLine renders one difference for human output, keeping environment changes to
// the variable name and a non-secret marker.
//
// A reserved name is labelled as awa-injected. Without that label the line reads as if
// the caller's environment or the allowlist had changed, which for a fact awa states
// itself would be false — and it is the difference a user cannot act on, so saying who
// put it there is the whole point.
func diffLine(d runcache.Difference) string {
	if d.Code == runcache.DiffEnvChanged {
		name := d.EnvName
		if domainconfig.IsReservedEnvName(name) {
			name += " (awa-injected)"
		}
		return "env " + name + " " + string(d.EnvChange) + " (" + d.Old + " -> " + d.New + ")"
	}
	return humanReason(string(d.Code)) + " (" + d.Old + " -> " + d.New + ")"
}

// humanReason renders a stable reason/difference token as a readable phrase.
func humanReason(token string) string {
	return strings.ReplaceAll(token, "-", " ")
}

// humanCategorySet is the high-signal subset of score categories shown in the
// human same/changed lines, keeping them concise: the low-signal policy/identity
// categories (trust, stdin, tty, skipped) are omitted from the summary but still
// appear in the differences detail and the JSON score.
var humanCategorySet = map[string]bool{
	"command": true, "executable": true, "cwd": true, "config": true,
	"scope": true, "env": true, "platform": true, "input_tree": true,
}

// displayCategories renders the high-signal categories of a score line, humanized
// (e.g. "input_tree" -> "input tree"). It returns "" when none apply.
func displayCategories(categories []string) string {
	var out []string
	for _, c := range categories {
		if humanCategorySet[c] {
			out = append(out, strings.ReplaceAll(c, "_", " "))
		}
	}
	return strings.Join(out, ", ")
}
