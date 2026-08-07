package sqliteindex_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"awarer/internal/domain/config"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/sqliteindex"
)

// stampVersion opens the database file directly and overwrites the schema
// version, simulating a database written by a different awa build.
func stampVersion(t *testing.T, dir string, version int) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("UPDATE schema_meta SET version = ?", version); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesNewerSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()

	stampVersion(t, dir, 999)
	if _, err := sqliteindex.Open(dir); err == nil {
		t.Fatalf("expected refusal of a newer schema version")
	}
}

func TestRebuildsOlderSchemaVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	idx := openIndex(t, dir)
	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	_ = tx.Upsert(ctx, sampleEntry(t, "old.go"))
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	// Pretend the database predates the current schema; reopening must rebuild it
	// (the index is acceleration-only), discarding the old row.
	stampVersion(t, dir, 0)
	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("reopen older schema: %v", err)
	}
	defer idx2.Close()
	if _, ok, _ := idx2.Lookup(ctx, relPath(t, "old.go")); ok {
		t.Errorf("rebuilt index still held a row from the older schema")
	}
}

// TestRebuildsOnDDLMismatchWithSameObjectSet proves the schema check compares each
// object's full DDL, not merely which objects exist: a database whose object set is
// exactly the current one, but whose file_index carries a different structure (here,
// no PRIMARY KEY on path and a nullable large_file_policy), is not current and is
// rebuilt. This is the only condition the object-set comparison alone cannot see, and
// it has to be caught — accepting the database would let a later write fail on a
// constraint this build never wrote instead of rebuilding.
func TestRebuildsOnDDLMismatchWithSameObjectSet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Create the current schema, then replace file_index with a same-columns,
	// different-structure variant and a stale row, restoring the index that dropping
	// the table takes with it.
	idx := openIndex(t, dir)
	_ = idx.Close()

	dbPath := filepath.Join(dir, sqliteindex.FileName)
	before := schemaObjectNames(t, dbPath)

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE file_index;
		CREATE TABLE file_index (
		  path TEXT, kind TEXT NOT NULL, size INTEGER, mtime_ns INTEGER, ctime_ns INTEGER,
		  mode INTEGER, dev INTEGER, ino INTEGER, nlink INTEGER, omitted_fields INTEGER NOT NULL DEFAULT 0,
		  content_hash TEXT, blob_present INTEGER NOT NULL, large_file_policy TEXT, skipped_reason TEXT,
		  skipped_os_error TEXT, symlink_target TEXT, traversal_source_path TEXT,
		  traversal_depth INTEGER NOT NULL DEFAULT 0,
		  last_seen_scan_id TEXT NOT NULL, updated_at TEXT NOT NULL
		);
		CREATE INDEX idx_file_index_last_seen ON file_index (last_seen_scan_id);
		INSERT INTO file_index (path, kind, blob_present, last_seen_scan_id, updated_at)
		  VALUES ('stale.go', 'file', 0, 'x', 'x');`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	// The fixture is only evidence about DDL text if it left the object set alone: a
	// missing or extra object would be caught by the coarser membership check and this
	// test would silently stop proving what it claims.
	if after := schemaObjectNames(t, dbPath); !slices.Equal(before, after) {
		t.Fatalf("fixture changed the object set (%v -> %v); it no longer isolates a DDL difference", before, after)
	}

	// Doctor classifies it as repairable stale shape rather than an unreadable error...
	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrSchemaStale) {
		t.Fatalf("Inspect over a differing DDL = %v, want ErrSchemaStale", err)
	}
	// ...and opening it rebuilds, discarding the row the foreign shape held.
	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("reopen over mismatched DDL: %v", err)
	}
	defer idx2.Close()
	if _, ok, _ := idx2.Lookup(ctx, relPath(t, "stale.go")); ok {
		t.Errorf("stale row survived: DDL mismatch was not rebuilt")
	}
	// The rebuilt schema must accept a fresh scan.
	tx, err := idx2.BeginScan(ctx, meta(t, 2000))
	if err != nil {
		t.Fatalf("BeginScan after rebuild: %v", err)
	}
	if err := tx.Upsert(ctx, sampleEntry(t, "new.go")); err != nil {
		t.Fatalf("upsert after rebuild: %v", err)
	}
	if err := tx.Commit(ctx, treeHash(t, "t2"), time.Unix(2, 0)); err != nil {
		t.Fatalf("Commit after rebuild: %v", err)
	}
}

// schemaObjectNames returns the sorted names of a database's user objects, read
// straight from sqlite_schema so a fixture's effect on the object set is observed
// rather than assumed.
func schemaObjectNames(t *testing.T, dbPath string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	rows, err := raw.Query("SELECT name FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// TestLookupRejectsCorruptOmittedFields proves Lookup fails loudly on an
// out-of-range omitted_fields value rather than casting unknown bits into the
// domain model.
func TestLookupRejectsCorruptOmittedFields(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	_ = tx.Upsert(ctx, sampleEntry(t, "a.go"))
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	// Corrupt the row with an omitted_fields value carrying an unknown bit. The CHECK
	// constraint would normally reject this, so disable check enforcement on this
	// connection to simulate corruption that bypassed it (a manual hex edit, or a row
	// written by an older build) — exactly what the read boundary must still catch.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName)+"?_pragma=ignore_check_constraints(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE file_index SET omitted_fields = 128 WHERE path = 'a.go'"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	if _, _, err := idx2.Lookup(ctx, relPath(t, "a.go")); err == nil {
		t.Errorf("Lookup accepted a corrupt omitted_fields value")
	}
}

// TestSchemaRejectsUnknownOmittedBits proves the DB-level CHECK pins both
// omitted-fields columns to the domain's known mask: a value carrying a bit outside
// it is rejected at write time, so mechanically impossible metadata cannot enter
// the database in the first place. A value of the full known mask is still accepted.
func TestSchemaRejectsUnknownOmittedBits(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)
	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	_ = tx.Upsert(ctx, sampleEntry(t, "a.go"))
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	if _, err := raw.Exec("UPDATE file_index SET omitted_fields = 128 WHERE path = 'a.go'"); err == nil {
		t.Errorf("file_index.omitted_fields accepted a bit outside the known mask")
	}
	if _, err := raw.Exec("UPDATE scans SET omitted_stat_fields = 128"); err == nil {
		t.Errorf("scans.omitted_stat_fields accepted a bit outside the known mask")
	}
	// The full known mask (every defined bit set) is a legitimate value.
	if _, err := raw.Exec("UPDATE file_index SET omitted_fields = ? WHERE path = 'a.go'", worktree.KnownFieldsMask); err != nil {
		t.Errorf("file_index.omitted_fields rejected the full known mask: %v", err)
	}
}

// TestRebuildsPartialSchemaWithoutSchemaMeta proves a current-shaped file_index
// without schema_meta is treated as a partial (not current) schema and rebuilt —
// rather than silently topped up, which would leave rows with no version lineage.
func TestRebuildsPartialSchemaWithoutSchemaMeta(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// Build the full current schema and a row, then strip everything but the
	// (current-shaped) file_index, leaving a partial schema with no schema_meta.
	idx := openIndex(t, dir)
	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	_ = tx.Upsert(ctx, sampleEntry(t, "stale.go"))
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE scans; DROP TABLE schema_meta;
		DROP INDEX idx_file_index_last_seen;`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("reopen partial schema: %v", err)
	}
	defer idx2.Close()
	if _, ok, _ := idx2.Lookup(ctx, relPath(t, "stale.go")); ok {
		t.Errorf("stale row survived: partial schema was topped up instead of rebuilt")
	}
}

