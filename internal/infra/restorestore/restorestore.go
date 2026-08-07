// Package restorestore implements the restore.RecoveryRepository port over the
// filesystem.
//
// It owns the on-disk shape of `.awa/restores`: one immutable directory per
// applied restore holding the pre-restore recovery observation's metadata, its
// captured manifest, and the covered path set it proves absence within. The
// captured file content itself lives in the shared content-addressed blob store,
// so a recovery observation references bytes rather than duplicating them — which
// is also why GC must treat a retained record as a blob-reachability root.
//
// The layout deliberately mirrors the run store rather than the checkpoint store:
// a recovery observation is a system-owned record, not a user-authored review
// landmark. It never enters checkpoint history and never moves `latest`.
//
//	.awa/restores/<operation-id>/meta.json            (small metadata, commit point)
//	.awa/restores/<operation-id>/before.manifest.jsonl (captured entries, canonical order)
//	.awa/restores/<operation-id>/scope.jsonl           (covered paths, canonical order)
//	.awa/restores/tmp/                                 (staging for an in-flight publish)
//
// meta.json is published last and exclusively, so a crashed write that left only
// payloads is invisible to readers and a colliding id fails loudly. Every read is
// a hostile-input boundary: unknown fields, trailing data, an id that disagrees
// with its directory name, a non-regular payload, or a manifest whose records do
// not reduce to the recorded tree hash are all corruption, never half-trusted.
package restorestore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/restore"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/manifestjsonl"
)

const (
	// metaName, manifestName, and scopeName are the fixed file names inside a
	// recovery observation directory.
	metaName     = "meta.json"
	manifestName = "before.manifest.jsonl"
	scopeName    = "scope.jsonl"

	// tmpDirName is the staging area for an in-flight publish. It is a sibling of
	// the records rather than a child of one, so a crashed publish leaves an
	// obviously incomplete artifact under a temp path that gc and doctor already
	// classify by age, never a half-built record that reads as real.
	tmpDirName = "tmp"

	// metaSchemaVersion is the record schema this build understands. A record
	// stamped with a different version is not readable by this build and is
	// classified as corruption rather than silently half-decoded: the project is in
	// formative mode, so there is exactly one current shape and no legacy reader.
	metaSchemaVersion = 1

	// recordPerm is the mode for a published, immutable payload. A recovery
	// observation captures the user's own worktree content identity, so it is
	// owner-only and read-only: it is never rewritten in place.
	recordPerm = paths.ReadOnlyFilePerm

	// maxMetaFileSize bounds the one document a record read materializes whole. The
	// largest legitimate metadata document is dominated by the normalized selection,
	// which comes from argv and is therefore bounded by the OS argument limit (~1 MiB),
	// so this leaves generous room for it while refusing a file whose size is chosen by
	// whatever wrote it. The manifest and scope are streamed and need no such cap.
	maxMetaFileSize = 4 << 20
)

// Repo stores recovery observations under .awa/restores. root is the project
// root, the trusted anchor for the no-follow traversal, so every component down
// to a file is checked for a symlink.
type Repo struct {
	root        string
	restoresRel string
	// hasher is the project's single hasher, used to derive a published manifest's
	// tree hash and to verify it again on read. Local content identity is fixed
	// BLAKE3, so the write and read sides cannot disagree about the primitive.
	hasher hashing.Hasher
	// fail is a test-only fault-injection seam: nil in production (a single
	// branch), set only from same-package _test.go. A hook returning a plain error
	// exercises the live rollback path; returning errCrash signals the caller to
	// leave the partial on-disk state behind, simulating a true crash a fresh
	// process must classify.
	fail func(stage failStage) error
}

// failStage names a fault-injection point inside Publish. The constants are the
// only stages the seam recognizes.
type failStage string

const (
	failAfterPayloadsBeforeMeta failStage = "after-payloads-before-meta"
	failAfterMetaBeforePublish  failStage = "after-meta-before-publish"
)

