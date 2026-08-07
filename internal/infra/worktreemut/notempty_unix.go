//go:build unix

package worktreemut

import (
	"errors"
	"syscall"
)

// isNotEmpty reports whether err is the kernel refusing to remove a non-empty
// directory. POSIX allows either ENOTEMPTY or EEXIST for this, so both are
// recognized: misclassifying it would turn the one deletion refusal that protects
// unobserved user content into a generic I/O failure.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
