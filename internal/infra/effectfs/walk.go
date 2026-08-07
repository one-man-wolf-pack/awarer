// Package effectfs is the filesystem adapter for the run cache's effect observer.
//
// It walks the project tree looking for the configured watched generated-output
// directory names (build, dist, target, node_modules, ...) wherever they appear —
// at the root and nested (packages/api/node_modules) — and reduces each to a
// stat-only sub-hash. It never reads file contents and never follows symlinks: a
// symlink is folded as a leaf by its own lstat, so there is no unbounded traversal
// and no root-escape question. Entries within a directory are combined with an
// order-independent 256-bit modular sum, so a directory's signature is deterministic
// regardless of readdir order and needs no in-memory sort or retention.
//
// Observation is fidelity-or-fail-closed: a directory is folded fully or, if it
// exceeds the entry budget, reported over-cap so the observer marks the whole
// observation unavailable (non-reusable). A partial/top-level signature is not used
// for reuse — a deep change that leaves an immediate child's stat unchanged would be
// invisible, which would risk a stale hit; a false miss is preferred over that.
package effectfs

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"

	"awarer/internal/app/effectobserve"
	"awarer/internal/domain/config"
	"awarer/internal/domain/hashing"
	"awarer/internal/infra/worktreefs"
)

// defaultMaxEntriesPerRoot caps the fully-observed traversal of one watched
// directory. A directory larger than this is reported over-cap and fails the
// observation closed, so a run over a very large generated-output directory is
// non-reusable rather than reused on a partial signature. It is generous enough to
// fully observe a typical node_modules while bounding the pathological case.
const defaultMaxEntriesPerRoot = 100_000

// defaultMaxEntriesTotal caps the entries folded across all watched directories in
// one observation, so a monorepo with many nested generated directories stays
// bounded. Exceeding it fails the observation closed the same way a single
// over-cap directory does.
const defaultMaxEntriesTotal = 400_000

// maxWatchedRoots bounds how many matching directories one observation will fold. A
// project with more than this many watched generated-output directories is
// pathological; the observation fails closed rather than folding an unbounded list.
const maxWatchedRoots = 1024

// defaultMaxDiscovery bounds the discovery traversal itself — the number of tree
// entries visited looking for matches, counted regardless of whether they match — so
// finding the watched directories (or proving there are none) can never become an
// unbounded walk of a huge repository. It is generous enough for any realistic source
// tree; beyond it the observation fails closed. It is separate from the per-directory
// entry budgets, which bound observing a matched directory's contents.
const defaultMaxDiscovery = 2_000_000

// readdirChunk is how many directory entries are read per syscall batch, so a single
// directory with a very large number of children is streamed rather than
// materialized in full.
const readdirChunk = 512

// digestLen is the fixed width of the modular accumulator: 256 bits, matching the
// project's BLAKE3-256 content digests.
const digestLen = 32

// errTooManyRoots fails an observation closed when the tree holds more watched
// directories than maxWatchedRoots.
var errTooManyRoots = errors.New("effectfs: too many watched directories to observe")

// errOverBudget fails an observation closed when a watched directory holds more
// entries than the budget allows, so it cannot be observed with full fidelity. The
// observer maps any Discover error to an unavailable (non-reusable) effect state.
var errOverBudget = errors.New("effectfs: watched directory too large to observe")

// errDiscoveryTooLarge fails an observation closed when finding the watched
// directories would visit more tree entries than the discovery budget allows.
var errDiscoveryTooLarge = errors.New("effectfs: project tree too large to discover watched directories")

// Walker implements effectobserve.Walker over the real filesystem.
type Walker struct {
	hasher       hashing.Hasher
	protected    map[string]bool
	maxPerRoot   int
	maxTotal     int
	maxDiscovery int
}

// New builds a Walker over the project hasher with the default entry budgets. The
// protected base names (.git, .awa) are a hard boundary skipped anywhere they
// appear, so observing never pulls VCS or awa state into the signature and never
// descends into awa's own store.
func New(hasher hashing.Hasher) *Walker {
	return NewWithLimits(hasher, defaultMaxEntriesPerRoot, defaultMaxEntriesTotal, defaultMaxDiscovery)
}

// NewWithLimits builds a Walker with explicit per-directory, total, and discovery
// entry budgets. Production uses New (the defaults); this exists for tuning and for
// tests that need to exercise the fail-closed paths without creating millions of
// files.
func NewWithLimits(hasher hashing.Hasher, maxPerRoot, maxTotal, maxDiscovery int) *Walker {
	protected := map[string]bool{}
	for _, p := range config.ProtectedExcludes() {
		protected[p] = true
	}
	return &Walker{
		hasher:       hasher,
		protected:    protected,
		maxPerRoot:   maxPerRoot,
		maxTotal:     maxTotal,
		maxDiscovery: maxDiscovery,
	}
}

