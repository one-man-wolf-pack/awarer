package changes_test

import (
	"bytes"
	"context"
	"errors"
	"os"
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
	putCheckpointFrontier(t, repo, h, idByte, created, files)
}

// putCheckpointFrontier stores a checkpoint whose completed coverage carries the given
// exclusion frontier.
func putCheckpointFrontier(t *testing.T, repo *checkpointjson.Repo, h hashing.Hasher, idByte byte, created time.Time, files map[string]hashing.ContentHash, frontier ...worktree.FrontierRecord) {
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
	if _, err := repo.PutManifest(context.Background(), build, scantest.CanonicalStream(entries, nil), scantest.FrontierStream(frontier...)); err != nil {
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

// coverageFixture stores a baseline whose frontier pruned gen/ and a later
// checkpoint that records gen/x.go and new.go. It returns the checkpoints directory
// so a test can damage the stored proof.
func coverageFixture(t *testing.T) (*changes.Service, string) {
	t.Helper()
	layout := paths.New(t.TempDir())
	h := blake3hash.New()
	repo := checkpointjson.NewRepo(layout)
	svc := changes.New(state.NewResolver(state.Deps{Checkpoints: repo, Hasher: h}))
	base := time.Unix(1_700_000_000, 0).UTC()
	gen, err := worktree.ParseRelPath("gen")
	if err != nil {
		t.Fatal(err)
	}
	putCheckpointFrontier(t, repo, h, 0x11, base, map[string]hashing.ContentHash{
		"a.go": hashOf(t, h, "a"),
	}, worktree.FrontierRecord{Path: gen, Limit: worktree.LimitSubtree})
	putCheckpoint(t, repo, h, 0x22, base.Add(time.Hour), map[string]hashing.ContentHash{
		"a.go":     hashOf(t, h, "a"),
		"gen/x.go": hashOf(t, h, "x"),
		"new.go":   hashOf(t, h, "new"),
	})
	return svc, layout.CheckpointsDir()
}

var coverageWant = map[string]compare.Counterpart{
	"gen/x.go": {Side: compare.SideLeft, Qualification: compare.QualOutsideObservation},
	"new.go":   {Side: compare.SideLeft, Qualification: compare.QualAbsent},
}

// streamCounterparts collects Stream's counterparts by path.
func streamCounterparts(svc *changes.Service, req changes.Request) (map[string]compare.Counterpart, error) {
	sr, err := svc.Stream(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sr.Close() }()
	got := map[string]compare.Counterpart{}
	for sr.Changes.Next() {
		c := sr.Changes.Change()
		got[c.PrimaryPath().String()] = c.Counterpart
	}
	return got, sr.Changes.Err()
}

// runCounterparts collects Run's counterparts by path.
func runCounterparts(svc *changes.Service, req changes.Request) (map[string]compare.Counterpart, error) {
	res, err := svc.Run(context.Background(), req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	got := map[string]compare.Counterpart{}
	for _, c := range res.ChangeSet.Changes {
		got[c.PrimaryPath().String()] = c.Counterpart
	}
	return got, nil
}

// Run is the materialized form of Stream: both report the same qualified facts.
func TestStreamAndRunQualifyFromStoredCoverage(t *testing.T) {
	svc, _ := coverageFixture(t)
	rng, _ := state.ParseRange("@-2..@-1")
	for _, renames := range []bool{false, true} {
		req := changes.Request{Range: rng, DetectRenames: renames}
		for name, collect := range map[string]func(*changes.Service, changes.Request) (map[string]compare.Counterpart, error){
			"Stream": streamCounterparts, "Run": runCounterparts,
		} {
			got, err := collect(svc, req)
			if err != nil {
				t.Fatalf("%s renames=%v: %v", name, renames, err)
			}
			if len(got) != len(coverageWant) || got["gen/x.go"] != coverageWant["gen/x.go"] || got["new.go"] != coverageWant["new.go"] {
				t.Errorf("%s renames=%v: counterparts = %+v, want %+v", name, renames, got, coverageWant)
			}
		}
	}
}

// A damaged stored frontier is corruption: neither form ends as a complete report,
// even when the path filter selects only a change whose proof is undamaged.
func TestStreamAndRunFailOnDamagedFrontier(t *testing.T) {
	svc, dir := coverageFixture(t)
	files, err := filepath.Glob(filepath.Join(dir, "*", "frontier.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	damaged := 0
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // test fixture path
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(`"gen"`)) {
			continue
		}
		if err := os.Chmod(f, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, bytes.Replace(data, []byte(`"gen"`), []byte(`"gem"`), 1), 0o600); err != nil {
			t.Fatal(err)
		}
		damaged++
	}
	if damaged != 1 {
		t.Fatalf("damaged %d frontier files, want 1 (of %v)", damaged, files)
	}
	rng, _ := state.ParseRange("@-2..@-1")
	newGo, _ := worktree.ParseRelPath("new.go")
	for _, filters := range [][]worktree.RelPath{nil, {newGo}} {
		req := changes.Request{Range: rng, PathFilters: filters}
		if _, err := streamCounterparts(svc, req); err == nil {
			t.Errorf("Stream filters=%v: succeeded over a damaged frontier", filters)
		}
		if res, err := svc.Run(context.Background(), req); err == nil {
			_ = res.Close()
			t.Errorf("Run filters=%v: succeeded over a damaged frontier: %+v", filters, res.ChangeSet.Changes)
		}
	}
}

// A comparison cancelled while it streams fails with the cancellation; it never
// ends as a complete report whose remaining entries look qualified.
func TestStreamCancelledDuringComparisonFails(t *testing.T) {
	svc, _ := coverageFixture(t)
	rng, _ := state.ParseRange("@-2..@-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sr, err := svc.Stream(ctx, changes.Request{Range: rng})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = sr.Close() }()
	cancel()
	for sr.Changes.Next() {
	}
	if err := sr.Changes.Err(); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
