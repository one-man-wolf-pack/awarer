package checkpointcmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/pattern"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/worktreefs"
	"awarer/internal/scantest"
)

// dummyEnv is placeholder env content; no fixture here holds a real secret.
const dummyEnv = "DUMMY_TOKEN=not-a-secret\n"

// storageByPath maps every entry of a persisted checkpoint to its storage class.
func (h *harness) storageByPath(t *testing.T, res Result) map[string]worktree.Entry {
	t.Helper()
	out := map[string]worktree.Entry{}
	for _, e := range h.loaded(t, res.Header.ID).entries {
		out[e.Path.String()] = e
	}
	return out
}

func requireStorage(t *testing.T, got map[string]worktree.Entry, want map[string]worktree.ContentStorageIntent) {
	t.Helper()
	for path, w := range want {
		e, ok := got[path]
		if !ok {
			t.Errorf("%s missing from the checkpoint manifest", path)
			continue
		}
		if e.Storage != w {
			t.Errorf("%s storage = %v, want %v", path, e.Storage, w)
		}
		if e.Content.IsZero() {
			t.Errorf("%s lost its content identity", path)
		}
	}
}

// TestDefaultHashOnlyPatternsSelectEnvFilesWithoutReadingThem couples the resolved
// default list to real checkpoint publication: selected env files stay comparable
// (content hash recorded) but own no blob and are never reopened for bytes, while the
// conventional exceptions and every nonmatch keep ordinary storage. app.cfg holds the
// same bytes as .env, so the shared store does hold that content — owned by app.cfg's
// record, never by .env's.
func TestDefaultHashOnlyPatternsSelectEnvFilesWithoutReadingThem(t *testing.T) {
	h := setup(t)
	for _, name := range []string{".env", ".env.local", "deploy/prod.env", "nested/deep/.env", ".env.d/inner.txt",
		".env.example", ".env.sample", ".env.template", "app.cfg", "env", ".envrc", "nested/.ENV", "prod.env.bak"} {
		h.writeFile(t, name, dummyEnv)
	}
	h.requireNoStoredBlobs(t)

	res := h.run(t, Request{Config: config.Defaults()})

	got := h.storageByPath(t, res)
	requireStorage(t, got, map[string]worktree.ContentStorageIntent{
		".env":             worktree.StorageHashOnly,
		".env.local":       worktree.StorageHashOnly,
		"deploy/prod.env":  worktree.StorageHashOnly,
		"nested/deep/.env": worktree.StorageHashOnly,
		".env.d/inner.txt": worktree.StorageHashOnly, // directory match selects descendants
		".env.example":     worktree.StorageBlob,
		".env.sample":      worktree.StorageBlob,
		".env.template":    worktree.StorageBlob,
		"app.cfg":          worktree.StorageBlob,
		"env":              worktree.StorageBlob,
		".envrc":           worktree.StorageBlob,
		"nested/.ENV":      worktree.StorageBlob, // case-sensitive
		"prod.env.bak":     worktree.StorageBlob,
	})
	for _, p := range []string{".env", ".env.local", "deploy/prod.env", "nested/deep/.env", ".env.d/inner.txt"} {
		if h.content.asked(p) {
			t.Errorf("%s was reopened for bytes although it is selected hash-only", p)
		}
	}
	// Every blob-stored file holds the same bytes, so exactly one blob is written; a
	// selected file must not become a second owner of it.
	if res.BlobsWritten != 1 {
		t.Errorf("BlobsWritten = %d, want 1", res.BlobsWritten)
	}
	if res.Header.Stats.HashOnly != 5 {
		t.Errorf("hash-only count = %d, want 5", res.Header.Stats.HashOnly)
	}
}

