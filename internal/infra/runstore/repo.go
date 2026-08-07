package runstore

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/runevidence"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/manifestjsonl"
)

const (
	// metaName, stdoutName, and stderrName are the fixed file names inside a run
	// entry directory. beforeManifestName and afterManifestName hold the pre/post
	// observed-state manifests captured by the mutation guard.
	metaName           = "meta.json"
	stdoutName         = "stdout.log"
	stderrName         = "stderr.log"
	beforeManifestName = "before.manifest.jsonl"
	afterManifestName  = "after.manifest.jsonl"

	// metaPerm is the mode for a published, immutable metadata file. Owner-only:
	// run metadata can carry sensitive evidence and must not be world-readable.
	metaPerm = paths.ReadOnlyFilePerm
	// payloadPerm is the mode for a published, immutable output payload. Like the
	// metadata it is read-only and owner-only: a completed entry never rewrites its
	// stdout/stderr, and captured output can contain secrets.
	payloadPerm = paths.ReadOnlyFilePerm
	// pointerPerm is the mode for a key pointer, which is rewritten on refresh. It is
	// owner-only writable like other awa-owned mutable state.
	pointerPerm = paths.FilePerm
)

// Repo implements the run store port.
var _ runcache.Store = (*Repo)(nil)

// Repo stores run entries and key pointers under .awa/runs. root is the project
// root, the trusted anchor for the no-follow traversal so every component down to
// a file is checked for a symlink. hasher computes and verifies output payload
// hashes.
type Repo struct {
	root    string
	runsRel string
	hasher  hashing.Hasher
	// fail is a test-only fault-injection seam: nil in production (a single branch),
	// set only from same-package _test.go. A hook returning a plain error exercises
	// the live rollback path; returning errCrash signals the caller to leave the
	// partial on-disk state behind, simulating a true crash a fresh process recovers
	// from.
	fail func(stage failStage) error
}

// failStage names a fault-injection point inside Commit. The constants are the only
// stages the seam recognizes.
type failStage string

const (
	failAfterPayloadBeforeMeta failStage = "after-payload-before-meta"
	failAfterMetaBeforePointer failStage = "after-meta-before-pointer"
)

// errCrash, returned by a fail hook, means "simulate a crash here": skip rollback so
// on-disk partial state survives for a fresh process to observe.
var errCrash = errors.New("runstore: simulated crash (test seam)")

// checkFail invokes the fail hook (if any) for stage, recording whether it asked for
// a crash-style leftover, and returns the hook's error so the caller aborts through
// the normal error path.
func (r *Repo) checkFail(stage failStage, crashed *bool) error {
	if r.fail == nil {
		return nil
	}
	err := r.fail(stage)
	if errors.Is(err, errCrash) {
		*crashed = true
	}
	return err
}

// New builds a run store over a project's layout. The hasher is the project's
// single hasher: local content identity is fixed BLAKE3, so the same hasher writes
// new payloads and verifies persisted ones.
func New(layout paths.Layout, hasher hashing.Hasher) *Repo {
	rel, err := fsx.RelUnder(layout.Root(), layout.RunsDir())
	if err != nil {
		// The runs directory is always under the project root; a failure here is a
		// wiring bug, not a runtime condition.
		panic(fmt.Sprintf("runstore: runs dir not under root: %v", err))
	}
	return &Repo{root: layout.Root(), runsRel: rel, hasher: hasher}
}

// keyMatches reports whether an entry's recorded key is reproducible from its
// recorded inputs. It is the single invariant both the write path (Commit) and the
// read path (Get) enforce, so the store never publishes an entry it would later
// reject.
func (r *Repo) keyMatches(entry runcache.RunEntry) bool {
	return entry.KeyInput.Compute(r.hasher) == entry.Key
}

func (r *Repo) entriesRel() string { return r.runsRel + "/entries" }
func (r *Repo) keysRel() string    { return r.runsRel + "/keys" }
func (r *Repo) tmpRel() string     { return r.runsRel + "/tmp" }

func (r *Repo) entryShardRel(id runcache.RunID) string {
	return r.entriesRel() + "/" + id.String()[:2]
}

func (r *Repo) entryDirRel(id runcache.RunID) string {
	return r.entryShardRel(id) + "/" + id.String()
}

func (r *Repo) keyShardRel(key runcache.RunKey) string {
	return r.keysRel() + "/" + hashing.Namespace + "/" + key.Hex()[:2]
}

func keyFileName(key runcache.RunKey) string { return key.Hex() + ".json" }

