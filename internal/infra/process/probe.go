package process

import (
	"context"
	"errors"
	"os/exec"
	"sync/atomic"
	"time"

	"awarer/internal/domain/runcache"
)

var _ runcache.Prober = (*Runner)(nil)

// probeWaitDelay bounds how long Wait keeps reading a probe's pipes after the probe
// exited or was killed. Without it, a descendant that inherited stdout or stderr keeps
// Wait blocked for as long as it lives, so the probe's own timeout would not bound
// the acquisition.
const probeWaitDelay = 500 * time.Millisecond

func (r *Runner) Probe(ctx context.Context, spec runcache.ProbeSpec) runcache.ProbeResult {
	fail := func(st runcache.FactStatus) runcache.ProbeResult { return runcache.ProbeResult{Status: st} }
	if len(spec.Argv) == 0 || spec.Env == nil || spec.Timeout <= 0 {
		return fail(runcache.FactStartFailed)
	}
	path, ok := lookExecutable(spec.Argv[0], spec.Dir, spec.Env)
	if !ok {
		return fail(runcache.FactMissingExecutable)
	}
	pctx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	stdout := &boundedSink{limit: spec.MaxStdout, cancel: cancel, keep: true}
	stderr := &boundedSink{limit: spec.MaxStderr, cancel: cancel}
	cmd := exec.CommandContext(pctx, path)
	cmd.Args = append([]string(nil), spec.Argv...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = probeWaitDelay
	if err := cmd.Start(); err != nil {
		return fail(runcache.FactStartFailed)
	}
	waitErr := cmd.Wait()

	switch {
	case stdout.overflow.Load():
		return fail(runcache.FactStdoutOverflow)
	case stderr.overflow.Load():
		return fail(runcache.FactStderrOverflow)
	case pctx.Err() != nil, errors.Is(waitErr, exec.ErrWaitDelay):
		return fail(runcache.FactTimedOut)
	}
	exit, err := exitStatus(waitErr)
	switch {
	case err != nil:
		return fail(runcache.FactReadFailed)
	case exit.Kind == runcache.ExitSignaled:
		return fail(runcache.FactSignaled)
	case exit.Code != 0:
		return fail(runcache.FactExitNonzero)
	case len(stdout.buf) == 0:
		return fail(runcache.FactEmptyStdout)
	}
	return runcache.ProbeResult{Status: runcache.FactAcquired, Stdout: stdout.buf}
}

// errProbeOverflow stops exec's copy from an overflowing stream. exec then closes its
// read end, so a writer still holding the pipe, the probe or a descendant, gets a
// broken pipe instead of having its output drained until the probe ends.
var errProbeOverflow = errors.New("identity probe output exceeded its bound")

type boundedSink struct {
	limit    int64
	n        int64
	keep     bool
	buf      []byte
	overflow atomic.Bool
	cancel   context.CancelFunc
}

func (s *boundedSink) Write(p []byte) (int, error) {
	if s.overflow.Load() {
		return 0, errProbeOverflow
	}
	if s.n+int64(len(p)) > s.limit {
		s.overflow.Store(true)
		s.buf = nil
		s.cancel()
		return 0, errProbeOverflow
	}
	s.n += int64(len(p))
	if s.keep {
		s.buf = append(s.buf, p...)
	}
	return len(p), nil
}
