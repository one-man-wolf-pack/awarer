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

// benchDuplicateHeavy re-checkpoints a stable file set, so blob references grow with
// checkpoint count while unique content hashes stay near the file count — the shape
// real histories showed, where most references repeat a hash already verified.
func benchDuplicateHeavy(b *testing.B, checkpoints, files int) *env {
	b.Helper()
	e := newEnv(b)
	set := make(map[string]string, files+1)
	for i := 0; i < files; i++ {
		set[fmt.Sprintf("pkg/dir%02d/file%03d.go", i%10, i)] = fmt.Sprintf("package p\n// stable %d\n", i)
	}
	for c := 0; c < checkpoints; c++ {
		// One changing marker keeps the checkpoints distinct without disturbing the
		// duplicate ratio.
		set["marker.go"] = fmt.Sprintf("package p\n// checkpoint %d\n", c)
		e.checkpointWith(b, e.cfg, set)
	}
	return e
}

// benchUniqueHeavy stores one checkpoint over many distinct files, so nearly every
// reference is a first sighting. It is the counterweight to the duplicate-heavy shape:
// here per-hash bookkeeping is pure overhead with no repeated read to save. The file
// count is chosen so total references are comparable to benchDuplicateHeavy's.
func benchUniqueHeavy(b *testing.B, files int) *env {
	b.Helper()
	e := newEnv(b)
	set := make(map[string]string, files)
	for i := 0; i < files; i++ {
		set[fmt.Sprintf("pkg/dir%02d/file%04d.go", i%20, i)] = fmt.Sprintf("package p\n// unique %d\n", i)
	}
	e.checkpointWith(b, e.cfg, set)
	return e
}

// benchDiagnose times whole doctor runs over an already built store: the fixture is the
// benchmark's variable, so every shape below differs only in the history it walks and
// in strict mode.
func benchDiagnose(b *testing.B, e *env, strict bool) {
	b.Helper()
	b.ReportAllocs()
	for b.Loop() {
		_ = e.run(b, noGit(), Request{Resolved: resolvedConfig(e.cfg), Strict: strict})
	}
}

// BenchmarkRunChecks measures non-strict diagnosis (presence/structure checks) over a
// populated store. Strict re-hashing is benchmarked separately because it is the
// deliberately more expensive verification path.
func BenchmarkRunChecks(b *testing.B) { benchDiagnose(b, benchPopulated(b, 50), false) }

// BenchmarkRunChecksStrict measures strict diagnosis, which re-hashes blobs and
// verifies checkpoint manifests against their tree hash — the heavier path that scales
// with total stored content.
func BenchmarkRunChecksStrict(b *testing.B) { benchDiagnose(b, benchPopulated(b, 50), true) }

// BenchmarkRunChecksDuplicateHeavy measures the shape reuse is meant to help: most blob
// references repeat a hash an earlier checkpoint already verified.
func BenchmarkRunChecksDuplicateHeavy(b *testing.B) {
	benchDiagnose(b, benchDuplicateHeavy(b, 30, 40), false)
}

func BenchmarkRunChecksDuplicateHeavyStrict(b *testing.B) {
	benchDiagnose(b, benchDuplicateHeavy(b, 30, 40), true)
}

// BenchmarkRunChecksUniqueHeavy measures the opposite shape, where per-hash bookkeeping
// buys nothing and its cost is exposed on its own.
func BenchmarkRunChecksUniqueHeavy(b *testing.B) {
	benchDiagnose(b, benchUniqueHeavy(b, 1200), false)
}

func BenchmarkRunChecksUniqueHeavyStrict(b *testing.B) {
	benchDiagnose(b, benchUniqueHeavy(b, 1200), true)
}