// Lookup resolves a run key to a reusable cache entry through the key pointer. A
// missing pointer is a clean miss; a pointer to a missing or corrupt entry is
// store corruption, never replayed as a partial hit. A pointer is published only
// for a reusable run, but Lookup re-checks the target's reuse state at the read
// boundary too, so a pointer that somehow targets a record-only or mutating run is
// rejected as corruption rather than replayed.
func (r *Repo) Lookup(key runcache.RunKey) (runcache.CacheEntry, bool, error) {
	data, err := r.readFile(r.keyShardRel(key) + "/" + keyFileName(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runcache.CacheEntry{}, false, nil
		}
		return runcache.CacheEntry{}, false, err
	}
	id, err := decodeKeyPointer(data)
	if err != nil {
		return runcache.CacheEntry{}, false, fmt.Errorf("%w: key pointer %s: %v", runcache.ErrCorruptStore, key.Hex(), err)
	}
	entry, err := r.Get(id)
	if err != nil {
		if errors.Is(err, runcache.ErrNotFound) {
			return runcache.CacheEntry{}, false, fmt.Errorf("%w: key %s points to missing run %s", runcache.ErrCorruptStore, key.Hex(), id.Short())
		}
		// A pointer to an entry whose schema this build cannot read is an index into
		// history it cannot interpret, not a hit: treat it as a clean miss so the command
		// re-executes and records a fresh current-schema entry. The unreadable entry is
		// left in place — gc retains and blocks on it until the user resets evidence.
		if errors.Is(err, runcache.ErrIncompatibleEntry) {
			return runcache.CacheEntry{}, false, nil
		}
		return runcache.CacheEntry{}, false, err
	}
	// The pointer is an index, not the source of truth: confirm the entry it
	// resolves to actually carries the requested key. A pointer corrupted or
	// miswritten to another valid run must never replay that run's output and exit
	// code as a hit for this key.
	if entry.Key != key {
		return runcache.CacheEntry{}, false, fmt.Errorf("%w: key %s points to run %s, which has a different key", runcache.ErrCorruptStore, key.Hex(), id.Short())
	}
	// A key pointer must only ever address a reusable run. The write path enforces
	// this (no pointer for a non-reusable run), but a hand-edited or stale pointer to
	// a record-only/mutating/unknown-post-state run would otherwise replay history as
	// a hit; reject it here so the lookup path is mechanically unable to return one.
	if !entry.Reuse.IsReusable() {
		return runcache.CacheEntry{}, false, fmt.Errorf("%w: key %s points to non-reusable run %s (%s)", runcache.ErrCorruptStore, key.Hex(), id.Short(), entry.Reuse.Reason())
	}
	return runcache.NewCacheEntryUnchecked(entry), true, nil
}

// manifestStream builds a verified stream over one of a recorded run's observation
// manifests, tagging structural errors with the run corrupt-store sentinel.
func (r *Repo) manifestStream(id runcache.RunID, ref runcache.ManifestRef) manifestjsonl.Stream {
	return manifestjsonl.Stream{
		Root:     r.root,
		Abs:      filepath.Join(r.root, filepath.FromSlash(r.entryDirRel(id)), ref.File),
		Expected: ref.RecordCount,
		Label:    "run " + id.Short(),
		Corrupt:  func(e error) error { return fmt.Errorf("%w: %v", runcache.ErrCorruptStore, e) },
	}
}

// OpenBeforeManifest returns a verified stream over a run's pre-execution observed
// state. Every stored record carries that observation, so this never reports it as
// unavailable: a record without one does not decode in the first place.
func (r *Repo) OpenBeforeManifest(id runcache.RunID) (worktree.ManifestStream, error) {
	read, err := r.openObservation(id, false)
	return read.Manifest(), err
}

// OpenAfterManifest returns a verified stream over a run's post-execution observed
// state. A run without an after-observation (a post-scan-failed run) yields
// ErrObservationUnavailable.
func (r *Repo) OpenAfterManifest(id runcache.RunID) (worktree.ManifestStream, error) {
	read, err := r.openObservation(id, true)
	return read.Manifest(), err
}

// Observation returns a verified stream over a recorded run's pre/post observed
// state along with that observation's tree hash, its persisted scan-config policy
// identity, and the run's reuse state, for the state-reference resolver. after
// selects the post-run observation. A run without the requested observation yields
// ErrObservationUnavailable, so a run reference fails loud rather than pretending a
// missing observation exists.
func (r *Repo) Observation(id runcache.RunID, after bool) (runcache.RunObservationRead, error) {
	return r.openObservation(id, after)
}

// openObservation is the single resolution path behind the observation accessors:
// load the entry, select the pre/post observation, and return the typed observation
// read — a verified stream plus the observed tree hash, the persisted
// scan_config_hash, and the run's reuse state. Only the after-observation can be
// absent, and only for a post-scan-failed run, so ErrObservationUnavailable belongs
// to that one case; a decoded entry always carries its before-observation. Keeping
// this in one place means the before/after distinction cannot drift between callers.
func (r *Repo) openObservation(id runcache.RunID, after bool) (runcache.RunObservationRead, error) {
	entry, err := r.Get(id)
	if err != nil {
		return runcache.RunObservationRead{}, err
	}
	obs := entry.Before
	if after {
		if entry.After == nil {
			return runcache.RunObservationRead{}, fmt.Errorf("%w: run %s has no after-observation (post-state unknown)", runcache.ErrObservationUnavailable, id.Short())
		}
		obs = entry.After
	}
	stream := r.verifiedManifestStream(id, obs.Manifest)
	return runcache.NewRunObservationRead(stream, obs.Manifest.TreeHash, obs.ScanConfigHash, entry.Reuse)
}

// verifiedManifestStream returns a stream whose drain re-derives the manifest's
// tree hash and checks it against the recorded ref, so a tampered observation
// manifest fails loud rather than feeding a wrong comparison. Building the stream
// cannot fail; the tree-hash check happens when the caller drains it.
func (r *Repo) verifiedManifestStream(id runcache.RunID, ref runcache.ManifestRef) worktree.ManifestStream {
	return &verifiedRunManifest{stream: r.manifestStream(id, ref), expected: ref.TreeHash, hasher: r.hasher, id: id, file: ref.File}
}

