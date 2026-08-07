package worktreefs

import (
	"io/fs"

	"awarer/internal/domain/worktree"
)

// StatSignatureOf builds a stat signature for a FileInfo using the same per-OS
// logic the walker uses internally. It is exported so the run cache's effect
// observer can reuse the syscall.Stat_t handling (size, mtime, mode, and the
// platform-specific ctime/dev/ino/nlink) without duplicating it.
func StatSignatureOf(info fs.FileInfo) worktree.StatSignature {
	return statSignature(info)
}
