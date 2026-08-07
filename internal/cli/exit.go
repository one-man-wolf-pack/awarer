package cli

import (
	"context"
	"errors"
	"fmt"

	"awarer/internal/infra/lockfile"
)

// ExitCode is the process exit status returned by a command.
//
// The full set is a fixed public contract so that scripts and agents can rely on
// stable semantics. Every code here is produced by a real command path: there
// are no reserved placeholders. The awa-owned range is 0–6 plus the interrupt
// code 130; "awa run" instead
// passes the wrapped command's own exit code through (see commandExit).
type ExitCode int

const (
	// Success: the command completed without error.
	ExitSuccess ExitCode = 0
	// GenericError: completed with differences, failed validation, or a
	// generic error.
	ExitGenericError ExitCode = 1
	// UsageError: invalid CLI usage (unknown command, bad flag).
	ExitUsageError ExitCode = 2
	// NotFound: something the invocation named does not exist — the project root or
	// config file, or a reference that resolved to nothing (an id prefix matching no
	// checkpoint, an out-of-range @-N, an unknown run or restore id, an observation
	// that was never captured or has since been reclaimed). It is absence, not
	// damage: ExitStateActionRequired is the code for state that is present but
	// needs an action before it can be trusted.
	ExitNotFound ExitCode = 3
	// ConfigError: configuration is present but invalid.
	ExitConfigError ExitCode = 4
	// StateActionRequired: awa's local state needs an action before it can be trusted
	// as it stands. Corruption is one case, not the definition: awa doctor also returns
	// this code for a perfectly harmless finding that simply has not been repaired yet,
	// such as a missing .awa/.gitignore guard. Naming it after corruption would tell a
	// script that data is damaged when the honest reading is "something is waiting for
	// you".
	ExitStateActionRequired ExitCode = 5
	// LockTimeout: a required lock could not be acquired in time.
	ExitLockTimeout ExitCode = 6
	// Interrupted: a non-"awa run" command was cancelled by an OS interrupt (Ctrl+C /
	// SIGINT) through the root context before it could publish authoritative state.
	// 130 = 128+SIGINT is the Unix convention and stays clear of awa's own 0–6 range.
	// "awa run" does not use this code: it passes through its killed child's own
	// signal-derived exit (137 for a SIGKILLed child, 130 only when the child itself
	// took SIGINT), so an interrupted run reports the child's result, not this constant.
	ExitInterrupted ExitCode = 130
)

// exitCodes is the ordered, authoritative catalog of the awa-owned process
// statuses (0–6 plus 130); it does NOT include a wrapped child's own exit, which
// "awa run" passes through separately. It is the single source the "awa help
// exit-codes" page is cross-checked against and that the CLI reference renders from,
// so a code can never ship documented-but-unlisted or listed-but-undocumented. Enum
// values carry no ordinal ordering (130 is far from 0–6), so this explicit slice
// fixes the presentation order; it is the exit-code analogue of configref.Sections().
func exitCodes() []ExitCode {
	return []ExitCode{
		ExitSuccess,
		ExitGenericError,
		ExitUsageError,
		ExitNotFound,
		ExitConfigError,
		ExitStateActionRequired,
		ExitLockTimeout,
		ExitInterrupted,
	}
}

// refName is the short, stable slug for a code, used by the CLI reference. It is
// distinct from the longer "awa help exit-codes" prose (which stays in the topic
// page); this is only a machine label for the generated reference document.
func (c ExitCode) refName() string {
	switch c {
	case ExitSuccess:
		return "success"
	case ExitGenericError:
		return "generic-error"
	case ExitUsageError:
		return "usage-error"
	case ExitNotFound:
		return "not-found"
	case ExitConfigError:
		return "config-error"
	case ExitStateActionRequired:
		return "state-action-required"
	case ExitLockTimeout:
		return "lock-timeout"
	case ExitInterrupted:
		return "interrupted"
	default:
		return "unknown"
	}
}

// commandExit carries the exit code of a command run through "awa run". Unlike a
// codedError it is not an awa failure: the command ran (or a cached result was
// replayed) and its own exit code is what awa returns, with no "awa: " error line.
// Run handles it after a handler returns, so a wrapped command exiting non-zero is
// reported as that code rather than as exit 1.
type commandExit struct {
	code int
}

func (e *commandExit) Error() string { return fmt.Sprintf("command exited with code %d", e.code) }

