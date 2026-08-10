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

// Request selects which checkpoints to return. Every shape it can express is a
// bounded newest window: the full project history is the timeline's, which "awa log
// --all" reaches directly, so this service has no way to ask for every header.
type Request struct {
	// Limit caps the number returned. A non-positive Limit means DefaultLimit. It
	// is ignored when Latest is set.
	Limit int
	// Latest returns only the newest checkpoint (for detailed "-1" output).
	Latest bool
}

// Result is the selected checkpoint headers plus the total available, so a renderer
// can show "showing N of M". log is a metadata-only command, so it reads headers —
// never the manifests — keeping its cost independent of checkpoint size. Total is the
// exact readable count across the whole store, not the length of Entries: the store
// read retains only the window this command shows. Skipped counts the incompatible
// records left out of Entries, so a renderer can warn that the listing is partial
// rather than silently short. Corrupt records are not skipped — they fail the whole
// command (see Run).
type Result struct {
	Entries []checkpoint.CheckpointHeader
	Total   int
	Skipped int
}

// Run returns the readable checkpoints newest-first. It reads headers only, so it
// never materializes a manifest, and it retains only the window it will show: the
// store read is always the bounded one, asked for exactly the effective limit, and the
// exact totals come from the health counts rather than from the returned entries.
//
// It draws the incompatible/corrupt line the store policy requires: a record whose
// schema this build cannot read is intact evidence awa has no reader for, so log
// lists the readable checkpoints and reports the unreadable ones as a skipped count.
// A genuinely corrupt record is durable damage, so log fails loud with ErrCorruptStore
// rather than degrading — a script must never mistake corruption for a normal partial
// listing. The corrupt count covers the whole store, so damage older than the shown
// window still fails the command.
func (s *Service) Run(ctx context.Context, req Request) (Result, error) {
	health, err := s.checkpoints.StoreHealthNewest(ctx, effectiveLimit(req))
	if err != nil {
		return Result{}, err
	}
	if n := health.Corrupt(); n > 0 {
		return Result{}, fmt.Errorf("%w: %d checkpoint(s) have corrupt metadata", checkpoint.ErrCorruptStore, n)
	}
	return Result{Entries: health.NewestHeaders(), Total: health.Recorded(), Skipped: health.Incompatible()}, nil
}

// effectiveLimit is how many newest checkpoints a request shows: one for the detailed
// "-1" form, otherwise the requested limit or the default. It is the single author of
// that rule, so the window the store retains is exactly the window rendered.
func effectiveLimit(req Request) int {
	if req.Latest {
		return 1
	}
	if req.Limit > 0 {
		return req.Limit
	}
	return DefaultLimit
}
