package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	apprun "awarer/internal/app/run"
	"awarer/internal/domain/runcache"
)

// TestExplainViewSkippedSamplesIncludeReason guards the JSON contract: a skipped
// input sample carries both its path and the reason it was skipped, so an agent
// reasoning about cacheability sees why an input dropped out, not just which one.
func TestExplainViewSkippedSamplesIncludeReason(t *testing.T) {
	res := apprun.ExplainResult{
		Outcome: apprun.OutcomeUncached,
		Reason:  "skipped-inputs",
		Subject: apprun.ExplainSubject{
			Skipped: runcache.SkippedSummary{
				Count:   1,
				Allowed: false,
				Samples: []runcache.SkippedSample{{Path: "big.bin", Reason: "large-file-skip"}},
			},
		},
	}

	data, err := json.Marshal(explainView(res))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc struct {
		Current struct {
			Skipped struct {
				Count   int  `json:"count"`
				Allowed bool `json:"allowed"`
				Samples []struct {
					Path   string `json:"path"`
					Reason string `json:"reason"`
				} `json:"samples"`
			} `json:"skipped"`
		} `json:"current"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, data)
	}
	if doc.Current.Skipped.Count != 1 || len(doc.Current.Skipped.Samples) != 1 {
		t.Fatalf("skipped = %+v, want one sample", doc.Current.Skipped)
	}
	s := doc.Current.Skipped.Samples[0]
	if s.Path != "big.bin" || s.Reason != "large-file-skip" {
		t.Errorf("sample = %+v, want path big.bin / reason large-file-skip", s)
	}
}

// envDifference builds one per-variable environment difference for the rendering tests.
func envDifference(name string) runcache.Difference {
	return runcache.Difference{
		Code:      runcache.DiffEnvChanged,
		Field:     "env",
		Old:       "<absent>",
		New:       "<set:blake3:deadbeef>",
		EnvName:   name,
		EnvChange: runcache.EnvAdded,
	}
}

// TestDifferenceLinesLeadWithThePrimaryReason pins the ordering rule. The comparison
// records differences in field order, which is not the order of usefulness: a reader
// looking at a miss needs the change that decided it before the detail that merely
// accompanies it.
func TestDifferenceLinesLeadWithThePrimaryReason(t *testing.T) {
	cmp := runcache.KeyComparison{Differences: []runcache.Difference{
		envDifference("LANG"),
		{Code: runcache.DiffInputTreeChanged, Field: "input_tree_hash", Old: "blake3:aa", New: "blake3:bb"},
		{Code: runcache.DiffCWDChanged, Field: "cwd", Old: ".", New: "sub"},
	}}
	// cwd outranks env, which outranks the input tree.
	if got := cmp.PrimaryReason(); got != runcache.DiffCWDChanged {
		t.Fatalf("fixture primary reason = %q, want cwd-changed; the ordering assertion below would prove nothing", got)
	}

	lines := differenceLines(cmp)
	if len(lines) != 3 {
		t.Fatalf("lines = %v, want three", lines)
	}
	if !strings.Contains(lines[0], "cwd") {
		t.Errorf("first line = %q, want the primary reason (cwd) to lead", lines[0])
	}
}

// The presentation guard: comparing against evidence recorded under a different
// effective environment can differ in the whole locale family plus the injected
// marker at once. Human output shows a bounded, deterministic prefix and says how
// much it withheld; it never silently truncates, and it never drops a
// non-environment difference to make room.
func TestDifferenceLinesBoundEnvironmentDetail(t *testing.T) {
	diffs := []runcache.Difference{
		{Code: runcache.DiffInputTreeChanged, Field: "input_tree_hash", Old: "blake3:aa", New: "blake3:bb"},
	}
	names := []string{"AWA_RUN", "LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME"}
	for _, n := range names {
		diffs = append(diffs, envDifference(n))
	}
	lines := differenceLines(runcache.KeyComparison{Differences: diffs})

	var envLines int
	for _, l := range lines {
		if strings.HasPrefix(l, "env ") {
			envLines++
		}
	}
	if envLines != maxHumanEnvDifferences {
		t.Errorf("printed %d env lines, want the bound %d", envLines, maxHumanEnvDifferences)
	}
	// The prefix is deterministic: the shown names are the first in the comparison's
	// own order, not an arbitrary subset.
	var shownNames []string
	for _, l := range lines {
		if strings.HasPrefix(l, "env ") {
			shownNames = append(shownNames, strings.Fields(l)[1])
		}
	}
	if want := names[:maxHumanEnvDifferences]; !slices.Equal(shownNames, want) {
		t.Errorf("shown env names = %v, want the leading prefix %v", shownNames, want)
	}
	// The non-environment difference survives the bound.
	var sawTree bool
	for _, l := range lines {
		if strings.Contains(l, "input tree changed") {
			sawTree = true
		}
	}
	if !sawTree {
		t.Error("the input-tree difference was dropped; the bound applies to environment detail only")
	}
	// And the omission is stated with exact counts, never implied — directly after the
	// environment lines it counts, so an unrelated difference cannot come between them.
	var noticeAt = -1
	for i, l := range lines {
		if strings.HasPrefix(l, "env: showing") {
			noticeAt = i
		}
	}
	if noticeAt < 0 {
		t.Fatalf("no omission notice in %v", lines)
	}
	if !strings.HasPrefix(lines[noticeAt-1], "env ") {
		t.Errorf("the line before the notice is %q, want the last shown environment difference", lines[noticeAt-1])
	}
	omitted := len(names) - maxHumanEnvDifferences
	for _, want := range []string{
		fmt.Sprintf("showing %d of %d", maxHumanEnvDifferences, len(names)),
		fmt.Sprintf("(+%d more)", omitted),
		"--json",
	} {
		if !strings.Contains(lines[noticeAt], want) {
			t.Errorf("omission line = %q, want it to contain %q", lines[noticeAt], want)
		}
	}
}

// TestDifferenceLinesSayNothingWhenNothingIsOmitted keeps the bound invisible in the
// ordinary case: a run that differs in one variable must not gain a counting line.
func TestDifferenceLinesSayNothingWhenNothingIsOmitted(t *testing.T) {
	lines := differenceLines(runcache.KeyComparison{Differences: []runcache.Difference{envDifference("CI")}})
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want exactly the one difference", lines)
	}
	if strings.Contains(lines[0], "more") {
		t.Errorf("line = %q, want no omission notice when nothing was omitted", lines[0])
	}
}

// TestExplainJSONKeepsEveryDifference proves the bound is presentation only. An agent
// diagnosing a miss must still receive the complete typed difference set, and the
// comparison that decides cacheability is untouched by how it is displayed.
func TestExplainJSONKeepsEveryDifference(t *testing.T) {
	diffs := []runcache.Difference{}
	names := []string{"AWA_RUN", "LANG", "LANGUAGE", "LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME"}
	for _, n := range names {
		diffs = append(diffs, envDifference(n))
	}
	cmp := runcache.KeyComparison{Differences: diffs}

	if got := len(differenceLines(cmp)); got >= len(names) {
		t.Fatalf("human output printed %d lines for %d differences; it is not bounded", got, len(names))
	}
	view := explainView(apprun.ExplainResult{Outcome: apprun.OutcomeMiss, Differences: cmp})
	if got := len(view.Differences); got != len(names) {
		t.Errorf("JSON differences = %d, want all %d: truncation must not reach the machine surface", got, len(names))
	}
}
