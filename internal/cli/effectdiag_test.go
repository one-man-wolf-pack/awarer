package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the CLI proof for the shared near-miss effect detail. One stored run
// stops being reusable because the watched generated-output state moved; every surface
// that reports that near miss must project the SAME optional effect object from the
// SAME candidate, and human status must spend at most one line on it.

// effectDiagDoc mirrors the shared effect diagnosis exactly as a machine consumer would
// decode it, so a surface that invented its own field names or dropped one would fail to
// match here rather than quietly decode into zero values.
type effectDiagDoc struct {
	Reason  string `json:"reason"`
	Root    string `json:"root"`
	Sample  string `json:"sample"`
	Actions []struct {
		Condition string   `json:"condition"`
		Action    string   `json:"action"`
		Argv      []string `json:"argv"`
	} `json:"actions"`
}

// effectNearMissDoc is the shared near-miss object, decoded down to the fields this
// proof needs.
type effectNearMissDoc struct {
	RunID  string         `json:"run_id"`
	Reason string         `json:"reason"`
	Effect *effectDiagDoc `json:"effect"`
}

// writeWatchedRoot writes a file into "target", which the default profile both excludes
// from the input scan and watches as generated output — so touching it moves effect
// state while the input tree stays put.
func writeWatchedRoot(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, "target")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedEffectNearMiss publishes one reusable run against a watched generated-output root,
// then changes that root so the stored run becomes an effect-state near miss. It returns
// the project root and the stored run's id.
func seedEffectNearMiss(t *testing.T) (root, runID string) {
	t.Helper()
	shAvailable(t)
	root = initProject(t)
	writeWatchedRoot(t, root, "built")

	if code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/sh", "-c", "true"); code != 0 {
		t.Fatalf("seed run exit = %d, stderr = %q", code, stderr)
	}

	var lsEnv struct {
		Data struct {
			Reusable []struct {
				ID string `json:"id"`
			} `json:"reusable"`
		} `json:"data"`
	}
	_, lsOut, _ := run("run", "ls", "--root", root, "--json")
	if err := json.Unmarshal([]byte(lsOut), &lsEnv); err != nil {
		t.Fatalf("invalid run ls JSON: %v\n%s", err, lsOut)
	}
	if len(lsEnv.Data.Reusable) != 1 {
		t.Fatalf("seed produced %d reusable runs, want 1\n%s", len(lsEnv.Data.Reusable), lsOut)
	}
	runID = lsEnv.Data.Reusable[0].ID

	// Move the watched generated-output state only: the stored run is now an
	// effect-state near miss, and nothing else about the project changed.
	writeWatchedRoot(t, root, "built-again-and-longer")
	return root, runID
}

// assertEffectDiagnosis checks one surface's projected effect object against the shared
// contract: the canonical reason, the already-observed root, the honest sample token,
// and the record-mode action naming the candidate's own stored command.
func assertEffectDiagnosis(t *testing.T, surface string, d *effectDiagDoc) {
	t.Helper()
	if d == nil {
		t.Fatalf("%s: near miss carries no effect object", surface)
	}
	if d.Reason != "effect-state-differs" {
		t.Errorf("%s: reason = %q, want effect-state-differs", surface, d.Reason)
	}
	if d.Root != "target" {
		t.Errorf("%s: root = %q, want the observed root %q", surface, d.Root, "target")
	}
	if d.Sample != "unavailable" {
		t.Errorf("%s: sample = %q, want unavailable", surface, d.Sample)
	}
	if len(d.Actions) != 3 {
		t.Fatalf("%s: %d actions, want the 3 typed exclude/effect-root/record steps", surface, len(d.Actions))
	}
	record := d.Actions[2]
	if record.Action != "record" {
		t.Fatalf("%s: last action = %q, want record", surface, record.Action)
	}
	want := []string{"awa", "run", "--record", "--", "/bin/sh", "-c", "true"}
	if strings.Join(record.Argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s: record argv = %v, want the candidate's own stored command %v", surface, record.Argv, want)
	}
}

