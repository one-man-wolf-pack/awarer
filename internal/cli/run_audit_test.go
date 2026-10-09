package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

// TestRunAuditRejectsEverythingButACount pins the audit grammar: a positive count
// within the published limit and nothing else — no operands, filters, time display,
// or child execution options — each refused as a usage error before any read.
func TestRunAuditRejectsEverythingButACount(t *testing.T) {
	root := initProject(t)
	for _, args := range [][]string{
		{"-n", "0"}, {"-n", "-1"}, {"-n", "abc"}, {"-n", "1001"}, {"--limit"},
		{"extra"}, {"--", "make"}, {"--command", "go"}, {"--time", "utc"},
		{"--all"}, {"--near"}, {"--record"}, {"--no-cache"}, {"--stdin", "null"},
		{"--scope", "src"}, {"--trust-mode", "fast"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			full := append([]string{"run", "audit", "--root", root}, args...)
			code, stdout, stderr := run(full...)
			if code != int(ExitUsageError) {
				t.Errorf("exit = %d, want usage error; stderr = %q", code, stderr)
			}
			if stdout != "" {
				t.Errorf("usage failure wrote stdout %q", stdout)
			}
		})
	}
	if code, _, stderr := run("run", "audit", "--root", root, "-n", "1000"); code != int(ExitSuccess) {
		t.Errorf("-n at the published limit: exit = %d, stderr = %q", code, stderr)
	}
}

