package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

type scopeDoc struct {
	Data struct {
		Left  map[string]any `json:"left"`
		Right map[string]any `json:"right"`
		Basis struct {
			ScanConfig map[string]string `json:"scan_config"`
		} `json:"basis"`
		Changes json.RawMessage `json:"changes"`
		Files   json.RawMessage `json:"files"`
		Summary map[string]int  `json:"summary"`
		Scope   struct {
			SkippedInScope              *int            `json:"skipped_in_scope"`
			IgnoreSources               []string        `json:"ignore_sources"`
			IgnoreSourcesBasis          string          `json:"ignore_sources_basis"`
			IgnoreSourceParticipation   json.RawMessage `json:"ignore_source_participation"`
			IgnoredPathsOutsideEvidence *bool           `json:"ignored_paths_outside_evidence"`
		} `json:"scope"`
	} `json:"data"`
}

func (d scopeDoc) delta() string {
	return string(d.Data.Changes) + string(d.Data.Files)
}

func comparisonScope(t *testing.T, root, command string, args ...string) scopeDoc {
	t.Helper()
	code, stdout, stderr := run(append([]string{command, "--root", root, "--json"}, args...)...)
	if code != int(ExitSuccess) {
		t.Fatalf("%s %v exit = %d, stderr = %q", command, args, code, stderr)
	}
	var doc scopeDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%s %v: invalid JSON: %v\n%s", command, args, err, stdout)
	}
	s := doc.Data.Scope
	if s.IgnoreSourcesBasis != "current-configuration" {
		t.Errorf("%s %v: ignore_sources_basis = %q, want current-configuration", command, args, s.IgnoreSourcesBasis)
	}
	var participation map[string]string
	if err := json.Unmarshal(s.IgnoreSourceParticipation, &participation); err != nil ||
		!reflect.DeepEqual(participation, map[string]string{"left": "unknown", "right": "unknown"}) {
		t.Errorf("%s %v: ignore_source_participation = %s, want unknown on both sides", command, args, s.IgnoreSourceParticipation)
	}
	if s.IgnoredPathsOutsideEvidence == nil || !*s.IgnoredPathsOutsideEvidence {
		t.Errorf("%s %v: ignored_paths_outside_evidence must be true", command, args)
	}
	if s.SkippedInScope == nil {
		t.Fatalf("%s %v: skipped_in_scope missing:\n%s", command, args, stdout)
	}
	if *s.SkippedInScope != doc.Data.Summary["skipped"] {
		t.Errorf("%s %v: skipped_in_scope = %d, want summary.skipped %d", command, args, *s.SkippedInScope, doc.Data.Summary["skipped"])
	}
	return doc
}

func checkpointNow(t *testing.T, root string) string {
	t.Helper()
	code, stdout, stderr := run("checkpoint", "--root", root)
	if code != int(ExitSuccess) {
		t.Fatalf("checkpoint exit = %d, stderr = %q", code, stderr)
	}
	return parseCheckpointID(t, stdout)
}

func TestComparisonScopeProvenanceOnEmptyAndNonEmptyDeltas(t *testing.T) {
	root := checkpointProject(t)
	for _, command := range []string{"changes", "diff"} {
		t.Run(command, func(t *testing.T) {
			empty := comparisonScope(t, root, command)
			if empty.delta() != "[]" || empty.Data.Right["kind"] != "now" {
				t.Fatalf("delta = %s against %v, want an empty array against now", empty.delta(), empty.Data.Right)
			}
			if want := []string{"awaignore", "baseline-excludes"}; !slices.Equal(empty.Data.Scope.IgnoreSources, want) {
				t.Errorf("ignore_sources = %v, want %v", empty.Data.Scope.IgnoreSources, want)
			}
		})
	}
	writeFile(t, root, "calc/calc.go", "package calc\n")
	for _, command := range []string{"changes", "diff"} {
		t.Run(command+" modified", func(t *testing.T) {
			if doc := comparisonScope(t, root, command); doc.Data.Summary["modified"] != 1 {
				t.Errorf("summary = %v, want one modified entry", doc.Data.Summary)
			}
		})
	}
}

