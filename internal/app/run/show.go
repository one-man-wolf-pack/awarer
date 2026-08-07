package run

import (
	"context"
	"io"

	"awarer/internal/domain/runcache"
)

// Resolve turns a run id or unambiguous id prefix into a full run id. A malformed
// reference, an unknown one, or an ambiguous prefix is reported distinctly via the
// runcache sentinel errors.
func (s *Service) Resolve(ctx context.Context, ref string) (runcache.RunID, error) {
	return s.deps.Store.Resolve(ctx, ref)
}

// Get loads a stored run entry by id.
func (s *Service) Get(id runcache.RunID) (runcache.RunEntry, error) {
	return s.deps.Store.Get(id)
}

// OpenStdout returns a reader over a run's stored stdout, verified against its
// recorded hash before the first byte is read.
func (s *Service) OpenStdout(id runcache.RunID) (io.ReadCloser, error) {
	return s.deps.Store.OpenStdout(id)
}

// OpenStderr returns a reader over a run's stored stderr, verified against its
// recorded hash before the first byte is read.
func (s *Service) OpenStderr(id runcache.RunID) (io.ReadCloser, error) {
	return s.deps.Store.OpenStderr(id)
}

// LatestRunID resolves the newest run whose metadata is valid, for "run show --last".
// It is a stored-only, metadata-only selection: --last means the newest record with
// valid metadata, not the newest whose payload bytes were eagerly re-hashed. Payload
// health is projected separately as typed inspectability, and explicit output
// selection (--tail/--grep/--stdout/--stderr) performs the byte verification. A newer
// entry whose metadata will not decode is skipped rather than blocking inspection of
// an older readable run, and every skip is reported: returning the newest READABLE
// run while staying silent about newer records would present a partial answer as a
// complete one. It returns runcache.ErrNotFound when no such run exists; it never
// falls back to a record it could not read, or to "now".
func (s *Service) LatestRunID(ctx context.Context) (runcache.RunID, SkippedLatest, error) {
	id, skipped, err := s.latestMatchingRunID(ctx, func(id runcache.RunID) error {
		_, err := s.Get(id)
		return err
	})
	if err != nil {
		return runcache.RunID{}, SkippedLatest{}, err
	}
	if id.IsZero() {
		return runcache.RunID{}, skipped, runcache.ErrNotFound
	}
	return id, skipped, nil
}
