package cli

import (
	"encoding/json"
	"strings"
	"testing"

	domdoctor "awarer/internal/domain/doctor"
)

// TestStoreCapacityProjection pins how each measurement state reaches JSON and the
// human line: a figure only for exact and partial, a threshold only while enabled,
// gaps as closed tokens, and partial worded as a lower bound.
func TestStoreCapacityProjection(t *testing.T) {
	must := func(c domdoctor.StoreCapacity, err error) domdoctor.StoreCapacity {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	cases := []struct {
		name     string
		c        domdoctor.StoreCapacity
		wantJSON string
		wantLine []string
	}{
		{"known zero", must(domdoctor.MeasuredStoreCapacity(0, 1<<30)),
			`{"scope":".awa","metric":"logical-bytes","measurement":"exact","observed_bytes":0,"threshold_bytes":1073741824,"incomplete_reasons":[]}`,
			[]string{"store: 0 B of logical file data in .awa/", "warning at 1.0 GiB"}},
		{"partial", must(domdoctor.MeasuredStoreCapacity(2048, 1<<30, domdoctor.GapWorkBudget)),
			`{"scope":".awa","metric":"logical-bytes","measurement":"partial","observed_bytes":2048,"threshold_bytes":1073741824,"incomplete_reasons":["work-budget"]}`,
			[]string{"at least 2.0 KiB", "(partial: work-budget)"}},
		{"unmeasured", must(domdoctor.UnmeasuredStoreCapacity(1 << 30)),
			`{"scope":".awa","metric":"logical-bytes","measurement":"unmeasured","observed_bytes":null,"threshold_bytes":1073741824,"incomplete_reasons":["unavailable"]}`,
			[]string{"size unmeasured (unavailable)"}},
		{"disabled", domdoctor.DisabledStoreCapacity(),
			`{"scope":".awa","metric":"logical-bytes","measurement":"disabled","observed_bytes":null,"threshold_bytes":null,"incomplete_reasons":[]}`,
			[]string{"size advisory off"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(storeCapacityViewOf(c.c))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.wantJSON {
				t.Errorf("JSON = %s\nwant   %s", got, c.wantJSON)
			}
			line := storeCapacityLine(c.c)
			for _, want := range c.wantLine {
				if !strings.Contains(line, want) {
					t.Errorf("line %q lacks %q", line, want)
				}
			}
		})
	}
}

// TestWatchedRootsProjection pins the watched-root audit's JSON and human shape: null
// when unrequested, and when requested three separate coverage facts — the search,
// each root's measurement, and the listed detail — with no total from an incomplete
// search and no threshold count without a threshold.
func TestWatchedRootsProjection(t *testing.T) {
	if v := watchedRootsViewOf(domdoctor.WatchedRootAudit{}); v != nil {
		t.Fatalf("unrequested audit projected as %+v, want null", v)
	}
	if lines := watchedRootsLines(domdoctor.WatchedRootAudit{}); len(lines) != 0 {
		t.Fatalf("unrequested audit printed %q", lines)
	}
	root := func(r domdoctor.WatchedRoot, err error) domdoctor.WatchedRoot {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	tally, err := domdoctor.NewWatchedRootTally([]string{"build", "gen/out"}, 1024, 2)
	if err != nil {
		t.Fatal(err)
	}
	tally.Add(root(domdoctor.MeasuredWatchedRoot("build", 2048)))
	tally.Add(root(domdoctor.MeasuredWatchedRoot("gen/out", 0)))
	tally.Add(root(domdoctor.UnmeasuredWatchedRoot("x/build", domdoctor.GapAggregateBudget)))
	got, err := json.Marshal(watchedRootsViewOf(tally.Audit()))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"scope":"effect-root-selectors","metric":"logical-bytes","selectors":["build","gen/out"],"threshold_bytes":1024,` +
		`"search":{"complete":true,"incomplete_reasons":[]},"roots_found":3,"roots_total":3,` +
		`"measurements":{"exact":2,"partial":0,"unmeasured":1},"observed_bytes":2048,"observed_bytes_complete":false,` +
		`"roots_reaching_threshold":1,"roots":[` +
		`{"path":"build","measurement":"exact","observed_bytes":2048,"incomplete_reasons":[]},` +
		`{"path":"gen/out","measurement":"exact","observed_bytes":0,"incomplete_reasons":[]}],` +
		`"roots_detail":{"complete":false,"shown":2,"limit":2,"omitted":1}}`
	if string(got) != want {
		t.Errorf("JSON = %s\nwant   %s", got, want)
	}
	lines := strings.Join(watchedRootsLines(tally.Audit()), "\n")
	for _, w := range []string{"3 found (search complete)", "at least 2.0 KiB", "1 unmeasured", "1 at or above 1.0 KiB",
		"build: 2.0 KiB", "gen/out: empty", "1 more found root(s) not listed (2 with the largest observed figures shown)"} {
		if !strings.Contains(lines, w) {
			t.Errorf("human lines lack %q:\n%s", w, lines)
		}
	}

	stopped, err := domdoctor.NewWatchedRootTally([]string{"build"}, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := stopped.SearchIncomplete(domdoctor.GapWorkBudget); err != nil {
		t.Fatal(err)
	}
	got, err = json.Marshal(watchedRootsViewOf(stopped.Audit()))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"threshold_bytes":null`, `"search":{"complete":false,"incomplete_reasons":["work-budget"]}`,
		`"roots_found":0,"roots_total":null`, `"roots_reaching_threshold":null`, `"roots":[]`} {
		if !strings.Contains(string(got), w) {
			t.Errorf("incomplete-search JSON lacks %s:\n%s", w, got)
		}
	}
	if line := strings.Join(watchedRootsLines(stopped.Audit()), "\n"); !strings.Contains(line, "none found so far (search incomplete: work-budget)") ||
		!strings.Contains(line, "no size warning") {
		t.Errorf("incomplete empty search line = %q", line)
	}
}
