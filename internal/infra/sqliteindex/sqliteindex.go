// Package sqliteindex implements the worktree.Index port over SQLite.
//
// It owns the worktree index database (.awa/indexes/worktree.sqlite): its
// schema, WAL configuration, and the transactional lifecycle of a scan. The
// index is mutable acceleration state, not the source of truth — it may be
// deleted and is transparently rebuilt on the next scan. All SQL and database
// handles are confined here; the package exposes only domain-typed methods, so
// no row ever escapes as a domain entity.
package sqliteindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"             // registers the pure-Go "sqlite" driver
	sqlitedriver "modernc.org/sqlite"  // for typed result-code classification
	sqlitelib "modernc.org/sqlite/lib" // SQLITE_* result codes

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/worktree"
)

// FileName is the worktree index database file name within the indexes
// directory.
const FileName = "worktree.sqlite"

// OwnedFiles returns the base names of every file the worktree index owns within
// the indexes directory: the database plus its WAL sidecars. A rebuild/invalidate
// must remove the whole group, since deleting only the main database leaves -wal
// and -shm behind to confuse the next open or accumulate as cruft.
func OwnedFiles() []string {
	return []string{FileName, FileName + "-wal", FileName + "-shm"}
}

// ErrSchemaTooNew reports an index stamped with a schema version newer than this
// build supports. It is distinguished from an unreadable/corrupt index because it
// is not safely repairable: rebuilding would silently downgrade state a newer awa
// owns. Doctor reports it without offering a rebuild.
var ErrSchemaTooNew = errors.New("worktree index schema is newer than this awa supports")

// ErrSchemaStale reports an index whose schema is older than, or structurally
// different from, the one this build owns — exactly the conditions Open rebuilds on
// (an older version, a partial schema, or a stray object). It is safely repairable:
// the index is acceleration-only, so a rebuild recreates it from the worktree.
var ErrSchemaStale = errors.New("worktree index schema is out of date and must be rebuilt")

// ErrCorrupt reports an index whose file is a malformed or non-database content
// (SQLITE_CORRUPT / SQLITE_NOTADB). The file is accessible but unusable; it is
// derived state, so it is safely repairable by discarding and rebuilding it.
var ErrCorrupt = errors.New("worktree index is corrupt")

// ErrUnavailable reports an index that could not be accessed — busy/locked (an
// active writer), a permission or cant-open error, or an I/O failure. It is not a
// statement about the index's content, so it is NOT repairable: deleting on a lock
// or permission problem could destroy a healthy index doctor merely could not read.
var ErrUnavailable = errors.New("worktree index is unavailable")

