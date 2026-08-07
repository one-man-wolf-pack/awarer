package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"awarer/internal/app"
	apprestore "awarer/internal/app/restore"
	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	domrestore "awarer/internal/domain/restore"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/lockfile"
	"awarer/internal/infra/restorespool"
	"awarer/internal/infra/restorestore"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/infra/worktreemut"
	"awarer/internal/output"
)

// restoreArgs is the parsed restore-local argument set: one source reference, the
// selection mode, and the two mode flags.
type restoreArgs struct {
	sourceTok string
	paths     []string
	all       bool
	apply     bool
	dryRun    bool
}

// runRestore handles "awa restore". Preview is the default and writes nothing;
// mutation requires --apply and an explicit selection.
func runRestore(w *output.Writer, inv invocation) error {
	ra, err := parseRestoreArgs(inv.args)
	if err != nil {
		return err
	}
	// Tokens after "--" are literal path selections, never references or flags —
	// the way to select a path that looks like a flag or a reference.
	ra.paths = append(ra.paths, inv.operands...)

	if ra.apply && ra.dryRun {
		return usageErrorf("--dry-run and --apply cannot be combined: --dry-run names the preview mode, --apply is the only mutating one")
	}
	if ra.sourceTok == "" {
		return usageErrorf("restore requires a source state reference\ntry: awa restore <checkpoint-id> -- <path...>")
	}
	if ra.all && len(ra.paths) > 0 {
		return usageErrorf("--all cannot be combined with path selections: choose all proven scope or name the paths")
	}
	if !ra.all && len(ra.paths) == 0 {
		return usageErrorf("restore requires one or more paths, or an explicit --all\nan omitted selection is never a whole-project restore")
	}

	ref, err := state.ParseRef(ra.sourceTok)
	if err != nil {
		return usageErrorf("%v", err)
	}
	if ref.Kind == state.RefNow {
		return usageErrorf("%q is not a restore source: the current worktree is the destination, not the evidence", ra.sourceTok)
	}

	proj, layout, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}
	// An apply writes durable evidence into .awa/ (the recovery observation and its
	// blobs), so it refuses to grow that directory unguarded, exactly like
	// checkpoint. A preview writes nothing and only warns.
	if err := guardPreflight(w, proj, ra.apply); err != nil {
		return err
	}

	selection, err := buildRestoreSelection(layout.Root(), ra)
	if err != nil {
		return err
	}

	svc, cleanup, err := buildRestoreService(inv.invCtx(), layout, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := svc.Run(inv.invCtx(), apprestore.Request{
		Project:   proj,
		Config:    cfg,
		Ref:       ref,
		Selection: selection,
		Apply:     ra.apply,
	})
	if err != nil {
		return classifyRestoreError(err)
	}

	td, err := timeDisplayFromFlag("", cfg)
	if err != nil {
		return usageErrorf("%v", err)
	}
	if inv.options.JSON {
		if err := emitRestoreJSON(w, res); err != nil {
			return err
		}
	} else {
		renderRestore(w, res, td)
	}
	return restoreOutcomeExit(res.Result)
}

// parseRestoreArgs parses the restore-local arguments: at most one source
// reference token, zero or more path selections, and the mode flags.
func parseRestoreArgs(args []string) (restoreArgs, error) {
	var ra restoreArgs
	haveSource := false
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, _, hasValue := splitFlag(tok)
		switch name {
		case "--apply":
			if err := noValue(name, hasValue); err != nil {
				return restoreArgs{}, err
			}
			ra.apply = true
		case "--dry-run":
			if err := noValue(name, hasValue); err != nil {
				return restoreArgs{}, err
			}
			ra.dryRun = true
		case "--all":
			if err := noValue(name, hasValue); err != nil {
				return restoreArgs{}, err
			}
			ra.all = true
		default:
			if strings.HasPrefix(tok, "-") {
				return restoreArgs{}, usageErrorf("unknown flag %q for restore", tok)
			}
			// The first bare token is the source reference; anything after it is a path
			// selection. A second reference-shaped token would otherwise be silently
			// taken as a path, which is exactly the operand confusion this removes.
			if !haveSource {
				ra.sourceTok, haveSource = tok, true
				continue
			}
			if strings.Contains(tok, "..") {
				return restoreArgs{}, usageErrorf("restore takes one source state reference, not a range (got %q)\nrestore always writes the current worktree; use -- to select a path that contains \"..\"", tok)
			}
			ra.paths = append(ra.paths, tok)
		}
	}
	return ra, nil
}

// buildRestoreSelection normalizes the selection through the same
// invocation-cwd-relative, root-confined path boundary changes and diff use, so a
// path means the same thing in every command.
func buildRestoreSelection(root string, ra restoreArgs) (domrestore.Selection, error) {
	if ra.all {
		return domrestore.NewAllSelection(), nil
	}
	filters, err := parsePathFilters(root, ra.paths)
	if err != nil {
		return domrestore.Selection{}, err
	}
	if len(filters) == 0 {
		// Every operand normalized to the project root itself, which is "everything"
		// spelled implicitly. Restore requires that decision to be explicit.
		return domrestore.Selection{}, usageErrorf("the selected path is the project root; use --all to restore all proven scope explicitly")
	}
	sel, err := domrestore.NewPathSelection(filters)
	if err != nil {
		return domrestore.Selection{}, usageErrorf("%v", err)
	}
	return sel, nil
}

