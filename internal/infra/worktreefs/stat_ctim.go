//go:build linux

package worktreefs

import "syscall"

// statCtimeNs returns the change-time in nanoseconds. Linux alone names the field Ctim;
// darwin and freebsd both spell it Ctimespec, in stat_ctimespec.go. The split is by
// field name because that is the entire difference — the signature it feeds is one
// implementation in stat_unix.go.
func statCtimeNs(st *syscall.Stat_t) int64 {
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec
}