// classifyDBError maps a raw database error to ErrCorrupt or ErrUnavailable using
// SQLite's result code, so doctor can offer a rebuild only for a provably-corrupt
// (and therefore safely-discardable) index and never for a transient access
// problem. A non-SQLite error (e.g. a context or driver failure) is unavailable:
// the conservative, non-destructive classification.
func classifyDBError(err error) error {
	var se *sqlitedriver.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlitelib.SQLITE_CORRUPT, sqlitelib.SQLITE_NOTADB:
			return fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// schemaVersion is the schema this build understands. A database stamped with a
// higher version was written by a newer awa and is refused; a lower version is
// rebuilt, since the index is acceleration-only and safe to recreate.
const schemaVersion = 1

// Index implements the worktree.Index port.
var _ worktree.Index = (*Index)(nil)

// Index is the SQLite-backed worktree index. Construct it with Open (mutable) or
// OpenReadOnly (acceleration only); close it with Close.
type Index struct {
	db         *sql.DB
	walEnabled bool
	// readOnly marks an index opened strictly for hash reuse (OpenReadOnly). It
	// makes two boundaries safe: writes (BeginScan/Invalidate) are refused up front
	// instead of failing deep in SQLite, and Lookup is fail-soft — a read or
	// corrupt-row error degrades to a miss so the caller re-hashes rather than
	// failing the command.
	readOnly bool
}

// errReadOnlyIndex reports a write attempt on a read-only acceleration index. A
// read-only caller runs ReadOnly scans that never persist, so reaching a write is a
// programming error, not a runtime condition.
var errReadOnlyIndex = errors.New("worktree index is read-only")

// Open opens (creating if necessary) the worktree index in indexesDir. It
// creates indexesDir if missing, configures WAL mode where supported, ensures
// the schema is current, and clears any dead incomplete-scan markers left by an
// interrupted run. The database file is created lazily on the first scan — not
// at project init.
func Open(indexesDir string) (*Index, error) {
	if err := os.MkdirAll(indexesDir, paths.DirPerm); err != nil {
		return nil, fmt.Errorf("creating index directory: %w", err)
	}
	dbPath := filepath.Join(indexesDir, FileName)
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("opening worktree index: %w", err)
	}
	// A single connection keeps WAL semantics simple and avoids modernc's
	// per-connection pragma surprises; the index is accessed serially per scan.
	db.SetMaxOpenConns(1)

	idx := &Index{db: db}
	if err := idx.initWithRetry(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// init above (its BEGIN IMMEDIATE and schema DDL) creates the database and its
	// WAL/SHM sidecars with a mode governed by umask, not necessarily owner-private.
	// Narrow them to the same owner-private policy as every other awa-owned file so
	// index evidence does not depend on the parent directory's mode alone.
	privatizeOwnedFiles(indexesDir)
	return idx, nil
}

// privatizeOwnedFiles narrows the worktree index files to paths.FilePerm. It is
// best-effort: a sidecar not present yet is skipped, and a chmod failure is not
// fatal because the index is acceleration-only and lives in an owner-private
// directory — the file mode is defense in depth, not the sole protection.
func privatizeOwnedFiles(indexesDir string) {
	for _, name := range OwnedFiles() {
		_ = os.Chmod(filepath.Join(indexesDir, name), paths.FilePerm)
	}
}

// openBusyWait bounds how long Open retries a transient lock during init. Concurrent
// first-openers contend on the WAL-mode switch and the initial schema read, which
// SQLite reports as BUSY/LOCKED immediately rather than honoring busy_timeout (a
// journal-mode change cannot wait on other connections). The index is
// acceleration-only and init is idempotent, so a bounded retry lets concurrent awa
// commands serialize gracefully instead of one failing spuriously.
const openBusyWait = 5 * time.Second

// initWithRetry runs init, retrying on a transient BUSY/LOCKED with small backoff up
// to openBusyWait, then giving up with the last error (classified ErrUnavailable, not
// corruption). The backoff is bounded and exists only for genuine cross-process
// contention, not as a substitute for real synchronization.
func (i *Index) initWithRetry() error {
	deadline := time.Now().Add(openBusyWait)
	backoff := 5 * time.Millisecond
	for {
		err := i.init()
		if err == nil {
			return nil
		}
		if !isLockedErr(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(backoff)
		if backoff < 100*time.Millisecond {
			backoff *= 2
		}
	}
}

// isLockedErr reports whether err is SQLite's transient BUSY or LOCKED, the
// contention conditions a bounded retry can clear.
func isLockedErr(err error) bool {
	var se *sqlitedriver.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff {
		case sqlitelib.SQLITE_BUSY, sqlitelib.SQLITE_LOCKED:
			return true
		}
	}
	return false
}

// Inspect verifies the worktree index without modifying it, for "awa doctor",
// opening it strictly read-only so it never rebuilds, migrates, or downgrades the
// way Open may. It returns one of four outcomes for the caller to classify:
//
//   - nil — healthy: a not-yet-initialized index (either absent, or an existing but
//     object-less database left by an interrupted lazy init — both built/populated on
//     the next scan), or one whose schema shape and version are exactly current. In
//     strict mode a current index must also pass SQLite's integrity_check.
//   - ErrSchemaTooNew — a readable version stamp newer than this build supports.
//     This is checked before the shape so it always wins: the index belongs to a
//     newer awa and must never be rebuilt or downgraded, so it is not repairable.
//   - ErrSchemaStale — a lower-version, partial, differing-DDL, or stray-object
//     schema, the same conditions Open rebuilds on. It is derived state safely
//     recreatable by a rebuild, so the caller may offer repair.
//   - ErrCorrupt — the file is a malformed/non-database content. It is accessible
//     but unusable derived state, so it too is safely repairable by a rebuild.
//   - ErrUnavailable — the index could not be accessed (busy/locked, permission, or
//     I/O). This is not a content judgment, so it is NOT repairable: doctor must not
//     delete an index it merely could not read.
func Inspect(indexesDir string, strict bool) error {
	dbPath := filepath.Join(indexesDir, FileName)
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// A stat failure on an existing path (e.g. permission) is an access problem,
		// not corruption: report it as unavailable so repair never deletes blindly.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return classifyDBError(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	// A local background context is deliberate here: Inspect issues only a few
	// bounded single-row metadata/pragma queries (schema version, object shape,
	// integrity check). The cancellable, size-proportional work — the worktree
	// walk and per-file index rows — runs on the scan path, whose Index methods
	// (Lookup/BeginScan/Invalidate) all take the root context.
	ctx := context.Background() //nolint:forbidigo // bounded doctor metadata reads; see comment above
	return checkSchema(ctx, db, strict)
}

// readOnlyDSN builds the modernc.org/sqlite connection string for opening dbPath
// strictly read-only: mode=ro guarantees the handle never creates, migrates, or
// writes the database, and unlike dsn it does not set journal_mode (switching modes
// is itself a write). It is shared by Inspect and OpenReadOnly so the read-only
// posture is defined in exactly one place.
//
// It stays mode=ro rather than immutable=1 on purpose: mode=ro reads a consistent
// committed snapshot through SQLite's normal WAL coordination, whereas immutable
// bypasses that coordination and could read a torn concurrent checkpoint as a
// plausible-but-wrong row — which for the acceleration reader would be a false reuse,
// exactly the outcome the fail-soft contract must never allow.
func readOnlyDSN(dbPath string) string {
	u := url.URL{Scheme: "file", Path: dbPath}
	return u.String() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
}

// checkSchema verifies that an already-opened read-only handle carries exactly the
// current schema, returning nil for a healthy or not-yet-initialized index and one
// of the classified sentinels (ErrSchemaTooNew/ErrSchemaStale/ErrCorrupt/
// ErrUnavailable) otherwise. It never writes, so it is safe on a mode=ro handle and
// shared by Inspect (doctor) and OpenReadOnly (acceleration).
func checkSchema(ctx context.Context, db *sql.DB, strict bool) error {
	// Read the stamped version first, so the too-new guard always wins: a readable
	// version newer than this build means a newer awa owns the index, and callers must
	// never rebuild or downgrade it — even if its schema shape also differs. A version
	// not readable in a recognized form (a missing or differently shaped schema_meta)
	// leaves verr set; the shape check below classifies that as rebuildable stale state.
	var v int
	verr := db.QueryRowContext(ctx, "SELECT version FROM schema_meta WHERE id = 1").Scan(&v)
	if verr == nil && v > schemaVersion {
		return fmt.Errorf("%w: version %d (this awa supports %d)", ErrSchemaTooNew, v, schemaVersion)
	}

	// Read the on-disk objects once. A query failure here means the file is not a
	// readable database. An empty database (no user objects) is a not-yet-initialized
	// index — what an interrupted lazy init leaves behind — which Open would populate
	// on the next scan with no data loss, so it is healthy, not damage.
	actual, err := readSchemaObjects(ctx, db)
	if err != nil {
		return classifyDBError(err)
	}
	if len(actual) == 0 {
		return nil
	}

	// Otherwise the index must carry exactly the current schema. Any lower-version,
	// partial, differing-DDL, or stray shape is rebuildable stale state — the same
	// judgment Open makes.
	expected, err := expectedSchemaDDL(ctx)
	if err != nil {
		return fmt.Errorf("reading worktree index schema: %w", err)
	}
	if !schemaObjectsCurrent(actual, expected) {
		return ErrSchemaStale
	}

	// The shape is current, so schema_meta(id, version) exists and verr should be nil:
	// a missing row or an older version is an inconsistency Open rebuilds; any other
	// version-read error despite the current shape is a real read failure.
	switch {
	case verr == nil:
		if v < schemaVersion {
			return ErrSchemaStale
		}
	case errors.Is(verr, sql.ErrNoRows):
		return ErrSchemaStale
	default:
		return classifyDBError(verr)
	}

	if strict {
		var result string
		if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
			return classifyDBError(err)
		}
		if result != "ok" {
			return fmt.Errorf("%w: integrity check: %s", ErrCorrupt, result)
		}
	}
	return nil
}

