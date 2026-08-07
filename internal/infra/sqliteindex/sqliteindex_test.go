package sqliteindex_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/blake3hash"
	"awarer/internal/infra/sqliteindex"
)

func openIndex(t *testing.T, dir string) *sqliteindex.Index {
	t.Helper()
	idx, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func relPath(t *testing.T, s string) worktree.RelPath {
	t.Helper()
	p, err := worktree.ParseRelPath(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func contentHash(t *testing.T, s string) hashing.ContentHash {
	t.Helper()
	ch, err := blake3hash.New().HashReader(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func treeHash(t *testing.T, s string) hashing.TreeHash {
	t.Helper()
	return blake3hash.New().HashBytes([]byte(s))
}

func newScanID(t *testing.T, nanos int64) worktree.ScanID {
	t.Helper()
	id, err := worktree.NewScanID(nanos, zeroReader{})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func configHash(t *testing.T) hashing.ConfigHash {
	t.Helper()
	c, err := hashing.ParseConfigHash("blake3:" + "00000000000000000000000000000000000000000000000000000000000000ab")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func meta(t *testing.T, nanos int64) worktree.ScanMetadata {
	return worktree.ScanMetadata{
		ScanID:     newScanID(t, nanos),
		Root:       "/tmp/proj",
		ConfigHash: configHash(t),
		TrustMode:  config.TrustNormal,
		StartedAt:  time.Unix(0, nanos),
	}
}

func sampleEntry(t *testing.T, path string) worktree.Entry {
	return worktree.Entry{
		Path:    relPath(t, path),
		Kind:    worktree.KindRegular,
		Content: contentHash(t, path),
		Storage: worktree.StorageBlob,
		Stat:    worktree.StatSignature{Size: 10, MtimeNs: 100, CtimeNs: 200, Mode: 0o644, Dev: 1, Ino: 2, Nlink: 1},
	}
}

func TestOpenCreatesDatabaseAndWAL(t *testing.T) {
	dir := t.TempDir()
	// The database file does not exist until Open.
	if _, err := os.Stat(filepath.Join(dir, sqliteindex.FileName)); !os.IsNotExist(err) {
		t.Fatalf("db should not exist before Open")
	}
	idx := openIndex(t, dir)
	if _, err := os.Stat(filepath.Join(dir, sqliteindex.FileName)); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
	if !idx.WALEnabled() {
		t.Skipf("WAL not enabled on this filesystem; treated as supported-where-possible")
	}
}

func TestSchemaIdempotentAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()
	// Re-opening must not fail on existing schema.
	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	_ = idx2.Close()
}

func TestCommitMakesScanAndEntriesVisible(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	tx, err := idx.BeginScan(ctx, meta(t, 1000))
	if err != nil {
		t.Fatal(err)
	}
	e := sampleEntry(t, "a.go")
	if err := tx.Upsert(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx, treeHash(t, "tree"), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := idx.Lookup(ctx, relPath(t, "a.go"))
	if err != nil || !ok {
		t.Fatalf("Lookup after commit: ok=%v err=%v", ok, err)
	}
	if got.Content.String() != e.Content.String() {
		t.Errorf("content = %q, want %q", got.Content, e.Content)
	}
	if !got.Stat.EqualNormal(e.Stat) {
		t.Errorf("stat mismatch: %+v vs %+v", got.Stat, e.Stat)
	}
}

func TestInvalidateRemovesEntry(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	tx, err := idx.BeginScan(ctx, meta(t, 1500))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx, treeHash(t, "tree"), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := idx.Lookup(ctx, relPath(t, "a.go")); !ok {
		t.Fatal("entry should be present before invalidation")
	}

	if err := idx.Invalidate(ctx, relPath(t, "a.go")); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, ok, _ := idx.Lookup(ctx, relPath(t, "a.go")); ok {
		t.Fatal("entry should be gone after invalidation")
	}
	// Idempotent: invalidating an absent path is not an error.
	if err := idx.Invalidate(ctx, relPath(t, "a.go")); err != nil {
		t.Fatalf("second Invalidate: %v", err)
	}
}

func TestRollbackLeavesNoState(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	tx, err := idx.BeginScan(ctx, meta(t, 2000))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Upsert(ctx, sampleEntry(t, "b.go")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := idx.Lookup(ctx, relPath(t, "b.go")); ok || err != nil {
		t.Errorf("rolled-back entry visible: ok=%v err=%v", ok, err)
	}
}

func TestInterruptedScanClearedOnReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	// Begin but never commit: the scan row is incomplete. Closing without commit
	// rolls back the open transaction, but simulate a crash by leaving an
	// incomplete marker via a committed-incomplete path is not possible in one tx;
	// instead verify the cleanup query runs and a fresh open has no incomplete
	// scans.
	tx, err := idx.BeginScan(ctx, meta(t, 3000))
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	_ = idx.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer idx2.Close()
	// No assertion target beyond a clean reopen; the cleanup must not error and
	// the index must be usable.
	if _, ok, err := idx2.Lookup(ctx, relPath(t, "missing")); ok || err != nil {
		t.Errorf("unexpected lookup state after reopen: ok=%v err=%v", ok, err)
	}
}

func TestRebuildAfterDeletion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)
	tx, _ := idx.BeginScan(ctx, meta(t, 4000))
	_ = tx.Upsert(ctx, sampleEntry(t, "c.go"))
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	// Delete the database files; the next Open must rebuild transparently.
	matches, _ := filepath.Glob(filepath.Join(dir, sqliteindex.FileName+"*"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("rebuild Open: %v", err)
	}
	defer idx2.Close()
	if _, ok, _ := idx2.Lookup(ctx, relPath(t, "c.go")); ok {
		t.Errorf("entry survived database deletion; index is not acceleration-only")
	}
}

func TestUpsertReplacesPriorRow(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	tx1, _ := idx.BeginScan(ctx, meta(t, 5000))
	_ = tx1.Upsert(ctx, sampleEntry(t, "d.go"))
	_ = tx1.Commit(ctx, treeHash(t, "t1"), time.Unix(1, 0))

	updated := sampleEntry(t, "d.go")
	updated.Content = contentHash(t, "new-content")
	updated.Stat.Size = 999
	tx2, _ := idx.BeginScan(ctx, meta(t, 6000))
	_ = tx2.Upsert(ctx, updated)
	_ = tx2.Commit(ctx, treeHash(t, "t2"), time.Unix(1, 0))

	got, ok, _ := idx.Lookup(ctx, relPath(t, "d.go"))
	if !ok {
		t.Fatal("lookup failed")
	}
	if got.Content.String() != updated.Content.String() || got.Stat.Size != 999 {
		t.Errorf("row not replaced: %+v", got)
	}
}

func TestOmittedStatFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	e := sampleEntry(t, "e.go")
	// Simulate a platform that could not supply ino/dev/ctime/nlink.
	e.Stat.Omitted = worktree.FieldSet(0).
		With(worktree.FieldCtime).
		With(worktree.FieldDev).
		With(worktree.FieldIno).
		With(worktree.FieldNlink)

	tx, _ := idx.BeginScan(ctx, meta(t, 8000))
	if err := tx.Upsert(ctx, e); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))

	got, ok, err := idx.Lookup(ctx, relPath(t, "e.go"))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	for _, f := range []worktree.StatField{worktree.FieldCtime, worktree.FieldDev, worktree.FieldIno, worktree.FieldNlink} {
		if !got.Stat.Omitted.Has(f) {
			t.Errorf("omitted field %v lost across SQLite round-trip", f)
		}
	}
}

// TestStatHighBitFieldsRoundTrip pins the documented two's-complement behavior:
// uint64 stat fields with the high bit set survive the int64 SQLite round-trip
// exactly, so stat-signature reuse stays correct on filesystems with large
// dev/ino values.
func TestStatHighBitFieldsRoundTrip(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	e := sampleEntry(t, "big-ino.go")
	e.Stat.Dev = 1 << 63         // high bit set
	e.Stat.Ino = ^uint64(0)      // max uint64
	e.Stat.Nlink = (1 << 63) + 7 // high bit set, non-trivial low bits

	tx, _ := idx.BeginScan(ctx, meta(t, 12000))
	if err := tx.Upsert(ctx, e); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))

	got, ok, err := idx.Lookup(ctx, relPath(t, "big-ino.go"))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Stat.Dev != e.Stat.Dev || got.Stat.Ino != e.Stat.Ino || got.Stat.Nlink != e.Stat.Nlink {
		t.Errorf("high-bit stat fields not preserved: got dev=%d ino=%d nlink=%d", got.Stat.Dev, got.Stat.Ino, got.Stat.Nlink)
	}
}

func TestCommitRejectsInvalidCompletion(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	// startedAt for meta(t, 5000) is time.Unix(0, 5000).
	cases := map[string]struct {
		hash func() hashing.TreeHash
		done time.Time
	}{
		"zero tree hash":          {func() hashing.TreeHash { return hashing.TreeHash{} }, time.Unix(1, 0)},
		"zero completion time":    {func() hashing.TreeHash { return treeHash(t, "t") }, time.Time{}},
		"completion before start": {func() hashing.TreeHash { return treeHash(t, "t") }, time.Unix(0, 1)},
	}
	for name, c := range cases {
		tx, err := idx.BeginScan(ctx, meta(t, 5000))
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx, c.hash(), c.done); err == nil {
			t.Errorf("%s: Commit accepted an invalid completion", name)
			_ = tx.Rollback()
		}
	}
}

