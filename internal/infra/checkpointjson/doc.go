// Package checkpointjson implements the checkpoint.Repository port over the
// filesystem, using an explicit, versioned JSON schema for checkpoint headers.
//
// The on-disk schema carries its own version so a reader never guesses what it is
// holding. Encoding and decoding go through the domain value-object constructors,
// so a round-trip re-validates every invariant — a corrupt or hand-edited file is
// rejected at decode rather than silently trusted.
package checkpointjson

import (
	"awarer/internal/domain/checkpoint"
)

// gitDoc is the persisted Git context of a checkpoint. It is shared by the header
// codec and every reader that reassembles a checkpoint from one.
type gitDoc struct {
	InWorktree  bool     `json:"in_worktree"`
	Branch      string   `json:"branch,omitempty"`
	Commit      string   `json:"commit,omitempty"`
	ShortCommit string   `json:"short_commit,omitempty"`
	Dirty       dirtyDoc `json:"dirty"`
}

type dirtyDoc struct {
	Clean     bool `json:"clean"`
	Modified  int  `json:"modified"`
	Added     int  `json:"added"`
	Deleted   int  `json:"deleted"`
	Renamed   int  `json:"renamed"`
	Untracked int  `json:"untracked"`
}

func encodeGit(g *checkpoint.GitMetadata) *gitDoc {
	if g == nil {
		return nil
	}
	return &gitDoc{
		InWorktree:  g.InWorktree,
		Branch:      g.Branch,
		Commit:      g.Commit,
		ShortCommit: g.ShortCommit,
		Dirty: dirtyDoc{
			Clean:     g.Dirty.Clean,
			Modified:  g.Dirty.Modified,
			Added:     g.Dirty.Added,
			Deleted:   g.Dirty.Deleted,
			Renamed:   g.Dirty.Renamed,
			Untracked: g.Dirty.Untracked,
		},
	}
}

func decodeGit(g *gitDoc) (*checkpoint.GitMetadata, error) {
	if g == nil {
		return nil, nil
	}
	return &checkpoint.GitMetadata{
		InWorktree:  g.InWorktree,
		Branch:      g.Branch,
		Commit:      g.Commit,
		ShortCommit: g.ShortCommit,
		Dirty: checkpoint.DirtySummary{
			Clean:     g.Dirty.Clean,
			Modified:  g.Dirty.Modified,
			Added:     g.Dirty.Added,
			Deleted:   g.Dirty.Deleted,
			Renamed:   g.Dirty.Renamed,
			Untracked: g.Dirty.Untracked,
		},
	}, nil
}