// OpenReadOnly opens an existing worktree index strictly read-only, to accelerate a
// scan by reusing committed content hashes without taking the run presence lock.
// Unlike Open it never creates, migrates, rebuilds, switches journal mode, clears
// scan markers, or privatizes files: mode=ro guarantees no write, and the schema is
// only verified, never repaired. It is acceleration only, so every open failure —
// absent or unreadable file, locked/busy, WAL/read error, stale or too-new schema,
// corruption — returns a classified error so the caller falls back to re-hashing,
// and once open the returned index's Lookup is fail-soft (a read/corrupt-row error
// degrades to a miss). It must not be used as a source of truth; the filesystem and
// content hashes remain authoritative when the index cannot be trusted.
func OpenReadOnly(ctx context.Context, indexesDir string) (*Index, error) {
	dbPath := filepath.Join(indexesDir, FileName)
	if _, err := os.Stat(dbPath); err != nil {
		// Absent, or an access/permission/IO problem: there is nothing to accelerate.
		// Report unavailable so the caller re-hashes rather than treating a missing or
		// unreadable index as corruption.
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return nil, classifyDBError(err)
	}
	db.SetMaxOpenConns(1)
	if err := checkSchema(ctx, db, false); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Index{db: db, readOnly: true}, nil
}

// init ensures the schema is current and records whether WAL is active. WAL is
// best-effort — enabled where supported — so a filesystem that cannot honor
// it does not fail the scan.
func (i *Index) init() error {
	// One bounded pragma query (journal_mode); see Inspect's note on why a local
	// background context is acceptable for these fixed metadata reads while the
	// size-proportional scan work carries the root context.
	ctx := context.Background() //nolint:forbidigo // bounded open-time pragma read; Open has no user context to thread
	var mode string
	if err := i.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("reading journal mode: %w", err)
	}
	i.walEnabled = mode == "wal"

	// Serialize schema setup across processes. Several awa commands may open the
	// index at once; without a single writer the schema can be observed half-built
	// and trigger a spurious, destructive rebuild. A BEGIN IMMEDIATE takes the
	// database write lock up front (honoring busy_timeout, unlike the lazy lock a
	// deferred transaction would upgrade to), so concurrent openers wait and then see
	// a complete schema. defer_foreign_keys keeps a legitimate rebuild's drops safe
	// regardless of table order, checking constraints only at commit.
	conn, err := i.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("opening index connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("locking index for init: %w", err)
	}
	// Roll back on any non-committed exit — an error or a panic in initLocked —
	// before the connection returns to the pool, so a half-done init never leaves a
	// dangling transaction the next BEGIN IMMEDIATE would trip over. This defer runs
	// before the conn.Close above (LIFO), so the rollback lands while the connection
	// is still usable.
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := i.initLocked(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("committing index init: %w", err)
	}
	committed = true
	return nil
}

