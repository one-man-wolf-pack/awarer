package compare

import "awarer/internal/domain/worktree"

// DefaultRenameBufferLimit exposes the production rename buffer limit to tests.
const DefaultRenameBufferLimit = defaultRenameBufferLimit

// CompareStreamLimited is CompareStream with an explicit rename buffer limit, for tests
// that need to drive the over-limit policy through the full comparison entry point.
func CompareStreamLimited(left, right worktree.ManifestCursor, opts Options, limit int) (ChangeCursor, error) {
	return compareStreamLimited(left, right, opts, limit)
}
