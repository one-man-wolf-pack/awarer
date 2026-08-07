package changes_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"awarer/internal/app/changes"
	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/compare"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/scantest"
)

// fixtureRoot is an absolute, cleaned path on whichever host runs the test. A checkpoint
// records the project root it was captured from and Checkpoint.Validate requires exactly
// that shape, so a fixture spelling a POSIX literal like "/abs/project" is not cosmetic:
// filepath.IsAbs rejects a volume-less path on Windows, which would fail every checkpoint
// built here and confine this package's coverage to unix.
func fixtureRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("abs", "project"))
	if err != nil {
		t.Fatalf("resolving a fixture root: %v", err)
	}
	return root
}

func hashOf(t *testing.T, h hashing.Hasher, s string) hashing.ContentHash {
	t.Helper()
	c, err := h.HashReader(bytes.NewReader([]byte(s)))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	return c
}

func putCheckpoint(t *testing.T, repo *checkpointjson.Repo, h hashing.Hasher, idByte byte, created time.Time, files map[string]hashing.ContentHash) {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{idByte}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	var entries []worktree.Entry
	for path, ch := range files {
		p, err := worktree.ParseRelPath(path)
		if err != nil {
			t.Fatalf("ParseRelPath: %v", err)
		}
		e, err := worktree.NewRegularEntry(p, ch, worktree.StorageBlob,
			worktree.StatSignature{Size: 10, MtimeNs: 5, Mode: 0o644,
				Omitted: worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)},
			worktree.TraversalInfo{})
		if err != nil {
			t.Fatalf("NewRegularEntry: %v", err)
		}
		entries = append(entries, e)
	}
	th, _ := hashing.ParseTreeHash("blake3:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	build := checkpoint.CheckpointBuild{
		ID: id, CreatedAt: created, Root: fixtureRoot(t), CommandCwd: ".", AwaVersion: "0.0.0-dev",
		ScanConfigHash: hashing.ConfigHashFromTree(th), CheckpointPolicyHash: hashing.ConfigHashFromTree(th),
		TrustMode:         config.TrustNormal,
		OmittedStatFields: worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink),
	}
	// The store derives the tree hash and stats from the records it streams.
	if _, err := repo.PutManifest(context.Background(), build, scantest.CanonicalStream(entries, nil)); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
}

func setup(t *testing.T) (*changes.Service, *checkpointjson.Repo, hashing.Hasher) {
	t.Helper()
	layout := paths.New(t.TempDir())
	hasher := blake3hash.New()
	repo := checkpointjson.NewRepo(layout)
	res := state.NewResolver(state.Deps{Checkpoints: repo, Hasher: hasher})
	return changes.New(res), repo, hasher
}

func TestChangesCheckpointToCheckpoint(t *testing.T) {
	svc, repo, h := setup(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	putCheckpoint(t, repo, h, 0x11, base, map[string]hashing.ContentHash{
		"src/a.go": hashOf(t, h, "one"),
		"src/b.go": hashOf(t, h, "stable"),
	})
	putCheckpoint(t, repo, h, 0x22, base.Add(time.Hour), map[string]hashing.ContentHash{
		"src/a.go": hashOf(t, h, "two"),
		"src/b.go": hashOf(t, h, "stable"),
	})

	rng, _ := state.ParseRange("@-2..@-1")
	res, err := svc.Run(context.Background(), changes.Request{Range: rng})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ChangeSet.Changes) != 1 || res.ChangeSet.Changes[0].Status != compare.Modified {
		t.Fatalf("want one Modified, got %+v", res.ChangeSet.Changes)
	}
	if res.ChangeSet.Changes[0].NewPath.String() != "src/a.go" {
		t.Errorf("changed path = %q", res.ChangeSet.Changes[0].NewPath)
	}
}

func TestChangesPathFilter(t *testing.T) {
	svc, repo, h := setup(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	putCheckpoint(t, repo, h, 0x11, base, map[string]hashing.ContentHash{
		"src/a.go": hashOf(t, h, "one"),
		"doc/x.md": hashOf(t, h, "one"),
	})
	putCheckpoint(t, repo, h, 0x22, base.Add(time.Hour), map[string]hashing.ContentHash{
		"src/a.go": hashOf(t, h, "two"),
		"doc/x.md": hashOf(t, h, "two"),
	})

	rng, _ := state.ParseRange("@-2..@-1")
	filter, _ := worktree.ParseRelPath("src")
	res, err := svc.Run(context.Background(), changes.Request{Range: rng, PathFilters: []worktree.RelPath{filter}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ChangeSet.Changes) != 1 || res.ChangeSet.Changes[0].NewPath.String() != "src/a.go" {
		t.Fatalf("path filter failed: %+v", res.ChangeSet.Changes)
	}
}
