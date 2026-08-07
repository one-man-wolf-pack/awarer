package acceptance

import (
	"regexp"
	"strings"
	"testing"
)

// utcStamp matches the absolute UTC render FormatTime uses for [ui].time = "utc"
// ("2006-01-02 15:04:05Z"), so the test asserts the switch away from the relative
// default ("just now") without pinning the exact instant.
var utcStamp = regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ`)

// TestRunSubcommandsHonorConfigOverrideTime pins two distinct contracts.
// run log is a config-aware listing: a global --config override that sets
// [ui].time = "utc" must reach its human timestamp. run show, by contrast, is a
// stored-only evidence inspection: it never loads project config, so
// --config is intentionally inert (the default relative display stays), while an
// explicit --time flag — a display preference, not project config — still applies.
func TestRunSubcommandsHonorConfigOverrideTime(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// Record a run so log/show have an entry with a fresh (relative: "just now")
	// timestamp under the default policy.
	if code, _, stderr := awa(t, root, "run", "--", h, "-out", "x"); code != 0 {
		t.Fatalf("run exit = %d, stderr = %q", code, stderr)
	}

	// A --config override file that flips the human time display to absolute UTC.
	write(t, root, "utc.toml", "[ui]\ntime = \"utc\"\n")

	// Baseline: no override, so the default relative policy applies to both.
	_, baseLog, _ := awa(t, root, "run", "log")
	if !strings.Contains(baseLog, "just now") {
		t.Errorf("baseline run log should render relative time (just now):\n%s", baseLog)
	}
	_, baseShow, _ := awa(t, root, "run", "show", "--last")
	if !strings.Contains(baseShow, "just now") {
		t.Errorf("baseline run show should render relative time (just now):\n%s", baseShow)
	}

	// run log honors --config: the [ui].time override reaches its human timestamp.
	_, ovLog, ovLogErr := awa(t, root, "run", "log", "--config", "utc.toml")
	if strings.Contains(ovLog, "just now") || !utcStamp.MatchString(ovLog) {
		t.Errorf("run log --config utc.toml did not honor [ui].time = utc (stderr %q):\n%s", ovLogErr, ovLog)
	}

	// run show is stored-only: --config is inert, so it keeps the default relative
	// display instead of switching to the override's UTC.
	_, ovShow, _ := awa(t, root, "run", "show", "--last", "--config", "utc.toml")
	if !strings.Contains(ovShow, "just now") {
		t.Errorf("run show --last is stored-only; --config must be inert and keep relative time:\n%s", ovShow)
	}

	// The explicit --time flag is a display preference, not project config, so it still
	// applies to the stored-only run show: --time utc renders absolute UTC.
	_, flagShow, flagErr := awa(t, root, "run", "show", "--last", "--time", "utc")
	if strings.Contains(flagShow, "just now") || !utcStamp.MatchString(flagShow) {
		t.Errorf("run show --time utc must render absolute UTC (stderr %q):\n%s", flagErr, flagShow)
	}
}
