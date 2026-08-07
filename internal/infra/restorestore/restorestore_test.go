package restorestore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/scantest"
)

// pathScope is a test-local CoveredScopeStream over a small, ordered path slice. It
// is the fixture side of the port: production scope streams are read from a durable
// record or derived from an operation set, and neither shape belongs in a test that
// only needs to name two paths. It re-opens a fresh cursor per Open, like every real
// stream, and refuses a misordered fixture so a broken test input fails at
// construction instead of producing a scope the reader would later reject.
type pathScope struct{ paths []worktree.RelPath }

func newPathScope(t *testing.T, paths ...worktree.RelPath) pathScope {
	t.Helper()
	for i, p := range paths {
		if p.IsZero() {
			t.Fatalf("covered scope entry %d has no path", i)
		}
		if i > 0 && !paths[i-1].Less(p) {
			t.Fatalf("covered scope fixture is not in strictly ascending canonical order at %q", p)
		}
	}
	return pathScope{paths: paths}
}

func (s pathScope) Open(context.Context) (restore.CoveredScopeCursor, error) {
	return &pathScopeCursor{paths: s.paths}, nil
}

type pathScopeCursor struct {
	paths []worktree.RelPath
	i     int
	cur   worktree.RelPath
}

func (c *pathScopeCursor) Next() bool {
	if c.i >= len(c.paths) {
		return false
	}
	c.cur = c.paths[c.i]
	c.i++
	return true
}

func (c *pathScopeCursor) Path() worktree.RelPath { return c.cur }
func (c *pathScopeCursor) Err() error             { return nil }
func (c *pathScopeCursor) Close() error           { return nil }

type harness struct {
	t      *testing.T
	root   string
	layout paths.Layout
	repo   *Repo
	hasher hashing.Hasher
}

func setup(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, paths.Dir), paths.DirPerm); err != nil {
		t.Fatalf("mkdir .awa: %v", err)
	}
	hasher := blake3hash.New()
	layout := paths.New(root)
	return &harness{t: t, root: root, layout: layout, repo: New(layout, hasher), hasher: hasher}
}

func (h *harness) id(nano int64) restore.OperationID {
	h.t.Helper()
	id, err := restore.NewOperationID(nano, bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, byte(nano)}))
	if err != nil {
		h.t.Fatalf("operation id: %v", err)
	}
	return id
}

func (h *harness) relPath(p string) worktree.RelPath {
	h.t.Helper()
	rp, err := worktree.ParseRelPath(p)
	if err != nil {
		h.t.Fatalf("rel path %q: %v", p, err)
	}
	return rp
}

func (h *harness) contentHash(seed byte) hashing.ContentHash {
	h.t.Helper()
	c, err := h.hasher.HashReader(bytes.NewReader([]byte{seed}))
	if err != nil {
		h.t.Fatalf("hash: %v", err)
	}
	return c
}

func (h *harness) entry(path string, seed byte) worktree.Entry {
	h.t.Helper()
	e, err := worktree.NewRegularEntry(h.relPath(path), h.contentHash(seed), worktree.StorageBlob,
		worktree.StatSignature{Size: 1, Mode: 0o644}, worktree.TraversalInfo{})
	if err != nil {
		h.t.Fatalf("entry %q: %v", path, err)
	}
	return e
}

func (h *harness) scanConfig() hashing.ConfigHash {
	h.t.Helper()
	tree, err := hashing.ParseTreeHash("blake3:" + strings.Repeat("a", 64))
	if err != nil {
		h.t.Fatalf("tree hash: %v", err)
	}
	return hashing.ConfigHashFromTree(tree)
}

func (h *harness) source() restore.Source {
	h.t.Helper()
	s, err := restore.NewSource(restore.SourceCheckpoint, "gmqd3dbpvs42abcd", "gmqd3dbpvs42abcd", "latest")
	if err != nil {
		h.t.Fatalf("source: %v", err)
	}
	return s
}

