package cli

import (
	"errors"
	"os"
	"strings"

	"awarer/internal/app/configcmd"
	"awarer/internal/app/state"
	"awarer/internal/app/stateprovider"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/provider"
	"awarer/internal/infra/projfs"
	"awarer/internal/output"
)

// stateSubcommands lists the external-state provider subcommands. Both honor
// --root/--config/--json and a --trust-mode/--strict override (the override matters
// only for a "now" side). resolve names exactly one state; compare takes one explicit
// A..B range and returns a summary-only relationship.
var stateSubcommands = []subcommand{
	{name: "resolve", summary: "resolve a state reference to a full immutable identity", caps: []capability{capRoot, capConfig, capJSON, capTrustMode}, run: runStateResolve, help: commandHelp{
		usage:    []string{"state resolve <state-ref> [--json]"},
		long:     "Resolve one state reference (now, latest, a checkpoint id or id prefix, @-N, or run:<id>:before|after) to a full immutable identity, atomically. A missing, corrupt, or GC-removed reference is a complete unavailable assessment with a stable reason and still exits 0; only malformed syntax is a usage error. Stored references never read config; a reference containing 'now' loads the effective config once.",
		examples: []string{"awa state resolve latest --json", "awa state resolve now --json", "awa state resolve run:<id>:before --json"},
	}},
	{name: "compare", summary: "compare two states for freshness (summary-only)", caps: []capability{capRoot, capConfig, capJSON, capTrustMode}, run: runStateCompare, help: commandHelp{
		usage:    []string{"state compare <left>..<right> [--json]"},
		long:     "Compare two states over one explicit A..B range and report equal, different, incomparable, or unavailable with bounded path-keyed counts and evidence-boundary facts. It never emits changed paths, renames, blobs, or diff bodies — use 'awa changes' or 'awa diff' for those. Expected degradation exits 0 with a stable reason.",
		examples: []string{"awa state compare <left>..<right> --json", "awa state compare latest..now --json"},
	}},
}

// stateCapabilities is the coarse set route validates "awa state" against; runState
// then applies the precise per-subcommand set.
func stateCapabilities() []capability { return unionCapabilities(stateSubcommands) }

// stateCmdHelp is the "awa state --help" frame; stateHelp appends the subcommand list
// from stateSubcommands so the names live in exactly one place.
var stateCmdHelp = commandHelp{
	usage: []string{"state <subcommand>"},
	topic: "integrations",
	long:  "Read-only external-state provider (awa-state/v1) for neighboring local tools. resolve turns a moving reference into a full immutable identity; compare returns a bounded freshness relationship. Both are pull-only and metadata-only: they never record a checkpoint, refresh a cache, read blob content, or change retention.",
}

// runState dispatches "awa state <subcommand>".
func runState(w *output.Writer, inv invocation) error {
	if inv.options.Help {
		return stateHelp(w, inv)
	}
	if len(inv.args) == 0 {
		return usageErrorf("state requires a subcommand: resolve or compare")
	}
	sub := inv.args[0]
	spec := lookupSubcommand(stateSubcommands, sub)
	if spec == nil {
		return usageErrorf("unknown state subcommand %q: want resolve or compare", sub)
	}
	return dispatchSubcommand(w, "state", spec, inv)
}

// stateHelp renders "awa state --help" and "awa state <sub> --help". It does no
// project or lock work.
func stateHelp(w *output.Writer, inv invocation) error {
	if len(inv.args) > 0 {
		if spec := lookupSubcommand(stateSubcommands, inv.args[0]); spec != nil {
			writeHelpBody(w, "awa state "+spec.name, subcommandHelp("state", *spec), hasCap(spec.caps, capJSON))
			return nil
		}
	}
	writeSubcommandOwnerHelp(w, "state", stateCmdHelp, stateSubcommands)
	return nil
}

