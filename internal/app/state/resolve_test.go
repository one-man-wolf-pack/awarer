package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/app/state"
	"awarer/internal/domain/checkpoint"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/checkpointjson"
	"awarer/internal/scantest"
)

const fullHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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

func newResolver(t *testing.T) (*state.Resolver, *checkpointjson.Repo) {
	t.Helper()
	layout := paths.New(t.TempDir())
	repo := checkpointjson.NewRepo(layout)
	r := state.NewResolver(state.Deps{
		Checkpoints: repo,
		Hasher:      blake3hash.New(),
	})
	return r, repo
}

// putCheckpoint builds and stores a minimal one-entry checkpoint with the given id
// byte, created time, and content hash hex (which drives the tree hash).
func putCheckpoint(t *testing.T, repo *checkpointjson.Repo, idByte byte, created time.Time, contentHex string) checkpoint.CheckpointID {
	t.Helper()
	id, err := checkpoint.NewCheckpointID(bytes.NewReader(bytes.Repeat([]byte{idByte}, 20)))
	if err != nil {
		t.Fatalf("NewCheckpointID: %v", err)
	}
	ch, err := hashing.ParseContentHash("blake3:" + contentHex)
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}
	p, err := worktree.ParseRelPath("a.txt")
	if err != nil {
		t.Fatalf("ParseRelPath: %v", err)
	}
	entry, err := worktree.NewRegularEntry(p, ch, worktree.StorageBlob,
		worktree.StatSignature{Size: 10, MtimeNs: 5, Mode: 0o644,
			Omitted: worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink)},
		worktree.TraversalInfo{})
	if err != nil {
		t.Fatalf("NewRegularEntry: %v", err)
	}
	build := checkpoint.CheckpointBuild{
		ID:                   id,
		CreatedAt:            created,
		Root:                 fixtureRoot(t),
		CommandCwd:           ".",
		AwaVersion:           "0.0.0-dev",
		ScanConfigHash:       hashing.ConfigHashFromTree(mustTree(t)),
		CheckpointPolicyHash: hashing.ConfigHashFromTree(mustTree(t)),
		TrustMode:            config.TrustNormal,
		OmittedStatFields:    worktree.FieldSet(0).With(worktree.FieldCtime).With(worktree.FieldDev).With(worktree.FieldIno).With(worktree.FieldNlink),
	}
	// The write derives the tree hash, stats, and record count from the records it
	// streams, so the fixture supplies only the metadata and the manifest.
	if _, err := repo.PutManifest(context.Background(), build, scantest.CanonicalStream([]worktree.Entry{entry}, nil)); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	return id
}

func mustTree(t *testing.T) hashing.TreeHash {
	t.Helper()
	h, err := hashing.ParseTreeHash("blake3:" + fullHex)
	if err != nil {
		t.Fatalf("ParseTreeHash: %v", err)
	}
	return h
}

func mustResolve(t *testing.T, r *state.Resolver, ref state.Ref) *state.ResolvedState {
	t.Helper()
	rs, err := r.Resolve(context.Background(), ref, state.NowContext{})
	if err != nil {
		t.Fatalf("Resolve(%+v): %v", ref, err)
	}
	return rs
}

func TestResolveLatestAndAtOne(t *testing.T) {
	r, repo := newResolver(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	_ = putCheckpoint(t, repo, 0x11, base, fullHex)
	newest := putCheckpoint(t, repo, 0x22, base.Add(time.Hour), "1111111111111111111111111111111111111111111111111111111111111111")

	latest := mustResolve(t, r, state.Ref{Kind: state.RefLatest})
	at1 := mustResolve(t, r, state.Ref{Kind: state.RefAtN, N: 1})
	for _, rs := range []*state.ResolvedState{latest, at1} {
		id, ok := rs.CheckpointID()
		if !ok || id != newest {
			t.Errorf("want newest %s, got %v (ok=%v)", newest.Short(), id.Short(), ok)
		}
	}
}

func TestResolveAtTwo(t *testing.T) {
	r, repo := newResolver(t)
	base := time.Unix(1_700_000_000, 0).UTC()
	previous := putCheckpoint(t, repo, 0x11, base, fullHex)
	_ = putCheckpoint(t, repo, 0x22, base.Add(time.Hour), "1111111111111111111111111111111111111111111111111111111111111111")

	at2 := mustResolve(t, r, state.Ref{Kind: state.RefAtN, N: 2})
	id, ok := at2.CheckpointID()
	if !ok || id != previous {
		t.Errorf("@-2 want %s, got %v", previous.Short(), id.Short())
	}
}

func TestResolveAtOutOfRange(t *testing.T) {
	r, repo := newResolver(t)
	_ = putCheckpoint(t, repo, 0x11, time.Unix(1_700_000_000, 0).UTC(), fullHex)
	_, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefAtN, N: 5}, state.NowContext{})
	if !errors.Is(err, state.ErrOutOfRange) {
		t.Fatalf("want ErrOutOfRange, got %v", err)
	}
}

func TestResolveLatestNoCheckpoints(t *testing.T) {
	r, _ := newResolver(t)
	_, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefLatest}, state.NowContext{})
	if !errors.Is(err, state.ErrNoCheckpoints) {
		t.Fatalf("want ErrNoCheckpoints, got %v", err)
	}
}

