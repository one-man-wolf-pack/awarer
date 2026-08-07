package runstore

import (
	"context"
	"fmt"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
)

// normalizeObservationRecord rewrites one record into the form a run observation is
// allowed to persist: a regular entry that the scanner marked StorageBlob becomes
// StorageHashOnly.
//
// The scanner's storage field is an *intent* — "this content would be stored" — and
// for a checkpoint that intent is honoured by a matching blob publication. A run
// observation publishes no content at all: it records what the worktree looked like
// before and after a command, and nothing more. Persisting the intent verbatim made
// the record claim a content capability the store never had, which a consumer can
// only discover by asking for bytes and being told the blob is missing. Recording
// hash-only says the true thing up front.
//
// Everything else is left exactly as scanned. A symlink keeps
// StorageInlineSymlinkTarget, because its target really is inside the record; a
// skipped input keeps its skip; an already hash-only entry is unchanged.
func normalizeObservationRecord(rec worktree.ManifestRecord) worktree.ManifestRecord {
	e, ok := rec.Entry()
	if !ok {
		return rec
	}
	if e.Kind != worktree.KindRegular || e.Storage != worktree.StorageBlob {
		return rec
	}
	e.Storage = worktree.StorageHashOnly
	return worktree.EntryRecord(e)
}

// hashOnlyCursor applies normalizeObservationRecord to every record it yields. It
// wraps the scan on the way into the observation manifest, so the bytes written and
// the tree hash derived from them come from the same normalized stream and cannot
// describe different records.
//
// Normalization does not move the tree hash: a regular entry's identity is its path,
// its content hash, and its permission bits, and the storage class is deliberately
// not folded in. That is what makes this safe to do at the persistence boundary
// rather than a change of what the observation means.
type hashOnlyCursor struct {
	inner worktree.ManifestCursor
	cur   worktree.ManifestRecord
}

func (c *hashOnlyCursor) Next() bool {
	if !c.inner.Next() {
		return false
	}
	c.cur = normalizeObservationRecord(c.inner.Record())
	return true
}

func (c *hashOnlyCursor) Record() worktree.ManifestRecord { return c.cur }
func (c *hashOnlyCursor) Err() error                      { return c.inner.Err() }
func (c *hashOnlyCursor) Close() error                    { return c.inner.Close() }

// verifiedRunManifest is a re-openable manifest stream over one of a recorded
// run's observation manifests that re-derives the manifest's tree hash on a full
// drain and checks it against the recorded ref. The underlying jsonl stream
// already verifies the record count; this layer adds the tree-hash check so a
// tampered observation manifest (records swapped while keeping the count) fails
// loud rather than feeding a wrong comparison or a false "unchanged" verdict.
type verifiedRunManifest struct {
	stream   worktree.ManifestStream
	expected hashing.TreeHash
	hasher   hashing.Hasher
	id       runcache.RunID
	file     string
}

func (m *verifiedRunManifest) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	cur, err := m.stream.Open(ctx)
	if err != nil {
		return nil, err
	}
	reducer, err := worktree.NewTreeReducer(m.hasher)
	if err != nil {
		_ = cur.Close()
		return nil, err
	}
	return &verifyingCursor{inner: cur, reducer: reducer, expected: m.expected, id: m.id, file: m.file}, nil
}

// verifyingCursor passes a run manifest's records through unchanged while folding
// them into a tree reducer; at a clean full drain it re-checks the derived tree
// hash against the recorded ref. A mismatch is ErrCorruptStore. A partial read does
// not run the check; comparison merges always drain both sides, so changes/diff get
// it for free.
type verifyingCursor struct {
	inner    worktree.ManifestCursor
	reducer  *worktree.TreeReducer
	expected hashing.TreeHash
	id       runcache.RunID
	file     string
	err      error
	done     bool
}

func (c *verifyingCursor) Next() bool {
	if c.done {
		return false
	}
	if !c.inner.Next() {
		c.done = true
		if err := c.inner.Err(); err != nil {
			c.err = err
			return false
		}
		if red := c.reducer.Finish(); red.Hash != c.expected {
			c.err = fmt.Errorf("%w: run %s manifest %s tree hash %s does not match recorded %s", runcache.ErrCorruptStore, c.id.Short(), c.file, red.Hash, c.expected)
		}
		return false
	}
	// A regular entry claiming blob storage cannot be a genuine record written by this
	// build: runstore normalizes every one to hash-only, and it owns no content
	// publication that could back the claim. Refusing it here keeps a forged or
	// hand-edited manifest from reaching a consumer as an ordinary "the blob is
	// missing" answer, which would read as reclaimed content rather than a record
	// asserting a capability the store never had.
	if e, ok := c.inner.Record().Entry(); ok && e.Kind == worktree.KindRegular && e.Storage == worktree.StorageBlob {
		c.err = fmt.Errorf("%w: run %s manifest %s claims blob storage for %s; a run observation publishes no content",
			runcache.ErrCorruptStore, c.id.Short(), c.file, e.Path)
		c.done = true
		return false
	}
	if err := c.reducer.Add(c.inner.Record()); err != nil {
		c.err = fmt.Errorf("%w: run %s manifest %s: %v", runcache.ErrCorruptStore, c.id.Short(), c.file, err)
		c.done = true
		return false
	}
	return true
}

func (c *verifyingCursor) Record() worktree.ManifestRecord { return c.inner.Record() }
func (c *verifyingCursor) Err() error                      { return c.err }
func (c *verifyingCursor) Close() error                    { return c.inner.Close() }