// initLocked performs schema setup and incomplete-scan cleanup on conn while the
// caller holds the immediate write lock.
func (i *Index) initLocked(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("deferring foreign keys for init: %w", err)
	}
	if err := ensureSchema(ctx, conn); err != nil {
		return err
	}
	// Incomplete scans carry no trusted state: their file rows never committed.
	// Clear the markers so they are never mistaken for usable scans.
	if _, err := conn.ExecContext(ctx, "DELETE FROM scans WHERE completed_at IS NULL"); err != nil {
		return fmt.Errorf("clearing incomplete scans: %w", err)
	}
	return nil
}

// querier is the slice of database access the schema setup needs, satisfied by
// *sql.DB, *sql.Tx, and *sql.Conn alike, so the same schema logic runs on a plain
// handle (Inspect's read-only probe) or inside the immediate-locked connection
// (init), without duplicating it.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ensureSchema brings the database to the current schema, enforcing the version
// contract. A schema_meta table of an unrecognized shape (an older awa, or
// corruption) is rebuilt before anything reads it, so a structural mismatch never
// surfaces as a raw SQL error. A newer-than-known version is refused loudly; an
// older one is rebuilt, which is safe because the index is acceleration-only. It
// runs on q (the immediate-locked connection during init), so concurrent openers
// cannot observe a half-built schema.
func ensureSchema(ctx context.Context, q querier) error {
	current, err := schemaMatches(ctx, q)
	if err != nil {
		return err
	}
	if !current {
		return rebuild(ctx, q)
	}
	if _, err := q.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("initializing worktree index schema: %w", err)
	}

	var v int
	err = q.QueryRowContext(ctx, "SELECT version FROM schema_meta WHERE id = 1").Scan(&v)
	if err == sql.ErrNoRows {
		// The metadata row is missing despite a current shape: rebuild to a known
		// state rather than guess.
		return rebuild(ctx, q)
	}
	if err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("%w: version %d (this awa supports %d)", ErrSchemaTooNew, v, schemaVersion)
	default:
		return rebuild(ctx, q)
	}
}

