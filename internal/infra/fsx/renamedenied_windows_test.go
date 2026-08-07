//go:build windows

package fsx

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// renameDeniedStatuses are the Win32 statuses that mean "an open handle stopped the
// fixture from replacing this directory's pathname" — the platform enforcing, one layer
// below awa, the outcome the anchoring oracle is asking about.
//
// Go opens directories with FILE_SHARE_READ|FILE_SHARE_WRITE and not FILE_SHARE_DELETE,
// so while the test holds its descriptor the directory cannot be renamed. A fixture that
// cannot inject its substitution has not found a hole; it has met the guarantee from the
// other side.
//
// Three statuses, because the refusal arrives from different calls: the held handle
// surfaces as a sharing violation, a byte-range lock as a lock violation, and some
// builds report the same conflict as a plain access denial. Narrowing the set on the
// argument that this call is only a rename would be guessing which refusal a given
// Windows build reports, and guessing low turns an expected platform refusal into a red
// lane.
var renameDeniedStatuses = []syscall.Errno{
	windows.ERROR_SHARING_VIOLATION,
	windows.ERROR_LOCK_VIOLATION,
	windows.ERROR_ACCESS_DENIED,
}

// isRenameDenied reports whether err is Windows refusing to rename a directory this
// test holds a descriptor on.
//
// Matching is by wrapped status identity, never message text: the runner is not
// guaranteed to be English. An unrecognized status falls through so the caller fails
// rather than recording a broken fixture as platform evidence.
func isRenameDenied(err error) bool {
	for _, status := range renameDeniedStatuses {
		if errors.Is(err, status) {
			return true
		}
	}
	return false
}