// TestSelectionIgnoresAnAlreadyStoredIdenticalBlob checkpoints .env first under an
// explicit empty selection — so its bytes are genuinely in the store — and then under
// the default. The second record must still claim no bytes: refusal cannot rest on
// physical blob absence.
func TestSelectionIgnoresAnAlreadyStoredIdenticalBlob(t *testing.T) {
	h := setup(t)
	h.writeFile(t, ".env", dummyEnv)

	keep := config.Defaults()
	keep.Checkpoint.HashOnlyPatterns = []string{}
	first := h.run(t, Request{Config: keep})
	firstEntry := h.storageByPath(t, first)[".env"]
	if firstEntry.Storage != worktree.StorageBlob {
		t.Fatalf("explicit [] stored .env as %v, want blob", firstEntry.Storage)
	}
	if has, _ := h.blobs.Has(firstEntry.Content); !has {
		t.Fatal("precondition: the .env blob must be in the store")
	}

	h.content.paths = nil
	second := h.run(t, Request{Config: config.Defaults()})
	e := h.storageByPath(t, second)[".env"]
	if e.Storage != worktree.StorageHashOnly {
		t.Fatalf("default selection stored .env as %v with an identical blob present, want hash-only", e.Storage)
	}
	if e.Content != firstEntry.Content {
		t.Error("hash-only .env must keep the same content identity")
	}
	if second.BlobsReused != 0 || second.BlobsWritten != 0 {
		t.Errorf("selected file touched the store: written=%d reused=%d", second.BlobsWritten, second.BlobsReused)
	}
	if h.content.asked(".env") {
		t.Error(".env was reopened although it is selected hash-only")
	}
}

// TestCustomHashOnlyPatternsReplaceTheDefaults pins whole-list replacement at the
// materialization owner: a custom list keeps none of the env defaults, a directory
// pattern selects descendants without pruning them, and a later exception un-selects.
func TestCustomHashOnlyPatternsReplaceTheDefaults(t *testing.T) {
	h := setup(t)
	h.writeFile(t, ".env", dummyEnv)
	h.writeFile(t, "secrets/a.txt", "a")
	h.writeFile(t, "secrets/deep/b.txt", "b")
	h.writeFile(t, "secrets/public.txt", "p")
	h.writeFile(t, "other/secrets.txt", "o")

	cfg := config.Defaults()
	cfg.Checkpoint.HashOnlyPatterns = []string{"secrets/", "!secrets/public.txt"}
	got := h.storageByPath(t, h.run(t, Request{Config: cfg}))
	requireStorage(t, got, map[string]worktree.ContentStorageIntent{
		".env":               worktree.StorageBlob,
		"secrets/a.txt":      worktree.StorageHashOnly,
		"secrets/deep/b.txt": worktree.StorageHashOnly,
		"secrets/public.txt": worktree.StorageBlob,
		"other/secrets.txt":  worktree.StorageBlob,
	})
}

// TestSelectionExceptionsNeverReenableBytes: a "!" exception removes only this list's
// selection. It cannot store bytes that store_file_contents = false or the large-file
// policy declined, nor bring back a skipped file.
func TestSelectionExceptionsNeverReenableBytes(t *testing.T) {
	t.Run("storage off", func(t *testing.T) {
		h := setup(t)
		h.writeFile(t, ".env.example", dummyEnv)
		cfg := config.Defaults()
		cfg.Checkpoint.StoreFileContents = false
		requireStorage(t, h.storageByPath(t, h.run(t, Request{Config: cfg})), map[string]worktree.ContentStorageIntent{
			".env.example": worktree.StorageHashOnly,
		})
	})
	t.Run("large-file policies", func(t *testing.T) {
		big := strings.Repeat("x", 64)
		for _, tc := range []struct {
			policy config.LargeFilePolicy
			want   worktree.ContentStorageIntent
		}{
			{config.LargeFileHashOnly, worktree.StorageHashOnly},
			{config.LargeFileStore, worktree.StorageBlob},
		} {
			h := setup(t)
			h.writeFile(t, ".env.example", big)
			h.writeFile(t, ".env", big+"y")
			cfg := config.Defaults()
			cfg.Hashing.MaxFileSize = 16
			cfg.Hashing.LargeFilePolicy = tc.policy
			requireStorage(t, h.storageByPath(t, h.run(t, Request{Config: cfg})), map[string]worktree.ContentStorageIntent{
				".env.example": tc.want,
				".env":         worktree.StorageHashOnly,
			})
		}

		h := setup(t)
		h.writeFile(t, ".env.example", big)
		cfg := config.Defaults()
		cfg.Hashing.MaxFileSize = 16
		cfg.Hashing.LargeFilePolicy = config.LargeFileSkip
		res := h.run(t, Request{Config: cfg})
		recs := h.loaded(t, res.Header.ID)
		if len(recs.skipped) != 1 || recs.skipped[0].Path.String() != ".env.example" {
			t.Fatalf("skipped = %v, want only .env.example", recs.skipped)
		}
	})
}