// schemaMatches reports whether db's user objects are exactly the current schema:
// the same set of objects schemaSQL produces, each with the exact canonical DDL
// (derived from schemaSQL itself, so the two cannot drift). An empty database is
// fresh and will be created in full; anything else — an older form, a partial schema
// (e.g. file_index present but schema_meta missing), or an unexpected stray object —
// is not current and is rebuilt, which is safe for an acceleration-only cache (a
// partial schema is never silently topped up, which would leave rows with no version
// lineage). It is the shared core of init's schema check (on the immediate-locked
// connection) and Inspect (the read-only doctor probe), so both judge "is this the
// schema this build owns?" by the same rule.
func schemaMatches(ctx context.Context, db querier) (bool, error) {
	expected, err := expectedSchemaDDL(ctx)
	if err != nil {
		return false, err
	}
	actual, err := readSchemaObjects(ctx, db)
	if err != nil {
		return false, err
	}
	return schemaObjectsCurrent(actual, expected), nil
}

// schemaObjectsCurrent reports whether actual is the complete current schema. An
// empty set is fresh — schemaSQL creates everything — and otherwise the object set
// must match expected exactly (no missing, extra, or differing objects). It is a
// pure comparison so callers that have already read the objects (Inspect) reuse the
// same rule without a second query.
func schemaObjectsCurrent(actual, expected map[string]string) bool {
	if len(actual) == 0 {
		return true // fresh database; schemaSQL creates everything
	}
	if len(actual) != len(expected) {
		return false
	}
	for name, ddl := range actual {
		want, ok := expected[name]
		if !ok || ddl != want {
			return false
		}
	}
	return true
}

// expectedDDL caches the canonical DDL of every owned object, computed once by
// applying schemaSQL to a throwaway in-memory database and reading back the exact
// CREATE statements SQLite persists. Deriving the expectation from schemaSQL means
// there is a single source of truth — the expected DDL cannot drift from what is
// actually created.
var (
	expectedDDLOnce sync.Once
	expectedDDLMap  map[string]string
	expectedDDLErr  error
)

func expectedSchemaDDL(ctx context.Context) (map[string]string, error) {
	expectedDDLOnce.Do(func() {
		mem, err := sql.Open("sqlite", "file::memory:")
		if err != nil {
			expectedDDLErr = fmt.Errorf("computing expected schema: %w", err)
			return
		}
		defer func() { _ = mem.Close() }()
		if _, err := mem.ExecContext(ctx, schemaSQL); err != nil {
			expectedDDLErr = fmt.Errorf("computing expected schema: %w", err)
			return
		}
		expectedDDLMap, expectedDDLErr = readSchemaObjects(ctx, mem)
	})
	return expectedDDLMap, expectedDDLErr
}

// readSchemaObjects returns the user objects (tables, indexes, views, triggers —
// everything but SQLite's internal sqlite_% objects) of db, keyed by name with
// their normalized CREATE statement.
func readSchemaObjects(ctx context.Context, db querier) (map[string]string, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT name, sql FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, fmt.Errorf("reading schema objects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, fmt.Errorf("reading schema objects: %w", err)
		}
		out[name] = normalizeDDL(ddl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading schema objects: %w", err)
	}
	return out, nil
}

// normalizeDDL collapses all runs of whitespace to a single space so DDL stored
// on multiple lines compares equal to the same statement written on one.
func normalizeDDL(ddl string) string {
	return strings.Join(strings.Fields(ddl), " ")
}

