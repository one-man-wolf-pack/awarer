package scanner

import (
	"context"
	"fmt"

	"awarer/internal/domain/worktree"
)

// MaterializeEntries drains the scan's manifest stream and returns its regular
// entries in canonical order. It is a test-only materializing adapter — it exists to
// let assertions inspect a whole scan result as a slice — so it lives in an
// export_test file and is never part of the production Result surface, where scale-
// sensitive code consumes the streaming Manifest instead. It panics only on the
// impossible case of the scan being unable to reopen the stream it just produced.
func (r Result) MaterializeEntries() []worktree.Entry {
	var out []worktree.Entry
	r.eachRecord(func(rec worktree.ManifestRecord) {
		if e, ok := rec.Entry(); ok {
			out = append(out, e)
		}
	})
	return out
}

// MaterializeSkipped drains the scan's manifest stream and returns its skipped inputs
// in canonical order. Like MaterializeEntries it is a test-only materializing adapter.
func (r Result) MaterializeSkipped() []worktree.SkippedInput {
	var out []worktree.SkippedInput
	r.eachRecord(func(rec worktree.ManifestRecord) {
		if sk, ok := rec.Skipped(); ok {
			out = append(out, sk)
		}
	})
	return out
}

func (r Result) eachRecord(fn func(worktree.ManifestRecord)) {
	cur, err := r.sorted.Stream().Open(context.Background())
	if err != nil {
		panic(fmt.Sprintf("scanner: reopening scan manifest: %v", err))
	}
	defer func() { _ = cur.Close() }()
	for cur.Next() {
		fn(cur.Record())
	}
	if err := cur.Err(); err != nil {
		panic(fmt.Sprintf("scanner: reading scan manifest: %v", err))
	}
}
