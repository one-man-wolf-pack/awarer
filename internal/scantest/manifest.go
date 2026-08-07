package scantest

import (
	"context"
	"sort"

	"awarer/internal/domain/worktree"
)

// CanonicalCursor turns in-memory entries and skipped inputs into one canonical
// manifest cursor the way a production source does: both kinds are wrapped as
// manifest records, sorted by path, and then guarded by worktree.Ordered so a
// duplicate or out-of-order fixture fails on iteration rather than being folded into
// a hash. Production has no slice-shaped adapter; this is the single test seam over
// the worktree primitives, so no test package restates the canonicalization.
//
// The caller may pass the two kinds in any order and in any arrangement — the sort
// establishes canonical order — but a path appearing twice (including once as an
// entry and once as a skip) is a broken fixture and surfaces through the cursor's
// Err, not silently.
func CanonicalCursor(entries []worktree.Entry, skipped []worktree.SkippedInput) worktree.ManifestCursor {
	records := make([]worktree.ManifestRecord, 0, len(entries)+len(skipped))
	for _, e := range entries {
		records = append(records, worktree.EntryRecord(e))
	}
	for _, s := range skipped {
		records = append(records, worktree.SkippedRecord(s))
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path().Less(records[j].Path()) })
	return worktree.Ordered(worktree.NewSliceCursor(records))
}

// CanonicalStream is the re-openable form of CanonicalCursor: every Open yields a
// fresh cursor over the same records, which is what a worktree.ManifestStream port
// promises. It is what a test passes where production passes a durable manifest.
func CanonicalStream(entries []worktree.Entry, skipped []worktree.SkippedInput) worktree.ManifestStream {
	return canonicalStream{entries: entries, skipped: skipped}
}

// canonicalStream holds the fixture slices and re-derives a cursor per Open. It keeps
// no cursor state of its own, so two concurrently open cursors cannot interfere.
type canonicalStream struct {
	entries []worktree.Entry
	skipped []worktree.SkippedInput
}

func (s canonicalStream) Open(context.Context) (worktree.ManifestCursor, error) {
	return CanonicalCursor(s.entries, s.skipped), nil
}