func (h *harness) selection(p ...string) restore.Selection {
	h.t.Helper()
	rps := make([]worktree.RelPath, 0, len(p))
	for _, s := range p {
		rps = append(rps, h.relPath(s))
	}
	sel, err := restore.NewPathSelection(rps)
	if err != nil {
		h.t.Fatalf("selection: %v", err)
	}
	return sel
}

func (h *harness) build(id restore.OperationID) restore.RecoveryBuild {
	return restore.RecoveryBuild{
		ID:             id,
		CreatedAt:      time.Unix(1_700_000_000, 0).UTC(),
		AwaVersion:     "test",
		ScanConfigHash: h.scanConfig(),
		Source:         h.source(),
		Selection:      h.selection("generated/client"),
	}
}

// publishOne writes a record covering two paths, one of which was absent at
// capture time (the create the restore was about to make).
func (h *harness) publishOne(id restore.OperationID) restore.RecoveryObservation {
	h.t.Helper()
	entries := []worktree.Entry{h.entry("generated/client/openapi.json", 1)}
	scope := newPathScope(h.t,
		h.relPath("generated/client/new.go"),
		h.relPath("generated/client/openapi.json"))
	rec, err := h.repo.Publish(context.Background(), h.build(id), scantest.CanonicalStream(entries, nil), scope)
	if err != nil {
		h.t.Fatalf("Publish: %v", err)
	}
	return rec
}

// --- publication ----------------------------------------------------------

func TestPublishRoundTripsThroughTheHostileReadBoundary(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	published := h.publishOne(id)

	got, err := h.repo.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID() != published.ID() {
		t.Errorf("Get id = %s, want %s", got.ID(), published.ID())
	}
	if !got.ContentComplete() {
		t.Error("a published record does not report complete content")
	}
	if got.Source().ID() != "gmqd3dbpvs42abcd" || got.Source().Requested() != "latest" {
		t.Errorf("source did not round-trip: %+v", got.Source())
	}
	if got.Selection().All() || got.Selection().Paths()[0].String() != "generated/client" {
		t.Errorf("selection did not round-trip: %v", got.Selection())
	}
	if got.Manifest().RecordCount != 1 {
		t.Errorf("manifest record count = %d, want 1", got.Manifest().RecordCount)
	}
	if got.Scope().PathCount != 2 {
		t.Errorf("scope path count = %d, want 2", got.Scope().PathCount)
	}
	if got.BeforeRef() != "restore:"+id.String()+":before" {
		t.Errorf("BeforeRef = %q", got.BeforeRef())
	}

	read, err := h.repo.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cur, err := read.Manifest().Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = cur.Close() }()
	var seen []string
	for cur.Next() {
		seen = append(seen, cur.Record().Path().String())
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain manifest: %v", err)
	}
	if len(seen) != 1 || seen[0] != "generated/client/openapi.json" {
		t.Errorf("manifest records = %v", seen)
	}
	// The covered scope is what proves the *absent* path was absent: it is in the
	// scope but not in the manifest, so an inverse restore knows to delete it.
	scopePaths, err := drainScope(context.Background(), read.Scope())
	if err != nil {
		t.Fatalf("drain covered scope: %v", err)
	}
	want := []string{"generated/client/new.go", "generated/client/openapi.json"}
	if strings.Join(scopePaths, ",") != strings.Join(want, ",") {
		t.Errorf("covered scope = %v, want %v", scopePaths, want)
	}
}

// drainScope pulls a covered-scope stream into its paths. It is the test-side
// materializer; production merges the cursor against the manifest instead.
func drainScope(ctx context.Context, stream restore.CoveredScopeStream) ([]string, error) {
	cur, err := stream.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close() }()
	var out []string
	for cur.Next() {
		out = append(out, cur.Path().String())
	}
	return out, cur.Err()
}

func TestPublishRefusesToReplaceAnImmutableRecord(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.publishOne(id)
	scope := newPathScope(t, h.relPath("a"))
	_, err := h.repo.Publish(context.Background(), h.build(id), scantest.CanonicalStream(nil, nil), scope)
	if !errors.Is(err, restore.ErrRecoveryIDCollision) {
		t.Fatalf("republish error = %v, want ErrRecoveryIDCollision", err)
	}
}