// TestLookupRejectsContentHashOnNonContentRow proves a content hash attached to a
// skipped (or otherwise non-content) row is rejected at the read boundary, so a
// corrupt or tampered cache row can never become a false reuse candidate.
func TestLookupRejectsContentHashOnNonContentRow(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	skip, err := worktree.NewSkippedInput(relPath(t, "big.bin"), worktree.ReasonLargeFileSkipPolicy, 100, worktree.StatSignature{}, "", worktree.SymlinkTarget{}, worktree.TraversalInfo{})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	if err := tx.RecordSkipped(ctx, skip); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	// Tamper: put a stray content hash on the skipped row.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE file_index SET content_hash = ? WHERE path = 'big.bin'", contentHash(t, "stray").String()); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	if _, _, err := idx2.Lookup(ctx, relPath(t, "big.bin")); err == nil {
		t.Errorf("Lookup accepted a content hash on a skipped row")
	}
}

// TestLookupRejectsContentHashOnNonRegularKind proves a content hash on a
// non-regular kind is rejected even when its storage intent looks valid — a
// tampered row (kind='symlink', storage='blob', a valid hash) must not become a
// reuse candidate.
func TestLookupRejectsContentHashOnNonRegularKind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	if err := tx.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	// Tamper: keep a valid hash and blob storage but flip the kind to symlink.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE file_index SET kind = 'symlink' WHERE path = 'a.go'"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	if _, _, err := idx2.Lookup(ctx, relPath(t, "a.go")); err == nil {
		t.Errorf("Lookup accepted a content hash on a symlink-kind row")
	}
}

