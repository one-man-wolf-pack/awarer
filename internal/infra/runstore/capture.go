package runstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"awarer/internal/domain/hashing"
	"awarer/internal/domain/paths"
	"awarer/internal/domain/runcache"
	"awarer/internal/domain/worktree"
	"awarer/internal/infra/fsx"
	"awarer/internal/infra/manifestjsonl"
)

// teeObservation streams an observation manifest into a staged entry file named
// name under relDir and returns the manifest ref derived from the reduced records.
// The manifest bytes are the source of truth; the ref's tree hash and record count
// come from the same single pass that writes them, so they cannot disagree.
func (r *Repo) teeObservation(ctx context.Context, relDir, name string, stream worktree.ManifestStream, hasher hashing.Hasher) (runcache.ManifestRef, error) {
	reducer, err := worktree.NewTreeReducer(hasher)
	if err != nil {
		return runcache.ManifestRef{}, err
	}
	if err := fsx.PublishStreamNoFollow(r.root, relDir, name, payloadPerm, func(w io.Writer) error {
		cur, err := stream.Open(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = cur.Close() }()
		// Normalize on the way in, so the persisted bytes and the tree hash derived
		// alongside them describe the same records: a run observation records identity
		// only and must not claim content it never published.
		return manifestjsonl.Tee(w, &hashOnlyCursor{inner: cur}, reducer)
	}); err != nil {
		return runcache.ManifestRef{}, err
	}
	red := reducer.Finish()
	return runcache.ManifestRef{File: name, TreeHash: red.Hash, RecordCount: red.Count}, nil
}

// truncationMarker renders the marker appended to a truncated output stream. It
// records the exact number of omitted bytes so the stored payload remains
// self-describing.
func truncationMarker(omitted int64) string {
	return fmt.Sprintf("\n[awa: output truncated, %d bytes omitted]\n", omitted)
}

// Begin starts a capture session: it creates two temp payload files under
// .awa/runs/tmp that the caller streams the command's stdout and stderr into,
// bounded by the given limits. The session is finished with Finalize + Commit to
// publish a complete entry, or Discard to drop the temp payloads.
func (r *Repo) Begin(limits runcache.CaptureLimits) (runcache.PendingRun, error) {
	if err := limits.Validate(); err != nil {
		return nil, fmt.Errorf("runstore: %w", err)
	}
	tmpDir, err := fsx.MkdirAllNoFollow(r.root, r.tmpRel(), paths.DirPerm)
	if err != nil {
		return nil, r.storeCorrupt(r.tmpRel(), err)
	}
	stdoutFile, soTmpName, err := fsx.CreateTempAt(tmpDir, ".stdout-")
	if err != nil {
		_ = tmpDir.Close()
		return nil, err
	}
	stderrFile, seTmpName, err := fsx.CreateTempAt(tmpDir, ".stderr-")
	if err != nil {
		_ = stdoutFile.Close()
		_ = fsx.RemoveAt(tmpDir, soTmpName)
		_ = tmpDir.Close()
		return nil, err
	}
	return &pendingRun{
		repo:          r,
		tmpDir:        tmpDir,
		stdoutTmpName: soTmpName,
		stderrTmpName: seTmpName,
		stdoutSink:    &captureSink{file: stdoutFile, limit: limits.MaxStdout},
		stderrSink:    &captureSink{file: stderrFile, limit: limits.MaxStderr},
	}, nil
}

// pendingRun is an in-progress capture: two temp payload files in .awa/runs/tmp
// and the directory descriptor they live in.
type pendingRun struct {
	repo          *Repo
	tmpDir        *os.File
	stdoutTmpName string
	stderrTmpName string
	stdoutSink    *captureSink
	stderrSink    *captureSink
	// stdoutCapture/stderrCapture are the capture metadata Finalize produced from
	// the actual payload bytes. Commit checks the entry it is asked to publish
	// carries exactly these, so the stored metadata can never disagree with the
	// stored payloads.
	stdoutCapture runcache.OutputCapture
	stderrCapture runcache.OutputCapture
	finalized     bool
	closed        bool
}

