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
