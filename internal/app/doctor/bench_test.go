package doctor

import (
	"fmt"
	"testing"
)

// benchPopulated builds a project with many checkpoints (each adding a unique blob plus
// a shared one) so doctor's checkpoint/blob/index/run/lock checks all have real work.
func benchPopulated(b *testing.B, checkpoints int) *env {
	b.Helper()
	e := newEnv(b)
	for i := 0; i < checkpoints; i++ {
		e.checkpointWith(b, e.cfg, map[string]string{
			"common.go": "package common // shared",
			fmt.Sprintf("pkg/dir%02d/file%05d.go", i%20, i): fmt.Sprintf("package p\n// unique %d\n", i),
		})
	}
	return e
}

// BenchmarkRunChecks measures non-strict diagnosis (presence/structure checks) over a
// populated store. Strict re-hashing is benchmarked separately because it is the
// deliberately more expensive verification path.
func BenchmarkRunChecks(b *testing.B) {
	e := benchPopulated(b, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.run(b, noGit(), Request{Resolved: resolvedConfig(e.cfg), Strict: false})
	}
}

// BenchmarkRunChecksStrict measures strict diagnosis, which re-hashes blobs and
// verifies checkpoint manifests against their tree hash — the heavier path that scales
// with total stored content.
func BenchmarkRunChecksStrict(b *testing.B) {
	e := benchPopulated(b, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.run(b, noGit(), Request{Resolved: resolvedConfig(e.cfg), Strict: true})
	}
}
