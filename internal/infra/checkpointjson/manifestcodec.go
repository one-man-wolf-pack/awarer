package checkpointjson

import (
	"fmt"

	"awarer/internal/domain/checkpoint"
	"awarer/internal/infra/manifestjsonl"
)

// The manifest record codec lives in manifestjsonl and is shared with the run store,
// so checkpoints and recorded runs encode their manifests in one identical format.
// This package streams through it and adds only the checkpoint-specific framing
// below; the record shape and its validation have a single owner.

// manifestStream builds a verified stream over a checkpoint's manifest.jsonl,
// tagging structural errors with the checkpoint corrupt-store sentinel.
func (r *Repo) manifestStream(id checkpoint.CheckpointID, expected int) manifestjsonl.Stream {
	return manifestjsonl.Stream{
		Root:     r.root,
		Abs:      r.manifestFor(id),
		Expected: expected,
		Label:    "checkpoint " + id.String(),
		Corrupt:  func(e error) error { return fmt.Errorf("%w: %v", checkpoint.ErrCorruptStore, e) },
	}
}