func TestPublishLeavesNoRecordWhenItFailsBeforeTheCommitPoint(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.repo.fail = func(stage failStage) error {
		if stage == failAfterPayloadsBeforeMeta {
			return errors.New("injected failure before the commit point")
		}
		return nil
	}
	entries := []worktree.Entry{h.entry("generated/client/openapi.json", 1)}
	scope := newPathScope(t, h.relPath("generated/client/openapi.json"))
	if _, err := h.repo.Publish(context.Background(), h.build(id), scantest.CanonicalStream(entries, nil), scope); err == nil {
		t.Fatal("Publish succeeded despite an injected failure")
	}
	// The oracle reads the filesystem, not a store method: nothing may be visible
	// at the record address, and the staged directory must be gone.
	if _, err := os.Stat(filepath.Join(h.layout.RestoresDir(), id.String())); !os.IsNotExist(err) {
		t.Errorf("a record directory survived a failed publish: %v", err)
	}
	staged, err := os.ReadDir(filepath.Join(h.layout.RestoresDir(), tmpDirName))
	if err == nil && len(staged) != 0 {
		t.Errorf("staged artifacts survived a failed publish: %v", staged)
	}
	if _, err := h.repo.Get(id); !errors.Is(err, restore.ErrRecoveryNotFound) {
		t.Errorf("Get after a failed publish = %v, want ErrRecoveryNotFound", err)
	}
}

func TestPublishCrashBeforeTheCommitPointLeavesOnlyReclaimableTemp(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.repo.fail = func(stage failStage) error {
		if stage == failAfterPayloadsBeforeMeta {
			return errCrash
		}
		return nil
	}
	scope := newPathScope(t, h.relPath("a"))
	if _, err := h.repo.Publish(context.Background(), h.build(id), scantest.CanonicalStream(nil, nil), scope); err == nil {
		t.Fatal("Publish succeeded despite a simulated crash")
	}
	// A crash leaves the staged directory under tmp/, where it is an obviously
	// incomplete artifact classified by age — never a record a reader can see.
	if _, err := os.Stat(filepath.Join(h.layout.RestoresDir(), id.String())); !os.IsNotExist(err) {
		t.Errorf("a crashed publish left a visible record: %v", err)
	}
	staged := filepath.Join(h.layout.RestoresDir(), tmpDirName, ".record-"+id.String())
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("crashed publish left no reclaimable temp artifact: %v", err)
	}
	// And the store's listing must not report it as a record.
	findings, err := h.repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("List reported %d finding(s) for a crashed publish: %+v", len(findings), findings)
	}
}

// --- hostile reads --------------------------------------------------------

