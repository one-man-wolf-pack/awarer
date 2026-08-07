package state

import (
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/worktree"
)

// StateEndpoint is the review-facing description of one side of a resolved range:
// what kind of state it is and the facts that name it (a checkpoint's id, checkpoint
// message, and creation time; a run observation's id and side). It carries no
// formatting — the CLI decides how to render the age and label — so the same facts
// can drive human headers now and machine surfaces later without duplication.
type StateEndpoint struct {
	Kind         Kind
	RequestedRef string
	CanonicalRef string

	// Checkpoint facts (set when Kind == KindCheckpoint).
	CheckpointID  checkpoint.CheckpointID
	HasCheckpoint bool
	Message       string
	CreatedAt     time.Time
	HasCreatedAt  bool

	// Checkpoint git facts (set when the baseline checkpoint captured git metadata with
	// a commit). GitCommit is the full commit hash the checkpoint was taken against; it
	// is the seam the CLI reads to classify the baseline against the current git HEAD,
	// so baseline-freshness never re-reads the checkpoint header. HasGit is false for a
	// non-git or commit-less checkpoint, or a non-checkpoint endpoint.
	GitCommit      string
	GitShortCommit string
	HasGit         bool

	// Run-observation facts (set when Kind == KindRunObservation).
	RunID  string
	RunSel RunObservationSel
	HasRun bool
}

// endpointFrom projects a resolved state into its review-facing endpoint facts.
func endpointFrom(s *ResolvedState) StateEndpoint {
	if s == nil {
		return StateEndpoint{}
	}
	e := StateEndpoint{Kind: s.Kind, RequestedRef: s.RequestedRef, CanonicalRef: s.CanonicalRef}
	switch s.Kind {
	case KindCheckpoint:
		if id, ok := s.CheckpointID(); ok {
			e.CheckpointID = id
			e.HasCheckpoint = true
		}
		if s.Header != nil {
			e.Message = s.Header.Message.FirstLine()
			e.CreatedAt = s.Header.CreatedAt
			e.HasCreatedAt = true
			if g := s.Header.Git; g != nil && g.Commit != "" {
				e.GitCommit = g.Commit
				e.GitShortCommit = g.ShortCommit
				e.HasGit = true
			}
		}
	case KindRunObservation:
		if id, sel, _, _, ok := s.RunObservation(); ok {
			e.RunID = id
			e.RunSel = sel
			e.HasRun = true
		}
	}
	return e
}

// StateRangeSummary is the resolved baseline/range of a changes or diff, as the
// facts a human context header needs: each endpoint and the path filter in effect.
// It is built from the same ResolvedState pair the comparison used, so the header
// can never disagree with what was actually compared.
type StateRangeSummary struct {
	Left        StateEndpoint
	Right       StateEndpoint
	PathFilters []string
}

// NewStateRangeSummary builds the summary from the resolved left/right states and the
// path filters applied to the comparison.
func NewStateRangeSummary(left, right *ResolvedState, filters []worktree.RelPath) StateRangeSummary {
	fs := make([]string, 0, len(filters))
	for _, f := range filters {
		fs = append(fs, f.String())
	}
	return StateRangeSummary{Left: endpointFrom(left), Right: endpointFrom(right), PathFilters: fs}
}