func (p *pendingRun) Stdout() io.Writer { return p.stdoutSink }
func (p *pendingRun) Stderr() io.Writer { return p.stderrSink }

// Finalize stops capture on both streams, appends a truncation marker where
// needed, and returns the resulting capture metadata. It is idempotent: a repeat
// call returns the already-computed captures without mutating the payloads, so a
// caller cannot double-append a marker or re-hash.
func (p *pendingRun) Finalize() (runcache.OutputCapture, runcache.OutputCapture, error) {
	if p.finalized {
		return p.stdoutCapture, p.stderrCapture, nil
	}
	so, err := p.stdoutSink.finalize(p.repo.hasher, stdoutName)
	if err != nil {
		return runcache.OutputCapture{}, runcache.OutputCapture{}, err
	}
	se, err := p.stderrSink.finalize(p.repo.hasher, stderrName)
	if err != nil {
		return runcache.OutputCapture{}, runcache.OutputCapture{}, err
	}
	p.stdoutCapture = so
	p.stderrCapture = se
	p.finalized = true
	return so, se, nil
}

// Commit atomically publishes the recorded run. It builds the entry directory in a
// temp directory — moving the captured payloads in, teeing the before/after
// observation manifests, and writing the metadata — then renames it into entries/
// and, only when the run is reusable, writes the key pointer last, so an
// interruption before the pointer write can never produce a hit and a non-reusable
// run never gets a pointer at all.
func (p *pendingRun) Commit(ctx context.Context, entry runcache.RunEntry, obs runcache.RunObservations) (err error) {
	if !p.finalized {
		return fmt.Errorf("runstore: Commit before Finalize")
	}
	r := p.repo

	// Close the payload files; they are renamed by name, not descriptor. Done up
	// front so every return path (including a validation rejection) releases them.
	_ = p.stdoutSink.file.Close()
	_ = p.stderrSink.file.Close()

	tmpEntryRel := r.tmpRel() + "/.entry-" + entry.ID.String()

	// Register cleanup before anything is created, so every failure path — a
	// rejected entry or a mid-publish error — leaves no trash: the staged entry dir
	// (with any payloads already moved into it) and any temp payloads not yet moved
	// out of tmp are both removed, and the tmp dir descriptor is always closed.
	committed := false
	crashed := false
	defer func() {
		if !committed && !crashed {
			// The staged entry dir lives directly under the pinned tmp descriptor, so
			// remove it through that descriptor rather than re-resolving its path — a
			// symlink swapped into an ancestor cannot then redirect the recursive delete.
			_ = fsx.RemoveTreeAt(p.tmpDir, ".entry-"+entry.ID.String())
			_ = fsx.RemoveAt(p.tmpDir, p.stdoutTmpName)
			_ = fsx.RemoveAt(p.tmpDir, p.stderrTmpName)
		}
		p.closed = true
		_ = p.tmpDir.Close()
	}()

	// Enforce the capture and key invariants the read path requires before doing any
	// publishing work, so an obviously bad entry is rejected cheaply.
	if entry.Stdout != p.stdoutCapture || entry.Stderr != p.stderrCapture {
		return fmt.Errorf("runstore: refusing to publish run %s: output captures do not match the finalized payloads", entry.ID.Short())
	}
	if !r.keyMatches(entry) {
		return fmt.Errorf("runstore: refusing to publish run %s: key does not match its recorded inputs", entry.ID.Short())
	}

	tmpEntryDir, err := fsx.MkdirAllNoFollow(r.root, tmpEntryRel, paths.DirPerm)
	if err != nil {
		return r.storeCorrupt(tmpEntryRel, err)
	}
	defer func() { _ = tmpEntryDir.Close() }()

	if err := fsx.RenameAt(p.tmpDir, p.stdoutTmpName, tmpEntryDir, stdoutName); err != nil {
		return err
	}
	if err := fsx.RenameAt(p.tmpDir, p.stderrTmpName, tmpEntryDir, stderrName); err != nil {
		return err
	}

	// Tee the pre/post observation manifests into the staged entry and fill the
	// entry's observation refs from the reduced result, so the manifest bytes are the
	// source of truth and the refs (tree hash, record count) are computed from them.
	// A before-observation is required for a recorded real execution; an
	// after-observation is absent exactly when the post-run scan failed.
	if obs.Before == nil {
		return fmt.Errorf("runstore: refusing to publish run %s: missing before-observation", entry.ID.Short())
	}
	beforeRef, err := r.teeObservation(ctx, tmpEntryRel, beforeManifestName, obs.Before, r.hasher)
	if err != nil {
		return err
	}
	entry.Before = &runcache.Observation{Manifest: beforeRef, ScanConfigHash: obs.BeforeScanConfigHash}
	if obs.After != nil {
		afterRef, err := r.teeObservation(ctx, tmpEntryRel, afterManifestName, obs.After, r.hasher)
		if err != nil {
			return err
		}
		entry.After = &runcache.Observation{Manifest: afterRef, ScanConfigHash: obs.AfterScanConfigHash}
	}

	// With the observation refs filled, re-validate the whole entry: this is where
	// the read-path invariants (reuse vs decision, before-tree-hash == keyed input,
	// after vs mutation) are enforced on the write path, so the store never publishes
	// a record Get would later reject as corrupt.
	if err := entry.Validate(); err != nil {
		return err
	}

	if err := r.checkFail(failAfterPayloadBeforeMeta, &crashed); err != nil {
		// On a crash the staged entry survives under tmp (never renamed into the
		// shard), so a fresh process sees a reclaimable temp orphan, never a hit.
		return err
	}
	metaBytes, err := encodeMeta(entry)
	if err != nil {
		return err
	}
	if err := fsx.PublishBytesNoFollow(r.root, tmpEntryRel, metaName, metaBytes, metaPerm); err != nil {
		return err
	}
	// Flush the staged entry's contents (payloads + meta) before it is published.
	// Not yet renamed into the shard, so a failure just returns and the deferred
	// cleanup removes the temp entry.
	if err := fsx.SyncDir(tmpEntryDir); err != nil {
		return err
	}

	// Publish the entry directory atomically into its shard.
	shardDir, err := fsx.MkdirAllNoFollow(r.root, r.entryShardRel(entry.ID), paths.DirPerm)
	if err != nil {
		return r.storeCorrupt(r.entryShardRel(entry.ID), err)
	}
	defer func() { _ = shardDir.Close() }()
	if err := fsx.RenameAt(p.tmpDir, ".entry-"+entry.ID.String(), shardDir, entry.ID.String()); err != nil {
		return err
	}
	// Flush the shard directory so the renamed entry is durable before the key
	// pointer can reference it: a crash must never leave a durable pointer to an entry
	// directory that did not survive. A real sync error means we cannot guarantee that
	// ordering, so roll the entry back and do not write the pointer. (On platforms
	// where a directory fsync is unsupported, SyncDir is a no-op and returns nil.)
	if err := fsx.SyncDir(shardDir); err != nil {
		return errors.Join(err, r.removeEntryDir(entry.ID))
	}

	// A non-reusable recorded run is durable history with no cache pointer: the commit
	// is complete once the entry is durably in its shard. Cache lookup is then
	// mechanically unable to reach it (there is no pointer to follow), which is the
	// whole point of recording mutating/record-only runs as history rather than hits.
	if !entry.Reuse.IsReusable() {
		committed = true
		return nil
	}

	if ferr := r.checkFail(failAfterMetaBeforePointer, &crashed); ferr != nil {
		// The entry is durably in entries/ but the key pointer is not yet written. A
		// crash leaves that orphan for gc/doctor to reclaim — Lookup must report a clean
		// miss, never a partial hit. A live error instead rolls the entry back, so a
		// failed commit never leaves a run List/Get can see yet Lookup cannot resolve.
		if crashed {
			return ferr
		}
		return errors.Join(ferr, r.removeEntryDir(entry.ID))
	}

	// The entry directory is now durably in entries/, but the commit is complete only
	// once the key pointer is written. If the pointer write fails — a recoverable
	// error — roll the published entry back, so a failed commit never leaves a run
	// that List and run log can see yet Lookup cannot resolve. (A crash in this window
	// can still orphan the entry; GC/doctor reclaim that. The live error path must not.)
	ptrBytes, err := encodeKeyPointer(entry.ID)
	if err != nil {
		return errors.Join(err, r.removeEntryDir(entry.ID))
	}
	if err := fsx.ReplaceBytesNoFollow(r.root, r.keyShardRel(entry.Key), keyFileName(entry.Key), ptrBytes, pointerPerm); err != nil {
		// The write may have created the pointer file before failing (e.g. a
		// post-rename sync error), so remove the pointer as well as the entry. Surface
		// any rollback error too: the caller must know if the store may hold an orphan,
		// not just that the pointer write failed.
		return errors.Join(err,
			r.removeFile(r.keyShardRel(entry.Key), keyFileName(entry.Key)),
			r.removeEntryDir(entry.ID))
	}
	committed = true
	return nil
}

