package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/compare"
)

type counterpartDoc struct {
	Data struct {
		Summary map[string]int    `json:"summary"`
		Changes []counterpartElem `json:"changes"`
		Files   []counterpartElem `json:"files"`
	} `json:"data"`
}

type counterpartElem struct {
	Status      string `json:"status"`
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Note        string `json:"note"`
	Counterpart *struct {
		Side          string `json:"side"`
		Qualification string `json:"qualification"`
	} `json:"counterpart"`
}

func (e counterpartElem) fact() (string, string) {
	p := e.NewPath
	if e.Status == "deleted" {
		p = e.OldPath
	}
	v := e.Status
	if e.Counterpart != nil {
		v += " " + e.Counterpart.Side + "/" + e.Counterpart.Qualification
	}
	return p, v
}

// counterpartProject edits an ignore file after a checkpoint, so the recorded scan
// config identities still match while gen/ leaves the current observation; it also
// adds and physically removes a file whose absence each side proves.
func counterpartProject(t *testing.T) string {
	t.Helper()
	root := excludeProject(t)
	writeFile(t, root, ".awaignore", "gen/\n")
	writeFile(t, root, "fresh.txt", "fresh\n")
	if err := os.Remove(filepath.Join(root, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	return root
}

var counterpartWant = map[string]string{
	".awaignore": "added left/absent",
	"fresh.txt":  "added left/absent",
	"gen":        "deleted right/outside-observation",
	"gen/a.txt":  "deleted right/outside-observation",
	"gen/b.txt":  "deleted right/outside-observation",
	"keep.txt":   "deleted right/absent",
}

func TestChangesCounterpartAcrossModes(t *testing.T) {
	root := counterpartProject(t)

	code, stdout, stderr := run("changes", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("changes exit = %d, stderr = %q", code, stderr)
	}
	for _, line := range []string{
		"A .awaignore\n", "A fresh.txt\n", "D keep.txt\n",
		"D gen (right: outside observation)\n",
		"D gen/a.txt (right: outside observation)\n",
		"D gen/b.txt (right: outside observation)\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("listing lacks %q:\n%s", line, stdout)
		}
	}
	// Matching identities do not erase the excluded-boundary explanation, and the
	// rows never claim a physical deletion.
	if mismatchWarnings(stderr) != 0 || strings.Contains(stdout, "removed") {
		t.Errorf("stdout = %q stderr = %q", stdout, stderr)
	}

	code, stdout, _ = run("changes", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("changes --json exit = %d", code)
	}
	assertCounterpartJSON(t, stdout, func(d counterpartDoc) []counterpartElem { return d.Data.Changes })

	code, stat, statErr := run("changes", "--root", root, "--stat")
	if code != int(ExitSuccess) || !strings.Contains(stat, "6 changed: 2 added, 0 modified, 4 deleted") {
		t.Errorf("--stat exit = %d stdout:\n%s", code, stat)
	}
	if !strings.Contains(statErr, "note: 3 added/deleted path(s) have no observed counterpart") {
		t.Errorf("--stat stderr = %q, want one bounded counterpart note", statErr)
	}

	code, names, namesErr := run("changes", "--root", root, "--name-only")
	lines := strings.Split(strings.TrimRight(names, "\n"), "\n")
	want := make([]string, 0, len(counterpartWant))
	for p := range counterpartWant {
		want = append(want, p)
	}
	slices.Sort(want)
	if code != int(ExitSuccess) || !slices.Equal(lines, want) {
		t.Errorf("--name-only exit = %d stdout = %q, want only %v", code, names, want)
	}
	if strings.Count(namesErr, "note: 3 added/deleted") != 1 {
		t.Errorf("--name-only stderr = %q, want the counterpart note once", namesErr)
	}
}

func TestDiffCounterpartAcrossModes(t *testing.T) {
	root := counterpartProject(t)

	code, stdout, stderr := run("diff", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("diff exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "diff gen/a.txt (right: outside observation)\n") ||
		!strings.Contains(stdout, "D gen (no content diff, right: outside observation)\n") ||
		!strings.Contains(stdout, "diff keep.txt\n") {
		t.Errorf("diff listing:\n%s", stdout)
	}

	code, stdout, _ = run("diff", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("diff --json exit = %d", code)
	}
	assertCounterpartJSON(t, stdout, func(d counterpartDoc) []counterpartElem { return d.Data.Files })

	code, stat, statErr := run("diff", "--root", root, "--stat")
	if code != int(ExitSuccess) || !strings.Contains(stat, "6 changed: 2 added, 0 modified, 4 deleted") ||
		!strings.Contains(statErr, "note: 3 added/deleted") {
		t.Errorf("diff --stat exit = %d stdout = %q stderr = %q", code, stat, statErr)
	}
}

func assertCounterpartJSON(t *testing.T, stdout string, elems func(counterpartDoc) []counterpartElem) {
	t.Helper()
	var doc counterpartDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	got := map[string]string{}
	for _, e := range elems(doc) {
		p, v := e.fact()
		got[p] = v
		// The fact is separate from the existing note.
		if strings.Contains(e.Note, "outside") || strings.Contains(e.Note, "unknown") {
			t.Errorf("%s: note %q carries the counterpart", p, e.Note)
		}
	}
	if len(got) != len(counterpartWant) {
		t.Errorf("elements = %v, want %v", got, counterpartWant)
	}
	for p, w := range counterpartWant {
		if got[p] != w {
			t.Errorf("%s = %q, want %q", p, got[p], w)
		}
	}
	if doc.Data.Summary["added"] != 2 || doc.Data.Summary["deleted"] != 4 {
		t.Errorf("summary = %v, want unchanged counts", doc.Data.Summary)
	}
}

func TestChangeLineAnnotatesOnlyUnprovedCounterparts(t *testing.T) {
	added := addedChange(t, "x.txt")
	cases := []struct {
		cp   compare.Counterpart
		note string
		want string
	}{
		{compare.Counterpart{}, "", "A x.txt"},
		{compare.Counterpart{Side: compare.SideLeft, Qualification: compare.QualAbsent}, "", "A x.txt"},
		{compare.Counterpart{Side: compare.SideLeft, Qualification: compare.QualSkippedBoundary}, "", "A x.txt (left: skipped boundary)"},
		{compare.Counterpart{Side: compare.SideLeft, Qualification: compare.QualUnknown}, "hash-only", "A x.txt (hash-only, left: coverage unknown)"},
	}
	for _, tc := range cases {
		c := added
		c.Counterpart, c.Note = tc.cp, tc.note
		if got := formatChangeLine(c); got != tc.want {
			t.Errorf("formatChangeLine(%+v) = %q, want %q", tc.cp, got, tc.want)
		}
	}
}
