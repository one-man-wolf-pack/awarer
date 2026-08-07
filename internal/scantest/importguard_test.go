package scantest_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scantestImportPath is the package this guard protects.
const scantestImportPath = `"awarer/internal/scantest"`

// TestNoProductionPackageImportsScantest pins the rule that makes this package safe
// to grow: scantest is test support, so only _test.go files may import it. The rule
// needs an oracle because scantest is an ordinary (non-test) package, whose helpers
// the compiler will happily link into any caller.
//
// CanonicalCursor and CanonicalStream are the sharp edge. They are test-owned
// materializing adapters: they hold a whole manifest in memory by design, which is
// exactly what a fixture wants and exactly what the stream-first boundary forbids of
// a production path. Production sources open bounded cursors over durable records;
// importing one of these instead would put an unbounded in-memory manifest on that
// path under a name that reads like ordinary support code.
func TestNoProductionPackageImportsScantest(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS and local-evidence trees, and this package's own sources —
			// scantest may of course refer to itself.
			switch d.Name() {
			case ".git", ".awa", "testdata":
				return fs.SkipDir
			}
			if path == filepath.Join(root, "internal", "scantest") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		checked++
		for _, imp := range f.Imports {
			if imp.Path.Value == scantestImportPath {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s is not a _test.go file but imports internal/scantest; test fixtures must not be reachable from a built binary", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
	// A walk that inspected nothing would pass vacuously.
	if checked < 100 {
		t.Fatalf("only %d non-test Go files inspected; the walk did not reach the module", checked)
	}
}

// moduleRoot locates the repository root by walking up from this package until it
// finds the go.mod that declares the module, so the guard does not depend on where
// the test binary is run from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the scantest package")
		}
		dir = parent
	}
}