// Get loads a run entry by id.
func (r *Repo) Get(id runcache.RunID) (runcache.RunEntry, error) {
	data, err := r.readFile(r.entryDirRel(id) + "/" + metaName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runcache.RunEntry{}, fmt.Errorf("%w: %s", runcache.ErrNotFound, id.Short())
		}
		return runcache.RunEntry{}, err
	}
	entry, err := decodeMeta(data)
	if err != nil {
		// An incompatible entry is intact and self-describing; this build simply has no
		// reader for its schema. Propagate the incompatible sentinel distinctly so lookup
		// treats it as a clean miss and doctor/gc classify it apart from damage, rather
		// than reporting durable corruption.
		if errors.Is(err, runcache.ErrIncompatibleEntry) {
			// Wrap the decode error itself, not the bare sentinel: it names the persisted and
			// expected schema versions, which is the whole diagnostic value for an operator
			// looking at a stale record.
			return runcache.RunEntry{}, fmt.Errorf("%w: run %s", err, id.Short())
		}
		return runcache.RunEntry{}, fmt.Errorf("%w: run %s: %v", runcache.ErrCorruptStore, id.Short(), err)
	}
	if entry.ID != id {
		return runcache.RunEntry{}, fmt.Errorf("%w: run file %s contains a different id %s", runcache.ErrCorruptStore, id, entry.ID)
	}
	// The recorded key must be reproducible from the recorded key inputs. A
	// hand-edited or corrupt meta.json can keep a plausible key while lying about
	// the command, env, or scope in key_input; recomputing catches that drift so
	// every reader (log, show, rm, lookup) fails loud rather than acting on it.
	if !r.keyMatches(entry) {
		return runcache.RunEntry{}, fmt.Errorf("%w: run %s key does not match its recorded inputs", runcache.ErrCorruptStore, id.Short())
	}
	// The layout fixes each stream's payload file name, so a stream's recorded file
	// must be exactly the one the layout assigns. The domain capture only knows the
	// name is non-empty; this read-path check is where stream identity is known, so
	// hand-edited metadata cannot point stdout at stderr.log (with a matching hash)
	// and have OpenStdout replay the wrong stream.
	if entry.Stdout.File != stdoutName {
		return runcache.RunEntry{}, fmt.Errorf("%w: run %s stdout points at %q, not %s", runcache.ErrCorruptStore, id.Short(), entry.Stdout.File, stdoutName)
	}
	if entry.Stderr.File != stderrName {
		return runcache.RunEntry{}, fmt.Errorf("%w: run %s stderr points at %q, not %s", runcache.ErrCorruptStore, id.Short(), entry.Stderr.File, stderrName)
	}
	// The layout also fixes each observation manifest's file name, so — as with the
	// payload streams — a recorded observation must name exactly the file the layout
	// assigns. Without this, hand-edited metadata could point an observation at another
	// in-entry file (or, joined into a path, at another store file), which the manifest
	// stream would then open. The tree-hash check would still catch a content mismatch,
	// but pinning the name here is the same read-path stream-identity guarantee the
	// stdout/stderr checks give.
	if entry.Before.Manifest.File != beforeManifestName {
		return runcache.RunEntry{}, fmt.Errorf("%w: run %s before-observation points at %q, not %s", runcache.ErrCorruptStore, id.Short(), entry.Before.Manifest.File, beforeManifestName)
	}
	if entry.After != nil && entry.After.Manifest.File != afterManifestName {
		return runcache.RunEntry{}, fmt.Errorf("%w: run %s after-observation points at %q, not %s", runcache.ErrCorruptStore, id.Short(), entry.After.Manifest.File, afterManifestName)
	}
	return entry, nil
}

// Resolve returns the single run id a short prefix identifies, strictly: no match
// is ErrNotFound and more than one is ErrAmbiguousPrefix.
func (r *Repo) Resolve(ctx context.Context, prefix string) (runcache.RunID, error) {
	if err := runcache.ValidateRunIDPrefix(prefix); err != nil {
		return runcache.RunID{}, err
	}
	ids, err := r.listIDs(ctx)
	if err != nil {
		return runcache.RunID{}, err
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return runcache.RunID{}, fmt.Errorf("%w: %s", runcache.ErrNotFound, prefix)
	case 1:
		return runcache.ParseRunID(matches[0])
	default:
		return runcache.RunID{}, fmt.Errorf("%w: %s matches %d runs", runcache.ErrAmbiguousPrefix, prefix, len(matches))
	}
}

// ListRefs returns the id of every stored run from the directory layout, without
// decoding metadata, so a corrupt-metadata entry is still listed (the caller can
// see and delete it). Layout corruption — a valid id in the wrong shard — is still
// reported by listIDs.
func (r *Repo) ListRefs(ctx context.Context) ([]runcache.RunID, error) {
	idStrs, err := r.listIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]runcache.RunID, 0, len(idStrs))
	for _, idStr := range idStrs {
		id, err := runcache.ParseRunID(idStr)
		if err != nil {
			continue // listIDs already validated the name; skip anything unexpected
		}
		out = append(out, id)
	}
	return out, nil
}

