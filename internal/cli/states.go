package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"awarer/internal/app/scanner"
	"awarer/internal/app/state"
	"awarer/internal/domain/blob"
	"awarer/internal/domain/checkpoint"
	domainconfig "awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/blobstore"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/infra/restorestore"
	"awarer/internal/infra/runstore"
	"awarer/internal/infra/worktreefs"
)

// buildStateResolver wires the composition root shared by "awa changes", "awa
// diff", and the "awa status" dirty summary: the checkpoint repository that loads
// stored state, the scanner that produces the ephemeral "now" state, and
// the blob store and hasher that back content reads for diffs. These are read-only
// comparison commands — they compare state, they do not publish it — so the scanner
// is wired through the fail-soft read-only worktree index: a mode=ro open never
// mutates the store, holds no presence lock, and any open/read failure quietly
// degrades to re-hashing (correctness over acceleration). It returns a cleanup that
// closes the index, which the caller must defer.
func buildStateResolver(layout paths.Layout, cfg domainconfig.Config) (*state.Resolver, func(), error) {
	hasher := blake3hash.New()
	idx := &lazyReadOnlyIndex{dir: layout.IndexesDir()}
	res := state.NewResolver(state.Deps{
		Checkpoints: checkpointjson.NewRepo(layout),
		Scanner:     scanner.NewReadOnly(worktreefs.New(), hasher, idx),
		Blobs:       blobstore.New(layout, hasher),
		Hasher:      hasher,
		Runs:        runstore.New(layout, hasher),
		Restores:    restorestore.New(layout, hasher),
	})
	return res, func() { _ = idx.Close() }, nil
}

// classifyStateError maps a state-resolution or comparison error to the right
// exit code: storage corruption (a missing/corrupt blob, a corrupt store) is
// exit 5; an unresolved reference (no checkpoints, an id prefix matching nothing,
// out of range) is not-found; an ambiguous id prefix is a usage error the user can
// fix by being more specific; everything else is generic. A malformed CHECKPOINT
// reference never reaches here — the parser rejects it as usage before resolution —
// but a malformed run id prefix still arrives as its own sentinel below, because run
// references are validated where the run store resolves them.
func classifyStateError(err error) error {
	if c := interruptError(err); c != nil {
		return c
	}
	switch {
	case errors.Is(err, checkpoint.ErrCorruptStore), errors.Is(err, blob.ErrCorruptBlob), errors.Is(err, runcache.ErrCorruptStore), errors.Is(err, restore.ErrCorruptRecoveryStore):
		return stateActionErrorf("%v", err)
	case errors.Is(err, checkpoint.ErrAmbiguousPrefix):
		return usageErrorf("%v\nuse a longer id prefix", err)
	case errors.Is(err, runcache.ErrAmbiguousPrefix):
		return usageErrorf("%v\nuse a longer run id prefix", err)
	case errors.Is(err, restore.ErrRecoveryAmbiguousPrefix):
		return usageErrorf("%v\nuse a longer restore operation id prefix", err)
	case errors.Is(err, runcache.ErrInvalidPrefix):
		return usageErrorf("%v", err)
	case errors.Is(err, state.ErrLatestUnreadable):
		return stateActionErrorf("%v\ntry 'awa log -n 5' to see readable checkpoints, 'awa doctor' to inspect, or %s", err, paths.ResetEvidenceHint())
	case errors.Is(err, state.ErrNoCheckpoints):
		return notFoundErrorf("%v\nrun 'awa checkpoint' to record one first", err)
	case errors.Is(err, runcache.ErrObservationUnavailable):
		return notFoundErrorf("%v", err)
	case errors.Is(err, restore.ErrRecoveryNotFound):
		return notFoundErrorf("%v\na restore recovery observation is retained for [gc].keep_restores_for and may already have been reclaimed", err)
	case errors.Is(err, state.ErrOutOfRange), errors.Is(err, checkpoint.ErrNotFound), errors.Is(err, runcache.ErrNotFound):
		return notFoundErrorf("%v", err)
	default:
		return genericErrorf("%v", err)
	}
}

// parsePathFilters resolves command path arguments to project-root-relative
// filters. Each argument is resolved relative to the caller's cwd, normalized to
// forward slashes under the root, and rejected if it escapes the root. An
// argument naming the root itself is dropped (it would match everything, which
// is the same as no filter).
func parsePathFilters(rootAbs string, args []string) ([]worktree.RelPath, error) {
	if len(args) == 0 {
		return nil, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, genericErrorf("determining current directory: %v", err)
	}
	rootClean := filepath.Clean(rootAbs)
	var filters []worktree.RelPath
	for _, arg := range args {
		abs := arg
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(cwd, arg)
		}
		abs = filepath.Clean(abs)
		relToRoot, err := filepath.Rel(rootClean, abs)
		if err != nil {
			return nil, usageErrorf("invalid path %q: %v", arg, err)
		}
		relToRoot = filepath.ToSlash(relToRoot)
		if relToRoot == ".." || strings.HasPrefix(relToRoot, "../") {
			return nil, usageErrorf("path %q is outside the project root %s", arg, rootClean)
		}
		if relToRoot == "." {
			// The root itself: equivalent to no filter, so do not narrow.
			continue
		}
		p, err := worktree.ParseRelPath(relToRoot)
		if err != nil {
			return nil, usageErrorf("invalid path %q: %v", arg, err)
		}
		filters = append(filters, p)
	}
	return filters, nil
}

// isNumericShortcut reports whether name is a "-N" range shortcut (e.g. "-2").
func isNumericShortcut(name string) bool {
	if len(name) < 2 || name[0] != '-' {
		return false
	}
	for _, r := range name[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// routePositional classifies a token that none of a command's own flags matched.
// A "-N" token or one containing ".." is the single range/shortcut (recorded via
// setRange); any other "-" token is an unknown flag; everything else is a path
// argument (isPath true). It is shared by changes and diff so the two never drift
// on how a positional token is interpreted.
func routePositional(cmd, tok, name string, hasValue bool, setRange func(string) error) (isPath bool, err error) {
	switch {
	case isNumericShortcut(name):
		if err := noValue(name, hasValue); err != nil {
			return false, err
		}
		return false, setRange(name)
	case strings.HasPrefix(tok, "-"):
		return false, usageErrorf("unknown flag %q for %s", tok, cmd)
	case strings.Contains(tok, ".."):
		return false, setRange(tok)
	default:
		return true, nil
	}
}
