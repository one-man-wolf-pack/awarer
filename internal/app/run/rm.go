package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"awarer/internal/domain/evidence"
	"awarer/internal/domain/runcache"
)

// RmRequest selects which stored runs to delete. Deletion is either by explicit
// id (one or more ids/prefixes) or by filter (--command and/or --older-than);
// filter deletion requires at least one filter so an empty filter never deletes
// everything.
type RmRequest struct {
	IDs          []string
	Command      string
	OlderThan    time.Duration
	HasOlderThan bool
	DryRun       bool
}

// RmResult reports the runs that were (or, under DryRun, would be) removed. Removed
// holds runs whose metadata was readable; RemovedUnreadable holds the ones removed
// despite metadata that would not decode, so an explicit "run rm <id>" can still
// clean up a record show and replay reject.
type RmResult struct {
	Removed           []runcache.RunEntry
	RemovedUnreadable []UnreadableRun
	DryRun            bool
}

// UnreadableRun is a run removed by id whose metadata would not decode, together with
// why. Damage and a schema this build cannot read are different facts about the
// store — and the documented recovery for the second one sends the user here — so the
// report must not answer "corrupt" for both. Reason carries the same closed
// vocabulary doctor and status use.
type UnreadableRun struct {
	ID     runcache.RunID
	Reason evidence.DiagnosticToken
}

// rmTarget is one run selected for deletion. A target reached by explicit id whose
// metadata will not decode carries only its id and the reason it would not decode.
type rmTarget struct {
	id       runcache.RunID
	entry    runcache.RunEntry
	unusable evidence.DiagnosticToken // empty when the metadata read cleanly
}

// Rm deletes stored runs by id or by filter. It is conservative: an unknown id is
// a not-found error, an ambiguous prefix is reported as such, and a partial
// deletion failure is surfaced rather than hidden. DryRun reports the targets
// without removing anything.
func (s *Service) Rm(ctx context.Context, req RmRequest) (RmResult, error) {
	targets, err := s.rmTargets(ctx, req)
	if err != nil {
		return RmResult{}, err
	}
	res := RmResult{DryRun: req.DryRun}
	// Hold a presence lock across the actual deletions so a concurrent destructive
	// gc does not race the removal. A dry run deletes nothing, so it never locks.
	if !req.DryRun && len(targets) > 0 && s.deps.RmLocks != nil {
		release, err := s.deps.RmLocks.Acquire()
		if err != nil {
			return RmResult{}, err
		}
		defer func() { _ = release() }()
	}
	for _, t := range targets {
		if !req.DryRun {
			if err := s.deps.Store.Delete(t.id); err != nil {
				return res, fmt.Errorf("deleting run %s: %w", t.id.Short(), err)
			}
		}
		if t.unusable != "" {
			res.RemovedUnreadable = append(res.RemovedUnreadable, UnreadableRun{ID: t.id, Reason: t.unusable})
		} else {
			res.Removed = append(res.Removed, t.entry)
		}
	}
	return res, nil
}

// rmTargets resolves the runs a delete request names, in id mode or filter mode.
func (s *Service) rmTargets(ctx context.Context, req RmRequest) ([]rmTarget, error) {
	// Id mode and filter mode are mutually exclusive: an explicit id is deleted
	// outright, so silently ignoring a filter alongside it would let "delete this id
	// if it matches" actually delete the id regardless. Reject the ambiguity rather
	// than guess which the caller meant.
	if len(req.IDs) > 0 && (req.Command != "" || req.HasOlderThan) {
		return nil, fmt.Errorf("run rm: pass either run ids or filters (--command/--older-than), not both")
	}
	if len(req.IDs) > 0 {
		seen := map[string]bool{}
		var out []rmTarget
		for _, ref := range req.IDs {
			id, err := s.deps.Store.Resolve(ctx, ref)
			if err != nil {
				return nil, err
			}
			if seen[id.String()] {
				continue
			}
			seen[id.String()] = true
			// Explicit deletion must work even when metadata will not decode: the id
			// resolved from the directory name, so the run can still be removed by id. Only
			// an undecodable record is tolerated here; a missing run or a real read error
			// still fails.
			entry, err := s.deps.Store.Get(id)
			switch {
			case err == nil:
				out = append(out, rmTarget{id: id, entry: entry})
			case errors.Is(err, runcache.ErrIncompatibleEntry):
				// A schema this build cannot read: the id still resolved from the directory
				// name, so the run can be removed by id even though it will not decode. This
				// is the explicit removal the incompatible-evidence guidance points at, so it
				// must report what it actually found rather than calling the record damaged.
				out = append(out, rmTarget{id: id, unusable: evidence.TokenMetadataIncompatible})
			case errors.Is(err, runcache.ErrCorruptStore):
				out = append(out, rmTarget{id: id, unusable: evidence.TokenMetadataCorrupt})
			default:
				return nil, err
			}
		}
		return out, nil
	}

	// Filter mode needs readable metadata, because the filters match on command and
	// start time, so it lists through the validating read path.
	if req.Command == "" && !req.HasOlderThan {
		return nil, fmt.Errorf("run rm: a filter (--command or --older-than) is required when no run id is given")
	}
	var cutoff time.Time
	if req.HasOlderThan {
		cutoff = s.deps.Clock.Now().Add(-req.OlderThan)
	}
	// List every run id in one directory pass, then decode each on demand and retain
	// only the ones the filter selects — the same "scan all runs" shape doctor and gc
	// use. Retained memory is O(matched entries) + O(all ids), and the walk is a single
	// O(n) pass (not the O(n²) a newest-first iterator would cost when drained whole).
	// The validating read path still fails loudly on the first corrupt/incompatible entry.
	ids, err := s.deps.Store.ListRefs(ctx)
	if err != nil {
		return nil, err
	}
	var matched []runcache.RunEntry
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e, err := s.deps.Store.Get(id)
		if err != nil {
			return nil, err
		}
		if req.Command != "" && !commandMatches(e, req.Command) {
			continue
		}
		if req.HasOlderThan && !e.StartedAt.Before(cutoff) {
			continue
		}
		matched = append(matched, e)
	}
	// Order is restored to start-time-descending so the reported targets are independent
	// of the id-derived listing order.
	sortNewestFirst(matched)
	out := make([]rmTarget, 0, len(matched))
	for _, e := range matched {
		out = append(out, rmTarget{id: e.ID, entry: e})
	}
	return out, nil
}
