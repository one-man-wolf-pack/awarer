package scanner_test

import (
	"context"
	"fmt"
	"testing"

	"awarer/internal/app/scanner"
	"awarer/internal/domain/config"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/sqliteindex"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// buildTree creates a generated directory tree of n files for benchmarking.
func buildTree(b *testing.B, root string, n int) {
	for i := 0; i < n; i++ {
		scantest.Write(b, root, fmt.Sprintf("pkg/dir%02d/file%04d.go", i%20, i), fmt.Sprintf("package p\n// file %d\n", i))
	}
}

func BenchmarkScanGeneratedTree(b *testing.B) {
	root := b.TempDir()
	proj := scantest.InitProject(b, root)
	buildTree(b, root, 1000)
	svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStatusReuseReadOnlyIndex measures the read-only acceleration on the
// status/run-reuse configuration (ReadOnly scans that never persist). Both
// sub-benchmarks scan an unchanged generated tree; "rehash-no-index" re-hashes every
// file, while "reuse-readonly-index" reuses committed hashes through a strictly
// read-only index. The delta is the acceleration that status's reuse observation now
// gets.
func BenchmarkStatusReuseReadOnlyIndex(b *testing.B) {
	const n = 1000
	scope := config.Defaults().HistoryScanScope()

	b.Run("rehash-no-index", func(b *testing.B) {
		root := b.TempDir()
		proj := scantest.InitProject(b, root)
		buildTree(b, root, n)
		svc := scanner.New(worktreefs.New(), blake3hash.New(), nil)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := svc.Scan(context.Background(), proj, config.Defaults(), scope, scanner.Options{ReadOnly: true}); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("reuse-readonly-index", func(b *testing.B) {
		root := b.TempDir()
		proj := scantest.InitProject(b, root)
		buildTree(b, root, n)
		layout, _ := proj.Paths()

		// Prime a mutable index with the committed hashes, then close it so the reuse
		// scan reads it strictly read-only — exactly how status observes an already-indexed
		// worktree.
		mut, err := sqliteindex.Open(layout.IndexesDir())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := scanner.New(worktreefs.New(), blake3hash.New(), mut).Scan(context.Background(), proj, config.Defaults(), scope, scanner.Options{}); err != nil {
			b.Fatal(err)
		}
		if err := mut.Close(); err != nil {
			b.Fatal(err)
		}

		ro, err := sqliteindex.OpenReadOnly(context.Background(), layout.IndexesDir())
		if err != nil {
			b.Fatal(err)
		}
		defer ro.Close()
		svc := scanner.New(worktreefs.New(), blake3hash.New(), ro)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := svc.Scan(context.Background(), proj, config.Defaults(), scope, scanner.Options{ReadOnly: true}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkIndexLookupUpdateRepeatedScan(b *testing.B) {
	root := b.TempDir()
	proj := scantest.InitProject(b, root)
	buildTree(b, root, 1000)
	layout, _ := proj.Paths()
	idx, err := sqliteindex.Open(layout.IndexesDir())
	if err != nil {
		b.Fatal(err)
	}
	defer idx.Close()
	svc := scanner.New(worktreefs.New(), blake3hash.New(), idx)

	// Prime the index, then benchmark repeated normal scans (reuse-heavy path).
	if _, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := svc.Scan(context.Background(), proj, config.Defaults(), config.Defaults().HistoryScanScope(), scanner.Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
