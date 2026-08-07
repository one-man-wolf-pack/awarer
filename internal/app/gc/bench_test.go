package gc

import (
	"context"
	"fmt"
	"testing"
	"time"

	gcdom "awarer/internal/domain/gc"
)

// benchPopulatedEnv builds a store with checkpoints and runs whose shape exposes any
// accidental quadratic in planning. Each checkpoint adds one file with unique content
// (a fresh blob) plus a shared file, so the blob count grows with the checkpoint count
// and the blob sweep must mark reachability across every retained manifest.
func benchPopulatedEnv(b *testing.B, checkpoints, runs int) *env {
	b.Helper()
	e := newEnv(b)
	e.writeFiles(b, map[string]string{"common.go": "package common // shared across every checkpoint"})
	base := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < checkpoints; i++ {
		// Unique content per checkpoint keeps producing fresh blobs; padding makes the
		// manifest non-trivial so streaming/marking does real work.
		e.writeFiles(b, map[string]string{
			fmt.Sprintf("pkg/dir%02d/file%05d.go", i%20, i): fmt.Sprintf("package p\n// unique %d\n", i),
		})
		e.checkpointAt(b, base.Add(time.Duration(i)*time.Second))
	}
	for i := 0; i < runs; i++ {
		e.seedRun(b, fmt.Sprintf("run-%d", i), base.Add(time.Duration(i)*time.Second))
	}
	return e
}

// BenchmarkPlanPopulatedStore measures a full mark/sweep plan over a store with many
// checkpoints, blobs, and runs. keep-last is small so most checkpoints are deletion
// candidates and the blob sweep actually runs (the expensive path), guarding against
// an O(checkpoints*blobs) reachability scan.
func BenchmarkPlanPopulatedStore(b *testing.B) {
	e := benchPopulatedEnv(b, 60, 40)
	now := time.Unix(1_700_100_000, 0).UTC()
	req, err := gcdom.NewGCRequest(false, gcdom.FilterAll, 2, 0, false)
	if err != nil {
		b.Fatal(err)
	}
	svc := New(Deps{
		Now:          func() time.Time { return now },
		Hostname:     "benchhost",
		ProcessAlive: func(int) bool { return false },
	})
	r := Request{Project: e.project, Config: e.cfg}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Plan(context.Background(), req, r); err != nil {
			b.Fatal(err)
		}
	}
}