// rebuild drops every user object in the database — not just the known tables, so
// a stray trigger, view, or index cannot survive — then recreates the schema and
// stamps the current version. It is used whenever the on-disk schema is older,
// newer-but-downgraded, or structurally unrecognized; the index is
// acceleration-only, so a full reset is always safe.
func rebuild(ctx context.Context, q querier) error {
	if err := dropAllObjects(ctx, q); err != nil {
		return fmt.Errorf("rebuilding worktree index: %w", err)
	}
	if _, err := q.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("recreating worktree index schema: %w", err)
	}
	if _, err := q.ExecContext(ctx, "UPDATE schema_meta SET version = ? WHERE id = 1", schemaVersion); err != nil {
		return fmt.Errorf("stamping schema version: %w", err)
	}
	return nil
}

// dropOrder ranks object types so dependents are dropped before their
// dependencies: triggers and views first, then indexes, then tables. A lower
// number is dropped earlier.
var dropOrder = map[string]int{"trigger": 0, "view": 1, "index": 2, "table": 3}

// dropAllObjects drops every user object in the database (everything but SQLite's
// internal sqlite_% objects), so nothing the current schema does not define can
// survive a rebuild.
func dropAllObjects(ctx context.Context, q querier) error {
	rows, err := q.QueryContext(ctx,
		"SELECT type, name FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		return fmt.Errorf("listing schema objects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type object struct{ typ, name string }
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name); err != nil {
			return fmt.Errorf("listing schema objects: %w", err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("listing schema objects: %w", err)
	}
	_ = rows.Close()

	sort.SliceStable(objects, func(a, b int) bool {
		return dropOrder[objects[a].typ] < dropOrder[objects[b].typ]
	})
	for _, o := range objects {
		stmt := fmt.Sprintf("DROP %s IF EXISTS %s", strings.ToUpper(o.typ), quoteIdent(o.name))
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("dropping %s %s: %w", o.typ, o.name, err)
		}
	}
	return nil
}

// quoteIdent quotes a SQLite identifier, doubling any embedded quote.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// boolToInt maps a bool to SQLite's 0/1 integer encoding.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// dsn builds the modernc.org/sqlite connection string for dbPath. The path is
// carried in a file URI so connection pragmas can be set, and is percent-encoded
// via net/url so a path containing URI metacharacters (?, #, %) cannot corrupt
// the DSN. The pragmas are appended as the raw query verbatim.
func dsn(dbPath string) string {
	u := url.URL{Scheme: "file", Path: dbPath}
	return u.String() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
}

// WALEnabled reports whether the database is running in WAL mode.
func (i *Index) WALEnabled() bool { return i.walEnabled }

// Close releases the database handle.
func (i *Index) Close() error { return i.db.Close() }

// Invalidate removes any indexed state for path, so the next scan re-hashes it
// rather than reusing a mapping a later step proved wrong. The index is
// acceleration state with no children referencing a file row, so the delete is
// always safe and idempotent (removing an absent path is not an error).
func (i *Index) Invalidate(ctx context.Context, path worktree.RelPath) error {
	if i.readOnly {
		return errReadOnlyIndex
	}
	if _, err := i.db.ExecContext(ctx, "DELETE FROM file_index WHERE path = ?", path.String()); err != nil {
		return fmt.Errorf("invalidating index entry %q: %w", path, err)
	}
	return nil
}

// Lookup returns the last indexed state for path, if present. On the mutable index
// a read or corrupt-row error is surfaced so real corruption is caught during
// authoritative maintenance. On a read-only acceleration index the same error is
// fail-soft: it degrades to a miss so the scanner re-hashes this path from the
// authoritative filesystem, because a lost acceleration must never fail the command.
func (i *Index) Lookup(ctx context.Context, path worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	entry, found, err := i.lookupRow(ctx, path)
	if err != nil && i.readOnly {
		// A cancelled/expired context is a stop signal, not an acceleration failure:
		// surface it so the scan aborts instead of pointlessly re-hashing this path from
		// disk. Genuine index read/WAL/corrupt-row errors still degrade to a miss.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return worktree.IndexedEntry{}, false, ctxErr
		}
		return worktree.IndexedEntry{}, false, nil
	}
	return entry, found, err
}

