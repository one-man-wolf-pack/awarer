package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const historyHint = "history: awa run log  # recorded executions, any reuse state"

// recordedRunID records one harmless command with --record in root and returns its
// full run id.
func recordedRunID(t *testing.T, root string, args ...string) string {
	t.Helper()
	return deliberateRunID(t, root, "--record", args...)
}

// deliberateRunID records one harmless command under a deliberate-recording flag
// (--record or --no-cache) in root and returns its full run id.
func deliberateRunID(t *testing.T, root, flag string, args ...string) string {
	t.Helper()
	if code, _, stderr := awa(t, root, append([]string{"run", flag, "--"}, args...)...); code != 0 {
		t.Fatalf("run %s: exit %d\n%s", flag, code, stderr)
	}
	_, stdout, _ := awa(t, root, "run", "log", "--json")
	var env struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || len(env.Data) == 0 {
		t.Fatalf("run log --json: %v\n%s", err, stdout)
	}
	return env.Data[0].ID
}

// TestRunLsEmptyReusePointsAtHistory confirms every human reuse listing that finds
// nothing reusable points at run log, without hiding the recorded row or its reason,
// and that a known-empty history keeps its plain statement.
func TestRunLsEmptyReusePointsAtHistory(t *testing.T) {
	root := initProject(t)
	h := helper(t)

	// Known-empty history: --all states it and adds no pointer.
	if code, stdout, _ := awa(t, root, "run", "ls", "--all"); code != 0 || stdout != "no runs recorded\n" {
		t.Errorf("empty run ls --all = exit %d %q, want only the known-empty line", code, stdout)
	}

	recordedRunID(t, root, h, "-out", "kept")

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"run", "ls"}, []string{"no reusable runs for the current state\n" + historyHint + "\n"}},
		{[]string{"run", "ls", "--all"}, []string{"not-reusable(record-only)\n" + historyHint + "\n"}},
		{[]string{"run", "ls", "--near"}, []string{
			"reusable now:\n  none\n  " + historyHint + "\n",
			"near misses:\n",
			"not-reusable(record-only)",
		}},
		// The command filter only searches the recent window, so an empty match is not
		// "no runs recorded", and the pointer carries the filter.
		{[]string{"run", "ls", "--all", "--command", "no such"}, []string{
			"no recent runs match the command filter\nhistory: awa run log --command 'no such'  #",
		}},
	} {
		code, stdout, stderr := awa(t, root, tc.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s", tc.args, code, stderr)
		}
		for _, want := range tc.want {
			if !strings.Contains(stdout, want) {
				t.Errorf("%v missing %q:\n%s", tc.args, want, stdout)
			}
		}
		if strings.Contains(stdout, "no runs recorded") {
			t.Errorf("%v claims no recorded runs:\n%s", tc.args, stdout)
		}
	}

	// A reusable result carries no pointer.
	awa(t, root, "run", "--", h, "-out", "cached")
	if _, stdout, _ := awa(t, root, "run", "ls"); strings.Contains(stdout, "history:") {
		t.Errorf("run ls with a reusable row added the history pointer:\n%s", stdout)
	}
}

// TestRunLsAllIncompatibleWindowIsNotEmptyHistory confirms that an --all window whose
// only reference is an incompatible newest record is not reported as an empty store:
// the listing keeps a qualified empty result and the history pointer, and run log
// still reaches the older readable run outside the window.
func TestRunLsAllIncompatibleWindowIsNotEmptyHistory(t *testing.T) {
	root := initProject(t)
	h := helper(t)
	recordedRunID(t, root, h, "-out", "OLDER-READABLE")
	recordedRunID(t, root, h, "-out", "NEWER")
	restampNewestRunIncompatible(t, root)

	code, stdout, stderr := awa(t, root, "run", "ls", "--all", "-n", "1")
	if code != 0 {
		t.Fatalf("run ls --all -n 1: exit %d\n%s", code, stderr)
	}
	if want := "no readable runs among the recent runs\n" + historyHint + "\n"; stdout != want {
		t.Errorf("run ls --all -n 1 = %q, want %q", stdout, want)
	}
	if code, stdout, _ := awa(t, root, "run", "log"); code != 0 || !strings.Contains(stdout, "OLDER-READABLE") {
		t.Errorf("run log does not reach the older readable run: exit %d\n%s", code, stdout)
	}
}

// TestRunInlineDiagnosisRecordOnlyNearestRoutesToHistory confirms a deliberately
// recorded nearest run (--record or --no-cache) is routed to reading its stored output
// by explicit id instead of the near-miss view, while the typed JSON suggestions keep
// their kinds and argv.
func TestRunInlineDiagnosisRecordOnlyNearestRoutesToHistory(t *testing.T) {
	h := helper(t)
	for _, tc := range []struct{ flag, reason string }{
		{"--record", "record-only"},
		{"--no-cache", "no-cache"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			root := initProject(t)
			id := deliberateRunID(t, root, tc.flag, h, "-out", "x")

			code, _, stderr := awa(t, root, "run", "--", h, "-out", "x")
			if code != 0 {
				t.Fatalf("run: exit %d\n%s", code, stderr)
			}
			for _, want := range []string{
				"nearest previous: " + id[:12] + " not-reusable(" + tc.reason + ")",
				"explain:  awa run explain --",
				"history:  awa run show " + id + " --tail 50",
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("footer missing %q:\n%s", want, stderr)
				}
			}
			if strings.Contains(stderr, "run ls --near") || strings.Contains(stderr, "--last") {
				t.Errorf("deliberately recorded nearest still routed to the near-miss view or --last:\n%s", stderr)
			}

			// Equivalent input in JSON mode (a fresh project, since the plain run above is
			// now a reusable entry): the machine diagnosis is unchanged.
			other := initProject(t)
			deliberateRunID(t, other, tc.flag, h, "-out", "x")
			_, stdout, _ := awa(t, other, "--json", "run", "--", h, "-out", "x")
			diag := decodeRunDiagnosis(t, stdout).Diagnosis
			if diag.Nearest == nil || diag.Nearest.Reason != tc.reason {
				t.Fatalf("JSON nearest = %+v, want the %s candidate", diag.Nearest, tc.reason)
			}
			var kinds []string
			for _, s := range diag.Suggestions {
				kinds = append(kinds, s.Kind)
			}
			if !reflect.DeepEqual(kinds, []string{"run-explain", "run-ls-near"}) {
				t.Errorf("JSON suggestion kinds = %v, want unchanged [run-explain run-ls-near]", kinds)
			}
		})
	}
}

