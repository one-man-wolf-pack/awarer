package timeline_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/timeline"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
)

type fakeSnaps struct {
	headers  []checkpoint.CheckpointHeader
	findings []checkpoint.ReadFinding
}

func (f fakeSnaps) StoreHealth(context.Context) (checkpoint.CheckpointStoreHealth, error) {
	return checkpoint.NewCheckpointStoreHealth(f.headers, f.findings), nil
}

type fakeRuns struct{ recs []timeline.RunRecord }

func (f fakeRuns) History(context.Context, int) ([]timeline.RunRecord, error) { return f.recs, nil }

type fakeDiff struct{}

func (fakeDiff) Counts(context.Context, checkpoint.CheckpointID, checkpoint.CheckpointID) (timeline.ChangeCounts, error) {
	return timeline.ChangeCounts{Added: 1, Modified: 2}, nil
}

type fakeGitHistory struct {
	commits []timeline.GitCommitBoundary
	err     error
	gotFrom time.Time
	gotTo   time.Time
	called  bool
}

func (f *fakeGitHistory) CommitsBetween(_ context.Context, since, until time.Time) ([]timeline.GitCommitBoundary, error) {
	f.gotFrom, f.gotTo, f.called = since, until, true
	return f.commits, f.err
}

func header(t *testing.T, at time.Time) checkpoint.CheckpointHeader {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(strings.NewReader(strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint.CheckpointHeader{ID: id, CreatedAt: at}
}

func recordedRun(t *testing.T, at time.Time, reuse runcache.ReuseState, withAfter bool) timeline.RunRecord {
	t.Helper()
	h, _ := hashing.ParseTreeHash("blake3:" + strings.Repeat("0", 64))
	id, _ := runcache.NewRunID(at.UnixNano(), strings.NewReader("0123456789abcdef"))
	ki := runcache.KeyInput{Command: runcache.Command{Argv: []string{"go", "test"}}, InputTreeHash: h, Platform: runcache.Platform{GOOS: "linux"}}
	e := runcache.RunEntry{
		ID:         id,
		KeyInput:   ki,
		StartedAt:  at,
		FinishedAt: at.Add(time.Second),
		Exit:       runcache.ExitStatus{Kind: runcache.ExitNormal, Code: 0},
		Reuse:      reuse,
		Before:     &runcache.Observation{Manifest: runcache.ManifestRef{File: "before.manifest.jsonl", TreeHash: h}},
	}
	if withAfter {
		e.After = &runcache.Observation{Manifest: runcache.ManifestRef{File: "after.manifest.jsonl", TreeHash: h}}
	}
	return timeline.RunRecord{ID: id, Entry: e}
}

func TestTimelineMergesAndLabels(t *testing.T) {
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	checkpoints := fakeSnaps{headers: []checkpoint.CheckpointHeader{header(t, base.Add(time.Hour)), header(t, base)}}
	rec, _ := runcache.RecordOnly(runcache.ReasonRecordOnly)
	runs := fakeRuns{recs: []timeline.RunRecord{recordedRun(t, base.Add(30*time.Minute), rec, true)}}

	svc := timeline.New(checkpoints, runs, nil, fakeDiff{}, nil)
	res, err := svc.Run(context.Background(), timeline.Request{Limit: 0})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two checkpoints + one run + before + after = 5 entries.
	if res.Total != 5 {
		t.Errorf("total = %d, want 5", res.Total)
	}
	// Newest-first: the newer checkpoint is first.
	if res.Entries[0].Kind != timeline.EntryCheckpoint {
		t.Errorf("first kind = %v, want checkpoint", res.Entries[0].Kind)
	}
	// The newer checkpoint has a predecessor, so it carries change counts.
	if res.Entries[0].Counts == nil || res.Entries[0].Counts.Added != 1 {
		t.Errorf("newest checkpoint counts = %+v, want added=1", res.Entries[0].Counts)
	}
	// The oldest checkpoint has no predecessor, so no counts.
	var oldest *timeline.TimelineEntry
	for i := range res.Entries {
		if res.Entries[i].Kind == timeline.EntryCheckpoint {
			oldest = &res.Entries[i]
		}
	}
	if oldest.Counts != nil {
		t.Error("the oldest checkpoint must carry no change counts")
	}
	// Every kind has a machine token and a label.
	kinds := map[string]bool{}
	for _, e := range res.Entries {
		kinds[e.Kind.Machine()] = true
		if e.Kind.Label() == "" {
			t.Error("entry kind has no label")
		}
	}
	for _, want := range []string{"checkpoint", "run", "run_before", "run_after"} {
		if !kinds[want] {
			t.Errorf("timeline missing kind %q", want)
		}
	}
}

func TestTimelineInterleavesGitCommitBoundaries(t *testing.T) {
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	// Two checkpoints straddling one git commit.
	checkpoints := fakeSnaps{headers: []checkpoint.CheckpointHeader{header(t, base.Add(time.Hour)), header(t, base)}}
	git := &fakeGitHistory{commits: []timeline.GitCommitBoundary{
		{Commit: "full1", ShortCommit: "abc123", Subject: "fix parser", Committed: base.Add(30 * time.Minute)},
	}}
	svc := timeline.New(checkpoints, nil, nil, nil, git)
	res, err := svc.Run(context.Background(), timeline.Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !git.called {
		t.Fatal("git history should have been queried")
	}
	// Window bounded to the awa records' span.
	if !git.gotFrom.Equal(base) || !git.gotTo.Equal(base.Add(time.Hour)) {
		t.Errorf("git window = [%v, %v], want [%v, %v]", git.gotFrom, git.gotTo, base, base.Add(time.Hour))
	}
	// Exactly one git-commit entry, ordered between the two checkpoints (newest-first:
	// newer checkpoint, git commit, older checkpoint).
	var gitEntries []timeline.TimelineEntry
	for _, e := range res.Entries {
		if e.Kind == timeline.EntryGitCommit {
			gitEntries = append(gitEntries, e)
		}
	}
	if len(gitEntries) != 1 {
		t.Fatalf("got %d git-commit entries, want 1", len(gitEntries))
	}
	if res.Entries[0].Kind != timeline.EntryCheckpoint ||
		res.Entries[1].Kind != timeline.EntryGitCommit ||
		res.Entries[2].Kind != timeline.EntryCheckpoint {
		t.Fatalf("order = %v/%v/%v, want checkpoint/git_commit/checkpoint",
			res.Entries[0].Kind, res.Entries[1].Kind, res.Entries[2].Kind)
	}
	// A git-commit entry is a context marker, never an addressable awa state reference.
	if gitEntries[0].Ref != "" {
		t.Errorf("git-commit entry Ref = %q, want empty (not addressable)", gitEntries[0].Ref)
	}
	if gitEntries[0].Kind.Machine() != "git_commit" {
		t.Errorf("git-commit machine token = %q", gitEntries[0].Kind.Machine())
	}
}

func TestTimelineSurfacesGitBoundaryError(t *testing.T) {
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	checkpoints := fakeSnaps{headers: []checkpoint.CheckpointHeader{header(t, base.Add(time.Hour)), header(t, base)}}
	git := &fakeGitHistory{err: errors.New("git log failed: bad object HEAD")}
	svc := timeline.New(checkpoints, nil, nil, nil, git)
	res, err := svc.Run(context.Background(), timeline.Request{})
	if err != nil {
		t.Fatalf("a git failure must not fail the timeline: %v", err)
	}
	if res.GitBoundaryError == "" {
		t.Error("a genuine git failure should be reported on the result")
	}
	// The timeline still renders its awa records.
	if res.Total != 2 {
		t.Errorf("total = %d, want 2 (the checkpoints still render)", res.Total)
	}
	for _, e := range res.Entries {
		if e.Kind == timeline.EntryGitCommit {
			t.Error("a failed git query must contribute no boundary entries")
		}
	}
}

func TestTimelineSkipsIncompatibleRunsWithoutFailing(t *testing.T) {
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	rec, _ := runcache.RecordOnly(runcache.ReasonRecordOnly)
	healthy := recordedRun(t, base, rec, true)
	incompatible := timeline.RunRecord{ID: healthy.ID, Incompatible: true}
	runs := fakeRuns{recs: []timeline.RunRecord{healthy, incompatible}}

	svc := timeline.New(fakeSnaps{}, runs, nil, nil, nil)
	res, err := svc.Run(context.Background(), timeline.Request{})
	if err != nil {
		t.Fatalf("an incompatible run must not fail the timeline: %v", err)
	}
	if res.SkippedRuns != 1 {
		t.Errorf("SkippedRuns = %d, want 1", res.SkippedRuns)
	}
	// The incompatible run contributes no entries; only the healthy run's do.
	for _, e := range res.Entries {
		if e.Kind == timeline.EntryGitCommit {
			t.Fatal("no git port was wired")
		}
	}
	if len(res.Entries) == 0 {
		t.Error("the healthy run should still contribute entries")
	}
}

func TestTimelineSkipsGitQueryWhenNoAwaRecords(t *testing.T) {
	git := &fakeGitHistory{commits: []timeline.GitCommitBoundary{{ShortCommit: "x", Committed: time.Now()}}}
	svc := timeline.New(fakeSnaps{}, nil, nil, nil, git)
	res, err := svc.Run(context.Background(), timeline.Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if git.called {
		t.Error("git history should not be queried when there are no awa records to interleave")
	}
	if res.Total != 0 {
		t.Errorf("total = %d, want 0", res.Total)
	}
}

func TestTimelinePostScanFailedHasNoAfter(t *testing.T) {
	base := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	runs := fakeRuns{recs: []timeline.RunRecord{recordedRun(t, base, runcache.UnknownPostState(), false)}}
	svc := timeline.New(fakeSnaps{}, runs, nil, nil, nil)
	res, err := svc.Run(context.Background(), timeline.Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, e := range res.Entries {
		if e.Kind == timeline.EntryRunAfter {
			t.Error("a post-scan-failed run must contribute no after-observation entry")
		}
	}
}