// lookupRow reads and validates the indexed row for path, returning an error on any
// read or corruption. Lookup applies the read-only fail-soft policy on top of it.
func (i *Index) lookupRow(ctx context.Context, path worktree.RelPath) (worktree.IndexedEntry, bool, error) {
	const q = `SELECT kind, size, mtime_ns, ctime_ns, mode, dev, ino, nlink, omitted_fields, content_hash, large_file_policy
	           FROM file_index WHERE path = ?`
	var (
		kind               string
		size, mtime, ctime int64
		mode               int64
		dev, ino, nlink    int64
		omitted            int64
		contentHash        sql.NullString
		storage            sql.NullString
	)
	err := i.db.QueryRowContext(ctx, q, path.String()).Scan(
		&kind, &size, &mtime, &ctime, &mode, &dev, &ino, &nlink, &omitted, &contentHash, &storage,
	)
	if err == sql.ErrNoRows {
		return worktree.IndexedEntry{}, false, nil
	}
	if err != nil {
		return worktree.IndexedEntry{}, false, fmt.Errorf("looking up %s: %w", path, err)
	}

	// Parse the persisted kind at the boundary; an unknown token is corruption.
	fileKind, err := worktree.ParseFileKind(kind)
	if err != nil {
		return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt kind for %s: %w", path, err)
	}
	omittedFields := worktree.FieldSet(omitted)
	if !omittedFields.Valid() {
		return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt omitted stat fields %d for %s", omitted, path)
	}
	entry := worktree.IndexedEntry{
		Stat: worktree.StatSignature{
			Size:    size,
			MtimeNs: mtime,
			CtimeNs: ctime,
			Mode:    uint32(mode),
			Dev:     uint64(dev),
			Ino:     uint64(ino),
			Nlink:   uint64(nlink),
			Omitted: omittedFields,
		},
	}
	if contentHash.Valid && contentHash.String != "" {
		ch, err := hashing.ParseContentHash(contentHash.String)
		if err != nil {
			return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt content hash for %s: %w", path, err)
		}
		entry.Content = ch
	}
	// Storage intent is always written, so a missing one is corruption — and it
	// must be parsed explicitly: the zero ContentStorageIntent is StorageBlob, so a
	// NULL/empty value left unparsed would masquerade as a valid blob and become a
	// reuse candidate.
	if !storage.Valid || storage.String == "" {
		return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt index row for %s: missing storage intent", path)
	}
	si, err := worktree.ParseContentStorageIntent(storage.String)
	if err != nil {
		return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt storage intent for %s: %w", path, err)
	}
	entry.Storage = si
	// A content hash is only meaningful for a regular file stored as a blob or
	// hash-only. A hash on any other kind, or with any other storage intent, can
	// only be corrupt or tampered cache state, so reject it at the read boundary
	// rather than let a stale hash become a false reuse candidate.
	if !entry.Content.IsZero() {
		if fileKind != worktree.KindRegular {
			return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt index row for %s: content hash on a %s", path, fileKind)
		}
		if entry.Storage != worktree.StorageBlob && entry.Storage != worktree.StorageHashOnly {
			return worktree.IndexedEntry{}, false, fmt.Errorf("corrupt index row for %s: content hash with %s storage", path, entry.Storage)
		}
	}
	return entry, true, nil
}

// BeginScan inserts an incomplete scan row and returns a transaction for the
// scan's file updates. The scan is valid only once the transaction commits.
func (i *Index) BeginScan(ctx context.Context, meta worktree.ScanMetadata) (worktree.ScanTx, error) {
	if i.readOnly {
		return nil, errReadOnlyIndex
	}
	if err := meta.Validate(); err != nil {
		return nil, err
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning scan: %w", err)
	}
	const ins = `INSERT INTO scans (id, started_at, completed_at, root, config_hash, trust_mode, omitted_stat_fields, fast_mode_weak_signature, tree_hash)
	             VALUES (?, ?, NULL, ?, ?, ?, ?, ?, NULL)`
	_, err = tx.ExecContext(ctx, ins,
		meta.ScanID.String(),
		meta.StartedAt.UTC().Format(time.RFC3339Nano),
		meta.Root,
		meta.ConfigHash.String(),
		meta.TrustMode.String(),
		int64(meta.OmittedStatFields),
		boolToInt(meta.FastModeWeakSignature),
	)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("recording scan start: %w", err)
	}
	return &scanTx{
		tx:        tx,
		scanID:    meta.ScanID,
		startedAt: meta.StartedAt,
	}, nil
}
