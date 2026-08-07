//go:build !unix && !windows

package worktreemut

import (
	"errors"
	"syscall"
)

// isNotEmpty reports whether err is the platform refusing to remove a non-empty
// directory, on a target that is neither unix nor windows.
//
// Nothing in the shipped matrix reaches this file: every target in CROSS_TARGETS is
// covered by the unix build tag or by notempty_windows.go. It is a fallback, not a
// checked platform — the POSIX errno values here are the best available guess, and the
// safe direction is the one they fail in. An unrecognized error stays a generic I/O
// failure rather than being reported as the deletion refusal, because that refusal is
// what protects unobserved user content and claiming it without proof would be worse
// than under-reporting it.
//
// It does not make the package build everywhere, and it never did: plan9 has no
// syscall.ENOTEMPTY at all, so this file does not compile there. That is unchanged by
// the windows split and out of the supported matrix either way.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