// Discover walks the project tree and returns every directory whose basename is one
// of watchNames, at any depth, each observed fully with a bounded no-follow
// traversal. Matched directories are not descended into (their contents are the
// observation); protected names are skipped. The traversal uses an explicit stack and
// chunked ReadDir (never os.ReadDir, which would read and sort a whole directory's
// entries before the budget could stop it), so both the discovery traversal (entries
// visited looking for matches) and each matched directory's observation are bounded in
// memory and time. It fails closed — returning an error the observer maps to an
// unavailable (non-reusable) effect state — when a directory cannot be read safely, a
// matched directory exceeds the entry budget, the tree holds more matching directories
// than the bound, or the discovery traversal exceeds its budget.
func (w *Walker) Discover(project string, watchNames []string) ([]effectobserve.DiscoveredRoot, error) {
	watch := make(map[string]bool, len(watchNames))
	for _, n := range watchNames {
		watch[n] = true
	}
	dc := &discovery{w: w, project: project, watch: watch, remaining: w.maxTotal}
	if err := dc.run(); err != nil {
		return nil, err
	}
	return dc.out, nil
}

// discovery is the mutable state of one Discover traversal: the walker and its budgets,
// the watch set, the running total/discovery counters, and the discovered directories.
type discovery struct {
	w         *Walker
	project   string
	watch     map[string]bool
	remaining int // shared per-observation entry budget left for matched directories
	visited   int // discovery entries seen so far, matched or not
	out       []effectobserve.DiscoveredRoot
}

// run traverses the tree with an explicit stack of directories to visit.
func (dc *discovery) run() error {
	stack := []string{dc.project}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if err := dc.readDir(dir, &stack); err != nil {
			return err
		}
	}
	return nil
}

