package checkpointjson

import (
	"context"
	"fmt"
	"testing"
	"time"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

func benchCheckpoint(b *testing.B, entries int) checkpointFixture {
	b.Helper()
	h, err := hashing.ParseContentHash("blake3:" + fullHex)
	if err != nil {
		b.Fatal(err)
	}
	th, err := hashing.ParseTreeHash("blake3:" + fullHex)
	if err != nil {
		b.Fatal(err)
	}
	id, err := checkpoint.ParseCheckpointID("0123456789abcdefghjkmnpqrstvwxyz")
	if err != nil {
		b.Fatal(err)
	}
	es := make([]worktree.Entry, 0, entries)
	for i := 0; i < entries; i++ {
		p, err := worktree.ParseRelPath(fmt.Sprintf("dir%03d/file%05d.txt", i%100, i))
		if err != nil {
			b.Fatal(err)
		}
		e, err := worktree.NewRegularEntry(p, h, worktree.StorageBlob,
			worktree.StatSignature{Size: int64(i), MtimeNs: int64(i), Mode: 0o644}, worktree.TraversalInfo{})
		if err != nil {
			b.Fatal(err)
		}
		es = append(es, e)
	}
	return checkpointFixture{
		build: checkpoint.CheckpointBuild{
			ID:                   id,
			CreatedAt:            time.Unix(1_700_000_000, 0).UTC(),
			Root:                 fixtureRoot(b),
			CommandCwd:           ".",
			AwaVersion:           "bench",
			ScanConfigHash:       hashing.ConfigHashFromTree(th),
			CheckpointPolicyHash: hashing.ConfigHashFromTree(th),
			TrustMode:            config.TrustNormal,
		},
		entries: es,
	}
}

// benchHeader is the header the benchmark fixture's records imply, derived the same
// way a write derives it.
func benchHeader(b *testing.B, f checkpointFixture) checkpoint.CheckpointHeader {
	b.Helper()
	red, err := worktree.ReduceCursor(blake3hash.New(), scantest.CanonicalCursor(f.entries, f.skipped))
	if err != nil {
		b.Fatal(err)
	}
	return f.build.Header(red.Hash, checkpoint.StatsFromReduced(red.Stats), red.Count)
}

func BenchmarkEncodeHeader(b *testing.B) {
	h := benchHeader(b, benchCheckpoint(b, 5000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encodeHeader(h); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeHeader(b *testing.B) {
	data, err := encodeHeader(benchHeader(b, benchCheckpoint(b, 5000)))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decodeHeader(data); err != nil {
			b.Fatal(err)
		}
	}
}

// benchStoreSize is how many checkpoints the store-health benchmarks build. It is a
// committed fixture size, chosen to stay quick in ordinary runs; the long-history
// numbers this refactor was measured against were taken by raising it locally, not by
// making every `go test -bench` run pay for them.
const benchStoreSize = 500

// benchStore writes n checkpoints, each with a real ten-record manifest the measured
// loops never read: the fixture is sized that way on purpose, so the numbers show
// header reading scaling with the number of checkpoints and not with how large their
// manifests are. Building the store is outside the measured region.
func benchStore(b *testing.B, n int) *Repo {
	b.Helper()
	repo := NewRepo(paths.New(b.TempDir()))
	for i := 0; i < n; i++ {
		s := benchCheckpoint(b, 10)
		// Each checkpoint needs a distinct id and time so the read has real work.
		s.build.ID = benchID(b, i)
		s.build.CreatedAt = time.Unix(1_700_000_000+int64(i), 0).UTC()
		if err := s.putErr(repo); err != nil {
			b.Fatal(err)
		}
	}
	return repo
}

// BenchmarkStoreHealthAll measures the explicit full-history read (`awa log --all`):
// every committed header is read and every readable one retained.
func BenchmarkStoreHealthAll(b *testing.B) {
	repo := benchStore(b, benchStoreSize)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := repo.StoreHealthAll(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStoreHealthNewest measures the habitual bounded read (`awa log`, `awa
// status`) over the same store. It reads exactly as many headers as the full case —
// honesty requires that — so the comparison against BenchmarkStoreHealthAll shows what
// bounding the retained window costs or saves, not a shorter scan.
func BenchmarkStoreHealthNewest(b *testing.B) {
	repo := benchStore(b, benchStoreSize)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := repo.StoreHealthNewest(context.Background(), 20); err != nil {
			b.Fatal(err)
		}
	}
}

func benchID(b *testing.B, n int) checkpoint.CheckpointID {
	b.Helper()
	var buf [20]byte
	buf[0] = byte(n)
	buf[1] = byte(n >> 8)
	id, err := checkpoint.NewCheckpointID(byteReader(buf[:]))
	if err != nil {
		b.Fatal(err)
	}
	return id
}

type byteReader []byte

func (r byteReader) Read(p []byte) (int, error) {
	n := copy(p, r)
	if n < len(p) {
		return n, fmt.Errorf("short")
	}
	return n, nil
}