// ListRefsNewest returns at most the limit newest run ids, newest first, without
// decoding metadata. Run ids are time-prefixed, so "newest" is the largest id
// strings; the walk keeps only the top limit through a bounded min-heap, so it
// neither sorts every id nor retains or returns more than limit of them. Enumerating
// the entry names is still linear in time in the number of stored runs (the
// content-addressed sharded layout has no separate newest-first index, so the set of
// existing ids must be read to find the newest), but eachEntryName streams the
// directory entries in bounded batches, so memory stays bounded by the heap (limit)
// plus a batch — not the shard's entry count. No run metadata or payload is read. A
// non-positive limit returns nil. A maintained newest-first index that would make the
// time sublinear is deferred future work.
func (r *Repo) ListRefsNewest(ctx context.Context, limit int) ([]runcache.RunID, error) {
	if limit <= 0 {
		return nil, nil
	}
	h := &minStringHeap{}
	if err := r.eachEntryName(ctx, func(name string) error {
		switch {
		case h.Len() < limit:
			heap.Push(h, name)
		case name > (*h)[0]:
			// The heap is full and this id is newer than the oldest kept: evict it.
			(*h)[0] = name
			heap.Fix(h, 0)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// The min-heap drains oldest-first; fill the result from the back so it ends up
	// newest-first. Names were validated by eachEntryName, so a parse failure here
	// would be an impossible state — surface it rather than drop an id.
	out := make([]runcache.RunID, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		id, err := runcache.ParseRunID(heap.Pop(h).(string))
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// CountRefs returns how many runs are stored, folded from the sharded entry layout
// without decoding metadata or retaining an id slice. eachEntryName streams the
// directory entries in bounded batches, so a large store — even one where many ids
// share a single shard — is counted in bounded memory. See the runcache.Store contract.
func (r *Repo) CountRefs(ctx context.Context) (int, error) {
	n := 0
	if err := r.eachEntryName(ctx, func(string) error {
		n++
		return nil
	}); err != nil {
		return 0, err
	}
	return n, nil
}

// IterRefsNewest yields stored run ids newest first, one at a time, in O(1) retained
// memory. Run ids are time-prefixed and sort chronologically, so "newest" is the
// lexically largest name. The sharded content-addressed layout has no newest-first
// index, so each successive id is found by a streamed enumeration pass that selects
// the largest name strictly below the last one yielded (an exclusive descending
// ceiling). No metadata or payload is read, and the full id list is never retained;
// each yielded id costs one pass over the entry names. A latest / latest-matching
// lookup that stops at the first readable or matching entry therefore costs a single
// pass in the common case. yield returning stop=true or a non-nil error ends the
// iteration; the yield error is propagated.
//
// The residual cost is O(passes * names): one enumeration per yielded id. For the
// latest-lookup callers that yield until the first match (usually the newest run)
// this is a single pass; a maintained newest-first index that would make repeated
// yields sublinear is deferred, consistent with ListRefsNewest.
func (r *Repo) IterRefsNewest(ctx context.Context, yield func(runcache.RunID) (bool, error)) error {
	haveCeiling := false
	var ceiling string // exclusive: only names strictly less than this remain eligible
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		best := ""
		found := false
		if err := r.eachEntryName(ctx, func(name string) error {
			if haveCeiling && name >= ceiling {
				return nil
			}
			if !found || name > best {
				best = name
				found = true
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			return nil
		}
		id, err := runcache.ParseRunID(best)
		if err != nil {
			// eachEntryName already validated the name, so an unparseable winner is an
			// impossible state — surface it rather than spin.
			return err
		}
		stop, yerr := yield(id)
		if yerr != nil {
			return yerr
		}
		if stop {
			return nil
		}
		ceiling = best
		haveCeiling = true
	}
}

// minStringHeap is a min-heap of run-id strings: its root is the smallest (oldest)
// id, so a bounded "keep the newest limit" walk can cheaply evict the oldest kept id
// when a newer one arrives.
type minStringHeap []string

func (h minStringHeap) Len() int           { return len(h) }
func (h minStringHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h minStringHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minStringHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *minStringHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// EntryPath returns the absolute on-disk directory of a run entry
// (.awa/runs/entries/<shard>/<id>), the canonical location a diagnostic should
// point at. It does not check existence — callers that already know an entry is
// corrupt use it only to report where the damaged record lives.
func (r *Repo) EntryPath(id runcache.RunID) string {
	return filepath.Join(r.root, filepath.FromSlash(r.entryDirRel(id)))
}

// OpenStdout returns a verified reader over a run's stored stdout.
func (r *Repo) OpenStdout(id runcache.RunID) (io.ReadCloser, error) {
	entry, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	return r.openVerified(id, entry.Stdout)
}

// OutputInspectability reports each captured stream's presence/readability without
// reading payload bytes, for metadata-only run-evidence inspection. It takes a validated
// run id and stats the two canonical payload files (Get enforces that a stored entry's
// captures point only at stdoutName/stderrName, so inspection reads those directly rather
// than trusting a caller-supplied file name). A missing file is a calm typed "missing",
// an unreadable one "unreadable". It does not read metadata at all, so a run GC-removed
// after the caller loaded its record degrades to two "missing" streams (the payload files
// vanish with the entry) rather than a hard not-found error — the honest, non-fatal
// evidence the builder wants. Content integrity stays unverified: no payload bytes are
// read or hashed here, so this must never be described as proving the bytes intact.
func (r *Repo) OutputInspectability(id runcache.RunID) (stdout, stderr runevidence.OutputInspectability, err error) {
	if id.IsZero() {
		return runevidence.OutputInspectability{}, runevidence.OutputInspectability{}, fmt.Errorf("output inspectability requires a run id")
	}
	so, err := r.inspectStream(id, stdoutName)
	if err != nil {
		return runevidence.OutputInspectability{}, runevidence.OutputInspectability{}, err
	}
	se, err := r.inspectStream(id, stderrName)
	if err != nil {
		return runevidence.OutputInspectability{}, runevidence.OutputInspectability{}, err
	}
	return so, se, nil
}

// inspectStream classifies one stored payload's presence without reading its bytes and
// without ever holding a readable descriptor to it: it uses the stat-only StatNoFollowAt
// capability (which returns no io.Reader), so byte-freeness is structural, not a
// convention this function must remember to honor. A missing file is a calm typed
// "missing", any other stat error "unreadable"; a payload whose bytes are unreadable
// (e.g. permission-denied) still stats present, because presence is readability of the
// entry, never a byte-integrity claim.
func (r *Repo) inspectStream(id runcache.RunID, file string) (runevidence.OutputInspectability, error) {
	rel := r.entryDirRel(id) + "/" + file
	switch err := fsx.StatNoFollowAt(r.root, rel); {
	case err == nil:
		return runevidence.NewOutputInspectability(runevidence.PresencePresent)
	case errors.Is(err, os.ErrNotExist):
		return runevidence.NewOutputInspectability(runevidence.PresenceMissing)
	default:
		return runevidence.NewOutputInspectability(runevidence.PresenceUnreadable)
	}
}

// OpenStderr returns a verified reader over a run's stored stderr.
func (r *Repo) OpenStderr(id runcache.RunID) (io.ReadCloser, error) {
	entry, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	return r.openVerified(id, entry.Stderr)
}

// openVerified hashes the stored payload and, on a match, rewinds the very same
// descriptor and returns it, so the bytes the caller replays are exactly the
// bytes that were verified. Hashing one fd and reopening the path would leave a
// window for a concurrent replace to swap in unverified bytes between the check
// and the replay; reusing the fd closes it. The descriptor is closed only on an
// error path — on success the caller owns and closes it.
func (r *Repo) openVerified(id runcache.RunID, c runcache.OutputCapture) (io.ReadCloser, error) {
	rel := r.entryDirRel(id) + "/" + c.File
	f, err := fsx.OpenNoFollowAt(r.root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: run %s payload %s is missing", runcache.ErrCorruptStore, id.Short(), c.File)
		}
		return nil, r.storeCorrupt(rel, err)
	}
	// The payload's size on disk must equal the stored-bytes the metadata records.
	// The hash proves the bytes are intact, but a hand-edited stored_bytes would
	// otherwise survive (correct hash, lying size) and make show/JSON report a false
	// length. Stat the same descriptor that is hashed and replayed below.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, r.storeCorrupt(rel, err)
	}
	if info.Size() != c.StoredBytes {
		_ = f.Close()
		return nil, fmt.Errorf("%w: run %s payload %s is %d bytes, metadata records %d", runcache.ErrCorruptStore, id.Short(), c.File, info.Size(), c.StoredBytes)
	}
	got, err := r.hasher.HashReader(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if got != c.Hash {
		_ = f.Close()
		return nil, fmt.Errorf("%w: run %s payload %s hash mismatch", runcache.ErrCorruptStore, id.Short(), c.File)
	}
	// A head-truncated payload ends with the exact truncation marker the writer
	// appended. The hash and size checks above pass for any self-consistent bytes, so
	// a payload whose tail was rewritten (with a matching hash and size) would still
	// replay a truncated stream with no — or a forged — marker. Verify the suffix is
	// the marker for the recorded omitted-byte count before replay.
	if c.Truncated && c.TruncationPolicy == runcache.TruncationHead {
		if err := verifyTruncationMarker(f, c); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%w: run %s payload %s: %v", runcache.ErrCorruptStore, id.Short(), c.File, err)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, r.storeCorrupt(rel, err)
	}
	return f, nil
}

// verifyTruncationMarker checks that f ends with exactly the truncation marker for
// c.OmittedBytes. f is positioned at end-of-file by the preceding hash read; the
// function leaves the offset undefined, so the caller rewinds before replay.
func verifyTruncationMarker(f *os.File, c runcache.OutputCapture) error {
	marker := truncationMarker(c.OmittedBytes)
	if int64(len(marker)) > c.StoredBytes {
		return fmt.Errorf("payload is %d bytes, shorter than its %d-byte truncation marker", c.StoredBytes, len(marker))
	}
	if _, err := f.Seek(c.StoredBytes-int64(len(marker)), io.SeekStart); err != nil {
		return err
	}
	buf := make([]byte, len(marker))
	if _, err := io.ReadFull(f, buf); err != nil {
		return err
	}
	if string(buf) != marker {
		return fmt.Errorf("truncated payload does not end with its truncation marker")
	}
	return nil
}

// Delete removes a run entry and any key pointer that still references it. It is
// conservative: a pointer that has since been repointed at a newer run (after a
// refresh) is left intact. A run whose metadata is corrupt can still be deleted:
// the key cannot be derived from the bad document, so Delete falls back to a
// content scan of the pointer files and a delete-by-id of the entry directory,
// rather than leaving the corrupt run wedged forever.
func (r *Repo) Delete(id runcache.RunID) error {
	entry, err := r.Get(id)
	if err != nil {
		// Corrupt and incompatible entries do not decode, so they are removed by id alone
		// (pointers-to-id first, then the entry dir) rather than through the key the
		// metadata would carry. This is the by-id removal `run rm <id>` relies on: an
		// entry the user has decided to discard must be deletable even though it will not
		// parse. GC never reaches here for either kind — it retains and blocks instead.
		if errors.Is(err, runcache.ErrCorruptStore) || errors.Is(err, runcache.ErrIncompatibleEntry) {
			return r.deleteCorrupt(id)
		}
		return err
	}
	// Clean up the key pointer before the entry, so removing the entry can never
	// leave a pointer dangling — a dangling pointer turns the next Lookup of this
	// key into store corruption instead of a clean miss.
	ptrFileRel := r.keyShardRel(entry.Key) + "/" + keyFileName(entry.Key)
	data, perr := r.readFile(ptrFileRel)
	switch {
	case perr == nil:
		// The file at this exact key path is this key's pointer. Decide whether to
		// remove it. Undecodable or naming this run → remove. Naming a different run is
		// a genuine refresh only if that run still exists and carries this same key; if
		// it does, leave the pointer intact. Otherwise the pointer cannot resolve to a
		// same-key run, so leaving it would turn a future Lookup of this key into
		// corruption — remove it for a clean miss instead.
		var remove bool
		if ptrID, derr := decodeKeyPointer(data); derr != nil || ptrID == id {
			remove = true
		} else {
			live, err := r.pointerIsLiveRefresh(ptrID, entry.Key)
			if err != nil {
				return err
			}
			remove = !live
		}
		if remove {
			if err := r.removeFile(r.keyShardRel(entry.Key), keyFileName(entry.Key)); err != nil {
				return err
			}
		}
	case errors.Is(perr, os.ErrNotExist):
		// No pointer references this key; nothing to clean up.
	default:
		// The pointer exists but could not be read. Removing the entry now would leave
		// it dangling, so fail loud rather than manufacture store corruption.
		return fmt.Errorf("%w: run %s key pointer %s: %v", runcache.ErrCorruptStore, id.Short(), ptrFileRel, perr)
	}
	return r.removeEntryDir(id)
}

// deleteCorrupt removes a run whose metadata could not be read. Without valid
// metadata the run's key is unknown, so its pointer cannot be located by path;
// instead every key pointer that decodes to this id is removed by scanning. As in
// Delete, pointers are cleared before the entry directory so a crash leaves at most
// a reclaimable orphan, never a dangling pointer.
func (r *Repo) deleteCorrupt(id runcache.RunID) error {
	if err := r.removePointersTo(id); err != nil {
		return err
	}
	return r.removeEntryDir(id)
}

// removePointersTo removes every key pointer whose content decodes to id, walking the
// keys/<namespace>/<shard>/ tree. Undecodable or unreadable pointers are left alone:
// they may belong to other keys, and a content scan cannot attribute them to this id.
func (r *Repo) removePointersTo(id runcache.RunID) error {
	namespaces, err := fsx.ReadDirNoFollow(r.root, r.keysRel())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return r.storeCorrupt(r.keysRel(), err)
	}
	for _, ns := range namespaces {
		if !ns.IsDir() {
			continue
		}
		nsRel := r.keysRel() + "/" + ns.Name()
		shards, err := fsx.ReadDirNoFollow(r.root, nsRel)
		if err != nil {
			return r.storeCorrupt(nsRel, err)
		}
		for _, shard := range shards {
			if !shard.IsDir() {
				continue
			}
			shardRel := nsRel + "/" + shard.Name()
			files, err := fsx.ReadDirNoFollow(r.root, shardRel)
			if err != nil {
				return r.storeCorrupt(shardRel, err)
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				data, rerr := r.readFile(shardRel + "/" + f.Name())
				if rerr != nil {
					continue // unreadable pointer; not attributable to this id
				}
				if ptrID, derr := decodeKeyPointer(data); derr != nil || ptrID != id {
					continue
				}
				if err := r.removeFile(shardRel, f.Name()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// KeyPointerProblem describes one corrupt key pointer found by InspectKeyPointers:
// its absolute path and a human detail. It is a diagnostic value, not a domain
// type — the application maps it to a finding.
type KeyPointerProblem struct {
	Path   string
	Detail string
}

// InspectKeyPointers walks the key pointer tree (.awa/runs/keys) and reports
// anything that would make a Lookup fail. For each pointer it applies the same
// validation Lookup does — undecodable content, a target run that does not exist
// (dangling), or a target whose key differs from the one the pointer's path encodes
// (miswritten or stale) — and it also reports unexpected node types in the tree's
// shape: a non-directory where the identity-namespace or a shard directory belongs,
// or a directory where a pointer file belongs. It does not re-report a pointer to a
// corrupt entry — the entry scan already surfaces that. A traversal failure that
// makes the tree itself unreadable is returned as a corrupt-store error; the
// per-pointer problems are returned in the slice so a diagnostic can list them all.
func (r *Repo) InspectKeyPointers() ([]KeyPointerProblem, error) {
	namespaces, err := fsx.ReadDirNoFollow(r.root, r.keysRel())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, r.storeCorrupt(r.keysRel(), err)
	}
	var problems []KeyPointerProblem
	for _, ns := range namespaces {
		nsRel := r.keysRel() + "/" + ns.Name()
		// The key tree is keys/blake3/<shard>/<hex>.json. A non-directory where the
		// identity-namespace directory belongs is structural corruption that would break
		// a Lookup, so report it rather than skip it.
		if !ns.IsDir() {
			problems = append(problems, r.structuralKeyProblem(nsRel, "expected an identity namespace directory but found a non-directory"))
			continue
		}
		shards, err := fsx.ReadDirNoFollow(r.root, nsRel)
		if err != nil {
			return nil, r.storeCorrupt(nsRel, err)
		}
		for _, shard := range shards {
			shardRel := nsRel + "/" + shard.Name()
			if !shard.IsDir() {
				problems = append(problems, r.structuralKeyProblem(shardRel, "expected a shard directory but found a non-directory"))
				continue
			}
			files, err := fsx.ReadDirNoFollow(r.root, shardRel)
			if err != nil {
				return nil, r.storeCorrupt(shardRel, err)
			}
			for _, f := range files {
				if f.IsDir() {
					problems = append(problems, r.structuralKeyProblem(shardRel+"/"+f.Name(), "expected a key pointer file but found a directory"))
					continue
				}
				if p, ok := r.inspectKeyPointer(ns.Name(), shardRel, f.Name()); ok {
					problems = append(problems, p)
				}
			}
		}
	}
	return problems, nil
}

// structuralKeyProblem builds a problem for an unexpected node type in the key
// pointer tree, with the absolute path of the offending node.
func (r *Repo) structuralKeyProblem(rel, detail string) KeyPointerProblem {
	return KeyPointerProblem{Path: filepath.Join(r.root, filepath.FromSlash(rel)), Detail: detail}
}

// inspectKeyPointer validates a single pointer file. ok reports whether a problem
// was found. The pointer's path encodes the key it stands for — keys/blake3/<shard>/
// <hex>.json — so a valid pointer must sit under the fixed identity namespace and
// resolve to an existing run whose key has that same hex; anything else is a corrupt
// pointer.
//
// The namespace and the key are reported separately on purpose. A pointer filed
// under a namespace this build does not own can carry a hex that matches its target
// perfectly, so collapsing the two into one "different key" message would name a
// cause that is not true and send the reader looking at the wrong thing.
func (r *Repo) inspectKeyPointer(nsName, shardRel, fileName string) (KeyPointerProblem, bool) {
	abs := filepath.Join(r.root, filepath.FromSlash(shardRel), fileName)
	problem := func(detail string) (KeyPointerProblem, bool) {
		return KeyPointerProblem{Path: abs, Detail: detail}, true
	}

	if nsName != hashing.Namespace {
		return problem("key pointer is filed under identity namespace " + strconv.Quote(nsName) + ", not " + strconv.Quote(hashing.Namespace))
	}

	data, err := r.readFile(shardRel + "/" + fileName)
	if err != nil {
		return problem("key pointer is unreadable: " + err.Error())
	}
	id, err := decodeKeyPointer(data)
	if err != nil {
		return problem("key pointer is undecodable: " + err.Error())
	}
	entry, err := r.Get(id)
	if err != nil {
		if errors.Is(err, runcache.ErrNotFound) {
			return problem("key pointer references missing run " + id.Short())
		}
		// A corrupt target entry is already reported by the entry scan; the pointer
		// itself is structurally fine, so do not double-report it here.
		return KeyPointerProblem{}, false
	}
	wantHex := strings.TrimSuffix(fileName, ".json")
	if entry.Key.Hex() != wantHex {
		return problem("key pointer references run " + id.Short() + ", which has a different key")
	}
	// A key pointer must only ever address a reusable run: Lookup rejects (and never
	// replays) a pointer to a record-only, mutating, or unknown-post-state run, so a
	// pointer that targets one is a corrupt index a diagnostic should surface.
	if !entry.Reuse.IsReusable() {
		return problem("key pointer references non-reusable run " + id.Short() + " (" + entry.Reuse.Reason().String() + ")")
	}
	return KeyPointerProblem{}, false
}

// pointerIsLiveRefresh reports whether ptrID names an existing run that carries
// key — i.e. the key's pointer was legitimately repointed by a refresh, so it must
// be left intact. A target that is missing or corrupt, or that carries a different
// key, is not a refresh: the pointer is stale or corrupt and the caller should
// remove it. A non-recoverable read error (neither missing nor corruption) is
// returned so the caller can fail loud rather than guess.
func (r *Repo) pointerIsLiveRefresh(ptrID runcache.RunID, key runcache.RunKey) (bool, error) {
	target, err := r.Get(ptrID)
	if err != nil {
		if errors.Is(err, runcache.ErrNotFound) || errors.Is(err, runcache.ErrCorruptStore) {
			return false, nil
		}
		return false, err
	}
	return target.Key == key, nil
}

// removeEntryDir removes a run's entry directory and its payloads. The removal is
// pinned to the entry's shard directory descriptor and descends only through
// no-follow directory handles, so it cannot be redirected outside .awa by a
// symlink swapped into an ancestor between any check and the delete. An absent
// shard or entry is not an error. It does not touch key pointers.
func (r *Repo) removeEntryDir(id runcache.RunID) error {
	shardRel := r.entryShardRel(id)
	shardDir, err := fsx.OpenDirNoFollow(r.root, shardRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return r.storeCorrupt(shardRel, err)
	}
	defer func() { _ = shardDir.Close() }()
	if err := fsx.RemoveTreeAt(shardDir, id.String()); err != nil {
		return fmt.Errorf("removing run %s: %w", id.Short(), err)
	}
	// Flush the shard so the unlink is durable. Otherwise a crash after a successful
	// delete could resurrect the entry — and during a Delete (pointer removed first)
	// that would leave a live entry with no pointer, or undo a rollback.
	if err := fsx.SyncDir(shardDir); err != nil {
		return fmt.Errorf("syncing shard after removing run %s: %w", id.Short(), err)
	}
	return nil
}

// listIDs returns the id strings of every entry directory. It reads only
// directory entries (no JSON decoding), descending entries/<shard>/<id>.
func (r *Repo) listIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := r.eachEntryName(ctx, func(name string) error {
		ids = append(ids, name)
		return nil
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

// eachEntryName invokes fn for every valid run-entry directory name found under the
// sharded entries layout, in directory-iteration order. It reads only directory
// names — never run metadata or payloads — and validates the shard placement of each
// entry, so a misplaced entry is reported as layout corruption rather than silently
// listed. fn returning an error stops the walk and propagates that error.
func (r *Repo) eachEntryName(ctx context.Context, fn func(name string) error) error {
	entriesRel := r.entriesRel()
	// Both directory levels are streamed in bounded batches: RunID shards are derived
	// from the id's timestamp prefix, so within any active window nearly all runs land
	// in one shard, and reading that shard's entries all at once would be O(runs) memory
	// — defeating the bounded contract CountRefs and IterRefsNewest depend on.
	err := fsx.EachDirEntryNoFollow(r.root, entriesRel, func(shard os.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !shard.IsDir() {
			return nil
		}
		shardRel := entriesRel + "/" + shard.Name()
		serr := fsx.EachDirEntryNoFollow(r.root, shardRel, func(e os.DirEntry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !e.IsDir() {
				return nil
			}
			if _, perr := runcache.ParseRunID(e.Name()); perr != nil {
				return nil
			}
			// An entry must live in the shard named by its id's first two hex chars.
			// A misplaced entry is layout corruption: it would make List see an id that
			// Get (which reads the canonical shard) cannot find, or, if duplicated across
			// shards, make Resolve report a single real run as an ambiguous prefix. Fail
			// loud instead of surfacing that as a misleading not-found/ambiguous result.
			if e.Name()[:2] != shard.Name() {
				return fmt.Errorf("%w: run %s is in shard %s, not its own shard %s", runcache.ErrCorruptStore, e.Name(), shard.Name(), e.Name()[:2])
			}
			return fn(e.Name())
		})
		// storeCorrupt transforms only symlink/not-directory rejections; a caller's fn
		// error or the misplaced-shard corruption above passes through unchanged.
		if serr != nil {
			return r.storeCorrupt(shardRel, serr)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return r.storeCorrupt(entriesRel, err)
	}
	return nil
}

// readFile reads a store-owned file anchored at the project root, refusing to
// follow a symlink at any component or to read a non-regular node.
func (r *Repo) readFile(rel string) ([]byte, error) {
	f, err := fsx.OpenNoFollowAt(r.root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, r.storeCorrupt(rel, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file (%s)", runcache.ErrCorruptStore, rel, info.Mode().Type())
	}
	return io.ReadAll(f)
}

// removeFile unlinks name inside relDir under root through a verified directory
// descriptor, then flushes the directory so the unlink is durable. An absent file
// or directory is not an error. Durability matters for the key pointer: a Delete
// removes the pointer before the entry, so the pointer's unlink must survive a
// crash or the entry could be removed while a stale pointer lingers.
func (r *Repo) removeFile(relDir, name string) error {
	dir, err := fsx.OpenDirNoFollow(r.root, relDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return r.storeCorrupt(relDir, err)
	}
	defer func() { _ = dir.Close() }()
	if err := fsx.RemoveAt(dir, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsx.SyncDir(dir)
}

// storeCorrupt maps a no-follow rejection on a store path to ErrCorruptStore,
// passing any other error through unchanged.
func (r *Repo) storeCorrupt(rel string, err error) error {
	switch {
	case fsx.IsSymlinkOpenRejection(err):
		return fmt.Errorf("%w: %s is reached through a symlink", runcache.ErrCorruptStore, rel)
	case errors.Is(err, fsx.ErrNotDirectory):
		return fmt.Errorf("%w: %s is not a directory", runcache.ErrCorruptStore, rel)
	default:
		return err
	}
}
