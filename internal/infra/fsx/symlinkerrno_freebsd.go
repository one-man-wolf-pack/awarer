//go:build freebsd

package fsx

import (
	"errors"

	"golang.org/x/sys/unix"
)

// isNoFollowSymlinkErrno reports whether err is the errno an O_NOFOLLOW open reports
// when the component it refused is a symlink.
//
// FreeBSD's open(2) does not use POSIX's ELOOP for this case: its ERRORS section reads
// "[EMLINK] O_NOFOLLOW was specified and the target is a symbolic link". ELOOP stays
// accepted because it remains the answer for a genuine resolution loop reached before
// the no-follow component.
//
// This is the whole FreeBSD-specific surface of the filesystem boundary. It is a
// predicate over one operation's errno rather than a classification of EMLINK in
// general: noFollowErr calls it only at the no-follow opens, so an EMLINK from
// linkat — a real link-count limit — keeps its own meaning everywhere.
func isNoFollowSymlinkErrno(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK)
}