// TestLookupRejectsMissingStorageIntent proves a row with an empty storage intent
// is rejected, rather than defaulting to the zero ContentStorageIntent (blob) and
// becoming a reuse candidate.
func TestLookupRejectsMissingStorageIntent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	tx, _ := idx.BeginScan(ctx, meta(t, 1000))
	if err := tx.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0))
	_ = idx.Close()

	// Tamper: blank out the storage intent (allowed by NOT NULL, but meaningless).
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE file_index SET large_file_policy = '' WHERE path = 'a.go'"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()
	if _, _, err := idx2.Lookup(ctx, relPath(t, "a.go")); err == nil {
		t.Errorf("Lookup accepted a row with an empty storage intent")
	}
}

// TestRebuildsDropsStrayObjects proves an unexpected object (here a trigger that
// would corrupt writes) is detected as a schema mismatch and removed by the
// rebuild, rather than surviving and continuing to affect rows.
func TestRebuildsDropsStrayObjects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	idx := openIndex(t, dir)
	_ = idx.Close()

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER stray AFTER INSERT ON file_index
		BEGIN UPDATE file_index SET kind = 'corrupted'; END;`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	idx2, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("reopen with stray trigger: %v", err)
	}
	defer idx2.Close()

	// The stray trigger must be gone.
	check, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var n int
	if err := check.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'stray'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("stray trigger survived rebuild")
	}

	// And it no longer affects writes: a normal upsert keeps its kind.
	tx, _ := idx2.BeginScan(ctx, meta(t, 1000))
	if err := tx.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx, treeHash(t, "t"), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := idx2.Lookup(ctx, relPath(t, "a.go"))
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Content.IsZero() {
		t.Errorf("row missing content after rebuild")
	}
}

// TestFileRowRequiresExistingScan proves the last_seen_scan_id foreign key bites
// when foreign_keys is on (as the production DSN sets it): a file row pointing at
// a scan that does not exist is rejected, while one pointing at a real scan is
// accepted. A write-path bug or a manual edit cannot leave an orphan row.
func TestFileRowRequiresExistingScan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_ = openIndex(t, dir).Close() // create the schema, then drop the handle

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName)+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	const insFile = `INSERT INTO file_index
	  (path, kind, omitted_fields, blob_present, large_file_policy, traversal_depth, last_seen_scan_id, updated_at)
	  VALUES (?, 'file', 0, 0, 'blob', 0, ?, ?)`
	now := time.Unix(1, 0).UTC().Format(time.RFC3339Nano)

	// A file row referencing a non-existent scan is rejected by the foreign key.
	if _, err := raw.ExecContext(ctx, insFile, "ghost.go", "no-such-scan", now); err == nil {
		t.Fatalf("expected foreign-key rejection for an unknown scan id")
	}

	// Positive control: with the referenced scan present, the same insert succeeds.
	if _, err := raw.ExecContext(ctx, `INSERT INTO scans (id, started_at, root, config_hash, trust_mode)
	  VALUES ('real-scan', ?, '/p', 'blake3:x', 'normal')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, insFile, "real.go", "real-scan", now); err != nil {
		t.Fatalf("file row referencing a real scan rejected: %v", err)
	}
}

