package gc

import (
	"context"
	"fmt"
	"path/filepath"

	"awarer/internal/domain/checkpoint"
	gcdom "awarer/internal/domain/gc"
	"awarer/internal/domain/hashing"
	restoredom "awarer/internal/domain/restore"
	"awarer/internal/domain/runcache"
	"awarer/internal/infra/fsx"
)

// executor performs a plan's delete actions through the owning repositories. It is
// reached only for a non-dry-run, non-blocked plan, so it never has to re-check
// those gates; its job is to delete in a safe order and record each outcome.
type executor struct {
	ctx   context.Context
	repos repos
	root  string
	// fail is a test-only interruption seam: nil in production (a single branch),
	// set only from same-package _test.go. It is called at the start of each deletion
	// phase with the kind about to be swept; a non-nil error stops the sweep at that
	// phase boundary, simulating a process interruption between phases. Because the
	// order is conservative (checkpoints → runs → blobs → temp), an interruption here
	// leaves only harmless extra state — never a retained record pointing at an
	// already-deleted blob. It cannot be reached from normal CLI usage: there is no
	// flag and no exported setter.
	fail func(kind gcdom.CandidateKind) error
}

// run executes the delete candidates in dependency order — checkpoints, then runs,
// then blobs, then temp — so a retained record never briefly references already
// deleted content, and a partial failure leaves the store consistent. Each
// candidate yields a DeletionResult; a failure is recorded and execution continues.
func (e *executor) run(plan gcdom.GCPlan) []gcdom.DeletionResult {
	dels := plan.Deletions()
	var results []gcdom.DeletionResult
	// Recovery observations are removed before the blob sweep for the same reason
	// checkpoints are: the record must stop referencing bytes before those bytes can
	// be reclaimed, so an interruption between the two phases leaves only extra
	// state, never a record pointing at a swept blob.
	for _, order := range []gcdom.CandidateKind{gcdom.KindCheckpoint, gcdom.KindRun, gcdom.KindRestore, gcdom.KindBlob, gcdom.KindTemp} {
		if e.fail != nil {
			if err := e.fail(order); err != nil {
				// Interrupted at this phase boundary: return what has already been
				// deleted. Earlier phases (higher in the dependency order) are done,
				// leaving only harmless extra state for a later gc or doctor to reclaim.
				return results
			}
		}
		for _, c := range dels {
			if c.Kind() != order {
				continue
			}
			results = append(results, e.delete(c))
		}
	}
	return results
}

// delete dispatches one candidate to the repository that owns its kind and returns
// the typed outcome.
func (e *executor) delete(c gcdom.GCCandidate) gcdom.DeletionResult {
	var err error
	switch c.Kind() {
	case gcdom.KindCheckpoint:
		err = e.deleteCheckpoint(c)
	case gcdom.KindRun:
		err = e.deleteRun(c)
	case gcdom.KindBlob:
		err = e.deleteBlob(c)
	case gcdom.KindRestore:
		err = e.deleteRestore(c)
	case gcdom.KindTemp:
		err = e.deleteTemp(c)
	default:
		err = fmt.Errorf("gc: cannot delete candidate of kind %s", c.Kind())
	}
	if err != nil {
		return gcdom.NewDeletionResult(c, 0, err)
	}
	return gcdom.NewDeletionResult(c, c.Bytes(), nil)
}

// deleteRestore reclaims one recovery observation. Its blobs are not removed here:
// they are ordinary content-addressed blobs and are reclaimed by the sweep only if
// nothing else references them.
func (e *executor) deleteRestore(c gcdom.GCCandidate) error {
	id, err := restoredom.ParseOperationID(c.ID())
	if err != nil {
		return fmt.Errorf("parsing restore operation id %q: %w", c.ID(), err)
	}
	return e.repos.restores.Delete(id)
}

func (e *executor) deleteCheckpoint(c gcdom.GCCandidate) error {
	id, err := checkpoint.ParseCheckpointID(c.ID())
	if err != nil {
		return fmt.Errorf("parsing checkpoint id %q: %w", c.ID(), err)
	}
	return e.repos.checkpoints.Delete(id)
}

func (e *executor) deleteRun(c gcdom.GCCandidate) error {
	id, err := runcache.ParseRunID(c.ID())
	if err != nil {
		return fmt.Errorf("parsing run id %q: %w", c.ID(), err)
	}
	return e.repos.runs.Delete(id)
}

func (e *executor) deleteBlob(c gcdom.GCCandidate) error {
	h, err := hashing.ParseContentHash(c.ID())
	if err != nil {
		return fmt.Errorf("parsing blob hash %q: %w", c.ID(), err)
	}
	return e.repos.blobs.Delete(h)
}

// deleteTemp removes a temp artifact through a verified, symlink-free parent
// directory descriptor, so the unlink cannot be redirected outside the project. The
// candidate path is the artifact's full on-disk path; its parent is reached
// no-follow from the project root and the leaf is removed by name.
func (e *executor) deleteTemp(c gcdom.GCCandidate) error {
	parent := filepath.Dir(c.Path())
	name := filepath.Base(c.Path())
	relParent, err := fsx.RelUnder(e.root, parent)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(e.root, relParent)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return fsx.RemoveTreeAt(dir, name)
}