// errCrash, returned by a fail hook, means "simulate a crash here": skip the
// rollback so on-disk partial state survives for a fresh process to observe.
var errCrash = errors.New("restorestore: simulated crash (test seam)")

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

// New builds a recovery-observation store over a project's layout.
func New(layout paths.Layout, hasher hashing.Hasher) *Repo {
	if hasher == nil {
		panic("restorestore.New: hasher must not be nil")
	}
	rel, err := fsx.RelUnder(layout.Root(), layout.RestoresDir())
	if err != nil {
		// The restores directory is always under the project root; a failure here is
		// a wiring bug, not a runtime condition.
		panic(fmt.Sprintf("restorestore: restores dir not under root: %v", err))
	}
	return &Repo{root: layout.Root(), restoresRel: rel, hasher: hasher}
}

var _ restore.RecoveryRepository = (*Repo)(nil)

func (r *Repo) tmpRel() string                            { return r.restoresRel + "/" + tmpDirName }
func (r *Repo) recordRel(id restore.OperationID) string   { return r.restoresRel + "/" + id.String() }
func (r *Repo) stagingName(id restore.OperationID) string { return ".record-" + id.String() }
func (r *Repo) recordAbs(id restore.OperationID, f string) string {
	return filepath.Join(r.root, filepath.FromSlash(r.recordRel(id)), f)
}

// corrupt tags a structural failure with the recovery-store corruption sentinel.
func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", restore.ErrCorruptRecoveryStore, fmt.Sprintf(format, args...))
}

// Publish writes one immutable recovery observation. The order is payloads,
// then metadata, then an atomic directory rename: the metadata is the commit
// point and the rename is the publication point, so a crash at any step leaves
// either nothing or a complete record — never a directory that reads as a valid
// observation while missing the bytes an undo would need.
func (r *Repo) Publish(ctx context.Context, build restore.RecoveryBuild, manifest worktree.ManifestStream, scope restore.CoveredScopeStream) (restore.RecoveryObservation, error) {
	if build.ID.IsZero() {
		return restore.RecoveryObservation{}, fmt.Errorf("restorestore: publishing a recovery observation requires an operation id")
	}
	if manifest == nil {
		return restore.RecoveryObservation{}, fmt.Errorf("restorestore: publishing recovery observation %s requires a manifest stream", build.ID.Short())
	}
	if scope == nil {
		return restore.RecoveryObservation{}, fmt.Errorf("restorestore: publishing recovery observation %s requires a covered-scope stream", build.ID.Short())
	}
	// An immutable record is never replaced. Check before doing any work so the
	// common failure is cheap; the exclusive rename below still guards the race.
	if _, err := r.Get(build.ID); err == nil {
		return restore.RecoveryObservation{}, fmt.Errorf("%w: %s", restore.ErrRecoveryIDCollision, build.ID)
	} else if !errors.Is(err, restore.ErrRecoveryNotFound) {
		return restore.RecoveryObservation{}, err
	}

	tmpDir, err := fsx.MkdirAllNoFollow(r.root, r.tmpRel(), paths.DirPerm)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	defer func() { _ = tmpDir.Close() }()

	stagingName := r.stagingName(build.ID)
	stagingRel := r.tmpRel() + "/" + stagingName
	committed, crashed := false, false
	defer func() {
		if !committed && !crashed {
			// The staged directory lives directly under the pinned tmp descriptor, so
			// remove it through that descriptor rather than re-resolving its path: a
			// symlink swapped into an ancestor cannot then redirect the delete.
			_ = fsx.RemoveTreeAt(tmpDir, stagingName)
		}
	}()

	stagingDir, err := fsx.MkdirAllNoFollow(r.root, stagingRel, paths.DirPerm)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	defer func() { _ = stagingDir.Close() }()

	manifestRef, err := r.teeManifest(ctx, stagingRel, manifest, r.hasher)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	scopeRef, err := r.writeScope(ctx, stagingRel, scope)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}

	// The record is built through the domain constructor before anything durable
	// claims to be a record, so a shape the reader would reject can never be
	// written in the first place.
	record, err := restore.NewRecoveryObservation(build, manifestRef, scopeRef, true)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}

	if err := r.checkFail(failAfterPayloadsBeforeMeta, &crashed); err != nil {
		return restore.RecoveryObservation{}, err
	}
	metaBytes, err := encodeMeta(record)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	if err := fsx.PublishBytesNoFollow(r.root, stagingRel, metaName, metaBytes, recordPerm); err != nil {
		return restore.RecoveryObservation{}, err
	}
	// Flush the staged directory's contents before it is published: not yet
	// renamed, so a failure here just returns and the deferred cleanup removes it.
	if err := fsx.SyncDir(stagingDir); err != nil {
		return restore.RecoveryObservation{}, err
	}
	if err := r.checkFail(failAfterMetaBeforePublish, &crashed); err != nil {
		return restore.RecoveryObservation{}, err
	}

	restoresDir, err := fsx.MkdirAllNoFollow(r.root, r.restoresRel, paths.DirPerm)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	defer func() { _ = restoresDir.Close() }()
	if err := fsx.RenameAt(tmpDir, stagingName, restoresDir, build.ID.String()); err != nil {
		return restore.RecoveryObservation{}, err
	}
	committed = true
	if err := fsx.SyncDir(restoresDir); err != nil {
		// The record may not be durable, and apply is about to rely on it as the only
		// undo evidence. Roll it back and fail rather than mutate the worktree behind
		// an observation that a crash could erase.
		return restore.RecoveryObservation{}, errors.Join(err, r.Delete(build.ID))
	}
	return record, nil
}

