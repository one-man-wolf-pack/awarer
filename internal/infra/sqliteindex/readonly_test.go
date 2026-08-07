package sqliteindex_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"awarer/internal/domain/worktree"
	"awarer/internal/infra/sqliteindex"
)

// seedIndex builds a committed index in dir holding one entry for path, then closes
// it, so a later OpenReadOnly has a real committed row to reuse.
func seedIndex(t *testing.T, dir, path string) worktree.Entry {
	t.Helper()
	ctx := context.Background()
	idx, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	tx, err := idx.BeginScan(ctx, meta(t, 1000))
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	e := sampleEntry(t, path)
	if err := tx.Upsert(ctx, e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0)); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return e
}

// ownedFile reports whether name is one of the worktree index's own files (the
// database or a WAL coordination sidecar).
func ownedFile(name string) bool {
	for _, owned := range sqliteindex.OwnedFiles() {
		if name == owned {
			return true
		}
	}
	return false
}

// TestOpenReadOnlyReusesCommittedHashes proves the happy path: a committed entry is
// returned unchanged through a strictly read-only open, under normal trust.
func TestOpenReadOnlyReusesCommittedHashes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	want := seedIndex(t, dir, "a.go")

	idx, err := sqliteindex.OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer idx.Close()

	got, ok, err := idx.Lookup(ctx, relPath(t, "a.go"))
	if err != nil || !ok {
		t.Fatalf("Lookup: ok=%v err=%v", ok, err)
	}
	if got.Content.String() != want.Content.String() {
		t.Errorf("content = %q, want %q", got.Content, want.Content)
	}
	if !got.Stat.EqualNormal(want.Stat) {
		t.Errorf("stat mismatch: %+v vs %+v", got.Stat, want.Stat)
	}
}

// TestOpenReadOnlyDoesNotMutateIndexFiles proves a read-only open plus a lookup does
// not rebuild, migrate, or otherwise alter the index database, and adds no file other
// than the index's own WAL coordination sidecars. Reading a WAL-mode database is
// coordinated (lock-respecting, consistent), so SQLite may (re)create -wal/-shm; that
// is the index's own machinery, not a mutation of its content — the database bytes
// stay identical.
func TestOpenReadOnlyDoesNotMutateIndexFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")

	dbPath := filepath.Join(dir, sqliteindex.FileName)
	beforeDB, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile db: %v", err)
	}

	idx, err := sqliteindex.OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if _, _, err := idx.Lookup(ctx, relPath(t, "a.go")); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	afterDB, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile db: %v", err)
	}
	if string(beforeDB) != string(afterDB) {
		t.Errorf("index database bytes changed under a read-only open")
	}

	// Any file that appeared must be one the index owns (the WAL sidecars), never a
	// stray or rebuilt artifact.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range ents {
		if !ownedFile(e.Name()) {
			t.Errorf("read-only open left an unowned file: %s", e.Name())
		}
	}
}

// TestOpenReadOnlyMissingIndexUnavailable proves an absent index reports Unavailable
// (so the caller re-hashes, never treats it as corruption) and creates no database.
func TestOpenReadOnlyMissingIndexUnavailable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	_, err := sqliteindex.OpenReadOnly(ctx, dir)
	if !errors.Is(err, sqliteindex.ErrUnavailable) {
		t.Fatalf("OpenReadOnly on missing index: err = %v, want ErrUnavailable", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, sqliteindex.FileName)); !os.IsNotExist(statErr) {
		t.Errorf("OpenReadOnly created a database for a missing index")
	}
}

// TestOpenReadOnlyRefusesWrites proves the read-only handle rejects the write path up
// front rather than failing deep in SQLite.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")

	idx, err := sqliteindex.OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer idx.Close()

	if _, err := idx.BeginScan(ctx, meta(t, 2000)); err == nil {
		t.Errorf("BeginScan on a read-only index should fail")
	}
	if err := idx.Invalidate(ctx, relPath(t, "a.go")); err == nil {
		t.Errorf("Invalidate on a read-only index should fail")
	}
}

