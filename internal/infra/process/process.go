// Package process implements the runcache.Runner and the run executable
// resolver over os/exec.
//
// It is the only run-cache package that starts child processes. Commands are run
// directly from an argv vector — never through a shell, and never by interpolating
// arguments into a string — and stdout and stderr are streamed to separate sinks
// so a large output on one stream cannot deadlock the other. A failure to start
// the process is reported as runcache.ErrStartFailed, distinct from a command
// that ran and exited non-zero.
package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"awarer/internal/domain/runcache"
)

// asExitError extracts an *exec.ExitError from a cmd.Wait error. It is the
// portable half of exit mapping; signalInfo, which reads platform wait status, is
// in the per-platform files.
func asExitError(err error) (*exec.ExitError, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee, true
	}
	return nil, false
}

// Runner implements the process execution port.
var _ runcache.Runner = (*Runner)(nil)

// Runner executes commands via os/exec. The zero value is usable; construct one
// with New for clarity at call sites.
type Runner struct{}

// New returns a process runner.
func New() *Runner { return &Runner{} }

// Run starts the command, streams its output to the spec's sinks, waits for it to
// finish, and reports its exit status and timing. A start failure is returned as
// runcache.ErrStartFailed; a process that runs and exits — with any code, or
// killed by a signal — is reported as a RunResult, not an error.
func (r *Runner) Run(ctx context.Context, spec runcache.RunSpec) (runcache.RunResult, error) {
	if len(spec.Argv) == 0 {
		return runcache.RunResult{}, fmt.Errorf("process: empty argv")
	}
	// A nil env is the os/exec sentinel for "inherit the parent's full environment".
	// For a cache runner that is an unkeyed-environment leak: the child would see
	// variables the key never captured. The caller must pass an explicit (possibly
	// empty) sanitized environment, so reject nil rather than silently inherit.
	if spec.Env == nil {
		return runcache.RunResult{}, fmt.Errorf("process: spec.Env must not be nil (pass an explicit sanitized environment)")
	}

	// Resolve the executable relative to the execution directory, not awa's own
	// cwd, so a relative PATH entry (e.g. node_modules/.bin or ".") and a relative
	// executable resolve from where the command actually runs — and to the same
	// binary the cache key was computed over. The app passes the already-resolved
	// path; fall back to the same dir-aware lookup when it could not resolve one.
	path := spec.Path
	if path == "" {
		p, ok := lookExecutable(spec.Argv[0], spec.Dir, spec.Env)
		if !ok {
			return runcache.RunResult{}, fmt.Errorf("%w: %s: executable not found", runcache.ErrStartFailed, spec.Argv[0])
		}
		path = p
	}

	cmd := exec.CommandContext(ctx, path)
	// argv[0] keeps the raw token as typed, the conventional invocation name; only
	// the resolved path drives which binary runs.
	cmd.Args = append([]string(nil), spec.Argv...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env // never nil here: the child sees exactly the sanitized env
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr

	switch spec.Stdin {
	case runcache.StdinTTY, runcache.StdinPipe:
		// Inherit the real stdin: an interactive TTY under --allow-tty, or a pipe/
		// file whose data the command must receive (such a run is never cached).
		cmd.Stdin = os.Stdin
	default:
		// A nil Stdin makes os/exec connect the child to the null device, so a
		// cacheable run never blocks waiting for interactive input.
		cmd.Stdin = nil
	}

	// Round(0) strips the monotonic clock reading, leaving only the wall-clock value.
	// A run entry is a persisted historical record: its duration must be the wall-clock
	// span that is serialized, not a monotonic span. Keeping the monotonic reading would
	// make DurationMs() (a Sub of monotonic readings) disagree with the duration recom-
	// puted from the serialized wall-clock timestamps on load, which a clock adjustment
	// between these two reads can shift by a millisecond — surfacing as false corruption.
	started := time.Now().Round(0)
	if err := cmd.Start(); err != nil {
		return runcache.RunResult{}, fmt.Errorf("%w: %v", runcache.ErrStartFailed, err)
	}
	waitErr := cmd.Wait()
	finished := time.Now().Round(0)

	exit, err := exitStatus(waitErr)
	if err != nil {
		return runcache.RunResult{}, err
	}
	return runcache.RunResult{Exit: exit, StartedAt: started, FinishedAt: finished}, nil
}

// exitStatus maps the error from cmd.Wait into an ExitStatus. A nil error is a
// clean exit; an *exec.ExitError carries the code and, on a signaled process, the
// signal; any other error is an awa-side wait failure rather than a command
// result.
func exitStatus(waitErr error) (runcache.ExitStatus, error) {
	if waitErr == nil {
		return runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0}, nil
	}
	ee, ok := asExitError(waitErr)
	if !ok {
		return runcache.ExitStatus{}, fmt.Errorf("waiting for command: %w", waitErr)
	}
	if name, code, signaled := signalInfo(ee); signaled {
		return runcache.ExitStatus{Kind: runcache.ExitSignaled, Code: code, Signal: name}, nil
	}
	return runcache.ExitStatus{Kind: runcache.ExitNormal, Code: ee.ExitCode()}, nil
}

// Resolve locates the command's executable and returns its path and a stat
// signature for the run key, resolving from the execution directory and the
// command's own env so the keyed binary matches the one Run will execute. It is
// best effort: a name that cannot be located returns ok=false, and the run still
// proceeds so the runner can surface any start failure.
//
// A nil env is rejected (ok=false) for the same reason Run rejects it: resolving a
// bare executable through nil env would read PATH/PATHEXT from awa's own
// environment — keying the binary against an unkeyed env while the command runs
// under the sanitized one. The caller must pass the same sanitized env Run gets.
func (r *Runner) Resolve(name, dir string, env []string) (string, string, bool) {
	if env == nil {
		return "", "", false
	}
	path, ok := lookExecutable(name, dir, env)
	if !ok {
		return "", "", false
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", "", false
	}
	stat := fmt.Sprintf("size=%d mode=%o mtime=%d", info.Size(), info.Mode().Perm(), info.ModTime().UnixNano())
	return path, stat, true
}
