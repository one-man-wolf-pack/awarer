//go:build !darwin && !linux && !freebsd

package worktreefs

import (
	"io/fs"

	"awarer/internal/domain/worktree"
)

// statSignature builds a stat signature on platforms without a known
// syscall.Stat_t layout. Size, mtime, and mode are portable; ctime, dev, ino,
// and nlink are unavailable and recorded as omitted so they are never guessed
// (and never falsely invalidate a reuse decision).
func statSignature(info fs.FileInfo) worktree.StatSignature {
	return worktree.StatSignature{
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
		Mode:    uint32(info.Mode()),
		Omitted: worktree.FieldSet(0).
			With(worktree.FieldCtime).
			With(worktree.FieldDev).
			With(worktree.FieldIno).
			With(worktree.FieldNlink),
	}
}