func TestResolveByIDPrefixUnique(t *testing.T) {
	r, repo := newResolver(t)
	id := putCheckpoint(t, repo, 0x11, time.Unix(1_700_000_000, 0).UTC(), fullHex)
	// The full id is always an unambiguous prefix.
	rs := mustResolve(t, r, state.Ref{Kind: state.RefCheckpointPrefix, Raw: id.String()})
	got, _ := rs.CheckpointID()
	if got != id {
		t.Errorf("prefix resolved to %s, want %s", got.Short(), id.Short())
	}
}

func TestResolveAmbiguousPrefixFails(t *testing.T) {
	r, repo := newResolver(t)
	a := putCheckpoint(t, repo, 0x00, time.Unix(1_700_000_000, 0).UTC(), fullHex)
	b := putCheckpoint(t, repo, 0x04, time.Unix(1_700_000_001, 0).UTC(), fullHex)
	// Find a shared prefix of the two ids; it must be ambiguous.
	prefix := commonPrefix(a.String(), b.String())
	if prefix == "" {
		t.Fatalf("ids %q and %q share no prefix; pick different id bytes", a.String(), b.String())
	}
	_, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefCheckpointPrefix, Raw: prefix}, state.NowContext{})
	if !errors.Is(err, checkpoint.ErrAmbiguousPrefix) {
		t.Fatalf("want ErrAmbiguousPrefix, got %v", err)
	}
}

// TestResolveUnmatchedPrefixIsNotFound proves the third outcome of prefix
// resolution stays distinct from the other two: a well-formed prefix that simply
// matches nothing is not found, never ambiguous and never silently resolved to the
// one checkpoint that does exist.
func TestResolveUnmatchedPrefixIsNotFound(t *testing.T) {
	r, repo := newResolver(t)
	id := putCheckpoint(t, repo, 0x11, time.Unix(1_700_000_000, 0).UTC(), fullHex)
	// A syntactically valid prefix that cannot match the stored id.
	unmatched := "z9"
	if strings.HasPrefix(id.String(), unmatched) {
		t.Fatalf("fixture id %q starts with the supposedly unmatched prefix", id.String())
	}
	_, err := r.Resolve(context.Background(), state.Ref{Kind: state.RefCheckpointPrefix, Raw: unmatched}, state.NowContext{})
	if !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestResolveCheckpointDetectsDerivedDrift proves the streaming read is the owner of
// the derived-fact guard: the tree hash and the stats are stored in the header for
// convenience but re-derived from the manifest as it is drained, so a header
// hand-edited to disagree with the records it summarizes is ErrCorruptStore rather
// than a believed fact. Both cases keep the header structurally valid and still
// claiming the current schema, so this is the corrupt half of the boundary and never
// the incompatible one — and both are edits a cheap header-only check cannot catch,
// which is exactly why the guard lives on the drain. (A record count that contradicts
// the header's own stats total is caught earlier, by header validation; a manifest
// truncated below the declared count is caught by the manifest stream.)
func TestResolveCheckpointDetectsDerivedDrift(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  byte
		edit  func(doc map[string]any)
		wants string
	}{
		{
			// Swap blobs/hash_only: the sum still equals files, so the header's own
			// structural guard passes while the manifest's real split contradicts it.
			name: "stats", seed: 0x33, wants: "stats",
			edit: func(doc map[string]any) {
				stats := doc["stats"].(map[string]any)
				stats["blobs"], stats["hash_only"] = stats["hash_only"], stats["blobs"]
			},
		},
		{
			name: "tree hash", seed: 0x34, wants: "tree hash",
			edit: func(doc map[string]any) {
				doc["tree_hash"] = "blake3:0000000000000000000000000000000000000000000000000000000000000000"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := paths.New(t.TempDir())
			repo := checkpointjson.NewRepo(layout)
			r := state.NewResolver(state.Deps{
				Checkpoints: repo,
				Hasher:      blake3hash.New(),
			})
			id := putCheckpoint(t, repo, tc.seed, time.Unix(1_700_000_000, 0).UTC(), fullHex)
			editHeader(t, filepath.Join(layout.CheckpointsDir(), id.String(), "header.json"), tc.edit)

			rs := mustResolve(t, r, state.Ref{Kind: state.RefCheckpointPrefix, Raw: id.String()})
			cur, err := rs.Manifest(context.Background())
			if err != nil {
				t.Fatalf("Manifest: %v", err)
			}
			for cur.Next() {
			}
			err = cur.Err()
			_ = cur.Close()
			if err == nil || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("drain err = %v, want a %s mismatch", err, tc.wants)
			}
			if !errors.Is(err, checkpoint.ErrCorruptStore) {
				t.Errorf("drain err = %v, want ErrCorruptStore", err)
			}
			if errors.Is(err, checkpoint.ErrIncompatibleFormat) {
				t.Errorf("a hand-edited current-schema header was classified incompatible: %v", err)
			}
		})
	}
}

// editHeader rewrites a published header's JSON object in place, restoring the
// read-only modes the store publishes with.
func editHeader(t *testing.T, headerPath string, edit func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(headerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(headerPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headerPath, out, 0o444); err != nil {
		t.Fatal(err)
	}
}

func commonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}