func TestCurrentConfigChangeRelabelsContextNotStoredOperands(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "keep.txt", "one\n")
	first := checkpointNow(t, root)
	writeFile(t, root, "keep.txt", "two\n")
	writeFile(t, root, "gen/a.txt", "a\n")
	second := checkpointNow(t, root)
	rng := first + ".." + second

	for _, command := range []string{"changes", "diff"} {
		t.Run(command, func(t *testing.T) {
			writeLocalConfig(t, root, "")
			before := comparisonScope(t, root, command, rng)
			writeLocalConfig(t, root, "[scope]\nuse_gitignore = true\nuse_awaignore = false\nextra_excludes = [\"gen/\"]\n")
			after := comparisonScope(t, root, command, rng)

			if !reflect.DeepEqual(before.Data.Left, after.Data.Left) || !reflect.DeepEqual(before.Data.Right, after.Data.Right) ||
				!reflect.DeepEqual(before.Data.Basis, after.Data.Basis) {
				t.Errorf("stored operands moved with today's config:\nbefore %+v %+v %+v\nafter  %+v %+v %+v",
					before.Data.Left, before.Data.Right, before.Data.Basis, after.Data.Left, after.Data.Right, after.Data.Basis)
			}
			if before.delta() != after.delta() || !reflect.DeepEqual(before.Data.Summary, after.Data.Summary) || before.Data.Summary["added"] == 0 {
				t.Errorf("delta moved with today's config or is empty:\nbefore %s\nafter  %s", before.delta(), after.delta())
			}
			if string(before.Data.Scope.IgnoreSourceParticipation) != string(after.Data.Scope.IgnoreSourceParticipation) {
				t.Errorf("participation moved with today's config: %s -> %s",
					before.Data.Scope.IgnoreSourceParticipation, after.Data.Scope.IgnoreSourceParticipation)
			}
			if want := []string{"awaignore", "baseline-excludes"}; !slices.Equal(before.Data.Scope.IgnoreSources, want) {
				t.Errorf("ignore_sources before = %v, want %v", before.Data.Scope.IgnoreSources, want)
			}
			if want := []string{"gitignore", "config-excludes", "baseline-excludes"}; !slices.Equal(after.Data.Scope.IgnoreSources, want) {
				t.Errorf("ignore_sources after = %v, want %v", after.Data.Scope.IgnoreSources, want)
			}
		})
	}
}

func TestEnabledButAbsentIgnoreFileLeavesNowParticipationUnknown(t *testing.T) {
	root := initProject(t)
	writeLocalConfig(t, root, "[scope]\nuse_gitignore = true\n")
	writeFile(t, root, "keep.txt", "one\n")
	checkpointNow(t, root)
	writeFile(t, root, "keep.txt", "two\n")
	for _, name := range []string{".gitignore", ".awaignore"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("fixture must have no %s: %v", name, err)
		}
	}

	for _, command := range []string{"changes", "diff"} {
		t.Run(command, func(t *testing.T) {
			doc := comparisonScope(t, root, command)
			if doc.Data.Right["kind"] != "now" {
				t.Fatalf("right = %v, want now", doc.Data.Right)
			}
			if want := []string{"gitignore", "awaignore", "baseline-excludes"}; !slices.Equal(doc.Data.Scope.IgnoreSources, want) {
				t.Errorf("ignore_sources = %v, want %v", doc.Data.Scope.IgnoreSources, want)
			}
		})
	}
}

func TestSkippedInScopeCountsTheSelectedDeltaOnly(t *testing.T) {
	root := initProject(t)
	writeFile(t, root, "a/one.txt", "one\n")
	writeFile(t, root, "b/two.txt", "two\n")
	checkpointNow(t, root)
	for _, rel := range []string{"a/one.txt", "b/two.txt"} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Chmod(abs, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(abs, 0o644) })
		if f, err := os.Open(abs); err == nil { //nolint:gosec // test fixture path
			_ = f.Close()
			t.Skip("platform or user does not enforce file read permissions")
		}
	}

	for _, command := range []string{"changes", "diff"} {
		t.Run(command, func(t *testing.T) {
			if all := comparisonScope(t, root, command); *all.Data.Scope.SkippedInScope != 2 {
				t.Errorf("unfiltered skipped_in_scope = %d, want 2", *all.Data.Scope.SkippedInScope)
			}
			if one := comparisonScope(t, root, command, "--", filepath.Join(root, "a")); *one.Data.Scope.SkippedInScope != 1 {
				t.Errorf("filtered skipped_in_scope = %d, want 1 of the 2 skipped inputs", *one.Data.Scope.SkippedInScope)
			}
		})
	}
}