// teeManifest streams the captured manifest into the staged directory and
// derives its ref from the same single pass that writes it, so the durable tree
// hash and record count can never disagree with the bytes.
func (r *Repo) teeManifest(ctx context.Context, relDir string, stream worktree.ManifestStream, hasher hashing.Hasher) (restore.ManifestRef, error) {
	reducer, err := worktree.NewTreeReducer(hasher)
	if err != nil {
		return restore.ManifestRef{}, err
	}
	if err := fsx.PublishStreamNoFollow(r.root, relDir, manifestName, recordPerm, func(w io.Writer) error {
		cur, err := stream.Open(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = cur.Close() }()
		return manifestjsonl.Tee(w, cur, reducer)
	}); err != nil {
		return restore.ManifestRef{}, err
	}
	red := reducer.Finish()
	return restore.ManifestRef{File: manifestName, TreeHash: red.Hash, RecordCount: red.Count}, nil
}

// scopeLine is the persisted form of one covered path. It is JSON rather than a
// bare line because a POSIX filename may contain a newline: JSON escaping keeps
// one path on one line no matter what it contains.
type scopeLine struct {
	Path string `json:"path"`
}

// writeScope streams the covered path set into the staged directory, counting
// the paths in the same pass that writes them so the durable count cannot
// disagree with the payload. Canonical order and uniqueness are enforced while
// streaming, so a planner bug that emitted an out-of-order path fails the write
// rather than producing a record the reader would later call corrupt.
func (r *Repo) writeScope(ctx context.Context, relDir string, scope restore.CoveredScopeStream) (restore.ScopeRef, error) {
	count := 0
	if err := fsx.PublishStreamNoFollow(r.root, relDir, scopeName, recordPerm, func(w io.Writer) error {
		count = 0
		cur, err := scope.Open(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = cur.Close() }()
		bw := bufio.NewWriter(w)
		enc := json.NewEncoder(bw)
		var prev worktree.RelPath
		for cur.Next() {
			p := cur.Path()
			if p.IsZero() {
				return fmt.Errorf("covered scope entry %d has no path", count)
			}
			if count > 0 && !prev.Less(p) {
				return fmt.Errorf("covered scope is not in strictly ascending canonical order at %q", p)
			}
			if err := enc.Encode(scopeLine{Path: p.String()}); err != nil {
				return fmt.Errorf("encoding covered path %q: %w", p, err)
			}
			prev = p
			count++
		}
		if err := cur.Err(); err != nil {
			return err
		}
		return bw.Flush()
	}); err != nil {
		return restore.ScopeRef{}, err
	}
	return restore.ScopeRef{File: scopeName, PathCount: count}, nil
}