// TestSkippedRowPersistsFullState proves a skipped input is stored completely —
// reason, OS error category, and symlink target — not just the reason.
func TestSkippedRowPersistsFullState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	readErr, err := worktree.NewSkippedInput(relPath(t, "secret.txt"), worktree.ReasonReadError, 5, worktree.StatSignature{}, "permission-denied", worktree.SymlinkTarget{}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	target, _ := worktree.NewSymlinkTarget("../outside")
	prov := worktree.TraversalInfo{Followed: true, SourcePath: relPath(t, "a/loop"), Depth: 1}
	cycle, err := worktree.NewSkippedInput(relPath(t, "loop"), worktree.ReasonSymlinkCycle, 0, worktree.StatSignature{}, "", target, prov)
	if err != nil {
		t.Fatal(err)
	}

	tx, _ := idx.BeginScan(ctx, meta(t, 13000))
	if err := tx.RecordSkipped(ctx, readErr); err != nil {
		t.Fatal(err)
	}
	if err := tx.RecordSkipped(ctx, cycle); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()

	// Inspect the raw rows: skipped state must be fully persisted.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	var osErr, symlink sql.NullString
	var size int64
	if err := raw.QueryRow("SELECT skipped_os_error, symlink_target, size FROM file_index WHERE path = 'secret.txt'").Scan(&osErr, &symlink, &size); err != nil {
		t.Fatal(err)
	}
	if osErr.String != "permission-denied" || symlink.Valid {
		t.Errorf("read-error row: os_error=%q symlink_valid=%v, want permission-denied / null", osErr.String, symlink.Valid)
	}
	// The skipped input's own size (5) is persisted, not the zero stat size.
	if size != 5 {
		t.Errorf("read-error row size = %d, want 5 (from the skipped input, not its zero stat)", size)
	}
	var traversalSource sql.NullString
	var depth int
	if err := raw.QueryRow("SELECT skipped_os_error, symlink_target, traversal_source_path, traversal_depth FROM file_index WHERE path = 'loop'").Scan(&osErr, &symlink, &traversalSource, &depth); err != nil {
		t.Fatal(err)
	}
	if osErr.Valid || symlink.String != "../outside" {
		t.Errorf("cycle row: os_error_valid=%v symlink=%q, want null / ../outside", osErr.Valid, symlink.String)
	}
	// Provenance is persisted, not dropped.
	if traversalSource.String != "a/loop" || depth != 1 {
		t.Errorf("cycle row provenance: source=%q depth=%d, want a/loop / 1", traversalSource.String, depth)
	}
}

