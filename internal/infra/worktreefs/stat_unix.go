//go:build darwin || linux || freebsd

package worktreefs

import (
	"io/fs"
	"syscall"

	"awarer/internal/domain/worktree"
)

// statSignature builds a full stat signature from a Unix FileInfo. Size and
// mtime come from the portable FileInfo; mode, ctime, dev, ino, and nlink come
// from the platform syscall.Stat_t via the per-OS helpers. Mode is the raw
// st_mode (e.g. 0o100644), so it matches the persisted manifest representation
// rather than Go's abstract FileMode bits. If the underlying value is not
// a *syscall.Stat_t (unexpected on these platforms), mode falls back to the
// portable bits and the syscall-only fields are marked omitted rather than
// guessed.
func statSignature(info fs.FileInfo) worktree.StatSignature {
	sig := worktree.StatSignature{
		Size:    info.Size(),
		MtimeNs: info.ModTime().UnixNano(),
		Mode:    uint32(info.Mode()),
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		sig.Omitted = sig.Omitted.
			With(worktree.FieldCtime).
			With(worktree.FieldDev).
			With(worktree.FieldIno).
			With(worktree.FieldNlink)
		return sig
	}
	// The widths of these fields vary by architecture as well as by operating system:
	// st_mode is uint16 on darwin and freebsd and uint32 on linux, st_dev is int32 on
	// darwin and uint64 on linux and freebsd, and st_nlink alone is 16, 32 and 64 bits
	// across the release matrix. Each conversion below is therefore mandatory on at least
	// one target and a no-op on at least one other. unconvert analyses one target at a
	// time and reports the no-op it happens to see, so each site carries a directive
	// rather than losing a conversion the rest of the matrix needs. Ino is uint64
	// everywhere and needs no conversion at all; if a platform ever changes that, the
	// build says so.
	sig.Mode = uint32(st.Mode) //nolint:unconvert // required on darwin and freebsd (uint16)
	sig.CtimeNs = statCtimeNs(st)
	sig.Dev = uint64(st.Dev) //nolint:unconvert // required on darwin (int32)
	sig.Ino = st.Ino
	sig.Nlink = uint64(st.Nlink) //nolint:unconvert // required on darwin (uint16) and linux/arm64 (uint32)
	return sig
}
