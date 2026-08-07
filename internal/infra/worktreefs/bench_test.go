package worktreefs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// builtinBenchExcludes mirrors a realistic set of root-relative built-in excludes.
var builtinBenchExcludes = []string{
	".git/", ".awa/", "node_modules/", "*.log", "*.tmp", "dist/", "build/", "vendor/",
}

// benchEngine builds an ignore engine primed with built-in excludes plus a per-dir
// .gitignore at several depths, so combinedFor must merge ancestor chains and the
// per-directory matcher cache is exercised the way a real walk exercises it.
func benchEngine(b *testing.B) (*ignoreEngine, []string) {
	b.Helper()
	root := b.TempDir()
	// A .gitignore at root and at two nested dirs, each contributing patterns that the
	// combined matcher for deep paths must consider.
	dirs := map[string]string{
		"":          "*.bak\n/secret.txt\ntmp/\n",
		"pkg":       "*.generated.go\ntestdata/\n",
		"pkg/dir05": "!keep.generated.go\nlocal*.go\n",
	}
	for dirRel, content := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(dirRel))
		if err := os.MkdirAll(abs, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(abs, ".gitignore"), []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	e := newIgnoreEngine(builtinBenchExcludes, true, true)
	for dirRel := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(dirRel))
		if err := e.loadDirAt(abs, dirRel); err != nil {
			b.Fatal(err)
		}
	}

	// A representative spread of paths: ignored and kept, shallow and deep, so the
	// matcher does real work and the per-directory cache is reused across iterations.
	var paths []string
	for i := 0; i < 200; i++ {
		paths = append(paths,
			fmt.Sprintf("pkg/dir%02d/file%04d.go", i%20, i),
			fmt.Sprintf("pkg/dir%02d/file%04d.generated.go", i%20, i),
			fmt.Sprintf("pkg/dir%02d/notes%04d.bak", i%20, i),
			fmt.Sprintf("node_modules/dep%02d/index.js", i%20),
		)
	}
	return e, paths
}

// BenchmarkIgnoreDecide measures the steady-state per-path ignore decision (the hot
// path during a worktree walk), where the per-directory combined matcher is already
// cached and each path is matched against the merged ancestor pattern set.
func BenchmarkIgnoreDecide(b *testing.B) {
	e, paths := benchEngine(b)
	// Warm the per-directory cache so the benchmark measures matching, not the one-time
	// combinedFor compile that a real walk amortizes across a directory's children.
	for _, p := range paths {
		e.decide(p, false)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.decide(paths[i%len(paths)], false)
	}
}