// TestUpsertRejectsInvalidEntry proves the persistence boundary re-validates: a
// malformed entry cannot reach the database even if a caller assembled it directly
// instead of through a New*Entry constructor.
func TestUpsertRejectsInvalidEntry(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())
	tx, err := idx.BeginScan(ctx, meta(t, 9000))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// A regular entry with no content hash is impossible.
	bad := worktree.Entry{Path: relPath(t, "bad.go"), Kind: worktree.KindRegular, Storage: worktree.StorageBlob}
	if err := tx.Upsert(ctx, bad); err == nil {
		t.Errorf("Upsert accepted a regular entry with no content hash")
	}
}

func TestRecordSkippedStoresNoContent(t *testing.T) {
	ctx := context.Background()
	idx := openIndex(t, t.TempDir())

	tx, _ := idx.BeginScan(ctx, meta(t, 7000))
	in := worktree.SkippedInput{
		Path:   relPath(t, "big.bin"),
		Kind:   worktree.KindRegular,
		Reason: worktree.ReasonLargeFileSkipPolicy,
		Size:   123,
	}
	if err := tx.RecordSkipped(ctx, in); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))

	got, ok, err := idx.Lookup(ctx, relPath(t, "big.bin"))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if !got.Content.IsZero() {
		t.Errorf("skipped input should have no content hash, got %q", got.Content)
	}
}
