package worktreefs_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/worktreefs"
)

// walkAllNodePaths runs the walker and returns the rel-paths of every emitted node
// (files and directories alike), so a test can assert that an excluded directory
// entry is never emitted — not only that its files are absent.
func walkAllNodePaths(t *testing.T, root string, scope config.ScanScope) []string {
	t.Helper()
	w := worktreefs.New()
	var got []string
	err := w.Walk(context.Background(), paths.New(root), scope, func(n worktree.Node) error {
		if p := n.Path.String(); p != "" {
			got = append(got, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Strings(got)
	return got
}

// TestWalkDirectoryOnlyPatternPrunesDirectory proves the walk-level effect of the
// directory-aware ignore decision: a directory matched by a directory-only .awaignore
// pattern is pruned entirely — the directory node is never emitted, so its identity
// cannot appear as an "added" entry when the directory first exists after a baseline.
// This is red on the pre-fix walker, which entered the directory and emitted it as a
// node while excluding only its files.
func TestWalkDirectoryOnlyPatternPrunesDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".awaignore", ".rezonator/\n")
	writeFile(t, root, "app.go", "package app")
	writeFile(t, root, ".rezonator/ledger.sqlite", "LEDGER")
	writeFile(t, root, ".rezonator/nested/other.db", "DB")

	nodes := walkAllNodePaths(t, root, config.Defaults().HistoryScanScope())

	for _, n := range nodes {
		if n == ".rezonator" || strings.HasPrefix(n, ".rezonator/") {
			t.Errorf("no node under a directory-only excluded pattern may be emitted, got %q (all: %v)", n, nodes)
		}
	}
	if !contains(nodes, "app.go") {
		t.Errorf("a real source file must still be scanned, got %v", nodes)
	}
}

// TestWalkDirectoryOnlyPatternEqualsExtraExcludes proves the equivalence the contract
// promises: excluding a directory via a `.awaignore` "foo/" pattern yields the same
// scanned set as excluding it via shared-config extra_excludes, whether or not the
// directory existed when a baseline was recorded.
func TestWalkDirectoryOnlyPatternEqualsExtraExcludes(t *testing.T) {
	// Case A: .awaignore with a directory-only pattern.
	rootA := t.TempDir()
	writeFile(t, rootA, ".awaignore", "vendorcache/\n")
	writeFile(t, rootA, "main.go", "package main")
	writeFile(t, rootA, "vendorcache/dep/x.bin", "x")

	// Case B: the equivalent shared-config extra_excludes.
	rootB := t.TempDir()
	writeFile(t, rootB, "main.go", "package main")
	writeFile(t, rootB, "vendorcache/dep/x.bin", "x")
	scopeB := config.Defaults().HistoryScanScope()
	scopeB.Exclude = append(append([]string(nil), scopeB.Exclude...), "vendorcache")

	gotA := walkAllNodePaths(t, rootA, config.Defaults().HistoryScanScope())
	gotB := walkAllNodePaths(t, rootB, scopeB)

	// Case A additionally has the .awaignore file itself; drop it before comparing the
	// scanned identity of the two exclusion mechanisms.
	filtered := gotA[:0]
	for _, n := range gotA {
		if n != ".awaignore" {
			filtered = append(filtered, n)
		}
	}
	if !equalStrings(filtered, gotB) {
		t.Errorf(".awaignore dir-only exclusion (%v) does not match extra_excludes exclusion (%v)", filtered, gotB)
	}
}