func TestGetRejectsTamperedMetadata(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, metaPath string, id restore.OperationID)
		wantErr error
	}{
		{
			name: "unknown field",
			mutate: func(t *testing.T, metaPath string, _ restore.OperationID) {
				replaceInFile(t, metaPath, `"schema_version": 1,`, `"schema_version": 1,`+"\n  \"surprise\": true,")
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
		{
			name: "trailing data",
			mutate: func(t *testing.T, metaPath string, _ restore.OperationID) {
				appendToFile(t, metaPath, "\n{}\n")
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
		{
			name: "unsupported schema version",
			mutate: func(t *testing.T, metaPath string, _ restore.OperationID) {
				replaceInFile(t, metaPath, `"schema_version": 1`, `"schema_version": 99`)
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
		{
			name: "id disagrees with the directory name",
			mutate: func(t *testing.T, metaPath string, id restore.OperationID) {
				other := strings.Repeat("f", len(id.String()))
				replaceInFile(t, metaPath, `"id": "`+id.String()+`"`, `"id": "`+other+`"`)
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
		{
			name: "content marked incomplete",
			mutate: func(t *testing.T, metaPath string, _ restore.OperationID) {
				replaceInFile(t, metaPath, `"content_complete": true`, `"content_complete": false`)
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
		{
			name: "manifest payload renamed",
			mutate: func(t *testing.T, metaPath string, _ restore.OperationID) {
				replaceInFile(t, metaPath, `"file": "before.manifest.jsonl"`, `"file": "elsewhere.jsonl"`)
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
		{
			name: "selection is both all and paths",
			mutate: func(t *testing.T, metaPath string, _ restore.OperationID) {
				replaceInFile(t, metaPath, `"all": false`, `"all": true`)
			},
			wantErr: restore.ErrCorruptRecoveryStore,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			id := h.id(1)
			h.publishOne(id)
			metaPath := filepath.Join(h.layout.RestoresDir(), id.String(), metaName)
			tc.mutate(t, metaPath, id)
			if _, err := h.repo.Get(id); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Get after %s = %v, want %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

// TestGetRefusesAnOversizedMetadataDocument proves the one document a read
// materializes whole is bounded rather than sized by whatever wrote it. A record
// directory is local state a damaged or hostile process can rewrite, and the padding
// here is whitespace *inside* the object — so the document still parses, carries the
// same fields, and passes every other guard. Only the size limit can refuse it, which
// is what makes this an oracle for the limit rather than for the strict decoder.
func TestGetRefusesAnOversizedMetadataDocument(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.publishOne(id)
	metaPath := filepath.Join(h.layout.RestoresDir(), id.String(), metaName)
	replaceInFile(t, metaPath, "{", "{"+strings.Repeat(" ", maxMetaFileSize))

	_, err := h.repo.Get(id)
	if !errors.Is(err, restore.ErrCorruptRecoveryStore) {
		t.Fatalf("Get an oversized record = %v, want %v", err, restore.ErrCorruptRecoveryStore)
	}
	if !strings.Contains(err.Error(), "metadata limit") {
		t.Fatalf("Get error %q must name the metadata size limit", err)
	}
}

func TestOpenRejectsATamperedManifestThatKeptItsRecordCount(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	entries := []worktree.Entry{h.entry("a.txt", 1), h.entry("b.txt", 2)}
	scope := newPathScope(t, h.relPath("a.txt"), h.relPath("b.txt"))
	if _, err := h.repo.Publish(context.Background(), h.build(id), scantest.CanonicalStream(entries, nil), scope); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Swap one record's content hash for another valid hash: the line count and
	// every structural check still pass, so only the tree-hash re-derivation can
	// catch it. That is the guard this test exists for.
	manifestPath := filepath.Join(h.layout.RestoresDir(), id.String(), manifestName)
	replaceInFile(t, manifestPath, h.contentHash(1).String(), h.contentHash(9).String())

	read, err := h.repo.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cur, err := read.Manifest().Open(context.Background())
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() { //nolint:revive // draining is the point: the check runs at clean EOF
	}
	if err := cur.Err(); !errors.Is(err, restore.ErrCorruptRecoveryStore) {
		t.Fatalf("drain error = %v, want ErrCorruptRecoveryStore", err)
	}
}

func TestOpenRejectsATamperedCoveredScope(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, scopePath string)
	}{
		{
			name: "path count no longer matches",
			mutate: func(t *testing.T, scopePath string) {
				appendToFile(t, scopePath, "{\"path\":\"zzz/extra.go\"}\n")
			},
		},
		{
			name: "out of canonical order",
			mutate: func(t *testing.T, scopePath string) {
				writeFile(t, scopePath, "{\"path\":\"generated/client/openapi.json\"}\n{\"path\":\"generated/client/new.go\"}\n")
			},
		},
		{
			name: "duplicate path",
			mutate: func(t *testing.T, scopePath string) {
				writeFile(t, scopePath, "{\"path\":\"generated/client/new.go\"}\n{\"path\":\"generated/client/new.go\"}\n")
			},
		},
		{
			name: "path escapes the project root",
			mutate: func(t *testing.T, scopePath string) {
				writeFile(t, scopePath, "{\"path\":\"../outside\"}\n{\"path\":\"generated/client/new.go\"}\n")
			},
		},
		{
			name: "unknown field",
			mutate: func(t *testing.T, scopePath string) {
				writeFile(t, scopePath, "{\"path\":\"a\",\"extra\":1}\n{\"path\":\"b\"}\n")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			id := h.id(1)
			h.publishOne(id)
			scopePath := filepath.Join(h.layout.RestoresDir(), id.String(), scopeName)
			// The payload is published read-only; a hostile edit would chmod first.
			if err := os.Chmod(scopePath, 0o600); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			tc.mutate(t, scopePath)
			read, err := h.repo.Open(context.Background(), id)
			if err != nil {
				// Rejection at open is equally acceptable; the payload never becomes evidence.
				if !errors.Is(err, restore.ErrCorruptRecoveryStore) {
					t.Fatalf("Open after %s = %v, want ErrCorruptRecoveryStore", tc.name, err)
				}
				return
			}
			if _, err := drainScope(context.Background(), read.Scope()); !errors.Is(err, restore.ErrCorruptRecoveryStore) {
				t.Fatalf("draining the covered scope after %s = %v, want ErrCorruptRecoveryStore", tc.name, err)
			}
		})
	}
}

func TestOpenReportsAMissingPayloadAsCorruption(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.publishOne(id)
	if err := os.Remove(filepath.Join(h.layout.RestoresDir(), id.String(), scopeName)); err != nil {
		t.Fatalf("remove scope: %v", err)
	}
	read, err := h.repo.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := drainScope(context.Background(), read.Scope()); !errors.Is(err, restore.ErrCorruptRecoveryStore) {
		t.Fatalf("reading a missing scope payload = %v, want ErrCorruptRecoveryStore", err)
	}
}

func TestReadRefusesASymlinkedPayload(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.publishOne(id)
	metaPath := filepath.Join(h.layout.RestoresDir(), id.String(), metaName)
	decoy := filepath.Join(h.root, "decoy.json")
	data, err := os.ReadFile(metaPath) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if err := os.WriteFile(decoy, data, 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if err := os.Remove(metaPath); err != nil {
		t.Fatalf("remove meta: %v", err)
	}
	if err := os.Symlink(decoy, metaPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := h.repo.Get(id); !errors.Is(err, restore.ErrCorruptRecoveryStore) {
		t.Fatalf("Get through a symlinked payload = %v, want ErrCorruptRecoveryStore", err)
	}
}

// --- listing, resolution, deletion ---------------------------------------

func TestListIsNewestFirstAndHonestAboutUnreadableRecords(t *testing.T) {
	h := setup(t)
	older := h.id(1)
	newer := h.id(2)
	h.publishOne(older)
	h.publishOne(newer)

	// A third record whose directory name is an id but whose metadata is gone: GC
	// must be able to see that a record exists and cannot be read, because that
	// makes blob reachability incomplete.
	broken := h.id(3)
	h.publishOne(broken)
	if err := os.Remove(filepath.Join(h.layout.RestoresDir(), broken.String(), metaName)); err != nil {
		t.Fatalf("remove meta: %v", err)
	}
	// And a stray directory that is not an id at all.
	if err := os.MkdirAll(filepath.Join(h.layout.RestoresDir(), "not-an-id"), paths.DirPerm); err != nil {
		t.Fatalf("mkdir stray: %v", err)
	}

	findings, err := h.repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(findings) != 4 {
		t.Fatalf("List returned %d finding(s), want 4: %+v", len(findings), findings)
	}
	readable, unreadable := 0, 0
	for _, f := range findings {
		if f.Readable() {
			readable++
		} else {
			unreadable++
		}
	}
	if readable != 2 || unreadable != 2 {
		t.Errorf("List reported %d readable / %d unreadable, want 2 / 2", readable, unreadable)
	}
	// Ids are time-prefixed, so newest-first ordering falls out of the id. Compare
	// only the id-bearing findings: a stray non-id directory sorts by its name and
	// its position is not part of the contract.
	var order []string
	for _, f := range findings {
		if !f.ID.IsZero() {
			order = append(order, f.ID.String())
		}
	}
	want := []string{broken.String(), newer.String(), older.String()}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("List is not newest-first: %v, want %v", order, want)
	}
}

func TestListOnAnAbsentStoreIsCleanAndEmpty(t *testing.T) {
	h := setup(t)
	findings, err := h.repo.List(context.Background())
	if err != nil {
		t.Fatalf("List on an absent store: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("List returned %d finding(s) for a project that never restored", len(findings))
	}
}

func TestResolvePrefix(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.publishOne(id)

	got, err := h.repo.ResolvePrefix(context.Background(), id.Short())
	if err != nil || got != id {
		t.Fatalf("ResolvePrefix(short) = %v, %v; want %s", got, err, id)
	}
	if _, err := h.repo.ResolvePrefix(context.Background(), strings.Repeat("f", 8)); !errors.Is(err, restore.ErrRecoveryNotFound) {
		t.Errorf("ResolvePrefix(no match) = %v, want ErrRecoveryNotFound", err)
	}
	if _, err := h.repo.ResolvePrefix(context.Background(), ""); err == nil {
		t.Error("an empty prefix resolved; a blank reference must never mean match-all")
	}
	if _, err := h.repo.ResolvePrefix(context.Background(), "zz"); err == nil {
		t.Error("a non-hex prefix resolved")
	}
}

func TestResolvePrefixReportsAmbiguity(t *testing.T) {
	h := setup(t)
	// Two ids sharing a long prefix: same timestamp, randomness differing only in
	// the final byte.
	mk := func(last byte) restore.OperationID {
		id, err := restore.NewOperationID(1, bytes.NewReader([]byte{9, 9, 9, 9, 9, 9, 9, last}))
		if err != nil {
			t.Fatalf("operation id: %v", err)
		}
		return id
	}
	a, b := mk(1), mk(2)
	h.publishOne(a)
	h.publishOne(b)
	shared := a.String()[:len(a.String())-2]
	if _, err := h.repo.ResolvePrefix(context.Background(), shared); !errors.Is(err, restore.ErrRecoveryAmbiguousPrefix) {
		t.Fatalf("ResolvePrefix(ambiguous) = %v, want ErrRecoveryAmbiguousPrefix", err)
	}
}

func TestDeleteIsIdempotentAndRemovesTheWholeRecord(t *testing.T) {
	h := setup(t)
	id := h.id(1)
	h.publishOne(id)
	if err := h.repo.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.layout.RestoresDir(), id.String())); !os.IsNotExist(err) {
		t.Errorf("record directory survived Delete: %v", err)
	}
	if err := h.repo.Delete(id); err != nil {
		t.Errorf("Delete of an absent record is not idempotent: %v", err)
	}
	if err := h.repo.Delete(h.id(42)); err != nil {
		t.Errorf("Delete of a never-published id failed: %v", err)
	}
}

func TestPublishRejectsAnIncompleteBuild(t *testing.T) {
	h := setup(t)
	scope := newPathScope(t, h.relPath("a"))
	if _, err := h.repo.Publish(context.Background(), restore.RecoveryBuild{}, scantest.CanonicalStream(nil, nil), scope); err == nil {
		t.Error("Publish accepted a build with no operation id")
	}
	if _, err := h.repo.Publish(context.Background(), h.build(h.id(1)), nil, scope); err == nil {
		t.Error("Publish accepted a nil manifest stream")
	}
}

// --- helpers --------------------------------------------------------------

func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := string(data)
	if !strings.Contains(s, old) {
		t.Fatalf("%s does not contain %q", path, old)
	}
	writeFile(t, path, strings.Replace(s, old, new, 1))
}

func appendToFile(t *testing.T, path, extra string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	writeFile(t, path, string(data)+extra)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	// Published payloads are read-only, so a rewrite must widen the mode first —
	// exactly what a hand-edit or hostile process would have to do.
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		t.Fatalf("chmod %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
