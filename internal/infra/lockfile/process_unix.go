//go:build darwin || linux || freebsd

package lockfile

import (
	"errors"
	"syscall"
)

// ProcessAlive reports whether a process with the given pid exists on this host.
// It sends the null signal (0), which performs error checking without delivering a
// signal: a nil error or EPERM (the process exists but we may not signal it) means
// alive; ESRCH means no such process. A non-positive pid is never alive. This is
// the owner-liveness evidence Classify uses for same-host stale detection.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
