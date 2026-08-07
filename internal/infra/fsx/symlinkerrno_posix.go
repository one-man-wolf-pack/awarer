//go:build darwin || linux

package fsx

import (
	"errors"

	"golang.org/x/sys/unix"
)

// isNoFollowSymlinkErrno reports whether err is the errno an O_NOFOLLOW open reports
// when the component it refused is a symlink. Here that is POSIX's ELOOP, so the
// normalization noFollowErr performs is the identity — the file exists so its FreeBSD
// counterpart has one place to differ, not because these platforms need adapting.
func isNoFollowSymlinkErrno(err error) bool {
	return errors.Is(err, unix.ELOOP)
}