// Discard removes the temp payloads without publishing anything. It is safe to
// call after Finalize and is a no-op once the session has been committed.
func (p *pendingRun) Discard() error {
	if p.closed {
		return nil
	}
	p.closed = true
	_ = p.stdoutSink.file.Close()
	_ = p.stderrSink.file.Close()
	_ = fsx.RemoveAt(p.tmpDir, p.stdoutTmpName)
	_ = fsx.RemoveAt(p.tmpDir, p.stderrTmpName)
	_ = p.tmpDir.Close()
	return nil
}

// captureSink writes up to limit bytes of a stream to a temp file while counting
// every byte seen, so output is captured bounded and never buffered whole in
// memory. Bytes beyond the limit are dropped from storage but still counted, and
// a marker recording the omitted count is appended on finalize.
type captureSink struct {
	file    *os.File
	limit   int64
	written int64 // bytes written to the file, excluding the truncation marker
	seen    int64 // total bytes observed from the process
	sealed  bool  // set by finalize; further writes are rejected
}

// Write stores the leading bytes up to the limit and counts the rest. It always
// reports the full length as consumed so the process is never blocked or errored
// by truncation. After finalize the sink is sealed: a late write is rejected
// rather than silently changing a payload whose hash has already been recorded.
func (w *captureSink) Write(p []byte) (int, error) {
	if w.sealed {
		return 0, fmt.Errorf("runstore: write to a finalized capture")
	}
	w.seen += int64(len(p))
	if w.written < w.limit {
		room := w.limit - w.written
		chunk := p
		if int64(len(chunk)) > room {
			chunk = p[:room]
		}
		if _, err := w.file.Write(chunk); err != nil {
			return 0, err
		}
		w.written += int64(len(chunk))
	}
	return len(p), nil
}

