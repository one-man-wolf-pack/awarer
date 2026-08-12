package effectfs_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/effectfs"
)

// buildEffectTree writes n stat-only files under a single watched generated-output root,
// the shape effect observation walks on a generated-output-heavy project.
func buildEffectTree(b *testing.B, root, watched string, n int) {
	b.Helper()
	dir := filepath.Join(root, watched)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "d"+strconv.Itoa(i%50), "f"+strconv.Itoa(i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

func buildSourceTree(b *testing.B, root string, dirs int) {
	b.Helper()
	for i := 0; i < dirs; i++ {
		p := filepath.Join(root, "p"+strconv.Itoa(i%40), "m"+strconv.Itoa(i%400), "s"+strconv.Itoa(i))
		if err := os.MkdirAll(p, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f.go"), []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}

// The tree deliberately matches no selector: a match would prune the walk at one root
// and stop measuring the frontier and per-directory path cost this exists to bound.
func BenchmarkDiscoverSourceTree(b *testing.B) {
	h := blake3hash.New()
	root := b.TempDir()
	buildSourceTree(b, root, 4_000)
	w := effectfs.New(h)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Discover(root, []string{"target", "artifacts/bin"}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDiscoverGeneratedRoot measures the effect-observation walk over a watched
// generated-output root — the stat-only traversal that dominates run/ls/status latency
// on a project with a large target/ or node_modules/. It is the surface the
// large-effect-root diagnostic explains.
func BenchmarkDiscoverGeneratedRoot(b *testing.B) {
	h := blake3hash.New()
	root := b.TempDir()
	buildEffectTree(b, root, "target", 5_000)
	w := effectfs.New(h)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Discover(root, []string{"target"}); err != nil {
			b.Fatal(err)
		}
	}
}
