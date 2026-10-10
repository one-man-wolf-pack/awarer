package worktreefs_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/pattern"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/worktreefs"
)

func TestWalkAppliesGitSemanticsAcrossSources(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{
		"abc", "abbc", "sub/gen/out", "deep/sub/gen/out", "x.log", "keep.log",
		"cache/blob", "nested/a.tmp", "nested/b.tmp", "nested/inner/a.tmp", ".awa/state", "build/x", "a[1]/x", "a1/x",
	} {
		writeFile(t, root, f, "x")
	}
	writeFile(t, root, ".awaignore", "a?c\nsub/gen\n!keep.log\n!build/\n")
	writeFile(t, root, "nested/.awaignore", "/a.tmp\n")
	writeFile(t, root, "a[1]/.awaignore", "x\n")

	cfg := config.Defaults()
	cfg.Scope.ExtraExcludes = []string{"*.log", "cache/", "!.awa", "!.awa/"}
	got := walkPaths(t, root, cfg)
	want := []string{
		".awaignore", "a1/x", "a[1]/.awaignore", "abbc", "build/x", "deep/sub/gen/out", "keep.log",
		"nested/.awaignore", "nested/b.tmp", "nested/inner/a.tmp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("walked %v\n   want %v", got, want)
	}
}

func TestWalkReportsEachNeverMatchingRuleOncePerSource(t *testing.T) {
	root := t.TempDir()
	for i := range 40 {
		writeFile(t, root, fmt.Sprintf("d%02d/f%02d.txt", i%5, i), "x")
	}
	writeFile(t, root, ".awaignore", "# note\\\r\nok\r\nx[\r\n")
	writeFile(t, root, "d01/.awaignore", "tail\\\n")

	cfg := config.Defaults()
	cfg.Scope.ExtraExcludes = []string{"fine", "[[:nope:]]"}
	cfg.History.ExtraExcludes = []string{"!"}

	var got []string
	w := worktreefs.NewReporting(func(d pattern.Diagnostic) { got = append(got, d.String()) })
	visited := 0
	err := w.Walk(context.Background(), paths.New(root), cfg.HistoryScanScope(), func(worktree.Node) error {
		visited++
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`scope.extra_excludes[1]: pattern "[[:nope:]]" never matches: unknown POSIX class [:nope:]`,
		`history.extra_excludes[0]: pattern "!" never matches: empty pattern`,
		`.awaignore:3: pattern "x[" never matches: unclosed bracket expression`,
		`d01/.awaignore:1: pattern "tail\\" never matches: trailing backslash`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("diagnostics:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if visited < 40 {
		t.Errorf("visited %d nodes; a never-matching rule must not shrink the observation", visited)
	}
}

func TestWalkFailsOnAnUncompilableExclude(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "x")
	scope := config.Defaults().HistoryScanScope()
	scope.Exclude = append(scope.Exclude, "a\nb")
	scope.ExcludeOrigins = nil

	err := worktreefs.New().Walk(context.Background(), paths.New(root), scope, func(worktree.Node) error { return nil }, nil)
	if err == nil || !strings.Contains(err.Error(), "line feed") {
		t.Errorf("Walk() error = %v, want the compile failure to stop the walk", err)
	}
}