// TestSelectionMatchesTheVirtualFollowedPath: a followed symlink is selected by the
// path it is reached by, not by its physical target's name. link.env reaches an
// ordinary-named file and is selected; envlink reaches a file named .env and is not.
func TestSelectionMatchesTheVirtualFollowedPath(t *testing.T) {
	h := setup(t)
	scantest.RequireSymlinks(t)
	h.writeFile(t, "hidden/real.txt", "followed-bytes")
	h.writeFile(t, "hidden/.env", dummyEnv)
	h.symlink(t, "link.env", "hidden/real.txt")
	h.symlink(t, "envlink", "hidden/.env")
	cfg := config.Defaults()
	cfg.Scope.FollowSymlinks = true
	cfg.Scope.ExtraExcludes = []string{"hidden"}

	got := h.storageByPath(t, h.run(t, Request{Config: cfg}))
	requireStorage(t, got, map[string]worktree.ContentStorageIntent{
		"link.env": worktree.StorageHashOnly,
		"envlink":  worktree.StorageBlob,
	})
	if !got["link.env"].Traversal.Followed {
		t.Error("link.env must keep its followed provenance")
	}
}

// TestRetentionIsAppliedIdenticallyOnEveryReopen: the manifest source is reopened on
// an id-collision retry, and each pass must apply the one compiled policy.
func TestRetentionIsAppliedIdenticallyOnEveryReopen(t *testing.T) {
	h := setup(t)
	h.writeFile(t, ".env", dummyEnv)
	h.writeFile(t, "a.txt", "a")
	cfg := config.Defaults()
	src := &manifestSource{svc: h.svc, retention: mustRetention(t, cfg.Checkpoint), scan: h.scan(t, cfg)}
	for pass := 0; pass < 2; pass++ {
		cur, err := src.Open(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for cur.Next() {
			e, _ := cur.Record().Entry()
			want := worktree.StorageBlob
			if e.Path.String() == ".env" {
				want = worktree.StorageHashOnly
			}
			if e.Kind == worktree.KindRegular && e.Storage != want {
				t.Errorf("pass %d: %s storage = %v, want %v", pass, e.Path, e.Storage, want)
			}
		}
		if err := cur.Err(); err != nil {
			t.Fatal(err)
		}
		_ = cur.Close()
	}
	if h.content.asked(".env") {
		t.Error(".env was reopened on a retry")
	}
}

// TestHashOnlyPatternsEnterOnlyTheCheckpointPolicyHash holds the worktree bytes
// constant (configuration arrives through the request, not a visible awa.toml) and
// varies only the selection. An explicit [] keeps the predecessor's exact two-byte
// encoding; any non-empty list, including the default, is distinct and
// order-sensitive; the scan config hash and tree hash never move.
func TestHashOnlyPatternsEnterOnlyTheCheckpointPolicyHash(t *testing.T) {
	h := setup(t)
	h.writeFile(t, ".env", dummyEnv)
	h.writeFile(t, "a.txt", "a")

	withList := func(list []string) Result {
		cfg := config.Defaults()
		cfg.Checkpoint.HashOnlyPatterns = list
		return h.run(t, Request{Config: cfg})
	}
	empty := withList([]string{})
	def := withList(config.HashOnlyDefaults())
	ab := withList([]string{"*.txt", "!a.txt"})
	ba := withList([]string{"!a.txt", "*.txt"})

	predecessor := hashing.ConfigHashFromTree(blake3hash.New().HashBytes([]byte{policyHashVersion, 1}))
	if empty.Header.CheckpointPolicyHash != predecessor {
		t.Errorf("explicit [] policy hash = %s, want the predecessor encoding %s", empty.Header.CheckpointPolicyHash, predecessor)
	}
	seen := map[hashing.ConfigHash]string{}
	for name, r := range map[string]Result{"empty": empty, "default": def, "ab": ab, "ba": ba} {
		if prev, dup := seen[r.Header.CheckpointPolicyHash]; dup {
			t.Errorf("%s and %s share a checkpoint policy hash", name, prev)
		}
		seen[r.Header.CheckpointPolicyHash] = name
		if r.Header.ScanConfigHash != empty.Header.ScanConfigHash {
			t.Errorf("%s changed the scan config hash", name)
		}
		if r.Header.TreeHash != empty.Header.TreeHash {
			t.Errorf("%s changed the tree hash", name)
		}
	}
}

func TestCheckpointPolicyHashSeparatesMatchingGenerations(t *testing.T) {
	h := blake3hash.New()
	empty := config.Defaults()
	empty.Checkpoint.HashOnlyPatterns = []string{}
	if checkpointPolicyHashAt(pattern.Generation, empty, h) != checkpointPolicyHashAt(pattern.Generation+1, empty, h) {
		t.Error("an empty selection has no patterns to reinterpret; its policy hash must not depend on the generation")
	}
	def := config.Defaults()
	if checkpointPolicyHashAt(pattern.Generation, def, h) != checkpointPolicyHash(def, h) {
		t.Error("the checkpoint policy hash does not carry the current matching generation")
	}
	if checkpointPolicyHashAt(pattern.Generation, def, h) == checkpointPolicyHashAt(pattern.Generation+1, def, h) {
		t.Error("an identical selection under another matching generation shares a checkpoint policy hash")
	}
}

func TestNeverMatchingRetentionPatternRefusesBeforePublication(t *testing.T) {
	h := setup(t)
	h.writeFile(t, "a.env", dummyEnv)
	cfg := config.Defaults()
	cfg.Checkpoint.HashOnlyPatterns = []string{"*.env", "x["}

	_, err := h.svc.Run(context.Background(), Request{Project: h.project, Config: cfg, CommandCwd: h.root})
	var invalid *config.ValidationError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "checkpoint.hash_only_patterns[1]") {
		t.Fatalf("Checkpoint() error = %v, want a located configuration refusal", err)
	}
	layout, err := h.project.Paths()
	if err != nil {
		t.Fatal(err)
	}
	if published, _ := os.ReadDir(layout.CheckpointsDir()); len(published) != 0 {
		t.Errorf("a refused retention list still published %d checkpoint entries", len(published))
	}
	h.requireNoStoredBlobs(t)
}

func TestRetentionCompilerFailureFailsTheCheckpoint(t *testing.T) {
	boom := errors.New("compiler failure")
	_, err := NewRetention(config.Defaults().Checkpoint, func([]pattern.Rule) (worktree.PathSelector, []pattern.Diagnostic, error) {
		return nil, nil, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("NewRetention() error = %v, want the compiler failure", err)
	}
}

func mustRetention(t *testing.T, cfg config.Checkpoint) Retention {
	t.Helper()
	r, err := NewRetention(cfg, worktreefs.CompilePatterns)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
