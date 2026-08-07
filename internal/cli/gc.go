package cli

import (
	"errors"
	"strconv"
	"strings"
	"time"

	gcapp "awarer/internal/app/gc"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	domainconfig "awarer/internal/domain/config"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/runcache"
	"awarer/internal/output"
)

// runGc handles "awa gc": conservative garbage collection of project-local .awa
// state. --dry-run renders the advisory Plan without mutating anything; a real run
// goes through Collect, which takes the collector lease and plans under it so the
// destructive decision is authoritative, never a stale plan built before the lease.
// --root/--config/--json are global; the local flags tune retention and scope. It
// consumes no operands.
//
// Exit code follows the plan's disposition: a healthy or lock-blocked run exits 0
// (a lock is "try again later", not damage), while storage corruption that blocks
// safe planning exits with the state-action-required code after the report is rendered
// and without deleting anything.
func runGc(w *output.Writer, inv invocation) error {
	var (
		dryRun          bool
		committed       bool
		runsOnly        bool
		checkpointsOnly bool
		blobsOnly       bool
		keepLast        = gcdom.KeepLastDefault
		olderThan       time.Duration
	)

	args := inv.args
	i := 0
	for i < len(args) {
		tok := args[i]
		i++
		name, value, hasValue := splitFlag(tok)
		switch name {
		case "--dry-run":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			dryRun = true
		case "--committed":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			committed = true
		case "--runs-only":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			runsOnly = true
		case "--checkpoints-only":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			checkpointsOnly = true
		case "--blobs-only":
			if err := noValue(name, hasValue); err != nil {
				return err
			}
			blobsOnly = true
		case "--older-than":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			d, err := domainconfig.ParseDuration(v)
			if err != nil {
				return usageErrorf("invalid value %q for --older-than: %v", v, err)
			}
			olderThan = d.Duration()
		case "--keep-last":
			v, err := requireValue(name, value, hasValue, args, &i)
			if err != nil {
				return err
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return usageErrorf("invalid value %q for --keep-last: want a non-negative integer", v)
			}
			keepLast = n
		default:
			if strings.HasPrefix(tok, "-") {
				return usageErrorf("unknown flag %q for gc", tok)
			}
			return usageErrorf("unexpected argument %q for gc", tok)
		}
	}
	filter, err := subsystemFilter(runsOnly, checkpointsOnly, blobsOnly)
	if err != nil {
		return err
	}

	gcreq, err := gcdom.NewGCRequest(dryRun, filter, keepLast, olderThan, committed)
	if err != nil {
		return usageErrorf("%v", err)
	}

	proj, _, cfg, err := loadProjectConfig(inv.options)
	if err != nil {
		return err
	}

	svc := gcapp.New(gcapp.Deps{})
	appReq := gcapp.Request{Project: proj, Config: cfg}

	// A dry run is the advisory Plan with no deletion; it takes no collector lease. A
	// real run goes through Collect, which takes the lease and plans under it, so the
	// destructive decision is authoritative against the store as it stands under the
	// collector's exclusive boundary — never a stale plan built before the lease.
	var result gcdom.GCResult
	if dryRun {
		plan, err := svc.Plan(inv.invCtx(), gcreq, appReq)
		if err != nil {
			return classifyGCError(err)
		}
		result = gcdom.NewResult(plan, nil, plan.Warnings())
	} else {
		result, err = svc.Collect(inv.invCtx(), gcreq, appReq)
		if err != nil {
			return classifyGCError(err)
		}
	}

	if inv.options.JSON {
		if err := w.JSON("gc", gcViewOf(result)); err != nil {
			return err
		}
	} else {
		renderGC(w, result)
	}

	// Exit code reflects what happened, reported above as a bare code (the report is
	// already rendered, so no extra "awa:" line). Storage corruption that blocked safe
	// planning exits with the state-action-required code like doctor. A partial execution
	// where some deletions failed must not report success: a destructive command that
	// left state behind exits non-zero so an agent does not trust a false "ok". A
	// lock-blocked run is a normal "try again later" and exits 0.
	switch {
	case result.Plan().StateActionBlocked():
		return &commandExit{code: int(ExitStateActionRequired)}
	case result.ExecSummary().Failed > 0:
		return &commandExit{code: int(ExitGenericError)}
	default:
		return nil
	}
}

// subsystemFilter resolves the mutually-exclusive *-only flags into a filter. At
// most one may be set; more than one is a usage error.
func subsystemFilter(runsOnly, checkpointsOnly, blobsOnly bool) (gcdom.SubsystemFilter, error) {
	n := 0
	for _, b := range []bool{runsOnly, checkpointsOnly, blobsOnly} {
		if b {
			n++
		}
	}
	if n > 1 {
		return 0, usageErrorf("--runs-only, --checkpoints-only, and --blobs-only are mutually exclusive")
	}
	switch {
	case runsOnly:
		return gcdom.FilterRunsOnly, nil
	case checkpointsOnly:
		return gcdom.FilterCheckpointsOnly, nil
	case blobsOnly:
		return gcdom.FilterBlobsOnly, nil
	default:
		return gcdom.FilterAll, nil
	}
}

// classifyGCError maps a planning/execution error to the right coded exit. A lock
// held past the timeout is the lock-timeout exit (6); a checkpoint, blob, or run
// store whose layout is corrupt surfaces as storage corruption (exit 5); an
// unrecognized conflicting lock is a generic failure (exit 1, not corruption — it
// is another tool's lock, not damaged awa state); anything else is generic too.
func classifyGCError(err error) error {
	if err == nil {
		return nil
	}
	if c := interruptError(err); c != nil {
		return c
	}
	if coded := lockErrorCode(err); coded != nil {
		return coded
	}
	if errors.Is(err, checkpoint.ErrCorruptStore) || errors.Is(err, blob.ErrCorruptBlob) || errors.Is(err, runcache.ErrCorruptStore) {
		return stateActionErrorf("%v", err)
	}
	return genericErrorf("gc failed: %v", err)
}
