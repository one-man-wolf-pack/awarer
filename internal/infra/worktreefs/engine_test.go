package worktreefs

import (
	"testing"

	gi "github.com/git-pkgs/gitignore"

	"awarer/internal/domain/pattern"
)

func mustEngine(tb testing.TB, excludes []string, useGitignore, useAwaignore bool) *ignoreEngine {
	tb.Helper()
	e, err := newIgnoreEngine(pattern.KeyRules("exclude", excludes), useGitignore, useAwaignore, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return e
}

func (e *ignoreEngine) mustIgnore(rel string, isDir bool) bool {
	ignored, err := e.ignores(rel, isDir)
	if err != nil {
		panic(err)
	}
	return ignored
}

func (e *ignoreEngine) mustCombined(dirRel string) *gi.Matcher {
	m, err := e.combinedFor(dirRel)
	if err != nil {
		panic(err)
	}
	return m
}
