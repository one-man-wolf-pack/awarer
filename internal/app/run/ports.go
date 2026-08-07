// Package run is the application service for the run cache.
//
// It owns the run workflow: build the effective input scan, compute the run key,
// look up a cached result and replay it, or execute the command and atomically
// publish a complete entry. It depends only on ports — a scanner, the run store
// and runner (defined in domain/runcache), an executable resolver, a clock, and
// an environment reader — so the filesystem, os/exec, and the JSON store stay
// behind interfaces and the orchestration here stays testable with fakes.
package run

import (
	"context"
	"time"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/projfs"
)

// Scanner produces the input state for a run. It is satisfied by
// *scanner.Service; the run scan never materializes blobs or records a checkpoint,
// it only needs entries, skipped inputs, and the tree hash.
type Scanner interface {
	Scan(ctx context.Context, project projfs.Project, cfg config.Config, scope config.ScanScope, opts scanner.Options) (scanner.Result, error)
}

// Clock reports the current time. It is injected so run ids and timestamps are
// deterministic under test.
type Clock interface {
	Now() time.Time
}

// EnvReader reads process environment variables. Lookup reports the value and
// whether the variable is set, so an unset variable is distinguished from one set
// to the empty string — a distinction the run key preserves. The child's
// environment is built solely from these lookups over the effective allowlist, so
// the command never inherits an unkeyed variable.
type EnvReader interface {
	Lookup(name string) (value string, present bool)
}

// EffectObserver computes the bounded effect-observation identity over the watched
// generated-output roots for a project. It is satisfied by *effectobserve.Service.
// project is the absolute project root; roots are the effective watch-name list,
// which is never empty. It runs read-only, so it is safe on both the run and explain
// paths. It is required by run.New. Alongside the key-folded observation identity it
// returns a runcache.EffectReport — a pure diagnostic side channel (never persisted,
// never keyed) that names a huge watched root when one dominates the observation, so
// the run service can raise a latency diagnostic. Both return types are domain values,
// so this port does not depend on the sibling app subsystem that implements it.
type EffectObserver interface {
	Observe(project string, roots []string) (runcache.EffectObservation, runcache.EffectReport, error)
}

// ExecutableResolver resolves a command's executable to a path and a stat
// signature for the key. It resolves from the execution directory dir using the
// same effective env the command will run with, so the keyed binary matches the
// one that executes. Resolution is best effort: ok is false when the executable
// cannot be located, in which case the run proceeds and the runner surfaces any
// start failure.
type ExecutableResolver interface {
	Resolve(name, dir string, env []string) (path, stat string, ok bool)
}
