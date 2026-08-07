package run

import (
	"context"
	"errors"
	"sort"

	"awarer/internal/domain/runcache"
)

// defaultLogLimit is the number of stored runs "run log" shows when no limit is
// given.
const defaultLogLimit = 20

// LogRequest selects which stored runs to list.
type LogRequest struct {
	// Limit caps the number of entries returned; zero means defaultLogLimit.
	Limit int
	// Command, when non-empty, keeps only runs whose first argv token matches it.
	Command string
}

// LogEntry is one listed run. For a readable run, Entry holds its metadata and
// Healthy reports whether its stored payloads verify. For a run whose metadata is
// corrupt, Corrupt is true and only ID is populated — so a recovery lister can
// still surface the id (to delete) without trusting the bad document. A healthy
// run is one that is not corrupt and whose payloads verify.
type LogEntry struct {
	ID      runcache.RunID
	Entry   runcache.RunEntry
	Corrupt bool
	Healthy bool
}

// Log lists stored runs newest first, optionally filtered by command and capped to
// a limit. It iterates ids from the directory layout so a run with corrupt metadata
// is listed (marked corrupt) rather than failing the whole command. A readable
// run's payloads are verified so a missing/tampered payload shows as unhealthy.
// Both metadata decode and payload verification run only on the entries actually
// returned (the newest within the limit), so the cost is bounded.
func (s *Service) Log(ctx context.Context, req LogRequest) ([]LogEntry, error) {
	ids, err := s.deps.Store.ListRefs(ctx)
	if err != nil {
		return nil, err
	}
	// Ids are time-prefixed, so descending id order is newest-first and total —
	// holding even for corrupt entries whose start time cannot be read.
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() > ids[j].String() })

	limit := req.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	out := make([]LogEntry, 0, limit)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) >= limit {
			break
		}
		entry, err := s.deps.Store.Get(id)
		switch {
		case err == nil:
			if req.Command != "" && !commandMatches(entry, req.Command) {
				continue
			}
			out = append(out, LogEntry{ID: id, Entry: entry, Healthy: s.verifyPayloads(entry) == nil})
		case errors.Is(err, runcache.ErrNotFound):
			continue // a concurrent delete removed it between listing and read
		case errors.Is(err, runcache.ErrIncompatibleEntry):
			continue // an incompatible entry cannot decode into the current model; it is retained
		case errors.Is(err, runcache.ErrCorruptStore):
			// Corrupt metadata: surface the id so it can be cleaned up. A command filter
			// cannot match a run whose command is unreadable, so skip it when filtering.
			if req.Command != "" {
				continue
			}
			out = append(out, LogEntry{ID: id, Corrupt: true})
		default:
			return nil, err
		}
	}
	return out, nil
}

// commandMatches reports whether a run's command matches a "--command" filter.
// The rule is simple and documented in tests: the run's first argv token (the
// executable as typed) must equal the filter exactly.
func commandMatches(e runcache.RunEntry, command string) bool {
	argv := e.KeyInput.Command.Argv
	return len(argv) > 0 && argv[0] == command
}

// sortNewestFirst orders entries by start time descending, breaking ties by id
// descending so the order is total and deterministic even for runs that share a
// timestamp.
func sortNewestFirst(entries []runcache.RunEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].StartedAt.Equal(entries[j].StartedAt) {
			return entries[i].StartedAt.After(entries[j].StartedAt)
		}
		return entries[i].ID.String() > entries[j].ID.String()
	})
}
