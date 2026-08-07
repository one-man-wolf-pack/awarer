//go:build darwin || freebsd

package worktreefs

import "syscall"

// statCtimeNs returns the change-time in nanoseconds. Darwin and FreeBSD both name the
// field Ctimespec; Linux's Ctim spelling lives in stat_ctim.go.
func statCtimeNs(st *syscall.Stat_t) int64 {
	return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec
}
