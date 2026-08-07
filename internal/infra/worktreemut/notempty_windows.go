//go:build windows

package worktreemut

import (
	"errors"
	"syscall"
)

// isNotEmpty reports whether err is Windows refusing to remove a non-empty directory.
//
// This needs its own file because the POSIX errno values do not reach here. Windows
// declares ENOTEMPTY and EEXIST as synthetic APPLICATION_ERROR constants the kernel
// never returns, and syscall.Errno.Is only answers the four portable os targets
// (ErrPermission, ErrExist, ErrNotExist, ErrUnsupported) — never a raw errno — so
// matching them compiles and always reports false. RemoveDirectory returns
// ERROR_DIR_NOT_EMPTY instead, and that is the identity to match.
//
// fs.ErrExist would also match, because Errno.Is maps ERROR_DIR_NOT_EMPTY onto it, but
// it maps ERROR_ALREADY_EXISTS and ERROR_FILE_EXISTS there too. The one deletion refusal
// that protects unobserved user content has to stay distinguishable from an unrelated
// "already exists", so the wrapped status is matched directly.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY)
}