// codedError is a user-facing error that carries the exit code awa should
// return. Routing and command handlers return these; Run converts them into a
// stderr message plus the process exit status.
type codedError struct {
	code ExitCode
	msg  string
}

func (e *codedError) Error() string { return e.msg }

// Code reports the exit status associated with the error.
func (e *codedError) Code() ExitCode { return e.code }

// usageErrorf builds a codedError with ExitUsageError. Use it for malformed
// invocations: unknown commands, unknown flags, bad flag values.
func usageErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitUsageError, msg: fmt.Sprintf(format, args...)}
}

// genericErrorf builds a codedError with ExitGenericError. Use it for commands
// that fail for an ordinary, non-usage reason.
func genericErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitGenericError, msg: fmt.Sprintf(format, args...)}
}

// jsonHumanModeError rejects combining a human-only text-shaping flag with --json.
// jsonAlreadyHas names what the JSON document already provides, so a single sentence
// template is shared by every such flag (changes/diff --stat, changes --name-only)
// and the wording can never drift between them.
func jsonHumanModeError(flag, jsonAlreadyHas string) *codedError {
	return usageErrorf("%s is a human output mode; --json already %s — use plain --json", flag, jsonAlreadyHas)
}

// jsonTimeFlagError rejects combining the human --time display preference with --json.
// JSON timestamps are always machine UTC RFC3339(Nano), so --time has no effect there;
// rejecting it (rather than validating and ignoring the value) keeps the "no silent
// no-op under --json" contract uniform across every command that accepts --time.
func jsonTimeFlagError() *codedError {
	return usageErrorf("--time controls human time display; JSON always uses UTC RFC3339 timestamps")
}

// notFoundErrorf builds a codedError with ExitNotFound. Use it when a project
// root or config file could not be located.
func notFoundErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitNotFound, msg: fmt.Sprintf(format, args...)}
}

// configErrorf builds a codedError with ExitConfigError. Use it when a config
// file is present but cannot be parsed or fails validation.
func configErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitConfigError, msg: fmt.Sprintf(format, args...)}
}

// stateActionErrorf builds a codedError with ExitStateActionRequired. Use it when
// on-disk state (the worktree index, the store) is unreadable or corrupt — the
// corruption case of that code, as opposed to doctor's unrepaired-finding case.
func stateActionErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitStateActionRequired, msg: fmt.Sprintf(format, args...)}
}

// lockTimeoutErrorf builds a codedError with ExitLockTimeout. Use it when a
// required lock was held by a live owner past the configured timeout, so the
// command truly could not proceed (distinct from state needing an action).
func lockTimeoutErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitLockTimeout, msg: fmt.Sprintf(format, args...)}
}

// interruptedErrorf builds a codedError with ExitInterrupted for the rare
// cancellation that has something to say. interruptError's terse message is right
// when stopping is all that happened; it is wrong when the cancelled command left
// state the operator now has to deal with, and that instruction must not be dropped
// merely because the cause was a Ctrl+C rather than a fault.
func interruptedErrorf(format string, args ...any) *codedError {
	return &codedError{code: ExitInterrupted, msg: fmt.Sprintf(format, args...)}
}

// interruptError returns an ExitInterrupted coded error when err is a root-context
// cancellation (Ctrl+C / SIGINT), else nil. Command error classifiers call it first
// so a cancelled scan, git call, or store read reports the interrupt code once
// rather than being dressed up as a generic, storage, or not-found failure. The
// message is intentionally terse — cancellation is an operator action, not a fault —
// which holds for every command that leaves nothing behind; see interruptedErrorf
// for the one that can.
func interruptError(err error) *codedError {
	if errors.Is(err, context.Canceled) {
		return &codedError{code: ExitInterrupted, msg: "interrupted"}
	}
	return nil
}

// lockErrorCode maps a lock-acquisition failure to its coded exit, or returns nil
// when err is not a lock error so the caller can fall through to its own
// classification. A lock held past the timeout is the lock-timeout exit (6); an
// unrecognized conflicting lock is a generic failure (1, not corruption — it is
// another tool's lock, not damaged awa state). Writers (checkpoint, run, run rm, doctor
// --repair) share this so a collector that outlasts the timeout reports the same
// exit everywhere.
func lockErrorCode(err error) *codedError {
	switch {
	case errors.Is(err, lockfile.ErrLockTimeout):
		return lockTimeoutErrorf("%v", err)
	case errors.Is(err, lockfile.ErrLockUnknown):
		return genericErrorf("%v", err)
	default:
		return nil
	}
}
