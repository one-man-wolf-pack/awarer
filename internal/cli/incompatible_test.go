package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"awarer/internal/domain/paths"
)

// checkpointHeaderPaths returns the header.json path of every committed checkpoint
// under the project's checkpoints directory, in directory order.
func checkpointHeaderPaths(t *testing.T, root string) []string {
	t.Helper()
	dir := paths.New(root).CheckpointsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read checkpoints dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hp := filepath.Join(dir, e.Name(), "header.json")
		if _, err := os.Stat(hp); err == nil {
			out = append(out, hp)
		}
	}
	return out
}

// makeIncompatible restamps a checkpoint header one version past whatever the current
// schema is, so the record declares a generation this build has no reader for. The
// number is read back out of the record rather than written as a literal, so the
// fixture stays correct across a schema bump and names no historical version.
func makeIncompatible(t *testing.T, headerPath string) {
	t.Helper()
	data, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	current, err := strconv.Atoi(string(m["schema_version"]))
	if err != nil {
		t.Fatalf("header has no numeric schema_version: %v", err)
	}
	m["schema_version"] = json.RawMessage(strconv.Itoa(current + 1))
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(headerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(headerPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, out, 0o444); err != nil {
		t.Fatal(err)
	}
}

// secondCheckpoint records another checkpoint so a partial-store test has a healthy
// record alongside the one it will make incompatible.
func secondCheckpoint(t *testing.T, root string) {
	t.Helper()
	writeFile(t, root, "calc/calc.go", "package calc\n\nfunc Add(a, b int) int {\n\treturn a + b + 1\n}\n")
	if code, _, stderr := run("checkpoint", "--root", root); code != int(ExitSuccess) {
		t.Fatalf("second checkpoint exit = %d, stderr = %q", code, stderr)
	}
}

// TestStatusJSONIncompatibleNotEmpty is acceptance scenario 1: a store whose only
// checkpoint declares an unreadable schema must not report as an empty healthy
// store. The machine output must distinguish incompatible from empty.
func TestStatusJSONIncompatibleNotEmpty(t *testing.T) {
	root := checkpointProject(t)
	headers := checkpointHeaderPaths(t, root)
	if len(headers) != 1 {
		t.Fatalf("want 1 checkpoint, got %d", len(headers))
	}
	makeIncompatible(t, headers[0])

	code, stdout, _ := run("status", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d", code)
	}
	var doc struct {
		Checkpoints struct {
			State        string `json:"state"`
			Recorded     int    `json:"recorded"`
			Populated    bool   `json:"populated"`
			Unreadable   int    `json:"unreadable"`
			Incompatible int    `json:"incompatible"`
		} `json:"checkpoints"`
		Review struct {
			Partial  bool `json:"partial"`
			Warnings []struct {
				Section string `json:"section"`
				Code    string `json:"code"`
			} `json:"warnings"`
		} `json:"review"`
	}
	decodeEnvelope(t, stdout, &doc)

	if doc.Checkpoints.State != "metadata-incompatible" {
		t.Errorf("checkpoints.state = %q, want metadata-incompatible", doc.Checkpoints.State)
	}
	if doc.Checkpoints.Unreadable != 1 || doc.Checkpoints.Incompatible != 1 {
		t.Errorf("unreadable=%d incompatible=%d, want 1/1", doc.Checkpoints.Unreadable, doc.Checkpoints.Incompatible)
	}
	// The point: an unreadable store must not look empty+healthy.
	if doc.Checkpoints.State == "store-empty" || (doc.Checkpoints.Recorded == 0 && doc.Checkpoints.Unreadable == 0) {
		t.Errorf("incompatible store reported as empty: %+v", doc.Checkpoints)
	}
	if !doc.Review.Partial {
		t.Error("review.partial = false, want true for a degraded store")
	}
	found := false
	for _, w := range doc.Review.Warnings {
		if w.Section == "checkpoints" && w.Code == "checkpoint-store-partial" {
			found = true
		}
	}
	if !found {
		t.Errorf("review warnings missing checkpoint-store-partial: %+v", doc.Review.Warnings)
	}
}

// TestStatusHumanIncompatibleWarns proves the human dashboard surfaces the degraded store
// with a recovery hint rather than "none yet".
func TestStatusHumanIncompatibleWarns(t *testing.T) {
	root := checkpointProject(t)
	makeIncompatible(t, checkpointHeaderPaths(t, root)[0])
	code, stdout, stderr := run("status", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status exit = %d", code)
	}
	all := stdout + stderr
	if strings.Contains(stdout, "checkpoints: none yet") {
		t.Errorf("incompatible store printed 'none yet': %q", stdout)
	}
	if !strings.Contains(all, "partially readable") || !strings.Contains(all, "awa init") {
		t.Errorf("status output missing recovery guidance: %q", all)
	}
}

// TestStatusHumanStructuralCorruptNotEmpty covers the structural-failure path: when
// the store listing itself fails (here a foreign node on the reserved id namespace),
// status must report the store as unreadable, never as "none yet".
func TestStatusHumanStructuralCorruptNotEmpty(t *testing.T) {
	root := checkpointProject(t)
	// A checkpoint's only address is its directory, so "<id>.json" beside the
	// committed "<id>/" is a node the store does not own at an address it reserves —
	// structural corruption of the listing, not an entry to skip.
	dir := paths.New(root).CheckpointsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var idName string
	for _, e := range entries {
		if e.IsDir() {
			idName = e.Name()
		}
	}
	if idName == "" {
		t.Fatal("no checkpoint directory found")
	}
	if err := os.WriteFile(filepath.Join(dir, idName+".json"), []byte("{}"), 0o444); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run("status", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("status exit = %d", code)
	}
	if strings.Contains(stdout, "checkpoints: none yet") {
		t.Errorf("structurally corrupt store printed 'none yet': %q", stdout)
	}
	if !strings.Contains(stdout+stderr, "cannot read checkpoint store") {
		t.Errorf("status did not surface the read failure: %q", stdout+stderr)
	}

	// The JSON contract must not report it as empty either, and the review dashboard
	// must flag the degraded store (review.partial + a structured warning) even though
	// there are no per-record unreadable counts — degraded evidence must reach the
	// dashboard so an agent cannot miss it.
	code, stdout, _ = run("status", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d", code)
	}
	var doc struct {
		Checkpoints struct {
			State string `json:"state"`
		} `json:"checkpoints"`
		Review struct {
			Partial  bool `json:"partial"`
			Warnings []struct {
				Section string `json:"section"`
				Code    string `json:"code"`
			} `json:"warnings"`
		} `json:"review"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Checkpoints.State == "store-empty" || doc.Checkpoints.State == "healthy" {
		t.Errorf("structurally corrupt store state = %q, want a degraded state", doc.Checkpoints.State)
	}
	if !doc.Review.Partial {
		t.Error("review.partial = false for a structurally degraded store, want true")
	}
	found := false
	for _, w := range doc.Review.Warnings {
		if w.Section == "checkpoints" && w.Code == "checkpoint-store-partial" {
			found = true
		}
	}
	if !found {
		t.Errorf("review missing checkpoint-store-partial for structural failure: %+v", doc.Review.Warnings)
	}
}

// TestLogIncompatibleShowsHealthyAndWarns is acceptance scenario 2 for log: healthy
// checkpoints are listed and the incompatible one is reported as skipped, not fatal.
func TestLogIncompatibleShowsHealthyAndWarns(t *testing.T) {
	root := checkpointProject(t)
	secondCheckpoint(t, root)
	headers := checkpointHeaderPaths(t, root)
	if len(headers) != 2 {
		t.Fatalf("want 2 checkpoints, got %d", len(headers))
	}
	makeIncompatible(t, headers[0])

	code, stdout, stderr := run("log", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("log exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "skipped") {
		t.Errorf("log did not warn about skipped checkpoint: %q", stderr)
	}
	// The one readable checkpoint must still appear.
	if strings.Count(stdout, "\n") == 0 {
		t.Errorf("log showed no readable checkpoints: %q", stdout)
	}

	// JSON carries the skipped count.
	code, stdout, _ = run("log", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("log --json exit = %d", code)
	}
	var doc struct {
		Skipped     int `json:"skipped"`
		Checkpoints []struct {
			ID string `json:"id"`
		} `json:"checkpoints"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Skipped != 1 {
		t.Errorf("log --json skipped = %d, want 1", doc.Skipped)
	}
	if len(doc.Checkpoints) != 1 {
		t.Errorf("log --json listed %d checkpoints, want 1 readable", len(doc.Checkpoints))
	}
}

// TestLogCorruptFailsLoud pins the incompatible/corrupt distinction: a genuinely corrupt
// (not incompatible) checkpoint header makes log fail loud with storage corruption, never
// degrade to a partial success — a script must be able to tell damage from a normal
// partial listing.
func TestLogCorruptFailsLoud(t *testing.T) {
	root := checkpointProject(t)
	hp := checkpointHeaderPaths(t, root)[0]
	if err := os.Chmod(hp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, []byte("{ not valid json"), 0o444); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run("log", "--root", root)
	if code != int(ExitStateActionRequired) {
		t.Fatalf("log over corrupt store exit = %d, want %d; stderr = %q", code, int(ExitStateActionRequired), stderr)
	}
}

// TestStatusJSONDegradedOmitsCanonicalLatest pins A-1: on a degraded store the
// canonical "latest" is unresolvable (state resolution refuses to guess), so status
// must not publish checkpoints.latest — otherwise an agent would read it and hit a
// storage error on "latest..now". The newest-readable is not the canonical latest.
func TestStatusJSONDegradedOmitsCanonicalLatest(t *testing.T) {
	root := checkpointProject(t)
	secondCheckpoint(t, root)
	headers := checkpointHeaderPaths(t, root)
	makeIncompatible(t, headers[0]) // one healthy, one incompatible → partial

	code, stdout, _ := run("status", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("status --json exit = %d", code)
	}
	var doc struct {
		Checkpoints struct {
			State  string          `json:"state"`
			Latest json.RawMessage `json:"latest"`
		} `json:"checkpoints"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Checkpoints.State != "read-partial" {
		t.Fatalf("state = %q, want read-partial", doc.Checkpoints.State)
	}
	if len(doc.Checkpoints.Latest) != 0 {
		t.Errorf("degraded store published checkpoints.latest = %s, want omitted", doc.Checkpoints.Latest)
	}
	// And the canonical latest reference must indeed be unresolvable, proving the
	// omission matches resolver behavior.
	if code, _, _ := run("changes", "--root", root); code != int(ExitStateActionRequired) {
		t.Errorf("changes latest..now exit = %d, want %d on a degraded store", code, int(ExitStateActionRequired))
	}
}

// TestLogAllIncompatibleDegrades pins A-2 for the full timeline: awa log --all degrades on
// an incompatible checkpoint (skip + warn + success) exactly like the default log, so the
// two views stay consistent.
func TestLogAllIncompatibleDegrades(t *testing.T) {
	root := checkpointProject(t)
	secondCheckpoint(t, root)
	makeIncompatible(t, checkpointHeaderPaths(t, root)[0])

	code, _, stderr := run("log", "--all", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("log --all over incompatible exit = %d, want 0; stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "skipped") || !strings.Contains(stderr, "incompatible") {
		t.Errorf("log --all missing incompatible-skipped warning; stderr = %q", stderr)
	}

	code, stdout, _ := run("log", "--all", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("log --all --json exit = %d", code)
	}
	var doc struct {
		Skipped int `json:"skipped"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Skipped != 1 {
		t.Errorf("log --all --json skipped = %d, want 1", doc.Skipped)
	}
}

// TestLogAllCorruptFailsLoud pins A-2's corrupt half: a corrupt checkpoint fails the
// full timeline loud (exit 5), never a degraded success.
func TestLogAllCorruptFailsLoud(t *testing.T) {
	root := checkpointProject(t)
	hp := checkpointHeaderPaths(t, root)[0]
	if err := os.Chmod(hp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hp, []byte("{ not valid json"), 0o444); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("log", "--all", "--root", root); code != int(ExitStateActionRequired) {
		t.Fatalf("log --all over corrupt exit = %d, want %d; stderr = %q", code, int(ExitStateActionRequired), stderr)
	}
}

// TestChangesLatestUnreadableFailsLoud is acceptance scenario 3: when the only
// checkpoint is unreadable, default changes (latest..now) must fail with an
// actionable diagnostic, not silently say "no changes" or "no checkpoints".
func TestChangesLatestUnreadableFailsLoud(t *testing.T) {
	root := checkpointProject(t)
	makeIncompatible(t, checkpointHeaderPaths(t, root)[0])

	code, _, stderr := run("changes", "--root", root)
	if code != int(ExitStateActionRequired) {
		t.Fatalf("changes exit = %d, want %d (storage); stderr = %q", code, int(ExitStateActionRequired), stderr)
	}
	if strings.Contains(stderr, "no checkpoints") {
		t.Errorf("changes claimed no checkpoints for an unreadable store: %q", stderr)
	}
	if !strings.Contains(stderr, "partially unreadable") {
		t.Errorf("changes missing actionable diagnostic: %q", stderr)
	}
}

// TestChangesExplicitHealthyIDStillWorks is acceptance scenario 2: an explicit
// readable checkpoint reference resolves even when another checkpoint is incompatible.
func TestChangesExplicitHealthyIDStillWorks(t *testing.T) {
	root := checkpointProject(t)
	secondCheckpoint(t, root)
	headers := checkpointHeaderPaths(t, root)
	makeIncompatible(t, headers[0])

	// Find the surviving readable checkpoint's id via log --json.
	code, stdout, _ := run("log", "--root", root, "--json")
	if code != int(ExitSuccess) {
		t.Fatalf("log --json exit = %d", code)
	}
	var doc struct {
		Checkpoints []struct {
			ShortID string `json:"short_id"`
		} `json:"checkpoints"`
	}
	decodeEnvelope(t, stdout, &doc)
	if len(doc.Checkpoints) != 1 {
		t.Fatalf("want 1 readable checkpoint, got %d", len(doc.Checkpoints))
	}
	healthy := doc.Checkpoints[0].ShortID

	code, _, stderr := run("changes", "--root", root, healthy+"..now")
	if code != int(ExitSuccess) {
		t.Fatalf("changes <healthy>..now exit = %d, stderr = %q", code, stderr)
	}
}

// TestDoctorReportsIncompatibleFormat is acceptance scenario 1 for doctor: the incompatible
// format is reported under a stable code and is not repaired.
func TestDoctorReportsIncompatibleFormat(t *testing.T) {
	root := checkpointProject(t)
	headerPath := checkpointHeaderPaths(t, root)[0]
	makeIncompatible(t, headerPath)

	code, stdout, _ := run("doctor", "--root", root, "--json")
	if code != int(ExitStateActionRequired) {
		t.Fatalf("doctor --json exit = %d, want %d", code, int(ExitStateActionRequired))
	}
	var doc struct {
		Health   string `json:"health"`
		Findings []struct {
			Code       string `json:"code"`
			Repairable bool   `json:"repairable"`
		} `json:"findings"`
	}
	decodeEnvelope(t, stdout, &doc)
	if doc.Health != "failed" {
		t.Errorf("doctor health = %q, want failed", doc.Health)
	}
	var f *struct {
		Code       string `json:"code"`
		Repairable bool   `json:"repairable"`
	}
	for i := range doc.Findings {
		if doc.Findings[i].Code == "checkpoint-incompatible-format" {
			f = &doc.Findings[i]
		}
	}
	if f == nil {
		t.Fatalf("no checkpoint-incompatible-format finding: %+v", doc.Findings)
	}
	if f.Repairable {
		t.Error("incompatible-format finding is repairable, want non-repairable")
	}

	// --repair must not touch the incompatible checkpoint: it stays on disk and stays
	// incompatible on a re-run.
	if code, _, _ := run("doctor", "--root", root, "--repair"); code != int(ExitStateActionRequired) {
		t.Fatalf("doctor --repair exit = %d, want %d (incompatible untouched)", code, int(ExitStateActionRequired))
	}
	if _, err := os.Stat(headerPath); err != nil {
		t.Errorf("incompatible checkpoint header removed by --repair: %v", err)
	}
}
