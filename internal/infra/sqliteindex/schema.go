package sqliteindex

import (
	"fmt"

	"awarer/internal/domain/worktree"
)

// schemaSQL is the idempotent worktree index schema. It matches the logical
// tables the worktree index requires and adds a schema_meta version table and
// an index on last_seen_scan_id. The large_file_policy column stores the entry's
// content storage intent (blob / hash-only / skipped / none), which is the
// concrete result of the large-file policy for that file. The
// omitted_fields column records which stat fields the platform could not supply,
// so a round-tripped StatSignature reports those fields as omitted rather than
// silently claiming a zero value is real. symlink_target stores a symlink's (or
// skipped symlink's) raw link target, skipped_os_error stores the OS error
// category of a read-error skip, and traversal_source_path / traversal_depth
// record how a followed node was reached — so the persisted row carries the full
// node, including a skipped input's provenance, rather than just its reason.
//
// The scans row records the full ScanMetadata, not just trust_mode:
// omitted_stat_fields preserves which stat fields the platform could not supply,
// and fast_mode_weak_signature records that the scan used fast mode's weaker
// signature. Without them the persisted history would look stronger than the scan
// actually was — fast mode and omitted fields must stay explicit in the metadata.
//
// last_seen_scan_id carries a FOREIGN KEY to scans(id) (foreign_keys is ON in the
// DSN), so a file row can only ever point at a real scan: a write-path bug, a
// manual edit, or a future migration cannot leave orphan rows. The default NO
// ACTION delete policy is intentional — a scan with file rows referencing it
// cannot be deleted, which matches the lifecycle: only incomplete, row-less scans
// are ever deleted. file_index is created before scans, which SQLite permits for a
// forward foreign-key reference (the constraint is enforced at row insert, by
// which point BeginScan has already inserted the parent scan in the same
// transaction).
//
// Both omitted-fields columns carry a CHECK that rejects bits outside the domain's
// known mask (worktree.KnownFieldsMask, injected below), so a mechanically
// impossible omission set — from a manual edit, a write-path bug, or a migration —
// cannot enter the database. The mask comes from the domain, keeping the stored
// constraint and the model in lockstep. SQLite's ~ is 64-bit signed, so
// (col & ~mask) = 0 holds exactly when no bit outside the low mask bits is set.
var schemaSQL = fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS file_index (
  path TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  size INTEGER,
  mtime_ns INTEGER,
  ctime_ns INTEGER,
  mode INTEGER,
  dev INTEGER,
  ino INTEGER,
  nlink INTEGER,
  omitted_fields INTEGER NOT NULL DEFAULT 0 CHECK ((omitted_fields & ~%[1]d) = 0),
  content_hash TEXT,
  blob_present INTEGER NOT NULL,
  large_file_policy TEXT NOT NULL,
  skipped_reason TEXT,
  skipped_os_error TEXT,
  symlink_target TEXT,
  traversal_source_path TEXT,
  traversal_depth INTEGER NOT NULL DEFAULT 0,
  last_seen_scan_id TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (last_seen_scan_id) REFERENCES scans (id)
);

CREATE TABLE IF NOT EXISTS scans (
  id TEXT PRIMARY KEY,
  started_at TEXT NOT NULL,
  completed_at TEXT,
  root TEXT NOT NULL,
  config_hash TEXT NOT NULL,
  trust_mode TEXT NOT NULL,
  omitted_stat_fields INTEGER NOT NULL DEFAULT 0 CHECK ((omitted_stat_fields & ~%[1]d) = 0),
  fast_mode_weak_signature INTEGER NOT NULL DEFAULT 0 CHECK (fast_mode_weak_signature IN (0, 1)),
  tree_hash TEXT
);

CREATE TABLE IF NOT EXISTS schema_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_file_index_last_seen ON file_index (last_seen_scan_id);

INSERT INTO schema_meta (id, version)
  SELECT 1, 1 WHERE NOT EXISTS (SELECT 1 FROM schema_meta);
`, worktree.KnownFieldsMask)