// TestStatusRecordOnlyNearestRoutesToHistory confirms the dashboard's deliberately
// recorded nearest candidate (--record or --no-cache) gains a human history follow-up by explicit id, while its typed
// next actions and JSON nearest object stay as they were.
func TestStatusRecordOnlyNearestRoutesToHistory(t *testing.T) {
	h := helper(t)
	for _, tc := range []struct{ flag, reason string }{
		{"--record", "record-only"},
		{"--no-cache", "no-cache"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			statusRoutesDeliberateNearest(t, h, tc.flag, tc.reason)
		})
	}
}

func statusRoutesDeliberateNearest(t *testing.T, h, flag, reason string) {
	t.Helper()
	root := initProject(t)
	id := deliberateRunID(t, root, flag, h, "-out", "x")

	code, stdout, stderr := awa(t, root, "status")
	if code != 0 {
		t.Fatalf("status: exit %d\n%s", code, stderr)
	}
	for _, want := range []string{
		"reusable:    none — 1 near miss(es), inspect with: awa run ls --near",
		"not-reusable(" + reason + ")\n  history:   awa run show " + id + " --tail 50\n",
		"awa run ls --near      # why recent checks are stale",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status missing %q:\n%s", want, stdout)
		}
	}

	_, js, _ := awa(t, root, "status", "--json")
	var env struct {
		Data struct {
			Review struct {
				Runs struct {
					Near    *int `json:"near"`
					Nearest *struct {
						RunID  string `json:"run_id"`
						Reason string `json:"reason"`
					} `json:"nearest"`
				} `json:"runs"`
				Next []struct {
					Kind string   `json:"kind"`
					Argv []string `json:"argv"`
				} `json:"next"`
			} `json:"review"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(js), &env); err != nil {
		t.Fatalf("status --json: %v\n%s", err, js)
	}
	runs := env.Data.Review.Runs
	if runs.Near == nil || *runs.Near != 1 || runs.Nearest == nil || runs.Nearest.RunID != id || runs.Nearest.Reason != reason {
		t.Errorf("status JSON runs = %+v, want the %s nearest", runs, reason)
	}
	for _, n := range env.Data.Review.Next {
		if strings.Contains(strings.Join(n.Argv, " "), "run show") {
			t.Errorf("status JSON gained a run-show next action: %+v", n)
		}
	}
	if strings.Contains(js, "history") {
		t.Errorf("status JSON carries the human history hint:\n%s", js)
	}
}

// TestRunShowNotFoundNamesSearchedProject records a run in project A, looks its
// explicit id up from a subdirectory of project B, and confirms the not-found
// diagnostic names the requested id and B's root while exit and empty stdout are
// unchanged in both human and JSON mode; the same id still reads in A.
func TestRunShowNotFoundNamesSearchedProject(t *testing.T) {
	a := initProject(t)
	b := initProject(t)
	h := helper(t)
	id := recordedRunID(t, a, h, "-out", "from-a")

	sub := filepath.Join(b, "nested", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	bRoot, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ref  string
		args []string
	}{
		{id, []string{"run", "show", id}},
		{id, []string{"--json", "run", "show", id}},
		{id[:12], []string{"run", "show", id[:12], "--tail", "5"}},
	} {
		args := tc.args
		code, stdout, stderr := awa(t, sub, args...)
		if code != 3 || stdout != "" {
			t.Errorf("%v from B: exit %d stdout %q, want exit 3 and empty stdout", args, code, stdout)
		}
		if !strings.Contains(stderr, "run "+tc.ref+" not found in project ") {
			t.Errorf("%v: diagnostic does not name the requested reference:\n%s", args, stderr)
		}
		if !strings.Contains(stderr, b) && !strings.Contains(stderr, bRoot) {
			t.Errorf("%v: diagnostic does not name B's root %s:\n%s", args, bRoot, stderr)
		}
		if strings.Contains(stderr, sub) || strings.Contains(stderr, a) {
			t.Errorf("%v: diagnostic names cwd or another project:\n%s", args, stderr)
		}
	}

	if code, stdout, stderr := awa(t, a, "run", "show", id, "--stdout"); code != 0 || stdout != "from-a" {
		t.Errorf("run show in A = exit %d stdout %q stderr %q, want the stored output", code, stdout, stderr)
	}
}

// TestRunShowLastNotFoundIsUnqualified confirms the --last selection keeps its own
// not-found handling: it is not an explicit reference, so no root qualification.
func TestRunShowLastNotFoundIsUnqualified(t *testing.T) {
	root := initProject(t)
	code, stdout, stderr := awa(t, root, "run", "show", "--last")
	if code != 3 || stdout != "" || strings.Contains(stderr, "not found in project") {
		t.Errorf("run show --last on an empty store = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