// TestEffectNearMissIsOneObjectAcrossSurfaces proves the shared-shape requirement:
// status, run ls --near, run explain, and the inline run diagnosis all emit the same
// effect object for the same candidate, and status carries it only inside the nested
// shared near-miss object rather than as a second status-level copy.
func TestEffectNearMissIsOneObjectAcrossSurfaces(t *testing.T) {
	root, runID := seedEffectNearMiss(t)

	// status --json: the field arrives through review.runs.nearest and nowhere else.
	var statusEnv struct {
		Data struct {
			Review struct {
				Runs struct {
					Reusable int                `json:"reusable"`
					Near     int                `json:"near"`
					Nearest  *effectNearMissDoc `json:"nearest"`
					// Effect must not exist at the runs level; a status-specific copy would
					// decode here and fail the assertion below.
					Effect *effectDiagDoc `json:"effect"`
				} `json:"runs"`
			} `json:"review"`
		} `json:"data"`
	}
	_, statusOut, _ := run("status", "--root", root, "--json")
	if err := json.Unmarshal([]byte(statusOut), &statusEnv); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, statusOut)
	}
	runs := statusEnv.Data.Review.Runs
	if runs.Reusable != 0 || runs.Near != 1 {
		t.Fatalf("status runs = reusable %d / near %d, want 0 / 1\n%s", runs.Reusable, runs.Near, statusOut)
	}
	if runs.Nearest == nil {
		t.Fatalf("status reported no nearest candidate\n%s", statusOut)
	}
	if runs.Effect != nil {
		t.Errorf("status carries a second status-level effect copy: %+v", runs.Effect)
	}
	assertEffectDiagnosis(t, "status --json", runs.Nearest.Effect)

	// run ls --near --json
	var lsEnv struct {
		Data struct {
			NearMisses []effectNearMissDoc `json:"near_misses"`
		} `json:"data"`
	}
	_, lsOut, _ := run("run", "ls", "--near", "--root", root, "--json")
	if err := json.Unmarshal([]byte(lsOut), &lsEnv); err != nil {
		t.Fatalf("invalid run ls JSON: %v\n%s", err, lsOut)
	}
	if len(lsEnv.Data.NearMisses) != 1 {
		t.Fatalf("run ls --near reported %d near misses, want 1\n%s", len(lsEnv.Data.NearMisses), lsOut)
	}
	assertEffectDiagnosis(t, "run ls --near --json", lsEnv.Data.NearMisses[0].Effect)

	// run explain --json, in both of its modes. They are separate candidate-building
	// paths in the application layer — the stored mode re-keys the one requested run,
	// while command mode ranks the newest window against the current key — so covering
	// only one would leave the other free to drop the projection unnoticed. Command mode
	// is also the form status suggests in `next:`.
	explainCandidates := func(t *testing.T, surface string, args ...string) []effectNearMissDoc {
		t.Helper()
		var env struct {
			Data struct {
				Candidates []effectNearMissDoc `json:"candidates"`
			} `json:"data"`
		}
		_, out, errOut := run(args...)
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%s: invalid JSON: %v\nstdout=%s\nstderr=%s", surface, err, out, errOut)
		}
		if len(env.Data.Candidates) != 1 {
			t.Fatalf("%s: reported %d candidates, want 1\n%s", surface, len(env.Data.Candidates), out)
		}
		return env.Data.Candidates
	}
	stored := explainCandidates(t, "run explain --from-run --json",
		"run", "explain", "--root", root, "--from-run", runID, "--to-now", "--json")
	assertEffectDiagnosis(t, "run explain --from-run --json", stored[0].Effect)

	command := explainCandidates(t, "run explain -- <cmd> --json",
		"run", "explain", "--root", root, "--cwd", root, "--json", "--", "/bin/sh", "-c", "true")
	if command[0].RunID != runID {
		t.Fatalf("run explain -- <cmd>: nearest = %s, want the seeded run %s", command[0].RunID, runID)
	}
	assertEffectDiagnosis(t, "run explain -- <cmd> --json", command[0].Effect)

	// The inline run diagnosis: a fresh run of the same command misses because the
	// watched state moved, and its footer's nearest candidate is the seeded run.
	var runEnv struct {
		Data struct {
			Cache struct {
				Diagnosis struct {
					Nearest *effectNearMissDoc `json:"nearest"`
				} `json:"diagnosis"`
			} `json:"cache"`
		} `json:"data"`
	}
	_, runOut, _ := run("run", "--root", root, "--cwd", root, "--json", "--", "/bin/sh", "-c", "true")
	if err := json.Unmarshal([]byte(runOut), &runEnv); err != nil {
		t.Fatalf("invalid run JSON: %v\n%s", err, runOut)
	}
	nearest := runEnv.Data.Cache.Diagnosis.Nearest
	if nearest == nil {
		t.Fatalf("inline diagnosis reported no nearest candidate\n%s", runOut)
	}
	if nearest.RunID != runID {
		t.Fatalf("inline nearest = %s, want the seeded run %s", nearest.RunID, runID)
	}
	assertEffectDiagnosis(t, "run --json cache.diagnosis.nearest", nearest.Effect)
}