// Get returns one record's validated metadata without reading its payloads.
func (r *Repo) Get(id restore.OperationID) (restore.RecoveryObservation, error) {
	data, err := r.readFile(r.recordRel(id) + "/" + metaName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return restore.RecoveryObservation{}, fmt.Errorf("%w: %s", restore.ErrRecoveryNotFound, id.Short())
		}
		return restore.RecoveryObservation{}, err
	}
	record, err := decodeMeta(data)
	if err != nil {
		return restore.RecoveryObservation{}, corrupt("recovery observation %s: %v", id.Short(), err)
	}
	// The directory name is an index into the store; the record inside is the
	// authority. A record found under a name that is not its own id means the store
	// was hand-edited or a rename went wrong, and every later lookup by that name
	// would return the wrong evidence.
	if record.ID() != id {
		return restore.RecoveryObservation{}, corrupt("recovery observation stored as %s declares id %s", id.Short(), record.ID().Short())
	}
	return record, nil
}

// Open returns the record plus verified streams over its payloads.
func (r *Repo) Open(ctx context.Context, id restore.OperationID) (restore.RecoveryRead, error) {
	record, err := r.Get(id)
	if err != nil {
		return restore.RecoveryRead{}, err
	}
	stream := r.verifiedManifestStream(record)
	return restore.NewRecoveryRead(record, stream, &scopeStream{repo: r, record: record})
}

// verifiedManifestStream wraps the raw jsonl stream in the tree-hash check, so a
// tampered manifest (records swapped while the count is preserved) fails loudly
// instead of driving a wrong restore. Building the stream cannot fail; the
// tree-hash check happens when the caller drains it.
func (r *Repo) verifiedManifestStream(record restore.RecoveryObservation) worktree.ManifestStream {
	ref := record.Manifest()
	inner := manifestjsonl.Stream{
		Root:     r.root,
		Abs:      r.recordAbs(record.ID(), ref.File),
		Expected: ref.RecordCount,
		Label:    "restore recovery observation " + record.ID().Short(),
		Corrupt:  func(e error) error { return corrupt("%v", e) },
	}
	return &verifiedManifest{stream: inner, expected: ref.TreeHash, hasher: r.hasher, id: record.ID()}
}

// scopeStream opens cursors over a record's covered-path payload. The payload is
// as large as the restore that produced it, so it is read as a stream and its
// order, uniqueness, and declared count are re-checked while streaming rather
// than trusted: a hand-edited scope file could otherwise widen what an inverse
// restore believes was proved absent.
type scopeStream struct {
	repo   *Repo
	record restore.RecoveryObservation
}

func (s *scopeStream) Open(ctx context.Context) (restore.CoveredScopeCursor, error) {
	r, record := s.repo, s.record
	rel := r.recordRel(record.ID()) + "/" + record.Scope().File
	f, err := fsx.OpenNoFollowAt(r.root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, corrupt("recovery observation %s covered-scope payload is missing", record.ID().Short())
		}
		if fsx.IsSymlinkOpenRejection(err) {
			return nil, corrupt("recovery observation %s covered-scope payload is reached through a symlink", record.ID().Short())
		}
		return nil, corrupt("recovery observation %s covered-scope payload: %v", record.ID().Short(), err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, corrupt("recovery observation %s covered-scope payload is not a regular file (%s)", record.ID().Short(), info.Mode().Type())
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxScopeLineBytes)
	return &scopeCursor{
		ctx:      ctx,
		f:        f,
		sc:       sc,
		id:       record.ID(),
		expected: record.Scope().PathCount,
	}, nil
}

// scopeCursor decodes one covered path per line, failing at the first malformed,
// out-of-order, or duplicate line with its line number, and verifying the path
// count against the record's declared count at clean EOF so a truncated or padded
// payload is never silently accepted.
type scopeCursor struct {
	ctx      context.Context
	f        *os.File
	sc       *bufio.Scanner
	id       restore.OperationID
	expected int

	cur   worktree.RelPath
	prev  worktree.RelPath
	line  int
	count int
	err   error
	done  bool
}