// buildRestoreService wires the composition root for "awa restore".
//
// The scanner is read-only: restore observes the worktree twice and must not
// mutate the shared index while doing so. The blob store is passed as restore's
// private content capability — the metadata-only external provider never receives
// it. Two locks are taken on an apply: an exclusive restore lock (restore against
// restore) and a writer presence lock (so a collector cannot reclaim the blobs
// this operation is about to read).
func buildRestoreService(ctx context.Context, layout paths.Layout, cfg domainconfig.Config) (*apprestore.Service, func(), error) {
	hasher := blake3hash.New()
	resolver, cleanup, err := buildStateResolver(layout, cfg)
	if err != nil {
		return nil, nil, err
	}
	idx := &lazyReadOnlyIndex{dir: layout.IndexesDir()}
	timeout := cfg.Locks.Timeout.Duration()
	svc := apprestore.New(apprestore.Deps{
		Resolver: resolver,
		Scanner:  scanner.NewReadOnly(worktreefs.New(), hasher, idx),
		Blobs:    blobstore.New(layout, hasher),
		Recovery: restorestore.New(layout, hasher),
		Current:  worktreefs.NewContentReader(layout.Root()),
		Spools: func(id domrestore.OperationID) (apprestore.Spool, error) {
			return restorespool.Open(layout, id)
		},
		Appliers: func(id domrestore.OperationID) (apprestore.Applier, error) {
			return worktreemut.New(layout, id, hasher)
		},
		Exclusive: newRestoreExclusiveLocker(ctx, layout, timeout),
		Presence:  newPresenceLocker(ctx, layout, lockfile.OpRestore, timeout),
		Now:       time.Now,
		Rand:      rand.Reader,
		Version:   app.VersionString(),
	})
	return svc, func() { cleanup(); _ = idx.Close() }, nil
}

// classifyRestoreError maps a restore failure to its exit code. Restore
// deliberately allocates no new code: the taxonomy already distinguishes the
// conditions it produces, and a code per preview count would be noise.
func classifyRestoreError(err error) error {
	if c := interruptError(err); c != nil {
		return c
	}
	if coded := lockErrorCode(err); coded != nil {
		return coded
	}
	if errors.Is(err, state.ErrObservationUnstable) {
		return genericErrorf("%v\nthe worktree changed while it was being observed; rerun once it is quiet", err)
	}
	return classifyStateError(err)
}

// restoreOutcomeExit turns a validated outcome into the process status. A preview
// always succeeds — reporting what it found is its job — while an outcome that did
// not complete the requested work is a failure the caller can branch on.
func restoreOutcomeExit(res domrestore.ApplyResult) error {
	switch res.Outcome() {
	case domrestore.OutcomePreview, domrestore.OutcomeApplied, domrestore.OutcomeNoOp:
		return nil
	case domrestore.OutcomeCancelled:
		return &codedError{code: ExitInterrupted, msg: "restore cancelled before any change was made"}
	case domrestore.OutcomeRefused:
		return genericErrorf("restore refused: %s", joinReasons(res.Reasons()))
	case domrestore.OutcomeConflict:
		return genericErrorf("restore conflicted: the selected state changed after it was planned")
	case domrestore.OutcomePartial:
		return genericErrorf("restore applied %d of %d operation(s) and stopped: %s", res.Completed(), res.Completed()+res.Remaining(), joinReasons(res.Reasons()))
	default:
		return genericErrorf("restore reported an unknown outcome")
	}
}

func joinReasons(reasons []domrestore.Reason) string {
	if len(reasons) == 0 {
		return "no reason recorded"
	}
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}

// selectionPaths renders a selection's paths as the typed data a JSON consumer
// gets: verbatim, unquoted, one entry per path.
func selectionPaths(sel domrestore.Selection) []string {
	ps := sel.Paths()
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return out
}

// pathOperands renders the "-- a b c" suffix of a suggested command, or the empty
// string for an --all selection.
//
// Paths are shell-quoted when they need it. The suggested apply, undo, and inspect
// lines exist to be copied, and a path holding a space would otherwise be pasted
// back as two operands — silently selecting something the user never chose.
func pathOperands(sel domrestore.Selection) string {
	if sel.All() {
		return ""
	}
	ps := selectionPaths(sel)
	quoted := make([]string, len(ps))
	for i, p := range ps {
		quoted[i] = shellOperand(p)
	}
	return " -- " + strings.Join(quoted, " ")
}

// shellOperand renders one path so a shell passes it through as a single argument.
// Ordinary paths are left bare — quoting everything would make the common
// suggestion harder to read for no benefit.
func shellOperand(p string) string {
	if p != "" && !strings.ContainsAny(p, " \t\n\"'\\$`&|;<>()*?[]{}#~!") {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
