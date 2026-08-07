// Package runevidence is the application builder for the run-evidence aggregate. It
// composes one validated run record with the before/after observed-state assessments
// the shared state assessor produces — exactly as `awa state resolve run:<id>:before|
// after` would — and typed output inspectability obtained without reading payload
// bytes. It is a stored-only read: it never scans the current worktree or loads
// current project config, so durable evidence stays inspectable when config is broken.
package runevidence

import (
	"context"

	"awarer/internal/app/state"
	"awarer/internal/app/stateprovider"
	"awarer/internal/domain/provider"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
)

// RunStore is the narrow port the builder needs: load a validated run record and stat
// its captured streams' inspectability without reading their bytes. The run store
// satisfies it.
type RunStore interface {
	Get(id runcache.RunID) (runcache.RunEntry, error)
	OutputInspectability(id runcache.RunID) (stdout, stderr runevidence.OutputInspectability, err error)
}

// Service builds run-evidence aggregates.
type Service struct {
	runs     RunStore
	assessor *stateprovider.Assessor
}

// NewService wires the builder over a run store and the shared state assessor. The
// assessor's resolver must carry the run-observation port so run:<id>:before|after
// references resolve, exactly as the state commands' composition root wires it.
func NewService(runs RunStore, assessor *stateprovider.Assessor) *Service {
	return &Service{runs: runs, assessor: assessor}
}

// Build composes the run-evidence aggregate for one run id. It loads the validated
// record (run-metadata corruption stays a loud storage error, never a partial
// aggregate), resolves the before/after observed-state assessments through the shared
// assessor so they are byte-identical to `state resolve`, and stats output
// inspectability without reading payload bytes. A run GC-removed between the record
// load and an observation resolution yields a typed unavailable assessment for that
// side, not a fault — each assessment is internally complete.
func (s *Service) Build(ctx context.Context, id runcache.RunID) (runevidence.Evidence, error) {
	record, err := s.runs.Get(id)
	if err != nil {
		return runevidence.Evidence{}, err
	}
	before, err := s.assessObservation(ctx, id, true)
	if err != nil {
		return runevidence.Evidence{}, err
	}
	after, err := s.assessObservation(ctx, id, false)
	if err != nil {
		return runevidence.Evidence{}, err
	}
	// Stat the canonical payload files directly (no second metadata read), so a run
	// GC-removed mid-build degrades to missing streams, never a hard not-found error.
	stdout, stderr, err := s.runs.OutputInspectability(record.ID)
	if err != nil {
		return runevidence.Evidence{}, err
	}
	return runevidence.New(record, before, after, stdout, stderr)
}

// assessObservation resolves one before/after observed-state assessment through the
// shared assessor using the full-id reference form (so the run id is unambiguous). It
// routes through the identical resolve+verify path `state resolve run:<id>:before|
// after` uses, guaranteeing the nested assessment matches the standalone command by
// construction rather than by a parallel projector.
func (s *Service) assessObservation(ctx context.Context, id runcache.RunID, before bool) (provider.Assessment, error) {
	// Build the reference through the aggregate's own authority so the assessment's
	// echoed InputRef is exactly what runevidence.New will validate against.
	refTok := runevidence.RunObservationRef(id, before)
	ref, err := state.ParseRef(refTok)
	if err != nil {
		return provider.Assessment{}, err
	}
	// Stored-only: an empty NowContext is never consulted for a run observation, which
	// resolves entirely from durable records.
	return s.assessor.Resolve(ctx, ref, refTok, state.NowContext{})
}
