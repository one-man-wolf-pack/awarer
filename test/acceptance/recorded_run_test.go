package acceptance

import (
	"strings"
	"testing"
)

// TestAcceptanceRecordedRunLifecycle exercises the recorded-run lifecycle end to end: a
// supervised --record run is durable history, inspectable via run show, addressable via run
// observation refs, separated from explicit checkpoints in the log, and exposed in
// JSON with machine timestamps only.
func TestAcceptanceRecordedRunLifecycle(t *testing.T) {
	root := initProject(t)
	write(t, root, "a.txt", "x")

	// A --record run executes supervised and mutates project state (creates new.txt).
	if code, _, stderr := awa(t, root, "run", "--record", "--", "sh", "-c", "echo build; : > new.txt"); code != 0 {
		t.Fatalf("run --record exit = %d, stderr = %q", code, stderr)
	}

	// run log lists the recorded run; grab its short id (first column).
	code, logOut, _ := awa(t, root, "run", "log")
	if code != 0 || strings.TrimSpace(logOut) == "" {
		t.Fatalf("run log = %d / %q", code, logOut)
	}
	short := strings.Fields(strings.SplitN(logOut, "\n", 2)[0])[0]
	if len(short) < 8 {
		t.Fatalf("run log first column is not a run id: %q", logOut)
	}

	// run show --last inspects the record-only run.
	code, showOut, _ := awa(t, root, "run", "show", "--last")
	if code != 0 || !strings.Contains(showOut, "record-only") {
		t.Errorf("run show --last = %d / %q (want record-only)", code, showOut)
	}

	// changes run:<id>:before..run:<id>:after reports the command-induced change.
	code, chOut, stderr := awa(t, root, "changes", "run:"+short+":before..run:"+short+":after")
	if code != 0 {
		t.Fatalf("changes run refs exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(chOut, "new.txt") {
		t.Errorf("changes run:before..after missing the created file:\n%s", chOut)
	}

	// The JSON comparison is self-describing: each side reports the run observation it
	// used and the recorded run's reuse classification.
	code, chJSON, _ := awa(t, root, "--json", "changes", "run:"+short+":before..run:"+short+":after")
	if code != 0 {
		t.Fatalf("changes run refs --json exit = %d", code)
	}
	if !strings.Contains(chJSON, `"kind": "run-observation"`) || !strings.Contains(chJSON, `"run_reuse": "record-only"`) {
		t.Errorf("changes JSON must report run-observation kind and reuse:\n%s", chJSON)
	}

	// Make an explicit checkpoint so the timeline has both kinds.
	if code, _, _ := awa(t, root, "checkpoint", "-m", "checkpoint"); code != 0 {
		t.Fatalf("checkpoint exit = %d", code)
	}

	// Default log excludes automatic run observations.
	code, defLog, _ := awa(t, root, "log")
	if code != 0 || strings.Contains(defLog, "run:before") || strings.Contains(defLog, "run:after") {
		t.Errorf("default log must exclude run observations:\n%s", defLog)
	}
	// log --all includes and labels them.
	code, allLog, _ := awa(t, root, "log", "--all")
	if code != 0 || !strings.Contains(allLog, "run:before") || !strings.Contains(allLog, "run:after") {
		t.Errorf("log --all must include labeled run observations:\n%s", allLog)
	}

	// JSON timeline uses machine timestamps and carries no relative prose.
	code, jsonLog, _ := awa(t, root, "--json", "log", "--all")
	if code != 0 {
		t.Fatalf("log --all --json exit = %d", code)
	}
	if strings.Contains(jsonLog, " ago") || strings.Contains(jsonLog, "human_time") || strings.Contains(jsonLog, "\"age\"") {
		t.Errorf("JSON timeline must not contain relative time prose:\n%s", jsonLog)
	}
	if !strings.Contains(jsonLog, "\"reuse\": \"record-only\"") {
		t.Errorf("JSON timeline missing the record-only run reuse state:\n%s", jsonLog)
	}
}
