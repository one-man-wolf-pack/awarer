package acceptance

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusReportsRunCache(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// Before any run, status reports an empty run cache.
	_, stdout, _ := awa(t, root, "status", "--json")
	var before struct {
		Data struct {
			RunCache struct {
				Recorded  int  `json:"recorded"`
				Populated bool `json:"populated"`
			} `json:"run_cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &before); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, stdout)
	}
	if before.Data.RunCache.Populated || before.Data.RunCache.Recorded != 0 {
		t.Errorf("empty run cache = %+v, want unpopulated", before.Data.RunCache)
	}

	// After a stored run, status reports the real count and latest run.
	awa(t, root, "run", "--", h, "-out", "x")
	_, stdout, _ = awa(t, root, "status", "--json")
	var after struct {
		Data struct {
			RunCache struct {
				Recorded  int  `json:"recorded"`
				Populated bool `json:"populated"`
				Latest    *struct {
					ExitCode int  `json:"exit_code"`
					Healthy  bool `json:"healthy"`
				} `json:"latest"`
			} `json:"run_cache"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &after); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, stdout)
	}
	if !after.Data.RunCache.Populated || after.Data.RunCache.Recorded != 1 {
		t.Errorf("run cache after run = %+v, want 1 recorded/populated", after.Data.RunCache)
	}
	if after.Data.RunCache.Latest == nil || !after.Data.RunCache.Latest.Healthy {
		t.Errorf("latest = %+v, want a healthy latest run", after.Data.RunCache.Latest)
	}

	// Human output reflects it too.
	if _, human, _ := awa(t, root, "status"); !strings.Contains(human, "run cache:   1 runs") {
		t.Errorf("status human output = %q, want a run-cache count", human)
	}
}
