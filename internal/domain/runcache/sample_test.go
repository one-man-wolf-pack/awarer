package runcache_test

import (
	"testing"

	"awarer/internal/domain/runcache"
)

func TestChangedPathSampleComplete(t *testing.T) {
	paths := []runcache.ChangedPath{
		{Path: "src/a.go", Status: "M"},
		{Path: "src/b.go", Status: "A"},
	}
	s, err := runcache.NewChangedPathSample(paths, 2)
	if err != nil {
		t.Fatalf("NewChangedPathSample: %v", err)
	}
	if s.Completeness() != runcache.SampleComplete {
		t.Errorf("completeness = %v, want complete", s.Completeness())
	}
	if s.Truncated() {
		t.Error("a complete sample must not report truncated")
	}
	if s.Omitted() != 0 {
		t.Errorf("omitted = %d, want 0", s.Omitted())
	}
	if got := s.Total(); got != 2 {
		t.Errorf("total = %d, want 2", got)
	}
}

func TestChangedPathSampleTruncated(t *testing.T) {
	paths := []runcache.ChangedPath{{Path: "src/a.go", Status: "M"}}
	s, err := runcache.NewChangedPathSample(paths, 5)
	if err != nil {
		t.Fatalf("NewChangedPathSample: %v", err)
	}
	if s.Completeness() != runcache.SampleTruncated {
		t.Errorf("completeness = %v, want truncated", s.Completeness())
	}
	if !s.Truncated() {
		t.Error("a prefix sample must report truncated")
	}
	if s.Omitted() != 4 {
		t.Errorf("omitted = %d, want 4", s.Omitted())
	}
}

// TestChangedPathSampleIsAlwaysComputed proves the constructor's closed range: every
// sample it yields is complete or truncated. There is no constructor for an
// unavailable sample, so a value that exists was always computed; absence is carried
// by an absent *ChangedPathSample at the projection boundary instead.
func TestChangedPathSampleIsAlwaysComputed(t *testing.T) {
	for _, c := range []struct {
		name  string
		paths []runcache.ChangedPath
		total int
	}{
		{"empty and complete", nil, 0},
		{"complete", []runcache.ChangedPath{{Path: "a", Status: "A"}}, 1},
		{"truncated", []runcache.ChangedPath{{Path: "a", Status: "A"}}, 9},
	} {
		s, err := runcache.NewChangedPathSample(c.paths, c.total)
		if err != nil {
			t.Fatalf("%s: NewChangedPathSample: %v", c.name, err)
		}
		if got := s.Completeness(); got != runcache.SampleComplete && got != runcache.SampleTruncated {
			t.Errorf("%s: completeness = %v, want complete or truncated", c.name, got)
		}
	}
}

// TestSampleUnavailableTokenIsStable pins the projection vocabulary an absent sample
// is rendered with. It is the only remaining use of SampleUnavailable: no constructed
// value carries it, but the machine surface still needs one stable token for absence.
func TestSampleUnavailableTokenIsStable(t *testing.T) {
	if got := runcache.SampleUnavailable.String(); got != "unavailable" {
		t.Errorf("SampleUnavailable.String() = %q, want %q", got, "unavailable")
	}
	if runcache.SampleUnavailable == runcache.SampleComplete {
		t.Error("the absent-sample token must stay distinct from a complete empty sample")
	}
}

func TestChangedPathSampleRejectsIllegalShapes(t *testing.T) {
	// More paths than the total is contradictory.
	if _, err := runcache.NewChangedPathSample([]runcache.ChangedPath{
		{Path: "a", Status: "A"},
		{Path: "b", Status: "A"},
	}, 1); err == nil {
		t.Error("accepted a sample with more paths than its total")
	}
	// A negative total is impossible.
	if _, err := runcache.NewChangedPathSample(nil, -1); err == nil {
		t.Error("accepted a negative total")
	}
	// Every entry must carry a path and a status.
	if _, err := runcache.NewChangedPathSample([]runcache.ChangedPath{{Status: "A"}}, 1); err == nil {
		t.Error("accepted an entry without a path")
	}
	if _, err := runcache.NewChangedPathSample([]runcache.ChangedPath{{Path: "a"}}, 1); err == nil {
		t.Error("accepted an entry without a status")
	}
}
