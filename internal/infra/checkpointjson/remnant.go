package checkpointjson

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/infra/fsx"
)

// A publisher leaves a manifest, a frontier, and at most one temporary file per
// publish step; a directory past this bound is not certified as its remnant.
const maxRemnantFiles = 16

type RemnantState int

const (
	RemnantAbsent RemnantState = iota + 1
	RemnantHeaderPresent
	RemnantEligible
	RemnantUnsupported
	RemnantUninspected
)

type Remnant struct {
	ID     checkpoint.CheckpointID
	Path   string
	State  RemnantState
	Detail string
}

func (r *Repo) EachHeaderless(ctx context.Context, fn func(Remnant) error) error {
	return r.eachIDDir(ctx, func(id checkpoint.CheckpointID, committed bool) error {
		if committed {
			return nil
		}
		rem, _, dir, err := r.inspect(ctx, id)
		if dir != nil {
			_ = dir.Close()
		}
		if err != nil {
			return err
		}
		if rem.State == RemnantAbsent || rem.State == RemnantHeaderPresent {
			return nil
		}
		return fn(rem)
	})
}

// RemoveHeaderless returns its own inspection: any state other than RemnantEligible
// means nothing was removed, and RemnantEligible with a nil error means the directory
// is gone.
//
// The caller must hold collector exclusion across the call. Eligibility is decided here
// rather than taken from an earlier EachHeaderless because a publisher may install its
// header between the two; trusting the earlier answer would delete a committed record.
func (r *Repo) RemoveHeaderless(ctx context.Context, id checkpoint.CheckpointID) (Remnant, error) {
	rem, names, dir, err := r.inspect(ctx, id)
	if err != nil || rem.State != RemnantEligible {
		return rem, err
	}
	// Where the *at primitives are real (darwin, linux, freebsd), unlinking through the
	// descriptor the shape was verified on keeps the removal in the inspected directory;
	// reopening the path could land in another one. The fallback re-resolves the path
	// either way and gives no such binding.
	err = r.removeRemnantFiles(ctx, dir, names)
	_ = dir.Close()
	if err != nil {
		return rem, err
	}
	relDir, err := fsx.RelUnder(r.root, r.dir)
	if err != nil {
		return rem, err
	}
	parent, err := fsx.OpenDirNoFollow(r.root, relDir)
	if err != nil {
		return rem, storeCorrupt(r.dir, err)
	}
	defer func() { _ = parent.Close() }()
	if err := ctx.Err(); err != nil {
		return rem, err
	}
	if err := fsx.RemoveDirAt(parent, id.String()); err != nil {
		return rem, fmt.Errorf("removing checkpoint directory %s: %w", id, err)
	}
	return rem, nil
}

func (r *Repo) removeRemnantFiles(ctx context.Context, dir *os.File, names []string) error {
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fsx.RemoveAt(dir, name); err != nil {
			return fmt.Errorf("removing %s: %w", filepath.Join(dir.Name(), name), err)
		}
		if err := r.checkFail(failAfterRemnantFileRemoved); err != nil {
			return err
		}
	}
	return nil
}

var errStopInspection = errors.New("checkpointjson: remnant inspection settled")

// The caller closes the returned directory, which is non-nil only for an eligible
// remnant. The error is non-nil only for cancellation or a path that cannot be
// expressed under the root; every other failure is RemnantUninspected, never absence.
func (r *Repo) inspect(ctx context.Context, id checkpoint.CheckpointID) (Remnant, []string, *os.File, error) {
	rem := Remnant{ID: id, Path: r.dirFor(id)}
	if err := ctx.Err(); err != nil {
		return rem, nil, nil, err
	}
	relIDDir, err := fsx.RelUnder(r.root, rem.Path)
	if err != nil {
		return rem, nil, nil, err
	}
	dir, err := fsx.OpenDirNoFollow(r.root, relIDDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			rem.State = RemnantAbsent
			return rem, nil, nil, nil
		}
		rem.State, rem.Detail = RemnantUninspected, storeCorrupt(rem.Path, err).Error()
		return rem, nil, nil, nil
	}
	eligible := false
	defer func() {
		if !eligible {
			_ = dir.Close()
		}
	}()

	// Any node at the header address is a present header, whatever its type and
	// whether or not it decodes: a corrupt or unreadable header is a record to keep.
	if _, _, err := fsx.LstatAt(dir, headerName); err == nil {
		rem.State = RemnantHeaderPresent
		return rem, nil, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		rem.State, rem.Detail = RemnantUninspected, "header presence is undetermined: "+err.Error()
		return rem, nil, nil, nil
	}

	var names []string
	err = fsx.EachDirEntryAt(dir, func(e os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		switch {
		case name == headerName:
			rem.State = RemnantHeaderPresent
			return errStopInspection
		case name != manifestName && name != frontierName && !fsx.IsPublishTempName(name):
			rem.State, rem.Detail = RemnantUnsupported, fmt.Sprintf("holds an unrecognized entry %q", name)
			return errStopInspection
		case len(names) == maxRemnantFiles:
			rem.State, rem.Detail = RemnantUnsupported, fmt.Sprintf("holds more than %d publication files", maxRemnantFiles)
			return errStopInspection
		}
		mode, _, err := fsx.LstatAt(dir, name)
		if err != nil {
			rem.State, rem.Detail = RemnantUninspected, fmt.Sprintf("entry %q cannot be inspected: %v", name, err)
			return errStopInspection
		}
		if !mode.IsRegular() {
			rem.State, rem.Detail = RemnantUnsupported, fmt.Sprintf("entry %q is not a regular file (%s)", name, mode.Type())
			return errStopInspection
		}
		names = append(names, name)
		return nil
	})
	switch {
	case errors.Is(err, errStopInspection):
		return rem, nil, nil, nil
	case err != nil && ctx.Err() != nil:
		return rem, nil, nil, ctx.Err()
	case err != nil:
		rem.State, rem.Detail = RemnantUninspected, "directory listing is incomplete: "+err.Error()
		return rem, nil, nil, nil
	}
	rem.State, eligible = RemnantEligible, true
	return rem, names, dir, nil
}