func (c *scopeCursor) Next() bool {
	if c.done {
		return false
	}
	if c.count%scopeCancelCheckInterval == 0 {
		if err := c.ctx.Err(); err != nil {
			c.err, c.done = err, true
			return false
		}
	}
	if !c.sc.Scan() {
		c.done = true
		if err := c.sc.Err(); err != nil {
			c.err = err
			return false
		}
		if c.count != c.expected {
			c.err = corrupt("recovery observation %s covered-scope payload holds %d path(s), record declares %d", c.id.Short(), c.count, c.expected)
		}
		return false
	}
	c.line++
	raw := c.sc.Bytes()
	if len(strings.TrimSpace(string(raw))) == 0 {
		c.err, c.done = corrupt("recovery observation %s covered-scope line %d is blank", c.id.Short(), c.line), true
		return false
	}
	p, err := decodeScopeLine(raw)
	if err != nil {
		c.err, c.done = corrupt("recovery observation %s covered-scope line %d: %v", c.id.Short(), c.line, err), true
		return false
	}
	if c.count > 0 && !c.prev.Less(p) {
		c.err, c.done = corrupt("recovery observation %s covered scope is not in strictly ascending canonical order at %q", c.id.Short(), p), true
		return false
	}
	c.cur, c.prev = p, p
	c.count++
	return true
}

func (c *scopeCursor) Path() worktree.RelPath { return c.cur }
func (c *scopeCursor) Err() error             { return c.err }
func (c *scopeCursor) Close() error           { return c.f.Close() }

const (
	// maxScopeLineBytes bounds one covered-path line. A path cannot legitimately
	// approach this, so a longer line is a corrupt or hostile payload, not a
	// reason to grow a buffer without limit.
	maxScopeLineBytes = 1 << 20
	// scopeCancelCheckInterval is how often a long scope read checks cancellation.
	scopeCancelCheckInterval = 512
)

// decodeScopeLine parses one covered-path line strictly: unknown fields
// rejected, exactly one object, no trailing data, and the path rebuilt through
// the domain constructor so a hostile absolute or escaping path cannot enter.
func decodeScopeLine(data []byte) (worktree.RelPath, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var line scopeLine
	if err := dec.Decode(&line); err != nil {
		return worktree.RelPath{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return worktree.RelPath{}, fmt.Errorf("covered-scope line has trailing data")
	}
	return worktree.ParseRelPath(line.Path)
}

// ResolvePrefix returns the single id a short prefix identifies.
func (r *Repo) ResolvePrefix(ctx context.Context, prefix string) (restore.OperationID, error) {
	if err := restore.ValidateOperationIDPrefix(prefix); err != nil {
		return restore.OperationID{}, err
	}
	ids, err := r.listIDs(ctx)
	if err != nil {
		return restore.OperationID{}, err
	}
	var matches []restore.OperationID
	for _, id := range ids {
		if strings.HasPrefix(id.String(), prefix) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return restore.OperationID{}, fmt.Errorf("%w: no recovery observation with id prefix %q", restore.ErrRecoveryNotFound, prefix)
	case 1:
		return matches[0], nil
	default:
		return restore.OperationID{}, fmt.Errorf("%w: %q matches %d recovery observations", restore.ErrRecoveryAmbiguousPrefix, prefix, len(matches))
	}
}

// List returns every stored record's health finding, newest first. It never
// collapses on a single unreadable record: GC needs to know that a record exists
// but cannot be read, because that makes blob reachability incomplete and the
// sweep must fail closed.
func (r *Repo) List(ctx context.Context) ([]restore.RecoveryFinding, error) {
	entries, err := fsx.ReadDirNoFollow(r.root, r.restoresRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// An absent store is a clean, empty store: no restore has ever been applied.
			return nil, nil
		}
		return nil, corrupt("restore store directory: %v", err)
	}
	var out []restore.RecoveryFinding
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		if name == tmpDirName {
			// Staging lives here by design and is classified as temp, not as a record.
			continue
		}
		id, perr := restore.ParseOperationID(name)
		if perr != nil {
			out = append(out, restore.RecoveryFinding{Name: name, Err: corrupt("%q under the restore store is not a recovery observation id: %v", name, perr)})
			continue
		}
		record, gerr := r.Get(id)
		if gerr != nil {
			out = append(out, restore.RecoveryFinding{ID: id, Err: gerr})
			continue
		}
		out = append(out, restore.RecoveryFinding{ID: id, Record: record})
	}
	// Newest first, with the id as a deterministic tie-break. Ids are
	// time-prefixed, so ordering by id is ordering by creation time even for an
	// unreadable record whose timestamp cannot be read.
	sort.Slice(out, func(i, j int) bool {
		return sortKey(out[i]) > sortKey(out[j])
	})
	return out, nil
}

