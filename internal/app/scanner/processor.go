package scanner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"

	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/manifestsort"
)

// maxSkippedSampleBuffer bounds how many skipped-input samples a scan retains for
// SkippedSummary, independent of how many inputs were skipped: the count and taint
// come from the reducer (which sees every record), so the retained samples are a
// bounded diagnostic head, not a full list. It comfortably exceeds every caller's
// requested sample size.
const maxSkippedSampleBuffer = 256

// processor feeds a scan's entries and skipped inputs into a bounded external sorter
// as the walker yields nodes, applying trust-mode reuse and large-file policy per
// node. It retains no full entry/skipped slice: the sorter spills to disk past its
// buffer, so a scan of millions of paths stays memory-bounded.
type processor struct {
	ctx     context.Context
	hasher  hashing.Hasher
	index   worktree.IndexLookup
	cfg     config.Config
	opts    Options
	maxSize int64

	sorter  *manifestsort.Sorter
	omitted worktree.FieldSet
	// samples holds the canonically-smallest skipped inputs (by path), kept sorted and
	// capped at maxSkippedSampleBuffer, so SkippedSummary reports a deterministic
	// canonical head regardless of walk order and without retaining every skipped input.
	// The authoritative count and taint come from the reducer.
	samples []worktree.SkippedSample
	// sources holds the verified content opener the walk built for each blob-intent
	// regular entry the scan's SourceRetention mode selected, keyed by RelPath string.
	// Post-scan reads go through these — applying the same shape, root-escape, and
	// symlink-chain checks the walk used to hash — rather than re-opening by path,
	// which would bypass them. It is created on the first selected entry and stays nil
	// otherwise, so a scan that selects nothing (and a followed-only scan of a worktree
	// with no followed blob entry) retains not even an empty registry.
	sources map[string]func() (io.ReadCloser, error)
}

// addEntry folds one entry into the sorter.
func (p *processor) addEntry(e worktree.Entry) error {
	return p.sorter.Add(worktree.EntryRecord(e))
}

// addSkipped folds one skipped input into the sorter and records it into the bounded
// canonical-head sample.
func (p *processor) addSkipped(s worktree.SkippedInput) error {
	if err := p.sorter.Add(worktree.SkippedRecord(s)); err != nil {
		return err
	}
	p.recordSkippedSample(worktree.SkippedSample{Path: s.Path.String(), Reason: s.Reason.String()})
	return nil
}

// recordSkippedSample inserts a skipped sample into the sorted, bounded samples slice
// so it holds the maxSkippedSampleBuffer canonically-smallest paths. A path that sorts
// beyond a full buffer is dropped; otherwise it is inserted in order and the largest is
// evicted. Paths are unique within a scan, so no de-duplication is needed. The cap
// keeps this O(buffer) memory regardless of how many inputs are skipped.
func (p *processor) recordSkippedSample(sample worktree.SkippedSample) {
	i := sort.Search(len(p.samples), func(i int) bool { return p.samples[i].Path >= sample.Path })
	if len(p.samples) >= maxSkippedSampleBuffer && i >= maxSkippedSampleBuffer {
		return // sorts past the retained head; not among the smallest.
	}
	p.samples = append(p.samples, worktree.SkippedSample{})
	copy(p.samples[i+1:], p.samples[i:])
	p.samples[i] = sample
	if len(p.samples) > maxSkippedSampleBuffer {
		p.samples = p.samples[:maxSkippedSampleBuffer]
	}
}

// visit processes one node. It is the callback handed to the walker.
func (p *processor) visit(n worktree.Node) error {
	p.omitted = p.omitted.Union(n.Stat.Omitted)

	// The walker may have already determined a node is skippable (symlink cycle,
	// root escape, exceeded depth). Record it verbatim.
	if n.Skipped {
		return p.skip(n, n.SkipReason)
	}

	switch n.Kind {
	case worktree.KindDir:
		e, err := worktree.NewDirEntry(n.Path, n.Traversal)
		if err != nil {
			return err
		}
		return p.addEntry(e)

	case worktree.KindSymlink:
		e, err := worktree.NewSymlinkEntry(n.Path, n.Symlink, n.Stat, n.Traversal)
		if err != nil {
			return err
		}
		return p.addEntry(e)

	case worktree.KindSpecial:
		return p.skip(n, worktree.ReasonSpecialFile)

	case worktree.KindRegular:
		return p.visitRegular(n)

	default:
		return fmt.Errorf("scanner: unhandled node kind %v for %s", n.Kind, n.Path)
	}
}