// TestStatusHumanShowsOneCompactEffectDetail proves the status density rule: exactly one
// detail line beneath the nearest candidate, naming the changed watched state and the
// observed root, with no repeated action tutorial and no fabricated changed-path sample.
// It also pins that the human line and the machine object describe one observation.
func TestStatusHumanShowsOneCompactEffectDetail(t *testing.T) {
	root, _ := seedEffectNearMiss(t)

	_, out, _ := run("status", "--root", root)
	lines := strings.Split(out, "\n")

	nearestAt, effectAt, effectCount := -1, -1, 0
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "  nearest:"):
			nearestAt = i
		case strings.HasPrefix(l, "  effect:"):
			effectCount++
			if effectAt == -1 {
				effectAt = i
			}
		}
	}
	if nearestAt == -1 {
		t.Fatalf("status printed no nearest line:\n%s", out)
	}
	if effectCount != 1 {
		t.Fatalf("status printed %d effect detail lines, want exactly 1:\n%s", effectCount, out)
	}
	if effectAt != nearestAt+1 {
		t.Errorf("effect detail is at line %d, want it directly beneath nearest at %d:\n%s", effectAt, nearestAt, out)
	}

	detail := lines[effectAt]
	for _, want := range []string{"watched generated-output state differs", `"target"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("effect detail %q does not name %q", detail, want)
		}
	}
	// The compact line is a summary, not the multi-line decision tutorial the run footer
	// prints, and it never claims a changed-path sample effect state cannot produce. The
	// forbidden strings are those two artifacts specifically, not the wording — a renderer
	// is free to say "changed" or "differs" as long as it names one honest fact.
	for _, forbidden := range []string{"awa run --record", "extra_effect_roots", "changed inputs", "changed-path"} {
		if strings.Contains(detail, forbidden) {
			t.Errorf("effect detail %q must stay compact but contains %q", detail, forbidden)
		}
	}
}

// TestHumanNearMissSurfacesShareTheEffectVocabulary proves requirement 7 for the human
// surfaces: run ls --near, run explain, and the inline run footer each spend one line on
// the same watched-state fact, in the same words. Placement and indentation are each
// surface's own business; the sentence is not.
func TestHumanNearMissSurfacesShareTheEffectVocabulary(t *testing.T) {
	root, runID := seedEffectNearMiss(t)
	const want = `watched generated-output state differs (dominant root "target")`

	_, lsOut, _ := run("run", "ls", "--near", "--root", root)
	if !strings.Contains(lsOut, want) {
		t.Errorf("run ls --near does not name the shared effect detail:\n%s", lsOut)
	}

	_, storedOut, _ := run("run", "explain", "--root", root, "--from-run", runID, "--to-now")
	if !strings.Contains(storedOut, want) {
		t.Errorf("run explain --from-run does not name the shared effect detail:\n%s", storedOut)
	}

	// Command mode builds its candidate through a different application path, so it is
	// probed separately rather than assumed to follow the stored mode.
	_, commandOut, _ := run("run", "explain", "--root", root, "--cwd", root, "--", "/bin/sh", "-c", "true")
	if !strings.Contains(commandOut, want) {
		t.Errorf("run explain -- <cmd> does not name the shared effect detail:\n%s", commandOut)
	}

	// The inline footer is awa diagnostics, so it lands on stderr.
	_, _, runErr := run("run", "--root", root, "--cwd", root, "--", "/bin/sh", "-c", "true")
	if !strings.Contains(runErr, want) {
		t.Errorf("the inline run footer does not name the shared effect detail:\n%s", runErr)
	}
}

// seedRootlessHistoricalNearMisses records two runs that each disturb the watched
// generated-output root while they execute. Both are stored as non-reusable history, so
// every near-miss surface short-circuits to their recorded verdict — a historical reason
// the current observation is no evidence about, hence rootless — and neither run is ever
// a reusable entry. It returns the project root.
func seedRootlessHistoricalNearMisses(t *testing.T) string {
	t.Helper()
	shAvailable(t)
	root := initProject(t)
	writeWatchedRoot(t, root, "built")

	for _, marker := range []string{"first", "second"} {
		code, _, stderr := run("run", "--root", root, "--cwd", root, "--display", "none",
			"--", "/bin/sh", "-c", "echo "+marker+" >> target/app")
		if code != 0 {
			t.Fatalf("seed run %s exit = %d, stderr = %q", marker, code, stderr)
		}
	}
	return root
}

// TestRootlessHistoricalCandidatesPrintNoEffectProse proves the information-increasing
// rule: a candidate whose effect diagnosis has no root adds nothing to the
// not-reusable(<reason>) token already beside it, so no human near-miss surface prints a
// paraphrase line — not once, and not once per candidate in a listing full of build runs.
// The machine object survives, because its sample fact and typed actions carry
// information no reason token does.
func TestRootlessHistoricalCandidatesPrintNoEffectProse(t *testing.T) {
	root := seedRootlessHistoricalNearMisses(t)

	// Human run ls --near: both candidates present with their reason, no prose line.
	_, lsOut, _ := run("run", "ls", "--near", "--root", root)
	if n := strings.Count(lsOut, "not-reusable(effect-state-differs)"); n != 2 {
		t.Fatalf("run ls --near listed %d effect-state near misses, want 2:\n%s", n, lsOut)
	}
	if strings.Contains(lsOut, "effect:") {
		t.Errorf("run ls --near paraphrased a rootless reason as prose:\n%s", lsOut)
	}

	// Human status and the inline footer of a fresh miss: same suppression.
	_, statusOut, _ := run("status", "--root", root)
	if !strings.Contains(statusOut, "not-reusable(effect-state-differs)") {
		t.Fatalf("status did not report the effect-state nearest candidate:\n%s", statusOut)
	}
	if strings.Contains(statusOut, "  effect:") {
		t.Errorf("status paraphrased a rootless reason as prose:\n%s", statusOut)
	}
	_, _, runErr := run("run", "--root", root, "--cwd", root, "--display", "none", "--", "/bin/sh", "-c", "true")
	if !strings.Contains(runErr, "nearest previous:") {
		t.Fatalf("the inline footer showed no nearest candidate:\n%s", runErr)
	}
	if strings.Contains(runErr, "    effect: ") {
		t.Errorf("the inline footer paraphrased a rootless reason as prose:\n%s", runErr)
	}

	// The structured object is untouched by that suppression: still present, still
	// rootless, still carrying the sample fact and the three typed actions.
	var lsEnv struct {
		Data struct {
			NearMisses []effectNearMissDoc `json:"near_misses"`
		} `json:"data"`
	}
	_, lsJSON, _ := run("run", "ls", "--near", "--root", root, "--json")
	if err := json.Unmarshal([]byte(lsJSON), &lsEnv); err != nil {
		t.Fatalf("invalid run ls JSON: %v\n%s", err, lsJSON)
	}
	if len(lsEnv.Data.NearMisses) != 2 {
		t.Fatalf("run ls --near --json reported %d near misses, want 2\n%s", len(lsEnv.Data.NearMisses), lsJSON)
	}
	for i, nm := range lsEnv.Data.NearMisses {
		if nm.Reason != "effect-state-differs" {
			t.Errorf("near miss %d: reason = %q, want effect-state-differs", i, nm.Reason)
		}
		if nm.Effect == nil {
			t.Fatalf("near miss %d: suppressing the human line also dropped the machine object\n%s", i, lsJSON)
		}
		if nm.Effect.Root != "" {
			t.Errorf("near miss %d: root = %q, want it absent for a historical verdict", i, nm.Effect.Root)
		}
		if nm.Effect.Sample != "unavailable" {
			t.Errorf("near miss %d: sample = %q, want unavailable", i, nm.Effect.Sample)
		}
		if len(nm.Effect.Actions) != 3 {
			t.Errorf("near miss %d: %d actions, want the 3 typed steps", i, len(nm.Effect.Actions))
		}
	}
}

// TestNonEffectNearMissHasNoEffectFieldAnywhere proves the gate holds through every
// projection: an input-tree-differs near miss omits the optional object in JSON and
// prints no detail line in human status, so the field's presence is itself a signal.
func TestNonEffectNearMissHasNoEffectFieldAnywhere(t *testing.T) {
	shAvailable(t)
	root := initProject(t)
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("run", "--root", root, "--cwd", root, "--", "/bin/sh", "-c", "true"); code != 0 {
		t.Fatalf("seed run exit = %d, stderr = %q", code, stderr)
	}
	if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	var lsEnv struct {
		Data struct {
			NearMisses []effectNearMissDoc `json:"near_misses"`
		} `json:"data"`
	}
	_, lsOut, _ := run("run", "ls", "--near", "--root", root, "--json")
	if err := json.Unmarshal([]byte(lsOut), &lsEnv); err != nil {
		t.Fatalf("invalid run ls JSON: %v\n%s", err, lsOut)
	}
	if len(lsEnv.Data.NearMisses) != 1 {
		t.Fatalf("run ls --near reported %d near misses, want 1\n%s", len(lsEnv.Data.NearMisses), lsOut)
	}
	nm := lsEnv.Data.NearMisses[0]
	if nm.Reason != "input-tree-differs" {
		t.Fatalf("near-miss reason = %q, want input-tree-differs", nm.Reason)
	}
	if nm.Effect != nil {
		t.Errorf("an input-tree-differs near miss carries an effect object: %+v", nm.Effect)
	}
	if strings.Contains(lsOut, `"effect"`) {
		t.Errorf("run ls --near JSON emits an effect key for a non-effect miss:\n%s", lsOut)
	}

	_, statusOut, _ := run("status", "--root", root)
	if strings.Contains(statusOut, "  effect:") {
		t.Errorf("status printed an effect detail for a non-effect near miss:\n%s", statusOut)
	}
}
