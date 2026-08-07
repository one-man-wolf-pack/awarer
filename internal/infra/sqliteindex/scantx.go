package sqliteindex

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
)

// scanTx is the transactional handle for one scan's updates. All file rows and
// the scan's completion commit together, so an interrupted scan leaves neither a
// completed scan row nor partial file rows. startedAt is the scan's start time,
// stamped on each file row to record when it was observed and used to sanity-check
// the completion time at Commit; the completion time itself is supplied to Commit,
// since that is when the scan is actually complete.
type scanTx struct {
	tx        *sql.Tx
	scanID    worktree.ScanID
	startedAt time.Time
}

// Upsert records an entry's current stat and content hash under this scan. The
// entry is re-validated here so a malformed row can never reach the database,
// even if a caller skipped tree construction.
func (s *scanTx) Upsert(ctx context.Context, e worktree.Entry) error {
	if err := e.Validate(); err != nil {
		return err
	}
	var contentHash any
	if !e.Content.IsZero() {
		contentHash = e.Content.String()
	}
	return s.write(ctx, fileRow{
		path:        e.Path.String(),
		kind:        e.Kind.String(),
		size:        e.Stat.Size,
		stat:        e.Stat,
		contentHash: contentHash,
		storage:     e.Storage.String(),
		skipped:     nil,
		osError:     nil,
		symlink:     nullableString(e.Symlink.String()),
		traversal:   e.Traversal,
	})
}

// RecordSkipped records a skipped input with no content hash under this scan. The
// input is re-validated here for the same reason as Upsert, and its full state —
// reason, OS error category, and symlink target — is persisted, not just the
// reason.
func (s *scanTx) RecordSkipped(ctx context.Context, in worktree.SkippedInput) error {
	if err := in.Validate(); err != nil {
		return err
	}
	return s.write(ctx, fileRow{
		path: in.Path.String(),
		kind: in.Kind.String(),
		// A skipped input carries its own size, which may be known even when its
		// stat signature is not — so size comes from the input, not from stat.
		size:        in.Size,
		stat:        in.Stat,
		contentHash: nil,
		storage:     worktree.StorageSkipped.String(),
		skipped:     in.Reason.String(),
		osError:     nullableString(in.OSError),
		symlink:     nullableString(in.Symlink.String()),
		traversal:   in.Traversal,
	})
}

// nullableString returns the string for storage, or nil (SQL NULL) when empty, so
// absent optional fields are stored as NULL rather than empty strings.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Commit marks the scan complete with its tree hash and completion time and
// commits atomically. It rejects a missing tree hash or completion time, and a
// completion time before the scan started, so a structurally impossible scan row
// can never be committed even though BeginScan is where most validation happens.
func (s *scanTx) Commit(ctx context.Context, treeHash hashing.TreeHash, completedAt time.Time) error {
	if treeHash.IsZero() {
		_ = s.tx.Rollback()
		return fmt.Errorf("completing scan: tree hash must not be empty")
	}
	if completedAt.IsZero() {
		_ = s.tx.Rollback()
		return fmt.Errorf("completing scan: completion time must not be zero")
	}
	if completedAt.Before(s.startedAt) {
		_ = s.tx.Rollback()
		return fmt.Errorf("completing scan: completion time %s is before start %s", completedAt, s.startedAt)
	}
	const upd = `UPDATE scans SET completed_at = ?, tree_hash = ? WHERE id = ?`
	if _, err := s.tx.ExecContext(ctx, upd,
		completedAt.UTC().Format(time.RFC3339Nano), treeHash.String(), s.scanID.String(),
	); err != nil {
		_ = s.tx.Rollback()
		return fmt.Errorf("completing scan: %w", err)
	}
	if err := s.tx.Commit(); err != nil {
		return fmt.Errorf("committing scan: %w", err)
	}
	return nil
}

// Rollback abandons the scan.
func (s *scanTx) Rollback() error { return s.tx.Rollback() }

// fileRow is the set of values written for one file_index upsert. size is the
// node's size — taken from the entry's stat for entries, and from the skipped
// input directly for skips (whose stat may be absent) — so the size column is
// never silently zeroed by an absent stat.
type fileRow struct {
	path        string
	kind        string
	size        int64
	stat        worktree.StatSignature
	contentHash any // string or nil
	storage     string
	skipped     any // string or nil
	osError     any // string or nil
	symlink     any // string or nil
	traversal   worktree.TraversalInfo
}

// write performs the upsert. A path seen again in a later scan replaces its prior
// row, so the index always reflects the most recent observation.
func (s *scanTx) write(ctx context.Context, r fileRow) error {
	const q = `
INSERT INTO file_index (
  path, kind, size, mtime_ns, ctime_ns, mode, dev, ino, nlink, omitted_fields,
  content_hash, blob_present, large_file_policy, skipped_reason, skipped_os_error,
  symlink_target, traversal_source_path, traversal_depth, last_seen_scan_id, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(path) DO UPDATE SET
  kind = excluded.kind,
  size = excluded.size,
  mtime_ns = excluded.mtime_ns,
  ctime_ns = excluded.ctime_ns,
  mode = excluded.mode,
  dev = excluded.dev,
  ino = excluded.ino,
  nlink = excluded.nlink,
  omitted_fields = excluded.omitted_fields,
  content_hash = excluded.content_hash,
  blob_present = excluded.blob_present,
  large_file_policy = excluded.large_file_policy,
  skipped_reason = excluded.skipped_reason,
  skipped_os_error = excluded.skipped_os_error,
  symlink_target = excluded.symlink_target,
  traversal_source_path = excluded.traversal_source_path,
  traversal_depth = excluded.traversal_depth,
  last_seen_scan_id = excluded.last_seen_scan_id,
  updated_at = excluded.updated_at`

	skipped := r.skipped
	if skipped == "" {
		skipped = nil
	}
	// dev/ino/nlink are uint64. SQLite's INTEGER is signed 64-bit, so a value with
	// the high bit set is stored as a negative integer. The int64<->uint64 cast is
	// two's-complement on both write and read (see Lookup), so the round-trip is
	// lossless and exact-equality stat comparisons stay correct. We never do signed
	// arithmetic or range queries on these columns, where the sign would matter.
	// TestStatHighBitFieldsRoundTrip pins this behavior.
	var traversalSource any
	if r.traversal.Followed {
		traversalSource = r.traversal.SourcePath.String()
	}
	_, err := s.tx.ExecContext(ctx, q,
		r.path, r.kind, r.size, r.stat.MtimeNs, r.stat.CtimeNs,
		int64(r.stat.Mode), int64(r.stat.Dev), int64(r.stat.Ino), int64(r.stat.Nlink), int64(r.stat.Omitted),
		r.contentHash, r.storage, skipped, r.osError, r.symlink,
		traversalSource, r.traversal.Depth,
		s.scanID.String(), s.startedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("writing file row %s: %w", r.path, err)
	}
	return nil
}
