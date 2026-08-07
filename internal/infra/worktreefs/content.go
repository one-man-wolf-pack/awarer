package worktreefs

import (
	"io"

	"awarer/internal/domain/worktree"
)

// ContentReader opens the current bytes of one worktree file on demand, through
// exactly the verified traversal a walk uses for the same file.
//
// It exists so a caller that needs the content of a few observed paths does not have
// to make the observation retain an opener for every file it scanned. A scan retains
// one closure per blob-intent regular entry when asked to (checkpoint and diff need
// that: they consume the whole scope), but restore needs the bytes of its selection
// only, and its selection is normally a tiny part of the project. Reconstructing an
// opener from the identity the observation already recorded keeps that cost
// proportional to the selection instead of to the worktree.
//
// The verification is not weaker for being late: openVerifiedRegularAt protects every
// path component against a symlink swap and then confirms, on the opened descriptor,
// that the node is still the regular file the observation stat'd. A node that changed
// in between surfaces as worktree.ErrObservationChanged rather than as bytes attributed
// to the old entry.
type ContentReader struct {
	root string
}

// NewContentReader builds a reader anchored at the project root, the trusted anchor
// every component below is checked against.
func NewContentReader(root string) *ContentReader { return &ContentReader{root: root} }

// Open returns a reader over the regular file at path, or an error if the path is no
// longer the node observed carries. observed is the stat signature the scan recorded
// for that entry.
func (r *ContentReader) Open(path worktree.RelPath, observed worktree.StatSignature) (io.ReadCloser, error) {
	return openVerifiedRegularAt(r.root, path.String(), observed)
}