// TestRunAuditEmptyAndMissingHistory pins the typed, non-null machine shape of a
// zero window and keeps its two known causes apart: a store with no run history
// location (no entries directory, or no runs directory at all) is missing history, one
// whose entries directory exists but is empty is empty history. Neither is an error
// or an unknown total. A history location that is not a directory fails the audit
// instead of reading as a healthy empty one.
func TestRunAuditEmptyAndMissingHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, runs string)
		history string
		human   string
	}{
		{"fresh store", func(*testing.T, string) {}, "missing", "no run history exists"},
		{"no runs directory", func(t *testing.T, runs string) {
			if err := os.RemoveAll(runs); err != nil {
				t.Fatal(err)
			}
		}, "missing", "no run history exists"},
		{"empty entries", func(t *testing.T, runs string) {
			if err := os.MkdirAll(filepath.Join(runs, "entries"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "empty", "run history is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := initProject(t)
			tc.setup(t, filepath.Join(root, ".awa", "runs"))
			code, stdout, stderr := run("run", "audit", "--root", root, "--json")
			if code != int(ExitSuccess) {
				t.Fatalf("exit = %d, stderr = %q", code, stderr)
			}
			var env struct {
				SchemaVersion int                        `json:"schema_version"`
				Command       string                     `json:"command"`
				Data          map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(stdout), &env); err != nil {
				t.Fatalf("parse: %v\n%s", err, stdout)
			}
			if env.SchemaVersion != 1 || env.Command != "run.audit" {
				t.Errorf("envelope = %d/%q", env.SchemaVersion, env.Command)
			}
			for _, key := range []string{"classifications", "suggestions"} {
				if string(env.Data[key]) != "[]" {
					t.Errorf("data.%s = %s, want []", key, env.Data[key])
				}
			}
			var window auditWindowDoc
			if err := json.Unmarshal(env.Data["window"], &window); err != nil {
				t.Fatal(err)
			}
			want := auditWindowDoc{History: tc.history, Requested: 200, Default: true, Max: 1000, Selected: 0, Older: "none", Coverage: "none"}
			if window != want {
				t.Errorf("window = %+v, want %+v", window, want)
			}
			var reads struct {
				Complete     bool            `json:"complete"`
				Availability json.RawMessage `json:"availability"`
			}
			if err := json.Unmarshal(env.Data["reads"], &reads); err != nil {
				t.Fatal(err)
			}
			if !reads.Complete || string(reads.Availability) != "[]" {
				t.Errorf("reads = %+v", reads)
			}

			code, stdout, _ = run("run", "audit", "--root", root)
			if code != int(ExitSuccess) || !strings.Contains(stdout, tc.human) {
				t.Errorf("human: exit %d stdout %q, want %q", code, stdout, tc.human)
			}
		})
	}

	t.Run("history location not a directory", func(t *testing.T) {
		root := initProject(t)
		if err := os.WriteFile(filepath.Join(root, ".awa", "runs", "entries"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := run("run", "audit", "--root", root, "--json")
		if code == int(ExitSuccess) || stdout != "" {
			t.Errorf("broken history: exit %d stdout %q stderr %q, want a failure with no document", code, stdout, stderr)
		}
	})
}

// TestRunAuditFailureContract covers the ordinary command failures: cancellation is
// an interruption and a failed report write is non-zero, unlike a partial audit.
func TestRunAuditFailureContract(t *testing.T) {
	root := initProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, _, stderr := runCtx(ctx, "run", "audit", "--root", root); code != int(ExitInterrupted) {
		t.Errorf("cancelled audit exit = %d, want %d; stderr = %q", code, ExitInterrupted, stderr)
	}
	var errBuf bytes.Buffer
	if code := Run(context.Background(), []string{"run", "audit", "--root", root, "--json"}, failWriter{}, &errBuf); code == int(ExitSuccess) {
		t.Error("a failed report write must not exit 0")
	}
}

func auditIDs(t *testing.T, n int) []runcache.RunID {
	t.Helper()
	out := make([]runcache.RunID, n)
	for i := range out {
		id, err := runcache.NewRunID(int64(1_800_000_000_000_000_000)-int64(i), strings.NewReader(strings.Repeat("x", 8)))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = id
	}
	return out
}

// TestRunAuditSuggestionsFollowObservedGroups pins the conditional drilldown: one
// inspection per observed group, anchored on a full example id; nothing for a
// deliberate recording; doctor only when evidence was unreadable; and no advice
// that changes inputs, recording, or configuration.
func TestRunAuditSuggestionsFollowObservedGroups(t *testing.T) {
	ids := auditIDs(t, 6)
	ex := func(id runcache.RunID) apprun.AuditExamples {
		return apprun.AuditExamples{IDs: []runcache.RunID{id}, Total: 1}
	}
	res := apprun.AuditResult{
		Classes: []apprun.AuditClassCount{
			{Group: apprun.AuditReusableWhenStored, Kind: runcache.ReuseReusable, Count: 1, Examples: ex(ids[0])},
			{Group: apprun.AuditDeliberateRecording, Kind: runcache.ReuseRecordOnly, Reason: runcache.ReasonRecordOnly, Count: 1, Examples: ex(ids[1])},
			{Group: apprun.AuditPolicyBlocker, Kind: runcache.ReuseRecordOnly, Reason: runcache.ReasonStdinNotKeyed, Count: 1, Examples: ex(ids[2])},
			{Group: apprun.AuditChangedState, Kind: runcache.ReuseNonReusable, Reason: runcache.ReasonMutatedState, Count: 1, Examples: ex(ids[3])},
		},
		Availability: []apprun.AuditAvailabilityCount{{State: apprun.AuditCorrupt, Count: 1, Examples: ex(ids[5])}},
	}
	got := []string{}
	for _, s := range auditSuggestions(res) {
		got = append(got, s.Kind+" "+s.Reason+" "+strings.Join(s.Argv, " "))
	}
	want := []string{
		"run-explain-from-run reusable-when-stored awa run explain --from-run " + ids[0].String() + " --to-now",
		"run-show-meta policy-blocker awa run show " + ids[2].String() + " --meta",
		"run-compare changed-state awa changes run:" + ids[3].String() + ":before..run:" + ids[3].String() + ":after",
		"help-topic reason-tokens awa help run",
		"doctor degraded-evidence awa doctor",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("suggestions:\n got %q\nwant %q", got, want)
	}
	for _, s := range auditSuggestions(res) {
		for _, bad := range []string{"--stdin", "--exclude", "--no-cache", "config", "env_allowlist"} {
			if strings.Contains(s.Display, bad) {
				t.Errorf("suggestion %q prescribes %s", s.Display, bad)
			}
		}
	}

	// An effect-state-differs run kept its inputs unchanged, so a before/after input
	// compare would be empty; the drilldown is its stored metadata instead.
	res = apprun.AuditResult{Classes: []apprun.AuditClassCount{
		{Group: apprun.AuditChangedState, Kind: runcache.ReuseNonReusable, Reason: runcache.ReasonEffectStateDiffers, Count: 1, Examples: ex(ids[4])},
	}}
	if s := auditSuggestions(res); len(s) != 1 || s[0].Kind != "run-show-meta" {
		t.Errorf("effect-state-differs suggestions = %+v", s)
	}

	// Only a deliberate recording: nothing to suggest, and still a non-null array.
	res = apprun.AuditResult{Classes: []apprun.AuditClassCount{auditNoCacheClass(ids[1])}}
	if s := auditSuggestions(res); s == nil || len(s) != 0 {
		t.Errorf("deliberate-only suggestions = %#v, want empty non-nil", s)
	}
}

func auditNoCacheClass(id runcache.RunID) apprun.AuditClassCount {
	return apprun.AuditClassCount{Group: apprun.AuditDeliberateRecording, Kind: runcache.ReuseRecordOnly, Reason: runcache.ReasonNoCache, Count: 1,
		Examples: apprun.AuditExamples{IDs: []runcache.RunID{id}, Total: 1}}
}

// TestRunAuditViewDisclosesPartialReadsAndTruncation pins that per-record gaps make
// the reads incomplete without failing, and that example truncation is disclosed
// separately from the class count, which stays exact.
func TestRunAuditViewDisclosesPartialReadsAndTruncation(t *testing.T) {
	ids := auditIDs(t, 5)
	res := apprun.AuditResult{
		Requested: 10, Selected: 9, Classified: 7, Unavailable: 2, Older: false,
		Classes: []apprun.AuditClassCount{{Group: apprun.AuditReusableWhenStored, Kind: runcache.ReuseReusable, Count: 7,
			Examples: apprun.AuditExamples{IDs: ids[:3], Total: 7}}},
		Availability: []apprun.AuditAvailabilityCount{{State: apprun.AuditUnreadBudget, Count: 2,
			Examples: apprun.AuditExamples{IDs: ids[3:5], Total: 2}}},
	}
	doc := auditView(res, false)
	if doc.Reads.Complete || doc.Window.Coverage != "all-retained" || doc.Window.Default {
		t.Errorf("reads complete %v coverage %q default %v", doc.Reads.Complete, doc.Window.Coverage, doc.Window.Default)
	}
	c := doc.Classifications[0]
	if c.Count != 7 || len(c.Examples.RunIDs) != 3 || c.Examples.Bounded.Complete || c.Examples.Bounded.Limit != apprun.AuditExampleLimit {
		t.Errorf("class view = %+v", c)
	}
	if c.Examples.RunIDs[0] != ids[0].String() {
		t.Errorf("examples carry full ids, got %q", c.Examples.RunIDs[0])
	}
	a := doc.Reads.Availability[0]
	if a.State != "unread-budget" || a.Count != 2 || !a.Examples.Bounded.Complete {
		t.Errorf("availability view = %+v", a)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"currently_reusable", "hit_rate", "rate", "percent", "%"} {
		if strings.Contains(string(data), banned) {
			t.Errorf("audit JSON contains %q: %s", banned, data)
		}
	}
}