// TestBeginScanPersistsScanMetadataFields proves the scans row preserves the full
// metadata — the omitted stat fields and the fast-mode weak-signature flag — so the
// persisted history reflects exactly how strong the scan was, not just its trust
// mode.
func TestBeginScanPersistsScanMetadataFields(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openIndex(t, dir)

	m := meta(t, 2000)
	m.TrustMode = config.TrustFast
	m.FastModeWeakSignature = true
	m.OmittedStatFields = worktree.FieldSet(0).With(worktree.FieldDev).With(worktree.FieldIno)

	tx, err := idx.BeginScan(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Upsert(ctx, sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx, treeHash(t, "t"), time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	var omitted, fastWeak int64
	if err := raw.QueryRow(
		"SELECT omitted_stat_fields, fast_mode_weak_signature FROM scans WHERE id = ?", m.ScanID.String(),
	).Scan(&omitted, &fastWeak); err != nil {
		t.Fatal(err)
	}
	if worktree.FieldSet(omitted) != m.OmittedStatFields {
		t.Errorf("omitted_stat_fields = %d, want %d", omitted, m.OmittedStatFields)
	}
	if fastWeak != 1 {
		t.Errorf("fast_mode_weak_signature = %d, want 1", fastWeak)
	}
}

func TestBeginScanRejectsInvalidMetadata(t *testing.T) {
	idx := openIndex(t, t.TempDir())
	// Zero-value metadata has no scan id, no root, no config hash, etc.
	if _, err := idx.BeginScan(context.Background(), worktree.ScanMetadata{}); err == nil {
		t.Errorf("BeginScan accepted zero-value metadata")
	}
}

// TestOpenHandlesPathWithURIMetacharacters proves a project path containing URI
// metacharacters (here '#') does not corrupt the connection string.
func TestOpenHandlesPathWithURIMetacharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "weird #dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := sqliteindex.Open(dir)
	if err != nil {
		t.Fatalf("Open with metacharacter path: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	tx, err := idx.BeginScan(context.Background(), meta(t, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Upsert(context.Background(), sampleEntry(t, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background(), treeHash(t, "t"), time.Unix(1, 0)); err != nil {
		t.Fatalf("commit with metacharacter path: %v", err)
	}
}

func TestInspectHealthy(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()

	if err := sqliteindex.Inspect(dir, false); err != nil {
		t.Fatalf("Inspect healthy = %v, want nil", err)
	}
	if err := sqliteindex.Inspect(dir, true); err != nil {
		t.Fatalf("Inspect strict healthy = %v, want nil", err)
	}
}

func TestInspectMissingIsHealthy(t *testing.T) {
	// A not-yet-created index is built lazily, so its absence is not corruption.
	if err := sqliteindex.Inspect(t.TempDir(), false); err != nil {
		t.Fatalf("Inspect missing = %v, want nil", err)
	}
}

func TestInspectTooNew(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()

	stampVersion(t, dir, 999)
	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrSchemaTooNew) {
		t.Fatalf("Inspect newer = %v, want ErrSchemaTooNew", err)
	}
}

func TestInspectStaleOlderVersion(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()

	stampVersion(t, dir, 0)
	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrSchemaStale) {
		t.Fatalf("Inspect older = %v, want ErrSchemaStale", err)
	}
}

func TestInspectEmptyDatabaseIsHealthy(t *testing.T) {
	// An existing but empty worktree.sqlite — what an interrupted lazy init leaves —
	// is not durable damage: Open would populate it on the next scan. Inspect must
	// treat it as healthy rather than an unreadable index.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, sqliteindex.FileName)
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Force the file into existence as a valid, object-less database.
	if _, err := raw.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if err := sqliteindex.Inspect(dir, false); err != nil {
		t.Fatalf("Inspect empty database = %v, want nil (healthy)", err)
	}
}

func TestInspectTooNewWinsOverShape(t *testing.T) {
	// A newer awa may change the schema and bump the version. The too-new guard must
	// win over the shape check so doctor never offers to rebuild (and --repair never
	// deletes) an index a newer awa owns.
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	// A stray object makes the shape non-current; the version stamp says it is newer.
	if _, err := raw.Exec(`CREATE TABLE stray (x INTEGER);
		UPDATE schema_meta SET version = 999;`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrSchemaTooNew) {
		t.Fatalf("Inspect newer foreign shape = %v, want ErrSchemaTooNew", err)
	}
}

func TestInspectCorruptFile(t *testing.T) {
	// A file that is not a SQLite database (bad header) is corrupt content: accessible
	// but unusable derived state, classified ErrCorrupt (repairable by rebuild).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sqliteindex.FileName), []byte("this is not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrCorrupt) {
		t.Fatalf("Inspect non-database file = %v, want ErrCorrupt", err)
	}
}

func TestInspectUnavailableOnPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()
	dbPath := filepath.Join(dir, sqliteindex.FileName)
	if err := os.Chmod(dbPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o644) })

	// An unreadable index is an access problem, never a content judgment, so it must
	// be ErrUnavailable (non-repairable) — doctor must not delete on a permission error.
	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrUnavailable) {
		t.Fatalf("Inspect unreadable index = %v, want ErrUnavailable", err)
	}
}

func TestInspectStaleStrayObject(t *testing.T) {
	dir := t.TempDir()
	idx := openIndex(t, dir)
	_ = idx.Close()

	// A stray object at the current version is a schema shape mismatch Open rebuilds.
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, sqliteindex.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE stray (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if err := sqliteindex.Inspect(dir, false); !errors.Is(err, sqliteindex.ErrSchemaStale) {
		t.Fatalf("Inspect stray object = %v, want ErrSchemaStale", err)
	}
}