// finalize appends a truncation marker if the stream exceeded the limit, syncs
// the file, seals it read-only, hashes the stored bytes, and returns the capture
// metadata.
func (w *captureSink) finalize(hasher hashing.Hasher, name string) (runcache.OutputCapture, error) {
	// Seal first, so any concurrent or late Write is rejected rather than racing
	// the marker append and hash below.
	w.sealed = true
	truncated := w.seen > w.limit
	var omitted int64
	policy := runcache.TruncationNone
	if truncated {
		omitted = w.seen - w.written
		marker := truncationMarker(omitted)
		if _, err := w.file.WriteString(marker); err != nil {
			return runcache.OutputCapture{}, err
		}
		w.written += int64(len(marker))
		policy = runcache.TruncationHead
	}
	if err := w.file.Sync(); err != nil {
		return runcache.OutputCapture{}, err
	}
	// Make the payload read-only now that its bytes are final. A published entry is
	// immutable, and its metadata is published 0o444; sealing the payload to match
	// removes the owner-writable window that would let the stored bytes drift from
	// the hash the metadata records.
	if err := w.file.Chmod(payloadPerm); err != nil {
		return runcache.OutputCapture{}, err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return runcache.OutputCapture{}, err
	}
	h, err := hasher.HashReader(w.file)
	if err != nil {
		return runcache.OutputCapture{}, err
	}
	return runcache.OutputCapture{
		OriginalBytes:    w.seen,
		StoredBytes:      w.written,
		Truncated:        truncated,
		OmittedBytes:     omitted,
		TruncationPolicy: policy,
		Hash:             h,
		File:             name,
	}, nil
}