// runStateResolve handles "awa state resolve <state-ref>".
func runStateResolve(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	if len(args) == 0 {
		return usageErrorf("state resolve requires a state reference (e.g. latest, now, <id>, run:<id>:before)")
	}
	if len(args) > 1 {
		return usageErrorf("state resolve takes exactly one state reference, got %d", len(args))
	}
	refTok := args[0]
	// A flag-shaped token is a mistyped flag, not a reference. The reference parser
	// would reject it too, but as invalid reference syntax; naming it an unknown flag
	// is the diagnosis that matches what the user actually typed. Its sibling
	// `state compare` already refuses one, because its A..B syntax leaves no room.
	if strings.HasPrefix(refTok, "-") {
		return rejectExtra("state resolve", refTok)
	}
	ref, err := state.ParseRef(refTok)
	if err != nil {
		return usageErrorf("%v", err)
	}
	if err := rejectRecoveryObservationRef(ref); err != nil {
		return err
	}

	proj, reason, ok := discoverProjectOptional(inv.options)
	if !ok {
		return emitResolveUnavailable(w, inv, refTok, reason)
	}
	layout, err := proj.Paths()
	if err != nil {
		return genericErrorf("resolving project paths: %v", err)
	}

	assessor, nowCtx, cleanup, err := buildStateContext(inv, proj, layout, ref.Kind == state.RefNow)
	if err != nil {
		return err
	}
	defer cleanup()

	assessment, err := assessor.Resolve(inv.invCtx(), ref, refTok, nowCtx)
	if err != nil {
		return classifyProviderFault(err)
	}
	return emitResolve(w, inv, assessment)
}

// rejectRecoveryObservationRef keeps a restore recovery observation out of the
// external-state provider contract. A recovery observation is awa's own evidence
// for undoing a restore it performed, visible in the full timeline and usable by
// the comparison commands; the awa-state/v1 kind vocabulary a neighboring tool
// consumes deliberately does not name it. Accepting it here would either add a
// public kind the contract does not commit to, or answer with "unknown state kind"
// dressed as an availability outcome — so it is a usage error that says where the
// observation can be inspected instead.
func rejectRecoveryObservationRef(ref state.Ref) error {
	if ref.Kind != state.RefRestoreObservation {
		return nil
	}
	return usageErrorf("%s is a restore recovery observation, which the awa-state/v1 provider does not name\ninspect it with 'awa changes %s..now', 'awa diff %s..now', or 'awa log --all'", ref.Display, ref.Display, ref.Display)
}

// runStateCompare handles "awa state compare <left>..<right>".
func runStateCompare(w *output.Writer, inv invocation) error {
	args := inv.args[1:]
	if len(args) == 0 {
		return usageErrorf("state compare requires an explicit range A..B")
	}
	if len(args) > 1 {
		return usageErrorf("state compare takes exactly one range, got %d", len(args))
	}
	rangeTok := args[0]
	// compare requires an explicit A..B: reject the empty default and the bare -N
	// shortcut that changes/diff accept, so a freshness check is never taken against
	// an implicit baseline.
	if !strings.Contains(rangeTok, "..") {
		return usageErrorf("state compare requires an explicit range A..B, got %q", rangeTok)
	}
	rng, err := state.ParseRange(rangeTok)
	if err != nil {
		return usageErrorf("%v", err)
	}
	if err := rejectRecoveryObservationRef(rng.Left); err != nil {
		return err
	}
	if err := rejectRecoveryObservationRef(rng.Right); err != nil {
		return err
	}

	proj, reason, ok := discoverProjectOptional(inv.options)
	if !ok {
		return emitCompareUnavailable(w, inv, reason)
	}
	layout, err := proj.Paths()
	if err != nil {
		return genericErrorf("resolving project paths: %v", err)
	}

	needNow := rng.Left.Kind == state.RefNow || rng.Right.Kind == state.RefNow
	assessor, nowCtx, cleanup, err := buildStateContext(inv, proj, layout, needNow)
	if err != nil {
		return err
	}
	defer cleanup()

	comparison, err := assessor.Compare(inv.invCtx(), rng, nowCtx)
	if err != nil {
		return classifyProviderFault(err)
	}
	return emitCompare(w, inv, comparison)
}

