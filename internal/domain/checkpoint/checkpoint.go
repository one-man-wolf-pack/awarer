package checkpoint

import (
	"fmt"
	"strings"

	"awarer/internal/domain/worktree"
)

// DirtySummary is a compact, stable summary of a git worktree's status at
// checkpoint time: counts by status class rather than full porcelain output, so
// it is deterministic and small enough to embed in every checkpoint.
type DirtySummary struct {
	Clean     bool
	Modified  int
	Added     int
	Deleted   int
	Renamed   int
	Untracked int
}

// GitMetadata is best-effort git context captured when the project is inside a git
// worktree. It is nil on the checkpoint header when the project is not a git repo,
// so "not git" and "git with no commits" are distinguishable.
type GitMetadata struct {
	InWorktree  bool
	Branch      string // empty when detached or unknown
	Commit      string // full hash, empty when HEAD has no commit
	ShortCommit string
	Dirty       DirtySummary
}

// Validate checks that present git metadata is internally consistent. It is
// invoked at the persistence boundary so a corrupt or hand-edited git block —
// metadata that is non-nil yet claims no worktree, or a short commit without a
// full one — is rejected rather than trusted as raw data.
func (g GitMetadata) Validate() error {
	if !g.InWorktree {
		return fmt.Errorf("git metadata is present but not in a worktree")
	}
	if g.Commit == "" && g.ShortCommit != "" {
		return fmt.Errorf("git metadata has a short commit but no full commit")
	}
	if g.Commit != "" && g.ShortCommit != "" && !strings.HasPrefix(g.Commit, g.ShortCommit) {
		return fmt.Errorf("git short commit %q is not a prefix of commit %q", g.ShortCommit, g.Commit)
	}
	return g.Dirty.Validate()
}

// Validate checks that a dirty summary is internally consistent: no negative
// counts, and Clean set exactly when there are no changes.
func (d DirtySummary) Validate() error {
	counts := []struct {
		n    int
		name string
	}{
		{d.Modified, "modified"},
		{d.Added, "added"},
		{d.Deleted, "deleted"},
		{d.Renamed, "renamed"},
		{d.Untracked, "untracked"},
	}
	total := 0
	for _, c := range counts {
		if c.n < 0 {
			return fmt.Errorf("git dirty summary has negative %s count %d", c.name, c.n)
		}
		total += c.n
	}
	if d.Clean != (total == 0) {
		return fmt.Errorf("git dirty summary clean=%v contradicts %d total changes", d.Clean, total)
	}
	return nil
}

// CheckpointStats are derived counts over a checkpoint's entries, cheap to compute
// once and convenient for status and log without re-walking the manifest.
type CheckpointStats struct {
	Files      int   `json:"files"`
	Dirs       int   `json:"dirs"`
	Symlinks   int   `json:"symlinks"`
	Blobs      int   `json:"blobs"`
	HashOnly   int   `json:"hash_only"`
	Skipped    int   `json:"skipped"`
	TotalBytes int64 `json:"total_bytes"`
}

// Validate checks that a stats block is internally possible, independent of any
// manifest. It is the cheap structural guard a header read runs before trusting
// the durable counts: no negative count or size, and the storage split
// (blobs + hash-only) must account for exactly the regular files, since every
// regular entry is stored one way or the other. A header read is now a hostile-
// input boundary on its own, so a mechanically impossible stats block is rejected
// here rather than only when the manifest is later read.
func (s CheckpointStats) Validate() error {
	for _, f := range []struct {
		n    int
		name string
	}{
		{s.Files, "files"},
		{s.Dirs, "dirs"},
		{s.Symlinks, "symlinks"},
		{s.Blobs, "blobs"},
		{s.HashOnly, "hash_only"},
		{s.Skipped, "skipped"},
	} {
		if f.n < 0 {
			return fmt.Errorf("checkpoint stats has negative %s count %d", f.name, f.n)
		}
	}
	if s.TotalBytes < 0 {
		return fmt.Errorf("checkpoint stats has negative total_bytes %d", s.TotalBytes)
	}
	if s.Blobs+s.HashOnly != s.Files {
		return fmt.Errorf("checkpoint stats blobs %d + hash_only %d do not account for %d files", s.Blobs, s.HashOnly, s.Files)
	}
	return nil
}

// StatsFromReduced maps the worktree reducer's stats into checkpoint stats. It is the
// only way CheckpointStats are produced: both the write path and the verifying read
// path derive them from a single reducer pass over the manifest records, so a header's
// durable counts can be re-checked against its manifest without a second walk and
// without a second counting rule that could disagree.
func StatsFromReduced(r worktree.ReducedStats) CheckpointStats {
	return CheckpointStats{
		Files:      r.Files,
		Dirs:       r.Dirs,
		Symlinks:   r.Symlinks,
		Blobs:      r.Blobs,
		HashOnly:   r.HashOnly,
		Skipped:    r.Skipped,
		TotalBytes: r.TotalBytes,
	}
}
