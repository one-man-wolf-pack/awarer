package cli

import (
	"encoding/json"
	"testing"

	"awarer/internal/domain/runcache"
)

// TestChangedPathsViewProjectsAbsentSample pins the projection boundary for an
// absent changed-path sample. A near miss whose reason cannot yield a sample carries
// no *ChangedPathSample at all, and that absence is the only representation of
// "unavailable" — the domain has no constructor for it. The machine surface must
// still emit the stable token and an empty item array rather than an omitted field, a
// null, or a completeness borrowed from a computed sample.
func TestChangedPathsViewProjectsAbsentSample(t *testing.T) {
	doc := changedPathsView(nil)
	if doc.Completeness != "unavailable" {
		t.Errorf("completeness = %q, want %q", doc.Completeness, "unavailable")
	}
	if doc.Shown != 0 || doc.Total != 0 {
		t.Errorf("absent sample claims shown=%d total=%d, want 0/0", doc.Shown, doc.Total)
	}
	if doc.Items == nil {
		t.Fatal("items must be an empty array, not null")
	}
	if len(doc.Items) != 0 {
		t.Errorf("items = %v, want empty", doc.Items)
	}
	// The encoded bytes are what an agent reads, so check the array survives encoding
	// as [] rather than being omitted or nulled.
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling changed-paths doc: %v", err)
	}
	if got, want := string(b), `{"completeness":"unavailable","shown":0,"total":0,"items":[]}`; got != want {
		t.Errorf("encoded absent sample = %s, want %s", got, want)
	}
}

// TestChangedPathsViewProjectsComputedSample is the positive half: a sample that
// exists reports the completeness it was constructed with and its true total, so a
// truncated prefix is never read as the whole change set.
func TestChangedPathsViewProjectsComputedSample(t *testing.T) {
	sample, err := runcache.NewChangedPathSample([]runcache.ChangedPath{
		{Path: "src/a.go", Status: "M"},
	}, 4)
	if err != nil {
		t.Fatalf("NewChangedPathSample: %v", err)
	}
	doc := changedPathsView(&sample)
	if doc.Completeness != "truncated" {
		t.Errorf("completeness = %q, want %q", doc.Completeness, "truncated")
	}
	if doc.Shown != 1 || doc.Total != 4 {
		t.Errorf("shown/total = %d/%d, want 1/4", doc.Shown, doc.Total)
	}
	if len(doc.Items) != 1 || doc.Items[0].Path != "src/a.go" || doc.Items[0].Status != "M" {
		t.Errorf("items = %v, want one M src/a.go", doc.Items)
	}
}