// readDir streams one directory's children in ReadDir chunks, counting every entry
// against the discovery budget before retaining anything, observing a matched watched
// directory, and pushing a non-matched subdirectory for later descent. Symlinks and
// files are ignored at the discovery level (only real directories match or descend), so
// no symlink is ever followed. A vanished directory or child is skipped; any other read
// error fails closed.
func (dc *discovery) readDir(dir string, stack *[]string) error {
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		ents, rerr := f.ReadDir(readdirChunk)
		for _, de := range ents {
			dc.visited++
			if dc.visited > dc.w.maxDiscovery {
				return errDiscoveryTooLarge
			}
			name := de.Name()
			if dc.w.protected[name] {
				continue
			}
			full := filepath.Join(dir, name)
			info, ierr := os.Lstat(full)
			if ierr != nil {
				if errors.Is(ierr, os.ErrNotExist) {
					continue
				}
				return ierr
			}
			if !info.IsDir() {
				continue
			}
			if dc.watch[name] {
				if err := dc.observe(full); err != nil {
					return err
				}
			} else {
				*stack = append(*stack, full)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// observe folds a matched watched directory into the result, drawing on the shared
// entry budget and failing closed past the matched-directory bound.
func (dc *discovery) observe(full string) error {
	budget := dc.w.maxPerRoot
	if dc.remaining < budget {
		budget = dc.remaining
	}
	subHash, entries, err := dc.w.observeDir(full, budget)
	if err != nil {
		// A budget overrun is the pathological huge-generated-output case (a `target/`
		// with millions of files). Name the offending root so the caller's latency
		// diagnostic can explain the stall, while the observation still fails closed. The
		// exact count is not carried: the walk stopped at the budget, so only the crossed
		// threshold is honest. Other read failures keep their bare error.
		if errors.Is(err, errOverBudget) {
			if rel, rerr := filepath.Rel(dc.project, full); rerr == nil {
				return &effectobserve.OverBudgetError{Path: filepath.ToSlash(rel), Limit: budget}
			}
		}
		return err
	}
	dc.remaining -= int(entries)
	rel, err := filepath.Rel(dc.project, full)
	if err != nil {
		return err
	}
	dc.out = append(dc.out, effectobserve.DiscoveredRoot{
		Path:    filepath.ToSlash(rel),
		SubHash: subHash,
		Entries: entries,
	})
	if len(dc.out) > maxWatchedRoots {
		return errTooManyRoots
	}
	return nil
}

// observeDir reduces one watched directory to a bounded stat signature, or fails with
// errOverBudget when it holds more entries than budget — too large to observe with
// full fidelity, so the observer fails the whole observation closed rather than reuse
// it on a partial signature that could hide an internal change.
func (w *Walker) observeDir(abs string, budget int) (hashing.TreeHash, int64, error) {
	acc, count, exceeded, err := w.walkDeep(abs, budget)
	if err != nil {
		return hashing.TreeHash{}, 0, err
	}
	if exceeded {
		return hashing.TreeHash{}, 0, errOverBudget
	}
	return w.finalize(acc, count), count, nil
}

// walkDeep folds every entry under dir (relative paths) into an order-independent
// accumulator, descending real subdirectories but never following symlinks. It stops
// and reports exceeded=true once the folded count would pass limit. A vanished entry
// (removed mid-walk) is skipped; any other read error fails closed.
func (w *Walker) walkDeep(dir string, limit int) (acc [digestLen]byte, count int64, exceeded bool, err error) {
	// An explicit stack keeps recursion depth off the goroutine stack for very deep
	// trees; each frame is a directory whose children still need folding.
	stack := []string{dir}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		cerr := w.readEntries(cur, func(rel string, info os.FileInfo) bool {
			if count >= int64(limit) {
				exceeded = true
				return false
			}
			w.addLeaf(&acc, rel, info)
			count++
			return true
		}, dir, &stack)
		if cerr != nil {
			return acc, count, exceeded, cerr
		}
		if exceeded {
			return acc, count, true, nil
		}
	}
	return acc, count, false, nil
}

// readEntries streams dir's children in batches, invoking fold for each surviving
// child (relative to base, slash-separated). Real subdirectories are pushed onto
// stack for descent; symlinks and files are folded as leaves. Protected base names
// are skipped. fold returning false stops the walk of this directory (the caller's
// budget was hit; it tracks that in its closure). A child that vanished between
// readdir and lstat is skipped; any other lstat/readdir error fails closed.
func (w *Walker) readEntries(dir string, fold func(rel string, info os.FileInfo) bool, base string, stack *[]string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		ents, rerr := f.ReadDir(readdirChunk)
		for _, de := range ents {
			name := de.Name()
			if w.protected[name] {
				continue
			}
			full := filepath.Join(dir, name)
			info, ierr := os.Lstat(full)
			if ierr != nil {
				if errors.Is(ierr, os.ErrNotExist) {
					continue
				}
				return ierr
			}
			rel := toRel(base, full)
			if !fold(rel, info) {
				return nil
			}
			if info.IsDir() {
				*stack = append(*stack, full)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// addLeaf folds one entry's identity — its relative path and stat signature (size,
// mtime, mode; never contents) — into the order-independent accumulator.
func (w *Walker) addLeaf(acc *[digestLen]byte, rel string, info os.FileInfo) {
	sig := worktreefs.StatSignatureOf(info)
	var b bytes.Buffer
	putString(&b, rel)
	putUint(&b, uint64(sig.Size))
	putUint(&b, uint64(sig.MtimeNs))
	putUint(&b, uint64(sig.Mode))
	leaf := digestBytes(w.hasher.HashBytes(b.Bytes()))
	addMod256(acc, leaf)
}

// finalize reduces the accumulator and entry count into the directory's sub-hash.
// Including the count distinguishes an empty directory from any other.
func (w *Walker) finalize(acc [digestLen]byte, count int64) hashing.TreeHash {
	var b bytes.Buffer
	b.Write(acc[:])
	putUint(&b, uint64(count))
	return w.hasher.HashBytes(b.Bytes())
}

// toRel renders full as a slash-separated path relative to base; the base itself
// renders as ".".
func toRel(base, full string) string {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return filepath.ToSlash(full)
	}
	return path.Clean(filepath.ToSlash(rel))
}

// digestBytes extracts the raw 256-bit digest from a TreeHash, normalized to
// digestLen bytes (left-padded or right-truncated) so the modular combiner has a
// fixed width. The hex payload comes from the value object rather than being
// re-split out of its rendered form, so the prefix syntax stays owned in one place.
func digestBytes(th hashing.TreeHash) [digestLen]byte {
	var out [digestLen]byte
	raw, err := hex.DecodeString(th.Hex())
	if err != nil {
		return out
	}
	if len(raw) >= digestLen {
		copy(out[:], raw[:digestLen])
		return out
	}
	copy(out[digestLen-len(raw):], raw)
	return out
}

// addMod256 adds d into acc as a big-endian 256-bit integer, discarding overflow.
// Addition is commutative, so the order entries are folded in does not matter.
func addMod256(acc *[digestLen]byte, d [digestLen]byte) {
	var carry uint16
	for i := digestLen - 1; i >= 0; i-- {
		sum := uint16(acc[i]) + uint16(d[i]) + carry
		acc[i] = byte(sum)
		carry = sum >> 8
	}
}

func putString(b *bytes.Buffer, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	b.Write(n[:])
	b.WriteString(s)
}

func putUint(b *bytes.Buffer, v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	b.Write(n[:])
}
