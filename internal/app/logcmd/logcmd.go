// Package logcmd is the application service behind "awa log".
//
// It reads persisted checkpoints newest-first and returns structured data;
// rendering (human, oneline, detail, JSON) is the CLI's job.
package logcmd

import (
	"context"
	"fmt"

	"awarer/internal/domain/checkpoint"
)

// DefaultLimit is how many checkpoints "awa log" shows when no limit is given.
const DefaultLimit = 20

// Service lists checkpoints. Construct it with New.
type Service struct {
	checkpoints checkpoint.Repository
}

// New builds the log service over the checkpoint repository.
func New(checkpoints checkpoint.Repository) *Service {
	return &Service{checkpoints: checkpoints}
}

// Request selects which checkpoints to return.
type Request struct {
	// Limit caps the number returned. A non-positive Limit means DefaultLimit. It
	// is ignored when All or Latest is set.
	Limit int
	// All returns every checkpoint, disabling the limit.
	All bool
	// Latest returns only the newest checkpoint (for detailed "-1" output).
	Latest bool
}

// Result is the selected checkpoint headers plus the total available, so a renderer
// can show "showing N of M". log is a metadata-only command, so it reads headers —
// never the manifests — keeping its cost independent of checkpoint size. Skipped
// counts the incompatible records left out of Entries, so a renderer can warn that
// the listing is partial rather than silently short. Corrupt records are not skipped
// — they fail the whole command (see Run).
type Result struct {
	Entries []checkpoint.CheckpointHeader
	Total   int
	Skipped int
}

// Run returns the readable checkpoints newest-first. It reads headers only, so it
// never materializes a manifest.
//
// It draws the incompatible/corrupt line the store policy requires: a record whose
// schema this build cannot read is intact evidence awa has no reader for, so log
// lists the readable checkpoints and reports the unreadable ones as a skipped count.
// A genuinely corrupt record is durable damage, so log fails loud with ErrCorruptStore
// rather than degrading — a script must never mistake corruption for a normal partial
// listing.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	health, err := s.checkpoints.StoreHealth(ctx)
	if err != nil {
		return Result{}, err
	}
	if n := health.Corrupt(); n > 0 {
		return Result{}, fmt.Errorf("%w: %d checkpoint(s) have corrupt metadata", checkpoint.ErrCorruptStore, n)
	}
	all := health.Headers()

	selected := all
	switch {
	case req.Latest:
		if len(all) > 1 {
			selected = all[:1]
		}
	case req.All:
		// keep all
	default:
		limit := req.Limit
		if limit <= 0 {
			limit = DefaultLimit
		}
		if len(all) > limit {
			selected = all[:limit]
		}
	}

	return Result{Entries: selected, Total: len(all), Skipped: health.Incompatible()}, nil
}