// TestOpenReadOnlyRefusesTooNewSchema proves a newer schema is refused (never served)
// so the caller falls back to re-hashing instead of trusting foreign rows.
func TestOpenReadOnlyRefusesTooNewSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")
	stampVersion(t, dir, 999)

	_, err := sqliteindex.OpenReadOnly(ctx, dir)
	if !errors.Is(err, sqliteindex.ErrSchemaTooNew) {
		t.Fatalf("OpenReadOnly on too-new schema: err = %v, want ErrSchemaTooNew", err)
	}
}

// TestOpenReadOnlyRefusesStaleSchemaWithoutRebuild proves a stale schema is refused
// and — unlike Open — never rebuilt: the read-only path must not mutate the store.
func TestOpenReadOnlyRefusesStaleSchemaWithoutRebuild(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")
	stampVersion(t, dir, 0)

	_, err := sqliteindex.OpenReadOnly(ctx, dir)
	if !errors.Is(err, sqliteindex.ErrSchemaStale) {
		t.Fatalf("OpenReadOnly on stale schema: err = %v, want ErrSchemaStale", err)
	}

	// The version must still be 0: OpenReadOnly must not have rebuilt or restamped it.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	if err := raw.QueryRow("SELECT version FROM schema_meta WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("reading version: %v", err)
	}
	if v != 0 {
		t.Errorf("OpenReadOnly restamped a stale schema to version %d; it must not mutate", v)
	}
}

// TestReadOnlyLookupFailSoftOnCorruptRow proves the read-only Lookup degrades a
// corrupt row to a miss (so the scanner re-hashes) while the mutable Lookup still
// surfaces the same corruption as an error. A failed acceleration is never a failure.
func TestReadOnlyLookupFailSoftOnCorruptRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")

	// Write an omitted_fields value carrying a bit outside the known mask, bypassing
	// the CHECK constraint, to simulate corruption that reached the file.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName)+"?_pragma=ignore_check_constraints(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE file_index SET omitted_fields = 128 WHERE path = 'a.go'"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	// Mutable open still surfaces the corruption as an error (authoritative maintenance).
	mut, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := mut.Lookup(ctx, relPath(t, "a.go")); err == nil {
		t.Errorf("mutable Lookup accepted a corrupt row")
	}
	_ = mut.Close()

	// Read-only open degrades the same corrupt row to a miss, with no error.
	ro, err := sqliteindex.OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	entry, ok, err := ro.Lookup(ctx, relPath(t, "a.go"))
	if err != nil {
		t.Errorf("read-only Lookup surfaced a corrupt row as an error: %v", err)
	}
	if ok {
		t.Errorf("read-only Lookup returned a corrupt row as a hit: %+v", entry)
	}
}

// TestReadOnlyLookupPropagatesCancellation proves a cancelled context is surfaced, not
// swallowed into a miss: cancellation is a stop signal, so the read-only Lookup must
// fail fast rather than send the scanner off to re-hash a path from disk.
func TestReadOnlyLookupPropagatesCancellation(t *testing.T) {
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")

	idx, err := sqliteindex.OpenReadOnly(context.Background(), dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer idx.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, err := idx.Lookup(ctx, relPath(t, "a.go"))
	if ok {
		t.Errorf("cancelled Lookup returned a hit")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled read-only Lookup: err = %v, want context.Canceled (fail fast, not a silent miss)", err)
	}
}

// TestReadOnlyLookupMissingRowIsMiss proves an absent path is a plain miss, so the
// scanner re-hashes exactly that path.
func TestReadOnlyLookupMissingRowIsMiss(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")

	idx, err := sqliteindex.OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer idx.Close()
	if _, ok, err := idx.Lookup(ctx, relPath(t, "absent.go")); ok || err != nil {
		t.Errorf("Lookup of an absent path: ok=%v err=%v, want miss", ok, err)
	}
}

// TestOpenReadOnlyCoexistsWithOpenMutable proves a read-only open needs no exclusive
// access or presence lock: it opens and reads while a mutable handle is also open.
func TestOpenReadOnlyCoexistsWithOpenMutable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seedIndex(t, dir, "a.go")

	mut, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mut.Close()

	ro, err := sqliteindex.OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly while mutable open: %v", err)
	}
	defer ro.Close()
	if _, ok, err := ro.Lookup(ctx, relPath(t, "a.go")); !ok || err != nil {
		t.Errorf("read-only Lookup alongside a mutable handle: ok=%v err=%v", ok, err)
	}
}