// sortKey is the newest-first ordering key: the id for a parsed name (which is
// time-prefixed), or the raw name for an unparseable one so it still sorts
// deterministically.
func sortKey(f restore.RecoveryFinding) string {
	if !f.ID.IsZero() {
		return f.ID.String()
	}
	return f.Name
}

// listIDs returns the parseable record ids, ignoring unreadable entries. It backs
// prefix resolution, where an unreadable neighbour must not prevent resolving a
// healthy record.
func (r *Repo) listIDs(ctx context.Context) ([]restore.OperationID, error) {
	entries, err := fsx.ReadDirNoFollow(r.root, r.restoresRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, corrupt("restore store directory: %v", err)
	}
	var out []restore.OperationID
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.Name() == tmpDirName {
			continue
		}
		id, perr := restore.ParseOperationID(e.Name())
		if perr != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// Delete removes a recovery observation. It is idempotent, so GC and a failed
// publish's rollback share one path.
func (r *Repo) Delete(id restore.OperationID) error {
	dir, err := fsx.OpenDirNoFollow(r.root, r.restoresRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = dir.Close() }()
	// The record is an awa-owned directory of awa-owned files; removing it through
	// the pinned parent descriptor keeps a swapped ancestor from redirecting the
	// delete. This is the one place a recursive removal is correct — it is inside
	// .awa, never a selected worktree path.
	if err := fsx.RemoveTreeAt(dir, id.String()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readFile reads one record file through the no-follow boundary and confirms it
// is a regular file, so neither a symlinked record directory nor a symlink at the
// file itself can redirect a read. The read is capped: a record directory is local
// state a hostile or damaged process can write, and a metadata document is the only
// thing read whole, so an oversized file is refused rather than turned into an
// allocation proportional to whatever is on disk.
func (r *Repo) readFile(rel string) ([]byte, error) {
	f, err := fsx.OpenNoFollowAt(r.root, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if fsx.IsSymlinkOpenRejection(err) {
			return nil, corrupt("%s is reached through a symlink", rel)
		}
		return nil, corrupt("%s: %v", rel, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, corrupt("%s is not a regular file (%s)", rel, info.Mode().Type())
	}
	if info.Size() > maxMetaFileSize {
		return nil, corrupt("%s is %d bytes, over the %d-byte metadata limit", rel, info.Size(), maxMetaFileSize)
	}
	// The stat above is the cheap answer; this one is the enforced one. Reading one
	// byte past the limit is what catches a file that grew after the stat, and it is
	// also why an oversized document can never be decoded from a truncated prefix
	// that happens to parse.
	data, err := io.ReadAll(io.LimitReader(f, maxMetaFileSize+1))
	if err != nil {
		return nil, corrupt("%s: %v", rel, err)
	}
	if len(data) > maxMetaFileSize {
		return nil, corrupt("%s exceeds the %d-byte metadata limit", rel, maxMetaFileSize)
	}
	return data, nil
}

// verifiedManifest re-derives the manifest's tree hash on a full drain and checks
// it against the recorded ref. The jsonl stream already verifies the record
// count; this layer catches a tampered manifest that kept the count.
type verifiedManifest struct {
	stream   manifestjsonl.Stream
	expected hashing.TreeHash
	hasher   hashing.Hasher
	id       restore.OperationID
}

func (m *verifiedManifest) Open(ctx context.Context) (worktree.ManifestCursor, error) {
	cur, err := m.stream.Open(ctx)
	if err != nil {
		return nil, err
	}
	reducer, err := worktree.NewTreeReducer(m.hasher)
	if err != nil {
		_ = cur.Close()
		return nil, err
	}
	return &verifyingCursor{inner: cur, reducer: reducer, expected: m.expected, id: m.id}, nil
}

type verifyingCursor struct {
	inner    worktree.ManifestCursor
	reducer  *worktree.TreeReducer
	expected hashing.TreeHash
	id       restore.OperationID
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
		// An empty manifest reduces to a hash the record does not carry (a record with
		// no entries records a zero tree hash), so only check when records exist.
		if red := c.reducer.Finish(); !c.expected.IsZero() && red.Hash != c.expected {
			c.err = corrupt("recovery observation %s manifest tree hash %s does not match the recorded %s", c.id.Short(), red.Hash, c.expected)
		}
		return false
	}
	if err := c.reducer.Add(c.inner.Record()); err != nil {
		c.err = corrupt("recovery observation %s manifest: %v", c.id.Short(), err)
		c.done = true
		return false
	}
	return true
}

func (c *verifyingCursor) Record() worktree.ManifestRecord { return c.inner.Record() }
func (c *verifyingCursor) Err() error                      { return c.err }
func (c *verifyingCursor) Close() error                    { return c.inner.Close() }

// --- metadata codec -------------------------------------------------------

// metaDoc is the persisted form of a recovery observation's metadata.
type metaDoc struct {
	SchemaVersion   int          `json:"schema_version"`
	ID              string       `json:"id"`
	CreatedAt       string       `json:"created_at"`
	AwaVersion      string       `json:"awa_version"`
	ScanConfigHash  string       `json:"scan_config_hash"`
	Source          sourceDoc    `json:"source"`
	Selection       selectionDoc `json:"selection"`
	Manifest        manifestDoc  `json:"manifest"`
	Scope           scopeDoc     `json:"scope"`
	ContentComplete bool         `json:"content_complete"`
}

type sourceDoc struct {
	Kind         string `json:"kind"`
	ID           string `json:"id"`
	CanonicalRef string `json:"canonical_ref"`
	Requested    string `json:"requested"`
}

type selectionDoc struct {
	All   bool     `json:"all"`
	Paths []string `json:"paths"`
}

type manifestDoc struct {
	File        string `json:"file"`
	TreeHash    string `json:"tree_hash"`
	RecordCount int    `json:"record_count"`
}

type scopeDoc struct {
	File      string `json:"file"`
	PathCount int    `json:"path_count"`
}

func encodeMeta(o restore.RecoveryObservation) ([]byte, error) {
	sel := selectionDoc{All: o.Selection().All()}
	for _, p := range o.Selection().Paths() {
		sel.Paths = append(sel.Paths, p.String())
	}
	treeHash := ""
	if !o.Manifest().TreeHash.IsZero() {
		treeHash = o.Manifest().TreeHash.String()
	}
	doc := metaDoc{
		SchemaVersion:  metaSchemaVersion,
		ID:             o.ID().String(),
		CreatedAt:      o.CreatedAt().UTC().Format(time.RFC3339Nano),
		AwaVersion:     o.AwaVersion(),
		ScanConfigHash: o.ScanConfigHash().String(),
		Source: sourceDoc{
			Kind:         o.Source().Kind().String(),
			ID:           o.Source().ID(),
			CanonicalRef: o.Source().CanonicalRef(),
			Requested:    o.Source().Requested(),
		},
		Selection:       sel,
		Manifest:        manifestDoc{File: o.Manifest().File, TreeHash: treeHash, RecordCount: o.Manifest().RecordCount},
		Scope:           scopeDoc{File: o.Scope().File, PathCount: o.Scope().PathCount},
		ContentComplete: o.ContentComplete(),
	}
	return json.MarshalIndent(doc, "", "  ")
}

// decodeMeta parses a persisted record strictly and rebuilds it through the
// domain constructors, so a hand-edited or foreign document cannot become a
// trusted recovery observation.
func decodeMeta(data []byte) (restore.RecoveryObservation, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var doc metaDoc
	if err := dec.Decode(&doc); err != nil {
		return restore.RecoveryObservation{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return restore.RecoveryObservation{}, fmt.Errorf("unexpected trailing data after JSON document")
	}
	if doc.SchemaVersion != metaSchemaVersion {
		return restore.RecoveryObservation{}, fmt.Errorf("unsupported schema version %d (this build reads %d)", doc.SchemaVersion, metaSchemaVersion)
	}
	id, err := restore.ParseOperationID(doc.ID)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	createdAt, err := time.Parse(time.RFC3339Nano, doc.CreatedAt)
	if err != nil {
		return restore.RecoveryObservation{}, fmt.Errorf("invalid created_at: %w", err)
	}
	scanConfig, err := hashing.ParseConfigHash(doc.ScanConfigHash)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	kind, err := parseSourceKind(doc.Source.Kind)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	source, err := restore.NewSource(kind, doc.Source.ID, doc.Source.CanonicalRef, doc.Source.Requested)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	selection, err := decodeSelection(doc.Selection)
	if err != nil {
		return restore.RecoveryObservation{}, err
	}
	if doc.Manifest.File != manifestName {
		return restore.RecoveryObservation{}, fmt.Errorf("manifest payload is named %q, not %s", doc.Manifest.File, manifestName)
	}
	if doc.Scope.File != scopeName {
		return restore.RecoveryObservation{}, fmt.Errorf("covered-scope payload is named %q, not %s", doc.Scope.File, scopeName)
	}
	var treeHash hashing.TreeHash
	if doc.Manifest.TreeHash != "" {
		treeHash, err = hashing.ParseTreeHash(doc.Manifest.TreeHash)
		if err != nil {
			return restore.RecoveryObservation{}, err
		}
	}
	return restore.NewRecoveryObservation(
		restore.RecoveryBuild{
			ID:             id,
			CreatedAt:      createdAt,
			AwaVersion:     doc.AwaVersion,
			ScanConfigHash: scanConfig,
			Source:         source,
			Selection:      selection,
		},
		restore.ManifestRef{File: doc.Manifest.File, TreeHash: treeHash, RecordCount: doc.Manifest.RecordCount},
		restore.ScopeRef{File: doc.Scope.File, PathCount: doc.Scope.PathCount},
		doc.ContentComplete,
	)
}

// parseSourceKind resolves the persisted source-kind token. It is the read-side
// counterpart of SourceKind.String, kept here because the persisted vocabulary is
// this store's boundary, not the domain's business.
func parseSourceKind(s string) (restore.SourceKind, error) {
	switch s {
	case "checkpoint":
		return restore.SourceCheckpoint, nil
	case "run-observation":
		return restore.SourceRunObservation, nil
	case "recovery-observation":
		return restore.SourceRecoveryObservation, nil
	default:
		return 0, fmt.Errorf("invalid restore source kind %q", s)
	}
}

// decodeSelection rebuilds a normalized selection, rejecting the both-at-once and
// neither states a hand-edited record could otherwise express.
func decodeSelection(doc selectionDoc) (restore.Selection, error) {
	if doc.All && len(doc.Paths) > 0 {
		return restore.Selection{}, fmt.Errorf("selection is both all and a path set")
	}
	if doc.All {
		return restore.NewAllSelection(), nil
	}
	parsed := make([]worktree.RelPath, 0, len(doc.Paths))
	for _, p := range doc.Paths {
		rp, err := worktree.ParseRelPath(p)
		if err != nil {
			return restore.Selection{}, err
		}
		parsed = append(parsed, rp)
	}
	return restore.NewPathSelection(parsed)
}