// visitRegular hashes a regular file (or applies the large-file policy), reusing
// an indexed hash when the trust mode permits.
func (p *processor) visitRegular(n worktree.Node) error {
	// A regular node must carry a content opener — it is the only way to hash it if
	// reuse misses. Check it at the boundary, before any policy or reuse decision,
	// so an opener-less regular node fails loudly even when reuse would short-circuit
	// the read, rather than relying on the walker's discipline to set it.
	if n.Open == nil {
		return fmt.Errorf("scanner: regular node %s has no content opener", n.Path)
	}

	large := n.Size() > p.maxSize

	if large && p.cfg.Hashing.LargeFilePolicy == config.LargeFileSkip {
		return p.skip(n, worktree.ReasonLargeFileSkipPolicy)
	}

	storage := worktree.StorageBlob
	if large && p.cfg.Hashing.LargeFilePolicy == config.LargeFileHashOnly {
		storage = worktree.StorageHashOnly
	}

	// An index lookup failure (corruption, I/O, schema) is a hard error: the index
	// is acceleration-only, but a failing index must never be silently downgraded
	// to a cache miss or a skipped input.
	content, reused, err := p.reuseHash(n)
	if err != nil {
		return err
	}
	if !reused {
		// A file read/hash failure, by contrast, is about the worktree input and is
		// eligible for the opt-in skip path.
		content, err = p.readAndHash(n)
		if err != nil {
			// A strict "now" observation refuses to tolerate an input that moved during
			// the scan: a changed node means the snapshot is not consistent, so abort the
			// walk with the opener's typed error (which already names the path and wraps
			// worktree.ErrObservationChanged) rather than record a skip. Checked before the
			// skip path so it wins even under AllowSkippedInputs.
			if p.opts.FailOnObservationChange && errors.Is(err, worktree.ErrObservationChanged) {
				return err
			}
			if p.opts.AllowSkippedInputs {
				// The skip itself carries the taint: recording a ReasonReadError skip is
				// what makes the scan incomplete, and the reducer derives that flag from
				// the same record sequence as the hash. No separate flag to keep in sync.
				return p.skipWithError(n, worktree.ReasonReadError, osErrorCategory(err))
			}
			return fmt.Errorf("reading %s: %w", n.Path, err)
		}
	}

	e, err := worktree.NewRegularEntry(n.Path, content, storage, n.Stat, n.Traversal)
	if err != nil {
		return err
	}
	// Decided here, on the finished record, because both facts the choice needs — the
	// storage intent and how the node was reached — are only settled now. The exact
	// opener the walk validated is kept, never a wrapper or a copy of what it closed
	// over. visitRegular has guaranteed n.Open is non-nil.
	if storage == worktree.StorageBlob && p.retainSource(e) {
		if p.sources == nil {
			p.sources = map[string]func() (io.ReadCloser, error){}
		}
		p.sources[n.Path.String()] = n.Open
	}
	return p.addEntry(e)
}

// retainSource reports whether e's verified content opener must outlive the walk, per
// the scan's SourceRetention mode.
func (p *processor) retainSource(e worktree.Entry) bool {
	switch p.opts.Sources {
	case RetainAllSources:
		return true
	case RetainFollowedSources:
		return e.Traversal.Followed
	default:
		return false
	}
}

// readAndHash opens the file and streams it through the hasher. visitRegular has
// already guaranteed n.Open is non-nil.
func (p *processor) readAndHash(n worktree.Node) (hashing.ContentHash, error) {
	rc, err := n.Open()
	if err != nil {
		return hashing.ContentHash{}, err
	}
	defer func() { _ = rc.Close() }()
	return p.hasher.HashReader(rc)
}

// reuseHash decides whether the indexed hash can stand in for re-reading the
// file. It returns (hash, true, nil) when reuse is permitted, (zero, false, nil)
// when the caller must read and hash, and a non-nil error when the index itself
// failed — which the caller must surface, never treat as a cache miss. Strict
// mode never reuses; normal compares the full stat signature; fast compares a
// weaker one. Options.ForceRehash overrides all of them: the index is not even
// consulted, so the content is read.
func (p *processor) reuseHash(n worktree.Node) (hashing.ContentHash, bool, error) {
	if p.opts.ForceRehash {
		return hashing.ContentHash{}, false, nil
	}
	if p.index == nil || p.cfg.Hashing.TrustMode == config.TrustStrict {
		return hashing.ContentHash{}, false, nil
	}
	indexed, ok, err := p.index.Lookup(p.ctx, n.Path)
	if err != nil {
		return hashing.ContentHash{}, false, fmt.Errorf("index lookup for %s: %w", n.Path, err)
	}
	if !ok || indexed.Content.IsZero() {
		return hashing.ContentHash{}, false, nil
	}
	var unchanged bool
	switch p.cfg.Hashing.TrustMode {
	case config.TrustFast:
		unchanged = n.Stat.EqualFast(indexed.Stat)
	default: // normal
		unchanged = n.Stat.EqualNormal(indexed.Stat)
	}
	if !unchanged {
		return hashing.ContentHash{}, false, nil
	}
	return indexed.Content, true, nil
}

// skip records node n as a skipped input for the given reason.
func (p *processor) skip(n worktree.Node, reason worktree.SkippedReason) error {
	return p.skipWithError(n, reason, "")
}

// skipWithError records a skipped input, attaching an OS error category (used for
// read errors). The skipped input is built through the domain constructor so its
// kind and reason cannot disagree.
func (p *processor) skipWithError(n worktree.Node, reason worktree.SkippedReason, osErr string) error {
	s, err := worktree.NewSkippedInput(n.Path, reason, n.Size(), n.Stat, osErr, n.Symlink, n.Traversal)
	if err != nil {
		return err
	}
	return p.addSkipped(s)
}

// osErrorCategory maps a filesystem error to a stable diagnostic category for a
// skipped read.
func osErrorCategory(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission-denied"
	case errors.Is(err, fs.ErrNotExist):
		return "not-found"
	default:
		return "io-error"
	}
}
