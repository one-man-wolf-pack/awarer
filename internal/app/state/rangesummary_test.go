package state

import (
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/worktree"
)

func TestNewStateRangeSummaryCheckpointBaseline(t *testing.T) {
	id, err := checkpoint.ParseCheckpointID("00000000000000000000000000000001")
	if err != nil {
		t.Fatalf("ParseCheckpointID: %v", err)
	}
	msg, err := checkpoint.ParseCheckpointMessage("review checkpoint\nsecond line")
	if err != nil {
		t.Fatalf("ParseCheckpointMessage: %v", err)
	}
	created := time.Unix(1_700_000_000, 0).UTC()
	left := &ResolvedState{
		Kind:         KindCheckpoint,
		RequestedRef: "latest",
		CanonicalRef: "latest",
		Header:       &checkpoint.CheckpointHeader{ID: id, CreatedAt: created, Message: msg},
	}
	right := &ResolvedState{Kind: KindNow, RequestedRef: "now", CanonicalRef: "now"}

	srcPath, err := worktree.ParseRelPath("src")
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	sum := NewStateRangeSummary(left, right, []worktree.RelPath{srcPath})

	if !sum.Left.HasCheckpoint || sum.Left.CheckpointID != id {
		t.Errorf("left checkpoint id = %v (has=%v), want %v", sum.Left.CheckpointID, sum.Left.HasCheckpoint, id)
	}
	if sum.Left.Message != "review checkpoint" {
		t.Errorf("left message = %q, want the first line only", sum.Left.Message)
	}
	if !sum.Left.HasCreatedAt || !sum.Left.CreatedAt.Equal(created) {
		t.Errorf("left created = %v (has=%v), want %v", sum.Left.CreatedAt, sum.Left.HasCreatedAt, created)
	}
	if sum.Right.Kind != KindNow {
		t.Errorf("right kind = %v, want now", sum.Right.Kind)
	}
	if len(sum.PathFilters) != 1 || sum.PathFilters[0] != "src" {
		t.Errorf("path filters = %v, want [src]", sum.PathFilters)
	}
	// A checkpoint with no git metadata carries no baseline commit.
	if sum.Left.HasGit {
		t.Errorf("checkpoint without git metadata should not report HasGit")
	}
}

func TestNewStateRangeSummaryCarriesBaselineGitCommit(t *testing.T) {
	id, err := checkpoint.ParseCheckpointID("00000000000000000000000000000002")
	if err != nil {
		t.Fatalf("ParseCheckpointID: %v", err)
	}
	git := &checkpoint.GitMetadata{InWorktree: true, Commit: "abc123full", ShortCommit: "abc123", Dirty: checkpoint.DirtySummary{Clean: true}}
	left := &ResolvedState{
		Kind:   KindCheckpoint,
		Header: &checkpoint.CheckpointHeader{ID: id, CreatedAt: time.Unix(1, 0).UTC(), Git: git},
	}
	sum := NewStateRangeSummary(left, &ResolvedState{Kind: KindNow}, nil)
	if !sum.Left.HasGit || sum.Left.GitCommit != "abc123full" || sum.Left.GitShortCommit != "abc123" {
		t.Errorf("baseline git = %+v, want commit abc123full/abc123", sum.Left)
	}
}

func TestNewStateRangeSummaryRunObservation(t *testing.T) {
	left := &ResolvedState{
		Kind:         KindRunObservation,
		RequestedRef: "run:abc:before",
		runID:        "abc123",
		runSel:       RunBefore,
	}
	right := &ResolvedState{Kind: KindNow}
	sum := NewStateRangeSummary(left, right, nil)
	if !sum.Left.HasRun || sum.Left.RunID != "abc123" || sum.Left.RunSel != RunBefore {
		t.Errorf("left run observation = %+v, want run abc123 before", sum.Left)
	}
	if sum.Left.HasCheckpoint {
		t.Error("a run observation endpoint must not claim a checkpoint")
	}
}
