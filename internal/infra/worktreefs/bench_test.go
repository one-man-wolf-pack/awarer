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
		e.ignores(p, false)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.ignores(paths[i%len(paths)], false)
	}
}

// benchShapeRoot builds a fixture root for one directory shape: three rule-bearing
// .gitignore boundaries plus leafs rule-free directories beneath the deepest one. The
// rule content is identical for every shape, so a shape only varies how many
// directories a walk must produce a matcher for.
func benchShapeRoot(b *testing.B, leafs int) (root string, ruleDirs, leafDirs []string) {
	b.Helper()
	root = b.TempDir()
	rules := map[string]string{
		"":          "*.bak\n/secret.txt\ntmp/\n",
		"pkg":       "*.generated.go\ntestdata/\n",
		"pkg/inner": "!keep.generated.go\nlocal*.go\n",
	}
	// Shallow-to-deep, the order a walk loads them in.
	ruleDirs = []string{"", "pkg", "pkg/inner"}
	for _, d := range ruleDirs {
		abs := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(abs, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(abs, ".gitignore"), []byte(rules[d]), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < leafs; i++ {
		d := fmt.Sprintf("pkg/inner/leaf%04d", i)
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			b.Fatal(err)
		}
		leafDirs = append(leafDirs, d)
	}
	return root, ruleDirs, leafDirs
}

// BenchmarkIgnoreMatcherConstruction measures the cold, once-per-walk cost of building
// the combined matchers a scan needs: a fresh engine loads the same rule content and
// then decides one path in every directory of the shape. BenchmarkIgnoreDecide
// deliberately warms the cache to isolate matching and so cannot observe this term;
// here the construction cost is the whole point, and comparing the shapes shows whether
// it tracks effective rule boundaries or directory count.
//
// The reported numbers are comparative evidence for a change, not an acceptance
// threshold: nothing in the suite asserts on them.
func BenchmarkIgnoreMatcherConstruction(b *testing.B) {
	shapes := []struct {
		name  string
		leafs int
	}{
		{"shallow_4_dirs", 4},
		{"directory_heavy_256_dirs", 256},
	}
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			root, ruleDirs, leafDirs := benchShapeRoot(b, s.leafs)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// A fresh engine per iteration: matcher construction is per-scan state, so
				// reusing one across iterations would measure a warm cache instead.
				e := newIgnoreEngine(builtinBenchExcludes, true, true)
				for _, d := range ruleDirs {
					if err := e.loadDirAt(filepath.Join(root, filepath.FromSlash(d)), d); err != nil {
						b.Fatal(err)
					}
				}
				for _, d := range leafDirs {
					if err := e.loadDirAt(filepath.Join(root, filepath.FromSlash(d)), d); err != nil {
						b.Fatal(err)
					}
					e.ignores(d+"/file.go", false)
				}
			}
		})
	}
}
