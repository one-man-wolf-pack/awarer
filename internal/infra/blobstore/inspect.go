package blobstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"awarer/internal/domain/blob"
	"awarer/internal/domain/hashing"
	"awarer/internal/infra/fsx"
)

// BlobInfo is one stored blob discovered by List: its durable content-address ref
// and its on-disk size. GC uses the ref to test reachability and the size to report
// reclaimable bytes.
type BlobInfo struct {
	Ref   blob.BlobRef
	Bytes int64
}

// List enumerates every well-formed blob in the store as a typed ref plus size,
// walking the sharded "<algo>/<ab>/<cd>/<hex>" layout through the same component-wise
// no-follow traversal Has and Open use. A malformed path — a stray file at the wrong
// depth, a name that is not a valid content hash, or a hash whose shards disagree
// with its directories — is skipped rather than reported as a blob, so GC never
// treats it as a sweepable orphan. A symlinked or non-directory component is store
// corruption surfaced as ErrCorruptBlob, which makes the caller block the blob sweep
// (an incomplete listing must never license deletion). An absent store is an empty
// list, not an error.
func (s *FS) List() ([]BlobInfo, error) {
	relBlobs, err := fsx.RelUnder(s.root, s.blobsDir)
	if err != nil {
		return nil, err
	}
	var out []BlobInfo
	// algo -> ab -> cd -> hex file. Each level lists no-follow; only real
	// subdirectories are descended, and only regular files are considered blobs.
	algoDirs, err := s.listDirNoFollow(relBlobs)
	if err != nil {
		return nil, err
	}
	for _, algo := range algoDirs {
		if !algo.IsDir() {
			continue
		}
		relAlgo := relBlobs + "/" + algo.Name()
		abDirs, err := s.listDirNoFollow(relAlgo)
		if err != nil {
			return nil, err
		}
		for _, ab := range abDirs {
			if !ab.IsDir() {
				continue
			}
			relAB := relAlgo + "/" + ab.Name()
			cdDirs, err := s.listDirNoFollow(relAB)
			if err != nil {
				return nil, err
			}
			for _, cd := range cdDirs {
				if !cd.IsDir() {
					continue
				}
				relCD := relAB + "/" + cd.Name()
				files, err := s.listDirNoFollow(relCD)
				if err != nil {
					return nil, err
				}
				for _, f := range files {
					info, ok := s.classifyBlobFile(algo.Name(), ab.Name(), cd.Name(), f)
					if ok {
						out = append(out, info)
					}
				}
			}
		}
	}
	// Directory reads are not ordered, so sort by content address to give callers a
	// deterministic listing — gc renders these as candidates and promises stable
	// output across platforms.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Ref.Hash().String() < out[j].Ref.Hash().String()
	})
	return out, nil
}

// listDirNoFollow lists relDir under the project root no-follow, mapping a missing
// directory to an empty listing (a store level that was never created is not an
// error) and a symlinked or non-directory component to ErrCorruptBlob (it is store
// corruption, and an incomplete listing must block the sweep).
func (s *FS) listDirNoFollow(relDir string) ([]os.DirEntry, error) {
	entries, err := fsx.ReadDirNoFollow(s.root, relDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if fsx.IsSymlinkOpenRejection(err) || errors.Is(err, fsx.ErrNotDirectory) {
			return nil, fmt.Errorf("%w: store path %s is not a real directory", blob.ErrCorruptBlob, relDir)
		}
		return nil, err
	}
	return entries, nil
}

// classifyBlobFile turns a leaf directory entry into a BlobInfo when, and only when,
// it is a well-formed blob: a regular file whose name parses as a content hash under
// the identity namespace directory it lives in, with shard directories that match the
// hash's own sharding. Anything else (a non-regular node, an unparsable name, a
// foreign namespace, mismatched shards) is skipped so GC never sweeps a path it
// cannot prove is a blob.
func (s *FS) classifyBlobFile(ns, ab, cd string, e os.DirEntry) (BlobInfo, bool) {
	if !e.Type().IsRegular() {
		return BlobInfo{}, false
	}
	h, err := hashing.ParseContentHash(ns + ":" + e.Name())
	if err != nil {
		return BlobInfo{}, false
	}
	// The on-disk shards must match the address the hash computes for itself; a file
	// filed under the wrong shard is not addressable and is not swept.
	bp, err := blob.PathFor(h)
	if err != nil {
		return BlobInfo{}, false
	}
	if bp.Rel() != ns+"/"+ab+"/"+cd+"/"+e.Name() {
		return BlobInfo{}, false
	}
	info, err := e.Info()
	if err != nil {
		return BlobInfo{}, false
	}
	ref, err := blob.NewBlobRef(h)
	if err != nil {
		return BlobInfo{}, false
	}
	return BlobInfo{Ref: ref, Bytes: info.Size()}, true
}

// Delete removes the blob addressed by h through a verified, symlink-free shard
// directory descriptor, so the unlink cannot be redirected outside the project by a
// swapped ancestor. It is idempotent: an already-absent blob is not an error. The
// caller is responsible for having proven the blob unreachable; Delete enforces only
// the path-safety boundary, not reachability.
func (s *FS) Delete(h hashing.ContentHash) error {
	finalPath, err := s.finalPath(h)
	if err != nil {
		return err
	}
	shardAbs := filepath.Dir(finalPath)
	name := filepath.Base(finalPath)
	relShard, err := fsx.RelUnder(s.root, shardAbs)
	if err != nil {
		return err
	}
	dir, err := fsx.OpenDirNoFollow(s.root, relShard)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // a missing shard means the blob is already gone
		}
		if fsx.IsSymlinkOpenRejection(err) || errors.Is(err, fsx.ErrNotDirectory) {
			return fmt.Errorf("%w: %s is not a real directory", blob.ErrCorruptBlob, shardAbs)
		}
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := fsx.RemoveAt(dir, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing blob %s: %w", h, err)
	}
	return nil
}