// buildStateContext wires the assessor and the NowContext, loading config lazily: a
// stored-only operation never reads or validates awa.toml/.awa/config.toml (or an
// explicit --config, which is accepted but inert), so existing evidence stays reachable
// while a current config is malformed. Only when a "now" side is present is the
// effective config loaded exactly once; a malformed required config is the normal
// config error (exit 4), never a provider unavailable outcome.
func buildStateContext(inv invocation, proj projfs.Project, layout paths.Layout, needNow bool) (*stateprovider.Assessor, state.NowContext, func(), error) {
	cfg, err := stateConfig(needNow, layout, inv.options, configcmd.LoadLayered)
	if err != nil {
		return nil, state.NowContext{}, nil, err
	}
	var nowCtx state.NowContext
	if needNow {
		// The stable-observation invariant is owned by the assessor (it sets it on every
		// resolve), so the CLI only supplies the project and effective config here.
		nowCtx = state.NowContext{Project: proj, Config: cfg}
	}
	resolver, cleanup, err := buildStateResolver(layout, cfg)
	if err != nil {
		return nil, state.NowContext{}, nil, err
	}
	return stateprovider.NewAssessor(resolver), nowCtx, cleanup, nil
}

// layeredConfigLoader loads the effective layered config for a "now" side. It is the seam
// the lazy-config contract is pinned on (production passes configcmd.LoadLayered): a
// stored-only operation must never invoke it, and a now operation must invoke it exactly
// once.
type layeredConfigLoader func(configcmd.LayerFiles, configcmd.Overrides) (domainconfig.ResolvedConfig, error)

// stateConfig resolves the config for a state operation, encoding the lazy-config rule in
// one place: a stored-only operation (needNow=false) returns the product defaults and
// never reads or validates any config layer, so existing evidence stays reachable while a
// current config is malformed; a "now" operation loads the effective layered config
// exactly once through load, where a malformed required config is the normal config error
// (exit 4). An explicit --config is inert for a stored-only operation because it is never
// read here.
func stateConfig(needNow bool, layout paths.Layout, opts GlobalOptions, load layeredConfigLoader) (domainconfig.Config, error) {
	if !needNow {
		return domainconfig.Defaults(), nil
	}
	files, err := projectLayerFiles(layout, opts)
	if err != nil {
		return domainconfig.Config{}, err
	}
	loaded, err := load(files, trustModeOverride(opts))
	if err != nil {
		return domainconfig.Config{}, classifyConfigLoad(err)
	}
	return loaded.Config(), nil
}

// discoverProjectOptional locates the project like resolveProject, but treats the
// provider as optional: a missing project is not-initialized, a permission failure is
// permission-denied, and any other discovery I/O fails closed to io-error — each a
// complete unavailable assessment (ok=false), never the ExitNotFound that changes/diff
// return. It is used only by the state handlers, so it cannot change any other command.
func discoverProjectOptional(opts GlobalOptions) (projfs.Project, provider.Reason, bool) {
	if opts.Root != "" {
		p, err := projfs.Open(opts.Root)
		return classifyDiscovery(p, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		return projfs.Project{}, provider.ReasonIOError, false
	}
	p, err := projfs.Find(wd)
	return classifyDiscovery(p, err)
}

func classifyDiscovery(p projfs.Project, err error) (projfs.Project, provider.Reason, bool) {
	switch {
	case err == nil:
		return p, "", true
	case errors.Is(err, projfs.ErrRootNotFound):
		return projfs.Project{}, provider.ReasonNotInitialized, false
	case errors.Is(err, os.ErrPermission):
		return projfs.Project{}, provider.ReasonPermissionDenied, false
	default:
		return projfs.Project{}, provider.ReasonIOError, false
	}
}

// classifyProviderFault maps the only faults a provider assessment can raise: an
// interruption (exit 130) or an impossible internal state (generic). Every expected
// degradation is already an in-band assessment, so it never reaches here.
func classifyProviderFault(err error) error {
	if c := interruptError(err); c != nil {
		return c
	}
	return genericErrorf("%v", err)
}

func emitResolveUnavailable(w *output.Writer, inv invocation, inputRef string, reason provider.Reason) error {
	assessment, err := provider.Unavailable(inputRef, reason)
	if err != nil {
		return genericErrorf("%v", err)
	}
	return emitResolve(w, inv, assessment)
}

func emitCompareUnavailable(w *output.Writer, inv invocation, reason provider.Reason) error {
	comparison, err := provider.ComparisonUnavailable(nil, nil, reason)
	if err != nil {
		return genericErrorf("%v", err)
	}
	return emitCompare(w, inv, comparison)
}
